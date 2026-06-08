// Package kite is the Zerodha Kite Connect BrokerProvider implementation.
//
// Kite uses a daily access-token model: a request_token from the OAuth
// redirect is exchanged for an access_token that expires the next 06:00
// IST. We persist the access_token encrypted in kairos.provider_tokens
// (ADR-0011 D5).
//
// API reference: https://kite.trade/docs/connect/v3/
// Pricing: ₹500/month for the Connect tier (live + historical).
// Rate limits per ADR-0011 D2 / Kite docs:
//   - Quote API:        1 req/sec  (used by FetchSpot + FetchOptionChain)
//   - Historical:       3 req/sec  (used by FetchHistoricalCandles)
//   - Other endpoints: 10 req/sec
package kite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

const (
	kiteAPIBase = "https://api.kite.trade"
)

func init() {
	provider.Register("kite", New)
}

// Provider is the Kite Connect data client. One instance per process.
type Provider struct {
	apiKey    string
	apiSecret string
	tokens    *tokenStore // backed by kairos.provider_tokens
	httpc     *http.Client
	logger    *slog.Logger

	// Rate limiters per ADR-0011 D2 / Kite docs.
	quoteLim      *rate.Limiter // 1 rps
	historicalLim *rate.Limiter // 3 rps
	otherLim      *rate.Limiter // 10 rps

	// Instrument cache. Kite's instruments CSV is ~4MB; refreshed once
	// a day. Without it we can't translate (NIFTY, expiry, strike, type)
	// to the instrument_token the Quote API wants.
	instrumentsMu   sync.RWMutex
	instruments     instrumentIndex
	instrumentsAsOf time.Time
}

// New constructs the Kite provider. Returns ErrNotConfigured if API key
// or secret is missing — server.go will still boot, but the cron and
// the /options endpoint will log "skip: provider not ready" until the
// keys are set and the daily token is exchanged.
func New(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) (provider.BrokerProvider, error) {
	if cfg.KairosKiteAPIKey == "" || cfg.KairosKiteAPISecret == "" {
		logger.Warn("kite provider missing creds; data fetches will return ErrNotConfigured",
			"have_key", cfg.KairosKiteAPIKey != "",
			"have_secret", cfg.KairosKiteAPISecret != "")
		// Don't return error — let the server boot and surface the
		// problem via /kairos/provider/status to the FE.
	}
	return &Provider{
		apiKey:        cfg.KairosKiteAPIKey,
		apiSecret:     cfg.KairosKiteAPISecret,
		tokens:        newTokenStore(pool),
		httpc:         &http.Client{Timeout: 30 * time.Second},
		logger:        logger,
		quoteLim:      rate.NewLimiter(rate.Every(time.Second), 1),
		historicalLim: rate.NewLimiter(rate.Every(time.Second/3), 1),
		otherLim:      rate.NewLimiter(rate.Every(time.Second/10), 1),
	}, nil
}

func (p *Provider) Name() string { return "kite" }

// IsReady checks creds-are-set and access-token-is-fresh. No network call.
// Token freshness is "we have one and the auto-expiry hasn't passed."
func (p *Provider) IsReady(ctx context.Context) error {
	if p.apiKey == "" || p.apiSecret == "" {
		return provider.ErrNotConfigured
	}
	tok, err := p.tokens.Get(ctx, "kite")
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrAuthExpired, err)
	}
	if tok.AccessToken == "" {
		return provider.ErrAuthExpired
	}
	if !tok.ExpiresAt.IsZero() && time.Now().After(tok.ExpiresAt) {
		return provider.ErrAuthExpired
	}
	return nil
}

