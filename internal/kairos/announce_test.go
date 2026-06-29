package kairos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestParseNSETime pins the date-parsing ladder: both observed NSE
// layouts, an_dt preferred over sort_date, and the now fallback when
// neither string parses. Expectations are built in nseIST so a parse
// in the wrong zone shifts the instant and fails .Equal.
func TestParseNSETime(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, nseIST)
	ist := func(y int, mo time.Month, d, h, mi, s int) time.Time {
		return time.Date(y, mo, d, h, mi, s, 0, nseIST)
	}
	cases := []struct {
		name     string
		anDt     string
		sortDate string
		want     time.Time
	}{
		{"an_dt NSE layout", "12-Jun-2026 18:30:45", "", ist(2026, time.June, 12, 18, 30, 45)},
		{"an_dt ISO-ish layout", "2026-06-12 18:30:45", "", ist(2026, time.June, 12, 18, 30, 45)},
		{"an_dt wins over sort_date", "12-Jun-2026 18:30:45", "2026-06-11 09:00:00", ist(2026, time.June, 12, 18, 30, 45)},
		{"bad an_dt falls back to sort_date", "not a date", "2026-06-11 09:15:00", ist(2026, time.June, 11, 9, 15, 0)},
		{"empty an_dt falls back to sort_date", "", "11-Jun-2026 09:15:00", ist(2026, time.June, 11, 9, 15, 0)},
		{"surrounding whitespace trimmed", "  12-Jun-2026 18:30:45  ", "", ist(2026, time.June, 12, 18, 30, 45)},
		{"both unparseable falls back to now", "garbage", "also garbage", now},
		{"both empty falls back to now", "", "", now},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseNSETime(c.anDt, c.sortDate, now)
			if !got.Equal(c.want) {
				t.Errorf("parseNSETime(%q, %q) = %s, want %s", c.anDt, c.sortDate, got, c.want)
			}
		})
	}
}

// TestFlexStringUnmarshalJSON: NSE has served seq_id as a string, a
// number, and null over time — all three must land as a string; a
// JSON type that's neither must error rather than silently coerce.
func TestFlexStringUnmarshalJSON(t *testing.T) {
	cases := []struct {
		in      string
		want    flexString
		wantErr bool
	}{
		{`"1234567"`, "1234567", false},
		{`1234567`, "1234567", false},
		{`12.5`, "12.5", false},
		{`null`, "", false},
		{`""`, "", false},
		{`true`, "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			var f flexString
			err := json.Unmarshal([]byte(c.in), &f)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s) = %q, want error", c.in, f)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s): %v", c.in, err)
			}
			if f != c.want {
				t.Errorf("Unmarshal(%s) = %q, want %q", c.in, f, c.want)
			}
		})
	}
}

// TestFlexStringInFeedItem: the same string/number flexibility through
// the real feed-item struct, where it actually matters.
func TestFlexStringInFeedItem(t *testing.T) {
	for _, raw := range []string{`{"seq_id":"777"}`, `{"seq_id":777}`} {
		var it nseAnnouncement
		if err := json.Unmarshal([]byte(raw), &it); err != nil {
			t.Fatalf("Unmarshal(%s): %v", raw, err)
		}
		if it.SeqID != "777" {
			t.Errorf("Unmarshal(%s) seq_id = %q, want 777", raw, it.SeqID)
		}
	}
	var it nseAnnouncement
	if err := json.Unmarshal([]byte(`{"seq_id":null}`), &it); err != nil {
		t.Fatalf("Unmarshal null seq_id: %v", err)
	}
	if it.SeqID != "" {
		t.Errorf("null seq_id = %q, want empty", it.SeqID)
	}
}

// TestMapNSEAnnouncementHeadline: attchmntText (the human subject
// line) wins; blank attachment text falls back to desc.
func TestMapNSEAnnouncementHeadline(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, nseIST)
	it := nseAnnouncement{Symbol: "RELIANCE", Desc: "Acquisition",
		AttachText: "  Reliance acquires X  ", SeqID: "1"}

	got := mapNSEAnnouncement(it, nil, now)
	if got.Headline != "Reliance acquires X" {
		t.Errorf("headline = %q, want attachment text (trimmed)", got.Headline)
	}
	if got.Category != "Acquisition" {
		t.Errorf("category = %q, want desc", got.Category)
	}

	it.AttachText = "   " // whitespace-only must not count as a headline
	got = mapNSEAnnouncement(it, nil, now)
	if got.Headline != "Acquisition" {
		t.Errorf("headline = %q, want fallback to desc", got.Headline)
	}
}

