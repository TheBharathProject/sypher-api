package marketdata

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"

	"log/slog"
)

// Handler exposes the marketdata Service over HTTP. Response shapes
// match kairos/lib/kairos-api.ts exactly (camelCase, envelope keys
// {quotes}, {candles}, {results}, {gainers,losers}).
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler wires a Handler over the Service.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// maxQuoteSymbols caps GET /quotes batch size. Matches Kite's own
// /quote ceiling (500) with a wide margin; the FE's biggest batch is
// the Nifty100 movers fetch, which the service makes internally.
const maxQuoteSymbols = 100

// maxOneMinuteSpan is the widest from..to window allowed for 1m
// candles — beyond a week the payload gets silly (≈2.6k bars) and the
// chunked provider fetches get slow.
const maxOneMinuteSpan = 7 * 24 * time.Hour

// wireCandle is the FE candle shape (kairos-api.ts ApiCandle):
// {t,o,h,l,c,v}. provider.Candle uses long JSON names, so the handler
// maps explicitly.
type wireCandle struct {
	T time.Time `json:"t"`
	O float64   `json:"o"`
	H float64   `json:"h"`
	L float64   `json:"l"`
	C float64   `json:"c"`
	V int64     `json:"v"`
}

// GetQuotes handles GET /kairos/marketdata/quotes?symbols=A,B —
// {quotes:[ApiQuote]}. At most maxQuoteSymbols per request.
func (h *Handler) GetQuotes(w http.ResponseWriter, r *http.Request) {
	symbols := splitCSV(r.URL.Query().Get("symbols"))
	if len(symbols) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "symbols is required (comma-separated)")
		return
	}
	if len(symbols) > maxQuoteSymbols {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "at most 100 symbols per request")
		return
	}
	quotes, err := h.svc.Quotes(r.Context(), symbols)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"quotes": nonNil(quotes)})
}

// GetCandles handles GET /kairos/marketdata/candles?symbol=&interval=
// &from=&to= — {candles:[ApiCandle]}. interval ∈ 1m|5m|15m|1h|1d;
// from/to are RFC3339 or YYYY-MM-DD (date-only is interpreted in IST,
// the market timezone); 1m spans are capped at 7 days.
func (h *Handler) GetCandles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	symbol := strings.TrimSpace(q.Get("symbol"))
	if symbol == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "symbol is required")
		return
	}
	interval := q.Get("interval")
	if !ValidInterval(interval) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "interval must be one of 1m|5m|15m|1h|1d")
		return
	}
	from, ok := parseTimeParam(q.Get("from"))
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "from must be RFC3339 or YYYY-MM-DD")
		return
	}
	to, ok := parseTimeParam(q.Get("to"))
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "to must be RFC3339 or YYYY-MM-DD")
		return
	}
	if !from.Before(to) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "from must be before to")
		return
	}
	if interval == "1m" && to.Sub(from) > maxOneMinuteSpan {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "1m candles are limited to a 7-day span")
		return
	}
	candles, err := h.svc.Candles(r.Context(), symbol, interval, from, to)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	out := make([]wireCandle, len(candles))
	for i, c := range candles {
		out[i] = wireCandle{T: c.Time, O: c.Open, H: c.High, L: c.Low, C: c.Close, V: c.Volume}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"candles": out})
}

// GetIndices handles GET /kairos/marketdata/indices — {quotes:[...]}
// for the fixed dashboard index set (constituents.go IndexSymbols).
func (h *Handler) GetIndices(w http.ResponseWriter, r *http.Request) {
	quotes, err := h.svc.Indices(r.Context())
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"quotes": nonNil(quotes)})
}

// Search handles GET /kairos/marketdata/search?q= —
// {results:[ApiSymbolSearchResult]}. q must be ≥ 2 characters: single
// characters match half the exchange and bust the search cache's key
// space for no UX gain.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) < 2 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "q must be at least 2 characters")
		return
	}
	results, err := h.svc.Search(r.Context(), q)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": nonNil(results)})
}

// GetMovers handles GET /kairos/marketdata/movers —
// {gainers:[...],losers:[...]} over the Nifty100.
func (h *Handler) GetMovers(w http.ResponseWriter, r *http.Request) {
	movers, err := h.svc.Movers(r.Context())
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, movers)
}

// GetSectors handles GET /kairos/marketdata/sectors — {quotes:[...]}
// for the NSE sectoral indices (constituents.go SectoralIndices).
func (h *Handler) GetSectors(w http.ResponseWriter, r *http.Request) {
	quotes, err := h.svc.Sectors(r.Context())
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"quotes": nonNil(quotes)})
}

// providerUnavailableBody is the 503 envelope: the standard
// {error,message} pair plus a machine-readable reason the FE can use
// to pick its PROVIDER OFFLINE copy (spec §1 DataState).
type providerUnavailableBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// writeProviderError maps service/provider errors onto HTTP. Reasons
// and messages are static strings — never err.Error(), which can carry
// upstream URLs and token fragments; the full error is slogged with
// the request ID instead (jobtracker writeDBError semantics).
func (h *Handler) writeProviderError(w http.ResponseWriter, r *http.Request, err error) {
	reason := ""
	switch {
	case errors.Is(err, ErrUnsupported):
		reason = "unsupported"
	case errors.Is(err, provider.ErrAuthExpired):
		reason = "auth_expired"
	case errors.Is(err, provider.ErrNotConfigured):
		reason = "not_configured"
	case errors.Is(err, provider.ErrNotImplemented):
		reason = "not_implemented"
	}
	if reason != "" {
		h.logger.Warn("marketdata provider unavailable",
			"reason", reason, "err", err,
			"path", r.URL.Path, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteJSON(w, http.StatusServiceUnavailable, providerUnavailableBody{
			Error:   "provider_unavailable",
			Message: "market data provider is unavailable",
			Reason:  reason,
		})
		return
	}
	if errors.Is(err, provider.ErrRateLimited) {
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "provider rate limit hit, retry shortly")
		return
	}
	if errors.Is(err, errBadInterval) {
		// Unreachable from HTTP (the whitelist check runs first); kept
		// so a future handler bug 400s instead of masquerading as 502.
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "interval must be one of 1m|5m|15m|1h|1d")
		return
	}
	h.logger.Error("marketdata provider error",
		"err", err, "path", r.URL.Path, "request_id", httpx.RequestID(r.Context()))
	httpx.WriteError(w, http.StatusBadGateway, "provider_error", "market data fetch failed")
}

// splitCSV splits a comma-separated list, trimming whitespace and
// dropping empty entries ("A,,B" → [A B]).
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseTimeParam accepts RFC3339 ("2026-06-12T09:15:00+05:30") or a
// bare date ("2026-06-12"), the latter interpreted as IST midnight —
// market data is IST-native, and a UTC-midnight reading would shift
// daily ranges by a session.
func parseTimeParam(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", s, istLocation); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// nonNil maps a nil slice to an empty one so envelopes marshal as
// {"quotes":[]} rather than {"quotes":null} — the FE types are arrays.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