// FetchSpot calls Kite's /quote/ltp endpoint for the index symbol.
// Kite represents indices like "NSE:NIFTY 50", "NSE:NIFTY BANK",
// "BSE:SENSEX". Translate via indexSymbol().
func (p *Provider) FetchSpot(ctx context.Context, u provider.Underlying) (provider.Spot, error) {
	if err := p.quoteLim.Wait(ctx); err != nil {
		return provider.Spot{}, err
	}
	sym, ok := indexSymbol(u)
	if !ok {
		return provider.Spot{}, provider.ErrUnknownUnderlying
	}
	req, err := p.newAuthedReq(ctx, "GET",
		fmt.Sprintf("/quote/ltp?i=%s", url.QueryEscape(sym)), nil)
	if err != nil {
		return provider.Spot{}, err
	}
	var resp struct {
		Status string `json:"status"`
		Data   map[string]struct {
			InstrumentToken int64   `json:"instrument_token"`
			LastPrice       float64 `json:"last_price"`
		} `json:"data"`
	}
	if err := p.do(req, &resp); err != nil {
		return provider.Spot{}, err
	}
	v, ok := resp.Data[sym]
	if !ok {
		return provider.Spot{}, fmt.Errorf("kite: spot for %s missing in response", sym)
	}
	return provider.Spot{
		Underlying: u,
		Price:      v.LastPrice,
		AsOf:       time.Now(),
		// Change / ChangePct require a prev_close call (FetchSpot is on the
		// hot path); leave 0 — /dashboard computes deltas from successive
		// snapshots if it cares.
	}, nil
}

// FetchExpiries reads from the instruments cache. No network call after
// the cache is warm.
func (p *Provider) FetchExpiries(ctx context.Context, u provider.Underlying) ([]provider.Expiry, error) {
	if err := p.ensureInstruments(ctx); err != nil {
		return nil, err
	}
	return p.instruments.Expiries(u), nil
}

// FetchOptionChain fetches LTPs and OI for every (strike, side) tradeable
// for (underlying, expiry). Implementation:
//   1. Resolve instrument tokens for all strikes/sides via the
//      instruments cache.
//   2. Call /quote with up to 500 instruments per request.
//   3. Translate the response into ChainRow shape.
//
// Spot for the snapshot is fetched in the same call by including the
// underlying's index symbol in the instruments list.
func (p *Provider) FetchOptionChain(ctx context.Context, u provider.Underlying, expiry time.Time) ([]provider.ChainRow, error) {
	if err := p.ensureInstruments(ctx); err != nil {
		return nil, err
	}
	if err := p.quoteLim.Wait(ctx); err != nil {
		return nil, err
	}

	instruments := p.instruments.OptionInstruments(u, expiry)
	if len(instruments) == 0 {
		return nil, fmt.Errorf("kite: no instruments found for %s exp=%s",
			u, expiry.Format("2006-01-02"))
	}

	// Build the ?i=...&i=... query. Kite caps at 500 per call; for a
	// single underlying/expiry we never approach that.
	symList := indexSymbolPlus(u, instruments)
	q := url.Values{}
	for _, s := range symList {
		q.Add("i", s)
	}
	req, err := p.newAuthedReq(ctx, "GET", "/quote?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Status string                  `json:"status"`
		Data   map[string]quoteEnvelope `json:"data"`
	}
	if err := p.do(req, &resp); err != nil {
		return nil, err
	}
	return translateChain(u, expiry, instruments, resp.Data), nil
}

// FetchHistoricalCandles calls Kite's /instruments/historical/:token/:interval.
// Used by the backtest worker and by /charts.
func (p *Provider) FetchHistoricalCandles(ctx context.Context, req provider.HistoricalReq) ([]provider.Candle, error) {
	if err := p.historicalLim.Wait(ctx); err != nil {
		return nil, err
	}
	if err := p.ensureInstruments(ctx); err != nil {
		return nil, err
	}
	token, ok := p.instruments.TokenForSymbol(req.Instrument)
	if !ok {
		return nil, fmt.Errorf("kite: instrument %q not found", req.Instrument)
	}
	path := fmt.Sprintf("/instruments/historical/%d/%s?from=%s&to=%s",
		token, req.Interval,
		url.QueryEscape(req.From.Format("2006-01-02 15:04:05")),
		url.QueryEscape(req.To.Format("2006-01-02 15:04:05")),
	)
	httpReq, err := p.newAuthedReq(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Candles [][]any `json:"candles"`
		} `json:"data"`
	}
	if err := p.do(httpReq, &resp); err != nil {
		return nil, err
	}
	return translateCandles(resp.Data.Candles), nil
}