// TestMapNSEAnnouncementNseID: seq_id (trimmed) is the dedup key when
// present; otherwise a deterministic sha-256 over the identifying
// quad, recomputed here from the same recipe so the derivation is
// pinned, not just its shape.
func TestMapNSEAnnouncementNseID(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, nseIST)
	it := nseAnnouncement{Symbol: "TCS", Desc: "Updates",
		AnDt: "12-Jun-2026 10:00:00", AttachFile: "https://x/doc.pdf"}

	got := mapNSEAnnouncement(it, nil, now)
	sum := sha256.Sum256([]byte("TCS|12-Jun-2026 10:00:00|Updates|https://x/doc.pdf"))
	if want := "sha-" + hex.EncodeToString(sum[:16]); got.NseID != want {
		t.Errorf("fallback nse_id = %q, want %q", got.NseID, want)
	}
	if again := mapNSEAnnouncement(it, nil, now); again.NseID != got.NseID {
		t.Error("fallback nse_id is not deterministic across calls")
	}
	it2 := it
	it2.Desc = "Updates II"
	if mapNSEAnnouncement(it2, nil, now).NseID == got.NseID {
		t.Error("different filings must hash to different nse_ids")
	}

	it.SeqID = "  424242  "
	if id := mapNSEAnnouncement(it, nil, now).NseID; id != "424242" {
		t.Errorf("nse_id with seq_id present = %q, want trimmed seq_id", id)
	}
}

// TestMapNSEAnnouncementTrimAndFallbacks: every string field is
// trimmed, announced-at falls back to now when both date strings are
// empty, and the raw payload passes through untouched for the jsonb
// column.
func TestMapNSEAnnouncementTrimAndFallbacks(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, nseIST)
	raw := json.RawMessage(`{"k":"v"}`)
	it := nseAnnouncement{
		Symbol:     "  INFY  ",
		CompanyNm:  " Infosys Limited ",
		Desc:       " Updates ",
		AttachFile: " https://x/doc.pdf ",
		SeqID:      " 9 ",
	}
	got := mapNSEAnnouncement(it, raw, now)
	checks := []struct{ name, got, want string }{
		{"symbol", got.Symbol, "INFY"},
		{"company", got.Company, "Infosys Limited"},
		{"category", got.Category, "Updates"},
		{"headline", got.Headline, "Updates"},
		{"attachmentUrl", got.AttachmentURL, "https://x/doc.pdf"},
		{"nseId", got.NseID, "9"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if !got.AnnouncedAt.Equal(now) {
		t.Errorf("announcedAt = %s, want now fallback %s", got.AnnouncedAt, now)
	}
	if string(got.Raw) != `{"k":"v"}` {
		t.Errorf("raw = %s, want source payload unchanged", got.Raw)
	}
}

// TestNSEErrorFormatting pins the typed error's message shape and
// Unwrap, which the cron's log lines and errors.As callers rely on.
func TestNSEErrorFormatting(t *testing.T) {
	if got := (&NSEError{Op: "fetch", Status: 403}).Error(); got != "nse fetch: status 403" {
		t.Errorf("Error() = %q, want %q", got, "nse fetch: status 403")
	}
	inner := errors.New("boom")
	e := &NSEError{Op: "decode", Err: inner}
	if got := e.Error(); got != "nse decode: boom" {
		t.Errorf("Error() = %q, want %q", got, "nse decode: boom")
	}
	if !errors.Is(e, inner) {
		t.Error("NSEError must unwrap to the underlying error")
	}
}

// newTestNSEClient builds an NSEClient pointed at a test server via
// the base field — the swap-out point left for exactly this.
func newTestNSEClient(t *testing.T, base string) *NSEClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &NSEClient{
		http: &http.Client{Timeout: 5 * time.Second, Jar: jar},
		base: base,
	}
}

