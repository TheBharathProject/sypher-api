package marketdata

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// stubCore satisfies the mandatory BrokerProvider surface with inert
// answers; concrete fakes embed it and add only what their test needs.
type stubCore struct{}

func (stubCore) Name() string                  { return "fake" }
func (stubCore) IsReady(context.Context) error { return nil }
func (stubCore) FetchOptionChain(context.Context, provider.Underlying, time.Time) ([]provider.ChainRow, error) {
	return nil, provider.ErrNotImplemented
}
func (stubCore) FetchExpiries(context.Context, provider.Underlying) ([]provider.Expiry, error) {
	return nil, provider.ErrNotImplemented
}
func (stubCore) FetchSpot(context.Context, provider.Underlying) (provider.Spot, error) {
	return provider.Spot{}, provider.ErrNotImplemented
}
func (stubCore) FetchHistoricalCandles(context.Context, provider.HistoricalReq) ([]provider.Candle, error) {
	return nil, provider.ErrNotImplemented
}

// fakeProvider has the optional capabilities; quotes/candle behaviour
// is injected per test.
type fakeProvider struct {
	stubCore
	quotes    []provider.Quote
	quotesErr error
	histCalls []provider.HistoricalReq
}

func (f *fakeProvider) FetchQuotes(_ context.Context, symbols []string) ([]provider.Quote, error) {
	return f.quotes, f.quotesErr
}

func (f *fakeProvider) SearchInstruments(_ context.Context, q string, limit int) ([]provider.Instrument, error) {
	return []provider.Instrument{{Symbol: "RELIANCE", Name: "Reliance Industries", Exchange: "NSE", Kind: "EQ"}}, nil
}

func (f *fakeProvider) FetchHistoricalCandles(_ context.Context, req provider.HistoricalReq) ([]provider.Candle, error) {
	f.histCalls = append(f.histCalls, req)
	return []provider.Candle{{Time: req.From, Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 100}}, nil
}

// noCapProvider implements only the core interface — no QuoteFetcher,
// no InstrumentSearcher — to exercise the 503 unsupported path.
type noCapProvider struct{ stubCore }

func newTestHandler(prov provider.BrokerProvider) *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(NewService(prov, logger), logger)
}