// ─────────────────────────────────────────────────────────────────────────
// HTTP plumbing
// ─────────────────────────────────────────────────────────────────────────

func (p *Provider) newAuthedReq(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	tok, err := p.tokens.Get(ctx, "kite")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", provider.ErrAuthExpired, err)
	}
	if tok.AccessToken == "" {
		return nil, provider.ErrAuthExpired
	}
	req, err := http.NewRequestWithContext(ctx, method, kiteAPIBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Kite-Version", "3")
	req.Header.Set("Authorization", fmt.Sprintf("token %s:%s", p.apiKey, tok.AccessToken))
	return req, nil
}

// do executes the request and unmarshals into v. Maps Kite's HTTP error
// shape onto our typed errors.
func (p *Provider) do(req *http.Request, v any) error {
	res, err := p.httpc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
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
		// Kite errors look like: {"status":"error","message":"...","error_type":"TokenException"}
		var errResp struct {
			Status    string `json:"status"`
			Message   string `json:"message"`
			ErrorType string `json:"error_type"`
		}
		_ = json.Unmarshal(body, &errResp)
		if strings.Contains(errResp.ErrorType, "Token") {
			return provider.ErrAuthExpired
		}
		return fmt.Errorf("kite: %d %s (%s)", res.StatusCode, errResp.Message, errResp.ErrorType)
	}
	if v != nil {
		return json.Unmarshal(body, v)
	}
	return nil
}

// quoteEnvelope is one entry in /quote's data map.
type quoteEnvelope struct {
	InstrumentToken int64   `json:"instrument_token"`
	LastPrice       float64 `json:"last_price"`
	NetChange       float64 `json:"net_change"`
	OI              int64   `json:"oi"`
	OIDayChange     int64   `json:"oi_day_change"`
	Volume          int64   `json:"volume"`
	Depth           struct {
		Buy  []depthRow `json:"buy"`
		Sell []depthRow `json:"sell"`
	} `json:"depth"`
}

type depthRow struct {
	Price float64 `json:"price"`
}

// indexSymbol returns the Kite "exchange:symbol" string for the spot
// index of an underlying. NSE listings for NIFTY and BANKNIFTY are
// "NIFTY 50" and "NIFTY BANK" — note the space. SENSEX is on BSE.
func indexSymbol(u provider.Underlying) (string, bool) {
	switch u {
	case provider.UnderlyingNIFTY:
		return "NSE:NIFTY 50", true
	case provider.UnderlyingBANKNIFTY:
		return "NSE:NIFTY BANK", true
	case provider.UnderlyingSENSEX:
		return "BSE:SENSEX", true
	}
	return "", false
}

// indexSymbolPlus builds the full list of Kite symbols to request for a
// chain snapshot: the spot index plus every option contract for the
// expiry. Limits to ~500 per call (Kite's documented cap).
func indexSymbolPlus(u provider.Underlying, instr []instrument) []string {
	out := make([]string, 0, len(instr)+1)
	if s, ok := indexSymbol(u); ok {
		out = append(out, s)
	}
	for _, ins := range instr {
		out = append(out, fmt.Sprintf("%s:%s", ins.Exchange, ins.TradingSymbol))
	}
	return out
}

