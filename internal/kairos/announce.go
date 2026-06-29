package kairos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

// Announcement is one row of kairos.announcements (migration 0027,
// spec §3.4). JSON tags are camelCase and must match ApiAnnouncement
// in kairos/lib/kairos-api.ts exactly — the FE client file is the
// contract. Raw carries the source NSE payload into the jsonb column
// and never crosses the API (json:"-").
type Announcement struct {
	ID            int64           `json:"id"`
	NseID         string          `json:"nseId,omitempty"`
	Symbol        string          `json:"symbol,omitempty"`
	Company       string          `json:"company,omitempty"`
	Category      string          `json:"category,omitempty"`
	Headline      string          `json:"headline"`
	AttachmentURL string          `json:"attachmentUrl,omitempty"`
	AnnouncedAt   time.Time       `json:"announcedAt"`
	Raw           json.RawMessage `json:"-"`
}

// NSEError is the typed failure every NSEClient path returns — the
// cron logs it and records a failed ingest run; nothing panics on a
// flaky NSE day (403s and 5xxs from their WAF are routine).
type NSEError struct {
	Op     string // "warmup" | "fetch" | "decode"
	Status int    // HTTP status when the failure was a bad status, else 0
	Err    error  // underlying error, nil when Status tells the story
}

func (e *NSEError) Error() string {
	msg := "nse " + e.Op
	if e.Status != 0 {
		msg += fmt.Sprintf(": status %d", e.Status)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *NSEError) Unwrap() error { return e.Err }

const (
	nseBaseURL          = "https://www.nseindia.com"
	nseAnnouncementsAPI = "/api/corporate-announcements?index=equities"

	// nseTimeout bounds each HTTP request (warm-up and fetch each get
	// the full budget; the cron fires every 10 minutes, so worst case
	// 30s is fine).
	nseTimeout = 15 * time.Second

	// nseUserAgent is a realistic Chrome UA — NSE's WAF rejects
	// default Go user agents outright.
	nseUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// nseIST is the exchange timezone for parsing announcement dates.
// Same load-or-fixed-offset fallback as internal/cron/jobs.IST.
var nseIST = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return loc
	}
	return time.FixedZone("IST", int((5*time.Hour + 30*time.Minute).Seconds()))
}()

// NSEClient fetches the NSE corporate-announcements feed. NSE's API
// requires session cookies issued by the homepage, so the client
// carries a cookie jar and warms it with a GET to nseindia.com before
// every feed fetch (jarred cookies expire quickly enough that warming
// per-fetch is simpler and safer than tracking expiry).
type NSEClient struct {
	http *http.Client
	base string // swap-out point for tests
}

// NewNSEClient builds a client with a fresh cookie jar and the
// standard timeout.
func NewNSEClient() *NSEClient {
	jar, _ := cookiejar.New(nil) // err is always nil with nil options
	return &NSEClient{
		http: &http.Client{Timeout: nseTimeout, Jar: jar},
		base: nseBaseURL,
	}
}

// nseAnnouncement is the subset of NSE's feed item we consume.
// seq_id is a flexString because NSE has served it both as a JSON
// string and as a number over time.
type nseAnnouncement struct {
	Symbol     string     `json:"symbol"`
	Desc       string     `json:"desc"`
	CompanyNm  string     `json:"sm_name"`
	AttachFile string     `json:"attchmntFile"`
	AttachText string     `json:"attchmntText"`
	AnDt       string     `json:"an_dt"`     // "12-Jun-2026 18:30:45" (IST)
	SortDate   string     `json:"sort_date"` // "2026-06-12 18:30:45" (IST)
	SeqID      flexString `json:"seq_id"`
}