// TestNSEClientFetchAnnouncements walks the full happy path against a
// fake NSE: warm-up issues the session cookie, the feed fetch must
// present it back (or 401, like the real WAF), both requests carry
// the Chrome UA, and the mapped rows come out with seq_id handled as
// string and number.
func TestNSEClientFetchAnnouncements(t *testing.T) {
	const feed = `[
		{"symbol":"RELIANCE","desc":"Acquisition","sm_name":"Reliance Industries Limited","attchmntFile":"https://archives.nseindia.com/doc.pdf","attchmntText":"Reliance acquires X","an_dt":"12-Jun-2026 18:30:45","sort_date":"2026-06-12 18:30:45","seq_id":"1234567"},
		{"symbol":"TCS","desc":"Updates","sm_name":"Tata Consultancy Services","attchmntFile":"","attchmntText":"","an_dt":"","sort_date":"2026-06-11 09:15:00","seq_id":9988776}
	]`
	var warmups, fetches int
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		warmups++
		if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "Chrome/") {
			t.Errorf("warmup User-Agent = %q, want a Chrome UA", ua)
		}
		http.SetCookie(w, &http.Cookie{Name: "nsit", Value: "session"})
		_, _ = w.Write([]byte("<html>nse</html>"))
	})
	mux.HandleFunc("/api/corporate-announcements", func(w http.ResponseWriter, r *http.Request) {
		fetches++
		if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "Chrome/") {
			t.Errorf("fetch User-Agent = %q, want a Chrome UA", ua)
		}
		if got := r.URL.Query().Get("index"); got != "equities" {
			t.Errorf("index = %q, want equities", got)
		}
		if ref := r.Header.Get("Referer"); !strings.Contains(ref, "/companies-listing/") {
			t.Errorf("Referer = %q, want the filings page", ref)
		}
		// The whole point of warm-up: no session cookie, no feed.
		if c, err := r.Cookie("nsit"); err != nil || c.Value != "session" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(feed))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestNSEClient(t, srv.URL).FetchAnnouncements(context.Background())
	if err != nil {
		t.Fatalf("FetchAnnouncements: %v", err)
	}
	if warmups != 1 || fetches != 1 {
		t.Errorf("warmups = %d, fetches = %d, want 1 and 1", warmups, fetches)
	}
	if len(got) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(got))
	}

	r0 := got[0]
	if r0.NseID != "1234567" || r0.Symbol != "RELIANCE" || r0.Company != "Reliance Industries Limited" {
		t.Errorf("row 0 identity = %q/%q/%q, want 1234567/RELIANCE/Reliance Industries Limited",
			r0.NseID, r0.Symbol, r0.Company)
	}
	if r0.Headline != "Reliance acquires X" || r0.Category != "Acquisition" {
		t.Errorf("row 0 headline/category = %q/%q", r0.Headline, r0.Category)
	}
	if want := time.Date(2026, time.June, 12, 18, 30, 45, 0, nseIST); !r0.AnnouncedAt.Equal(want) {
		t.Errorf("row 0 announcedAt = %s, want %s", r0.AnnouncedAt, want)
	}
	if !strings.Contains(string(r0.Raw), `"symbol":"RELIANCE"`) {
		t.Errorf("row 0 raw = %s, want the source item", r0.Raw)
	}

	r1 := got[1]
	if r1.NseID != "9988776" { // numeric seq_id from the wire
		t.Errorf("row 1 nseId = %q, want 9988776", r1.NseID)
	}
	if r1.Headline != "Updates" { // empty attchmntText falls back to desc
		t.Errorf("row 1 headline = %q, want Updates", r1.Headline)
	}
	if want := time.Date(2026, time.June, 11, 9, 15, 0, 0, nseIST); !r1.AnnouncedAt.Equal(want) {
		t.Errorf("row 1 announcedAt = %s, want sort_date fallback %s", r1.AnnouncedAt, want)
	}
}

// TestNSEClientWarmupStatusError: a WAF'd homepage surfaces as
// *NSEError{Op:warmup} with the status, and the feed is never hit.
func TestNSEClientWarmupStatusError(t *testing.T) {
	var fetches int
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/api/corporate-announcements", func(http.ResponseWriter, *http.Request) {
		fetches++
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := newTestNSEClient(t, srv.URL).FetchAnnouncements(context.Background())
	var nseErr *NSEError
	if !errors.As(err, &nseErr) {
		t.Fatalf("err = %v (%T), want *NSEError", err, err)
	}
	if nseErr.Op != "warmup" || nseErr.Status != http.StatusForbidden {
		t.Errorf("NSEError = {Op:%q Status:%d}, want {Op:warmup Status:403}", nseErr.Op, nseErr.Status)
	}
	if fetches != 0 {
		t.Errorf("feed fetched %d times after failed warmup, want 0", fetches)
	}
}

// TestNSEClientFetchStatusError: warm-up fine, feed 401s → typed
// fetch error with the status.
func TestNSEClientFetchStatusError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/corporate-announcements", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := newTestNSEClient(t, srv.URL).FetchAnnouncements(context.Background())
	var nseErr *NSEError
	if !errors.As(err, &nseErr) {
		t.Fatalf("err = %v (%T), want *NSEError", err, err)
	}
	if nseErr.Op != "fetch" || nseErr.Status != http.StatusUnauthorized {
		t.Errorf("NSEError = {Op:%q Status:%d}, want {Op:fetch Status:401}", nseErr.Op, nseErr.Status)
	}
}

// TestNSEClientDecodeError: a 200 with a non-array body (NSE serves
// HTML error pages with 200 on bad days) is a typed decode error.
func TestNSEClientDecodeError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/corporate-announcements", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"not":"an array"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := newTestNSEClient(t, srv.URL).FetchAnnouncements(context.Background())
	var nseErr *NSEError
	if !errors.As(err, &nseErr) {
		t.Fatalf("err = %v (%T), want *NSEError", err, err)
	}
	if nseErr.Op != "decode" || nseErr.Err == nil || nseErr.Status != 0 {
		t.Errorf("NSEError = {Op:%q Status:%d Err:%v}, want decode with underlying error", nseErr.Op, nseErr.Status, nseErr.Err)
	}
}

// TestNSEClientConnectionError: transport-level failures are typed
// too — never a bare net error escaping to the cron.
func TestNSEClientConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening anymore

	_, err := newTestNSEClient(t, url).FetchAnnouncements(context.Background())
	var nseErr *NSEError
	if !errors.As(err, &nseErr) {
		t.Fatalf("err = %v (%T), want *NSEError", err, err)
	}
	if nseErr.Op != "warmup" || nseErr.Err == nil {
		t.Errorf("NSEError = {Op:%q Err:%v}, want warmup with underlying error", nseErr.Op, nseErr.Err)
	}
}