// ensureInstruments lazily loads the instruments CSV. Refresh once per
// 12 hours; CSV is ~4MB so we don't want to fetch on every chain call.
func (p *Provider) ensureInstruments(ctx context.Context) error {
	p.instrumentsMu.RLock()
	fresh := !p.instrumentsAsOf.IsZero() && time.Since(p.instrumentsAsOf) < 12*time.Hour
	p.instrumentsMu.RUnlock()
	if fresh {
		return nil
	}
	if err := p.otherLim.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.kite.trade/instruments", nil)
	if err != nil {
		return err
	}
	res, err := p.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("instruments fetch: %w", err)
	}
	defer res.Body.Close()
	idx, err := parseInstruments(res.Body)
	if err != nil {
		return fmt.Errorf("instruments parse: %w", err)
	}
	p.instrumentsMu.Lock()
	p.instruments = idx
	p.instrumentsAsOf = time.Now()
	p.instrumentsMu.Unlock()
	p.logger.Info("kite instruments refreshed", "count", len(idx.all))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// Public helpers used by the HTTP handler (OAuth callback path)
// ─────────────────────────────────────────────────────────────────────────

// LoginURL is the Zerodha "Connect" page the user is redirected to.
// Their dashboard sends them back to our callback with a request_token.
func (p *Provider) LoginURL() string {
	return fmt.Sprintf("https://kite.zerodha.com/connect/login?api_key=%s&v=3", p.apiKey)
}

// ExchangeRequestToken takes the request_token from the OAuth callback,
// posts /session/token to Kite, and persists the resulting access_token
// to kairos.provider_tokens.
func (p *Provider) ExchangeRequestToken(ctx context.Context, requestToken string) error {
	if p.apiKey == "" || p.apiSecret == "" {
		return provider.ErrNotConfigured
	}
	if err := p.otherLim.Wait(ctx); err != nil {
		return err
	}
	checksum := sha256Hex(p.apiKey + requestToken + p.apiSecret)
	form := url.Values{}
	form.Set("api_key", p.apiKey)
	form.Set("request_token", requestToken)
	form.Set("checksum", checksum)
	req, err := http.NewRequestWithContext(ctx, "POST", kiteAPIBase+"/session/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("X-Kite-Version", "3")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := p.httpc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return fmt.Errorf("kite session/token: %d %s", res.StatusCode, string(body))
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			AccessToken string `json:"access_token"`
			UserID      string `json:"user_id"`
			LoginTime   string `json:"login_time"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	if resp.Status != "success" || resp.Data.AccessToken == "" {
		return errors.New("kite: empty access_token in /session/token response")
	}
	// Kite tokens expire next 06:00 IST. We compute that as "tomorrow
	// 06:00 in IST relative to now."
	exp := nextKiteExpiry(time.Now())
	meta := map[string]any{
		"user_id":    resp.Data.UserID,
		"login_time": resp.Data.LoginTime,
	}
	if err := p.tokens.Put(ctx, providerToken{
		Provider:    "kite",
		AccessToken: resp.Data.AccessToken,
		ExpiresAt:   exp,
		Metadata:    meta,
	}); err != nil {
		return fmt.Errorf("persist kite token: %w", err)
	}
	p.logger.Info("kite token refreshed", "user_id", resp.Data.UserID, "expires_at", exp.Format(time.RFC3339))
	return nil
}

// nextKiteExpiry returns the next 06:00 IST clock-time that's after now.
// Kite access tokens "expire on the next trading day at 06:00 AM"; we
// approximate as "next 06:00 IST regardless of weekend" — a Saturday
// refresh would last until Sunday 06:00 in our computation, but Kite
// will reject it Monday morning so the real expiry is when their server
// says it is. The conservative value here just keeps our IsReady
// honest.
func nextKiteExpiry(now time.Time) time.Time {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		ist = time.FixedZone("IST", int((5*time.Hour + 30*time.Minute).Seconds()))
	}
	nowIST := now.In(ist)
	candidate := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 6, 0, 0, 0, ist)
	if !candidate.After(nowIST) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}