// flexString unmarshals a JSON string or number into a string.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// FetchAnnouncements warms the cookie jar against the NSE homepage,
// then fetches and maps the equities corporate-announcements feed.
// Every failure path returns *NSEError; it never panics.
//
// nse_id choice: NSE's seq_id — the feed's own per-announcement
// sequence number, stable across re-fetches and unique per filing
// (the symbol/date/subject trio is NOT unique: a company can file two
// "Updates" in the same second). Rows missing seq_id fall back to a
// SHA-256 of (symbol, an_dt, desc, attachment) so dedup still works
// deterministically.
func (c *NSEClient) FetchAnnouncements(ctx context.Context) ([]Announcement, error) {
	if err := c.warmup(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+nseAnnouncementsAPI, nil)
	if err != nil {
		return nil, &NSEError{Op: "fetch", Err: err}
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", c.base+"/companies-listing/corporate-filings-announcements")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &NSEError{Op: "fetch", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain so the connection is reusable, then report. 401/403
		// from the WAF are the common case here.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, &NSEError{Op: "fetch", Status: resp.StatusCode}
	}

	// Decode to raw items first so each Announcement keeps its exact
	// source payload for the jsonb raw column.
	var items []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, &NSEError{Op: "decode", Err: err}
	}
	now := time.Now().In(nseIST)
	out := make([]Announcement, 0, len(items))
	for _, raw := range items {
		var it nseAnnouncement
		if err := json.Unmarshal(raw, &it); err != nil {
			return nil, &NSEError{Op: "decode", Err: err}
		}
		out = append(out, mapNSEAnnouncement(it, raw, now))
	}
	return out, nil
}

// warmup GETs the NSE homepage to collect the session cookies the API
// endpoint requires. Non-2xx is a failure — without cookies the API
// call is guaranteed to 401.
func (c *NSEClient) warmup(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/", nil)
	if err != nil {
		return &NSEError{Op: "warmup", Err: err}
	}
	c.setHeaders(req)
	req.Header.Set("Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	resp, err := c.http.Do(req)
	if err != nil {
		return &NSEError{Op: "warmup", Err: err}
	}
	defer resp.Body.Close()
	// Drain (capped) so keep-alive can reuse the connection for the
	// API call — same TLS session matters to NSE's WAF.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &NSEError{Op: "warmup", Status: resp.StatusCode}
	}
	return nil
}

// setHeaders applies the browser-shaped headers both requests share.
func (c *NSEClient) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", nseUserAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Connection", "keep-alive")
}

// mapNSEAnnouncement converts one feed item. Headline prefers the
// attachment text (the human-readable subject line) over desc (the
// filing category, e.g. "Acquisition") so the FE always has something
// to show; announced-at falls back an_dt → sort_date → now so the row
// still enters the windowed feed when NSE's date strings misbehave.
func mapNSEAnnouncement(it nseAnnouncement, raw json.RawMessage, now time.Time) Announcement {
	headline := strings.TrimSpace(it.AttachText)
	if headline == "" {
		headline = strings.TrimSpace(it.Desc)
	}
	nseID := strings.TrimSpace(string(it.SeqID))
	if nseID == "" {
		sum := sha256.Sum256([]byte(it.Symbol + "|" + it.AnDt + "|" + it.Desc + "|" + it.AttachFile))
		nseID = "sha-" + hex.EncodeToString(sum[:16])
	}
	return Announcement{
		NseID:         nseID,
		Symbol:        strings.TrimSpace(it.Symbol),
		Company:       strings.TrimSpace(it.CompanyNm),
		Category:      strings.TrimSpace(it.Desc),
		Headline:      headline,
		AttachmentURL: strings.TrimSpace(it.AttachFile),
		AnnouncedAt:   parseNSETime(it.AnDt, it.SortDate, now),
		Raw:           raw,
	}
}

// parseNSETime tries NSE's two observed date layouts on an_dt then
// sort_date, in IST; falls back to now.
func parseNSETime(anDt, sortDate string, now time.Time) time.Time {
	layouts := []string{"02-Jan-2006 15:04:05", "2006-01-02 15:04:05"}
	for _, v := range []string{strings.TrimSpace(anDt), strings.TrimSpace(sortDate)} {
		if v == "" {
			continue
		}
		for _, layout := range layouts {
			if t, err := time.ParseInLocation(layout, v, nseIST); err == nil {
				return t
			}
		}
	}
	return now
}
