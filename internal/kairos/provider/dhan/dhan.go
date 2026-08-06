// Package dhan is the Dhan API BrokerProvider implementation.
//
// Dhan uses a long-lived access token (~1 year) obtained from the Dhan
// developer console — no daily OAuth exchange required. Auth is simply
// two static env vars (KAIROS_DHAN_ACCESS_TOKEN + KAIROS_DHAN_CLIENT_ID).
//
// API reference: https://dhanhq.co/docs/v2/
// Rate limits: Dhan doesn't publish hard per-second limits; we use
// conservative values (5 rps for market data, 3 rps for historical)
// to stay well within undocumented thresholds.
//
// Instrument IDs: Dhan uses numeric security IDs instead of Kite's
// tokens. The three underlying indices are hardcoded:
//
//	NIFTY 50    → securityId=13,  segment="IDX_I"
//	NIFTY BANK  → securityId=25,  segment="IDX_I"
//	SENSEX      → securityId=51,  segment="IDX_I"
//
// Option contract IDs come from the daily security master CSV cached in
// memory (same 12h refresh pattern as the kite package).
package dhan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

const (
	dhanAPIBase      = "https://api.dhan.co/v2"
	instrumentsCSVURL = "https://images.dhan.co/api-data/api-scrip-master.csv"
)

// Compile-time capability assertions.
var (
	_ provider.QuoteFetcher       = (*Provider)(nil)
	_ provider.InstrumentSearcher = (*Provider)(nil)
)

func init() {
	provider.Register("dhan", New)
}

// underlyingMeta holds the Dhan API parameters for each supported index.
type underlyingMeta struct {
	SecurityID int
	Segment    string // "IDX_I" for all three
	Name       string // matches SEM_ENTITY_NAME in the instruments CSV
}

var underlyingMap = map[provider.Underlying]underlyingMeta{
	provider.UnderlyingNIFTY:     {13, "IDX_I", "NIFTY"},
	provider.UnderlyingBANKNIFTY: {25, "IDX_I", "BANKNIFTY"},
	provider.UnderlyingSENSEX:    {51, "IDX_I", "SENSEX"},
}

// Provider is the Dhan API data client. One instance per process.
type Provider struct {
	accessToken string
	clientID    string
	httpc       *http.Client
	logger      *slog.Logger

	quoteLim *rate.Limiter // 5 rps — market data
	histLim  *rate.Limiter // 3 rps — historical charts

	// Instruments cache: Dhan's security master CSV (~daily refresh).
	instrumentsMu   sync.RWMutex
	instruments     instrumentIndex
	instrumentsAsOf time.Time
}

// New constructs the Dhan provider. Missing credentials are warned but
// not fatal — the server boots and surfaces the problem via IsReady /
// /kairos/provider/status.
func New(cfg *config.Config, _ *pgxpool.Pool, logger *slog.Logger) (provider.BrokerProvider, error) {
	if cfg.KairosDhanAccessToken == "" || cfg.KairosDhanClientID == "" {
		logger.Warn("dhan provider missing creds; data fetches will return ErrNotConfigured",
			"have_token", cfg.KairosDhanAccessToken != "",
			"have_client_id", cfg.KairosDhanClientID != "")
	}
	return &Provider{
		accessToken: cfg.KairosDhanAccessToken,
		clientID:    cfg.KairosDhanClientID,
		httpc:       &http.Client{Timeout: 30 * time.Second},
		logger:      logger,
		quoteLim:    rate.NewLimiter(5, 1),
		histLim:     rate.NewLimiter(3, 1),
	}, nil
}

func (p *Provider) Name() string { return "dhan" }