func doGet(t *testing.T, fn http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	fn(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestGetQuotesValidation(t *testing.T) {
	h := newTestHandler(&fakeProvider{})

	if rec := doGet(t, h.GetQuotes, "/kairos/marketdata/quotes"); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing symbols: got %d, want 400", rec.Code)
	}
	big := strings.Repeat("X,", 100) + "Y" // 101 symbols
	if rec := doGet(t, h.GetQuotes, "/kairos/marketdata/quotes?symbols="+big); rec.Code != http.StatusBadRequest {
		t.Fatalf("101 symbols: got %d, want 400", rec.Code)
	}
}

func TestGetQuotesEnvelopeMatchesFEContract(t *testing.T) {
	ts := time.Date(2026, 6, 12, 10, 30, 0, 0, time.UTC)
	h := newTestHandler(&fakeProvider{quotes: []provider.Quote{{
		Symbol: "RELIANCE", Last: 2900.5, Change: 12.5, ChangePct: 0.43,
		Volume: 1000, OHLC: provider.OHLC{O: 2890, H: 2910, L: 2880, C: 2888}, TS: ts,
	}}})

	rec := doGet(t, h.GetQuotes, "/kairos/marketdata/quotes?symbols=RELIANCE")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var body struct {
		Quotes []map[string]json.RawMessage `json:"quotes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Quotes) != 1 {
		t.Fatalf("got %d quotes, want 1", len(body.Quotes))
	}
	// Field names are the FE contract (ApiQuote) — camelCase, nested ohlc.
	for _, field := range []string{"symbol", "last", "change", "changePct", "volume", "ohlc", "ts"} {
		if _, ok := body.Quotes[0][field]; !ok {
			t.Errorf("quote missing FE contract field %q", field)
		}
	}
}

func TestGetQuotesUnsupportedProviderIs503(t *testing.T) {
	h := newTestHandler(&noCapProvider{})

	rec := doGet(t, h.GetQuotes, "/kairos/marketdata/quotes?symbols=RELIANCE")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
	var body providerUnavailableBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error != "provider_unavailable" {
		t.Errorf("error code = %q, want provider_unavailable", body.Error)
	}
	if body.Reason != "unsupported" {
		t.Errorf("reason = %q, want unsupported", body.Reason)
	}
}

func TestGetCandlesValidation(t *testing.T) {
	h := newTestHandler(&fakeProvider{})

	cases := []struct {
		name, target string
	}{
		{"missing symbol", "/c?interval=1d&from=2026-01-01&to=2026-02-01"},
		{"bad interval", "/c?symbol=RELIANCE&interval=2m&from=2026-01-01&to=2026-02-01"},
		{"bad from", "/c?symbol=RELIANCE&interval=1d&from=01-01-2026&to=2026-02-01"},
		{"missing to", "/c?symbol=RELIANCE&interval=1d&from=2026-01-01"},
		{"from not before to", "/c?symbol=RELIANCE&interval=1d&from=2026-02-01&to=2026-01-01"},
		{"1m span over 7d", "/c?symbol=RELIANCE&interval=1m&from=2026-01-01&to=2026-01-09"},
	}
	for _, tc := range cases {
		if rec := doGet(t, h.GetCandles, tc.target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", tc.name, rec.Code)
		}
	}
}

func TestGetCandlesChunksAndMapsToWireShape(t *testing.T) {
	prov := &fakeProvider{}
	h := newTestHandler(prov)

	// 90 days of 5m candles → two provider calls (60d + 30d chunks).
	rec := doGet(t, h.GetCandles, "/c?symbol=RELIANCE&interval=5m&from=2026-01-01&to=2026-04-01")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if len(prov.histCalls) != 2 {
		t.Fatalf("provider called %d times, want 2 (60-day chunking)", len(prov.histCalls))
	}
	for _, call := range prov.histCalls {
		if call.Interval != "5minute" {
			t.Errorf("provider interval = %q, want 5minute", call.Interval)
		}
	}
	var body struct {
		Candles []map[string]json.RawMessage `json:"candles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Candles) != 2 { // one fake candle per chunk
		t.Fatalf("got %d candles, want 2", len(body.Candles))
	}
	for _, field := range []string{"t", "o", "h", "l", "c", "v"} {
		if _, ok := body.Candles[0][field]; !ok {
			t.Errorf("candle missing FE contract field %q", field)
		}
	}
}

func TestSearchValidation(t *testing.T) {
	h := newTestHandler(&fakeProvider{})

	if rec := doGet(t, h.Search, "/s?q=a"); rec.Code != http.StatusBadRequest {
		t.Fatalf("1-char query: got %d, want 400", rec.Code)
	}
	rec := doGet(t, h.Search, "/s?q=rel")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var body struct {
		Results []provider.Instrument `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Results) != 1 || body.Results[0].Symbol != "RELIANCE" {
		t.Fatalf("unexpected results: %+v", body.Results)
	}
}

func TestGetMoversSortsByChangePct(t *testing.T) {
	quotes := make([]provider.Quote, 0, 20)
	for i := 1; i <= 20; i++ {
		quotes = append(quotes, provider.Quote{Symbol: "S", ChangePct: float64(i)})
	}
	h := newTestHandler(&fakeProvider{quotes: quotes})

	rec := doGet(t, h.GetMovers, "/kairos/marketdata/movers")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var body Movers
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Gainers) != 8 || len(body.Losers) != 8 {
		t.Fatalf("got %d gainers / %d losers, want 8 / 8", len(body.Gainers), len(body.Losers))
	}
	if body.Gainers[0].ChangePct != 20 {
		t.Errorf("top gainer changePct = %v, want 20", body.Gainers[0].ChangePct)
	}
	if body.Losers[0].ChangePct != 1 {
		t.Errorf("top loser changePct = %v, want 1 (worst first)", body.Losers[0].ChangePct)
	}
}

func TestRegisterRoutesAppliesAuthMiddleware(t *testing.T) {
	h := newTestHandler(&fakeProvider{})
	mux := http.NewServeMux()
	var gated int
	requireUser := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gated++
			next.ServeHTTP(w, r)
		})
	}
	RegisterRoutes(mux, h, requireUser)

	paths := []string{
		"/kairos/marketdata/quotes?symbols=RELIANCE",
		"/kairos/marketdata/candles?symbol=RELIANCE&interval=1d&from=2026-01-01&to=2026-02-01",
		"/kairos/marketdata/indices",
		"/kairos/marketdata/search?q=rel",
		"/kairos/marketdata/movers",
		"/kairos/marketdata/sectors",
	}
	for _, p := range paths {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("GET %s not routed (status %d)", p, rec.Code)
		}
	}
	if gated != len(paths) {
		t.Errorf("auth middleware ran %d times, want %d", gated, len(paths))
	}
}