// IsReady checks that both credentials are configured. No network call —
// Dhan tokens are long-lived; we don't cache them in provider_tokens.
func (p *Provider) IsReady(_ context.Context) error {
	if p.accessToken == "" || p.clientID == "" {
		return provider.ErrNotConfigured
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// Core BrokerProvider methods
// ─────────────────────────────────────────────────────────────────────────

// FetchSpot calls POST /v2/marketfeed/ltp with the index security ID.
func (p *Provider) FetchSpot(ctx context.Context, u provider.Underlying) (provider.Spot, error) {
	meta, ok := underlyingMap[u]
	if !ok {
		return provider.Spot{}, provider.ErrUnknownUnderlying
	}
	if err := p.quoteLim.Wait(ctx); err != nil {
		return provider.Spot{}, err
	}
	body := map[string][]int{meta.Segment: {meta.SecurityID}}
	var resp struct {
		Status string                          `json:"status"`
		Data   map[string]map[string]ltpEntry `json:"data"`
	}
	if err := p.postJSON(ctx, "/marketfeed/ltp", body, &resp); err != nil {
		return provider.Spot{}, err
	}
	segData, ok := resp.Data[meta.Segment]
	if !ok {
		return provider.Spot{}, fmt.Errorf("dhan: segment %s absent in LTP response", meta.Segment)
	}
	entry, ok := segData[fmt.Sprintf("%d", meta.SecurityID)]
	if !ok {
		return provider.Spot{}, fmt.Errorf("dhan: security %d absent in LTP response", meta.SecurityID)
	}
	return provider.Spot{
		Underlying: u,
		Price:      entry.LastPrice,
		Change:     entry.Change,
		ChangePct:  entry.ChangePct,
		AsOf:       time.Now(),
	}, nil
}

// FetchExpiries reads from the instruments CSV cache — no network call
// after the cache is warm (same pattern as kite).
func (p *Provider) FetchExpiries(ctx context.Context, u provider.Underlying) ([]provider.Expiry, error) {
	if err := p.ensureInstruments(ctx); err != nil {
		return nil, err
	}
	p.instrumentsMu.RLock()
	defer p.instrumentsMu.RUnlock()
	return p.instruments.Expiries(u), nil
}

// FetchOptionChain calls POST /v2/optionchain and translates the
// strike-keyed response into []ChainRow.
func (p *Provider) FetchOptionChain(ctx context.Context, u provider.Underlying, expiry time.Time) ([]provider.ChainRow, error) {
	meta, ok := underlyingMap[u]
	if !ok {
		return nil, provider.ErrUnknownUnderlying
	}
	if err := p.quoteLim.Wait(ctx); err != nil {
		return nil, err
	}
	body := map[string]any{
		"UnderlyingScrip": meta.SecurityID,
		"UnderlyingSeg":   meta.Segment,
		"Expiry":          expiry.Format("2006-01-02"),
	}
	var resp chainResp
	if err := p.postJSON(ctx, "/optionchain", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data.OC) == 0 {
		return nil, fmt.Errorf("dhan: empty option chain for %s exp=%s", u, expiry.Format("2006-01-02"))
	}
	return translateChain(u, expiry, resp), nil
}

// FetchHistoricalCandles dispatches to /v2/charts/intraday or
// /v2/charts/historical depending on the interval. The instrument is
// looked up in the instruments cache to obtain Dhan's security ID.
func (p *Provider) FetchHistoricalCandles(ctx context.Context, req provider.HistoricalReq) ([]provider.Candle, error) {
	if err := p.histLim.Wait(ctx); err != nil {
		return nil, err
	}
	if err := p.ensureInstruments(ctx); err != nil {
		return nil, err
	}
	ins, ok := p.instruments.ForSymbol(req.Instrument)
	if !ok {
		// Try "SEGMENT:SECURITY_ID" raw format for index candles.
		var err error
		ins, ok, err = parseRawInstrumentStr(req.Instrument)
		if err != nil || !ok {
			return nil, fmt.Errorf("dhan: instrument %q not found in cache", req.Instrument)
		}
	}
	if req.Interval == "day" {
		return p.fetchDailyCandles(ctx, ins, req.From, req.To)
	}
	return p.fetchIntradayCandles(ctx, ins, kiteToDhanInterval(req.Interval), req.From, req.To)
}

// ─────────────────────────────────────────────────────────────────────────
// QuoteFetcher capability
// ─────────────────────────────────────────────────────────────────────────

// FetchQuotes implements provider.QuoteFetcher via POST /v2/marketfeed/quote.
// Symbols are "EXCHANGE:TRADINGSYMBOL" or bare NSE symbols. Unknown
// symbols are silently omitted — same contract as the kite implementation.
func (p *Provider) FetchQuotes(ctx context.Context, symbols []string) ([]provider.Quote, error) {
	if err := p.ensureInstruments(ctx); err != nil {
		return nil, err
	}
	if err := p.quoteLim.Wait(ctx); err != nil {
		return nil, err
	}
	// Build Dhan's grouped-by-segment request body and a reverse lookup
	// so we can reconstruct the original symbol in the response.
	groups := map[string][]int{}    // segment → []securityID
	type keyMeta struct {
		symbol string
	}
	byKey := map[string]keyMeta{} // "SEGMENT:ID" → original symbol
	for _, sym := range symbols {
		ins, ok := p.resolveSymbol(sym)
		if !ok {
			continue
		}
		groups[ins.Segment] = append(groups[ins.Segment], int(ins.SecurityID))
		byKey[fmt.Sprintf("%s:%d", ins.Segment, ins.SecurityID)] = keyMeta{sym}
	}
	if len(groups) == 0 {
		return nil, nil
	}
	var resp quoteResp
	if err := p.postJSON(ctx, "/marketfeed/quote", groups, &resp); err != nil {
		return nil, err
	}
	out := make([]provider.Quote, 0, len(symbols))
	for seg, idMap := range resp.Data {
		for idStr, e := range idMap {
			km, ok := byKey[seg+":"+idStr]
			if !ok {
				continue
			}
			change := e.Change
			changePct := e.ChangePct
			if e.Close != 0 && change == 0 {
				change = e.LastPrice - e.Close
				changePct = change / e.Close * 100
			}
			out = append(out, provider.Quote{
				Symbol:    km.symbol,
				Last:      e.LastPrice,
				Change:    change,
				ChangePct: changePct,
				Volume:    e.Volume,
				OHLC: provider.OHLC{
					O: e.Open,
					H: e.High,
					L: e.Low,
					C: e.Close,
				},
				TS: time.Now(),
			})
		}
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────
// InstrumentSearcher capability
// ─────────────────────────────────────────────────────────────────────────

// SearchInstruments implements provider.InstrumentSearcher against the
// in-memory instruments cache. No network call once warm.
func (p *Provider) SearchInstruments(ctx context.Context, query string, limit int) ([]provider.Instrument, error) {
	if err := p.ensureInstruments(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxSearchResults {
		limit = maxSearchResults
	}
	p.instrumentsMu.RLock()
	defer p.instrumentsMu.RUnlock()
	return p.instruments.Search(query, limit), nil
}

// ─────────────────────────────────────────────────────────────────────────
// Historical helpers
// ─────────────────────────────────────────────────────────────────────────

type historicalBody struct {
	SecurityID      string `json:"securityId"`
	ExchangeSegment string `json:"exchangeSegment"`
	InstrumentType  string `json:"instrument"`
	ExpiryCode      int    `json:"expiryCode"`
	OType           string `json:"otype"`
	Strike          string `json:"strike"`
	FromDate        string `json:"fromDate"`
	ToDate          string `json:"toDate"`
}

type intradayBody struct {
	SecurityID      string `json:"securityId"`
	ExchangeSegment string `json:"exchangeSegment"`
	InstrumentType  string `json:"instrument"`
	Interval        string `json:"interval"`
	FromDate        string `json:"fromDate"`
	ToDate          string `json:"toDate"`
}

type ohlcvResp struct {
	Status    string    `json:"status"`
	Open      []float64 `json:"open"`
	High      []float64 `json:"high"`
	Low       []float64 `json:"low"`
	Close     []float64 `json:"close"`
	Volume    []int64   `json:"volume"`
	StartTime []int64   `json:"start_Time"`
}

func (p *Provider) fetchDailyCandles(ctx context.Context, ins instrument, from, to time.Time) ([]provider.Candle, error) {
	body := historicalBody{
		SecurityID:      fmt.Sprintf("%d", ins.SecurityID),
		ExchangeSegment: ins.Segment,
		InstrumentType:  ins.InstrType,
		ExpiryCode:      0,
		OType:           string(ins.OptionType),
		Strike:          strikeStr(ins.Strike),
		FromDate:        from.Format("2006-01-02"),
		ToDate:          to.Format("2006-01-02"),
	}
	var resp ohlcvResp
	if err := p.postJSON(ctx, "/charts/historical", body, &resp); err != nil {
		return nil, err
	}
	return assembleCandles(resp), nil
}

func (p *Provider) fetchIntradayCandles(ctx context.Context, ins instrument, interval string, from, to time.Time) ([]provider.Candle, error) {
	body := intradayBody{
		SecurityID:      fmt.Sprintf("%d", ins.SecurityID),
		ExchangeSegment: ins.Segment,
		InstrumentType:  ins.InstrType,
		Interval:        interval,
		FromDate:        from.Format("2006-01-02"),
		ToDate:          to.Format("2006-01-02"),
	}
	var resp ohlcvResp
	if err := p.postJSON(ctx, "/charts/intraday", body, &resp); err != nil {
		return nil, err
	}
	return assembleCandles(resp), nil
}

// assembleCandles zips the parallel OHLCV + timestamp arrays Dhan returns.
func assembleCandles(r ohlcvResp) []provider.Candle {
	n := len(r.StartTime)
	for _, s := range []int{len(r.Open), len(r.High), len(r.Low), len(r.Close)} {
		if s < n {
			n = s
		}
	}
	out := make([]provider.Candle, 0, n)
	for i := 0; i < n; i++ {
		vol := int64(0)
		if i < len(r.Volume) {
			vol = r.Volume[i]
		}
		out = append(out, provider.Candle{
			Time:   time.Unix(r.StartTime[i], 0),
			Open:   r.Open[i],
			High:   r.High[i],
			Low:    r.Low[i],
			Close:  r.Close[i],
			Volume: vol,
		})
	}
	return out
}

// kiteToDhanInterval maps Kite's interval vocabulary to Dhan's intraday
// interval strings. Dhan supports: 1, 5, 15, 25, 60 minutes.
func kiteToDhanInterval(kite string) string {
	switch kite {
	case "minute":
		return "1"
	case "3minute":
		return "1" // no direct 3m; fall back to 1m
	case "5minute":
		return "5"
	case "10minute":
		return "5" // closest
	case "15minute":
		return "15"
	case "30minute":
		return "25" // closest
	case "60minute":
		return "60"
	default:
		return "1"
	}
}

func strikeStr(s float64) string {
	if s == 0 {
		return ".00"
	}
	return fmt.Sprintf("%.2f", s)
}

// ─────────────────────────────────────────────────────────────────────────
// Option-chain translation
// ─────────────────────────────────────────────────────────────────────────

type chainResp struct {
	Status string `json:"status"`
	Data   struct {
		OC              map[string]strikeData `json:"oc"`
		UnderlyingValue float64               `json:"underlying_value"`
	} `json:"data"`
}

type strikeData struct {
	Call chainLeg `json:"call"`
	Put  chainLeg `json:"put"`
}

type chainLeg struct {
	OI       int64   `json:"OI"`
	OIChange int64   `json:"changeInOI"`
	Volume   int64   `json:"lastTradedVolume"`
	LTP      float64 `json:"LTP"`
	Bid      float64 `json:"bid"`
	Ask      float64 `json:"ask"`
}

func translateChain(u provider.Underlying, expiry time.Time, resp chainResp) []provider.ChainRow {
	spot := resp.Data.UnderlyingValue
	now := time.Now()
	out := make([]provider.ChainRow, 0, len(resp.Data.OC)*2)
	for strikeKey, sd := range resp.Data.OC {
		strike, err := parseStrikeKey(strikeKey)
		if err != nil {
			continue
		}
		for _, side := range []struct {
			leg provider.OptionType
			cl  chainLeg
		}{
			{provider.OptionTypeCE, sd.Call},
			{provider.OptionTypePE, sd.Put},
		} {
			if side.cl.LTP == 0 && side.cl.OI == 0 {
				continue
			}
			out = append(out, provider.ChainRow{
				Underlying:   u,
				ExpiryDate:   expiry,
				Strike:       strike,
				OptionType:   side.leg,
				SnapshotTime: now,
				Spot:         spot,
				LTP:          side.cl.LTP,
				Bid:          side.cl.Bid,
				Ask:          side.cl.Ask,
				OI:           side.cl.OI,
				OIChange:     side.cl.OIChange,
				Volume:       side.cl.Volume,
			})
		}
	}
	return out
}

func parseStrikeKey(s string) (int, error) {
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return 0, err
	}
	return int(f), nil
}

// ─────────────────────────────────────────────────────────────────────────
// Market feed quote types (FetchQuotes)
// ─────────────────────────────────────────────────────────────────────────

type ltpEntry struct {
	LastPrice float64 `json:"last_price"`
	Change    float64 `json:"change"`
	ChangePct float64 `json:"change_percentage"`
}

type quoteResp struct {
	Status string                            `json:"status"`
	Data   map[string]map[string]quoteEntry `json:"data"`
}

type quoteEntry struct {
	LastPrice float64 `json:"last_price"`
	Open      float64 `json:"open"`
	Close     float64 `json:"close"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Volume    int64   `json:"volume"`
	Change    float64 `json:"change"`
	ChangePct float64 `json:"change_percentage"`
}

// ─────────────────────────────────────────────────────────────────────────
// Symbol resolution
// ─────────────────────────────────────────────────────────────────────────

// resolveSymbol maps an "EXCHANGE:SYMBOL" or bare NSE symbol to a Dhan
// instrument (security ID + segment). Used by FetchQuotes.
func (p *Provider) resolveSymbol(sym string) (instrument, bool) {
	p.instrumentsMu.RLock()
	ins, ok := p.instruments.ForSymbol(sym)
	p.instrumentsMu.RUnlock()
	if ok {
		return ins, true
	}
	// Bare NSE equity (no colon): try "NSE:SYM".
	if !strings.Contains(sym, ":") {
		p.instrumentsMu.RLock()
		ins, ok = p.instruments.ForSymbol("NSE:" + sym)
		p.instrumentsMu.RUnlock()
		return ins, ok
	}
	return instrument{}, false
}

// parseRawInstrumentStr handles "SEGMENT:SECURITY_ID" pairs that callers
// may pass directly (e.g. "IDX_I:13" for a NIFTY index candle request).
func parseRawInstrumentStr(s string) (instrument, bool, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return instrument{}, false, nil
	}
	var id int64
	if _, err := fmt.Sscanf(parts[1], "%d", &id); err != nil {
		return instrument{}, false, nil
	}
	instrType := "EQUITY"
	switch parts[0] {
	case "IDX_I":
		instrType = "INDEX"
	case "NSE_FO", "BSE_FO":
		instrType = "OPTIDX"
	}
	return instrument{
		SecurityID: id,
		Segment:    parts[0],
		InstrType:  instrType,
	}, true, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Instruments cache
// ─────────────────────────────────────────────────────────────────────────

// ensureInstruments lazily loads the Dhan security master CSV.
// Refreshes every 12h — same cadence as the kite provider.
func (p *Provider) ensureInstruments(ctx context.Context) error {
	p.instrumentsMu.RLock()
	fresh := !p.instrumentsAsOf.IsZero() && time.Since(p.instrumentsAsOf) < 12*time.Hour
	p.instrumentsMu.RUnlock()
	if fresh {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", instrumentsCSVURL, nil)
	if err != nil {
		return err
	}
	res, err := p.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("dhan instruments fetch: %w", err)
	}
	defer res.Body.Close()
	idx, err := parseInstruments(res.Body)
	if err != nil {
		return fmt.Errorf("dhan instruments parse: %w", err)
	}
	p.instrumentsMu.Lock()
	p.instruments = idx
	p.instrumentsAsOf = time.Now()
	p.instrumentsMu.Unlock()
	p.logger.Info("dhan instruments refreshed",
		"options", len(idx.all),
		"searchable", len(idx.search))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// HTTP plumbing
// ─────────────────────────────────────────────────────────────────────────

func (p *Provider) newAuthedReq(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, dhanAPIBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("access-token", p.accessToken)
	req.Header.Set("client-id", p.clientID)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// postJSON marshals body as JSON, POST's it, and unmarshals the response
// into v. Maps Dhan HTTP error shapes onto our typed errors.
func (p *Provider) postJSON(ctx context.Context, path string, body any, v any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := p.newAuthedReq(ctx, "POST", path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	res, err := p.httpc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode == 429 {
		return provider.ErrRateLimited
	}
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return provider.ErrAuthExpired
	}
	if res.StatusCode >= 400 {
		var errBody struct {
			Remarks string `json:"remarks"`
			Status  string `json:"status"`
		}
		_ = json.Unmarshal(raw, &errBody)
		return fmt.Errorf("dhan: %d %s (%s)", res.StatusCode, errBody.Remarks, errBody.Status)
	}
	if v != nil {
		return json.Unmarshal(raw, v)
	}
	return nil
}
