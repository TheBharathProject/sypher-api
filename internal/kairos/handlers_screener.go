package kairos

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/kairos/marketdata"
)

// ─────────────────────────────────────────────────────────────────────────
// /kairos/screener + /kairos/fundamentals/{symbol}
//
// Response shapes are dictated by kairos/lib/kairos-api.ts:
//   GET /kairos/screener?preset=&sort=&dir= → {rows:[ApiScreenerRow], pricesLive:bool}
//   GET /kairos/fundamentals/{symbol}       → ApiFundamental (bare)
//
// Routes are registered by the wiring task; the symbol handler reads
// {symbol} via r.PathValue.
// ─────────────────────────────────────────────────────────────────────────

// WithMarketData attaches the marketdata service that powers the
// screener's live-price merge. Optional — without it the screener
// still serves ratios, with pricesLive=false. Returns the Handler for
// chaining (jobtracker With* idiom).
func (h *Handler) WithMarketData(md *marketdata.Service) *Handler {
	h.marketData = md
	return h
}

// ScreenerRow is one screener table row: stored ratios plus the
// best-effort live quote fields (ApiScreenerRow — last/change/
// changePct are optional in the contract, so omitted when the
// provider is down or the symbol didn't quote).
type ScreenerRow struct {
	FundamentalsRow
	Last      *float64 `json:"last,omitempty"`
	Change    *float64 `json:"change,omitempty"`
	ChangePct *float64 `json:"changePct,omitempty"`
}

// Screener handles GET /kairos/screener?preset=&sort=&dir=. Ratios
// come from kairos.equity_fundamentals (preset/sort semantics in
// store_screener.go); prices are merged in live from marketdata,
// best-effort: a down provider (or no marketdata wired) yields
// pricesLive=false with the price fields absent — rows are always
// served. The FE shows a STALE tag off pricesLive.
func (h *Handler) Screener(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := h.store.ListFundamentals(r.Context(), q.Get("preset"), q.Get("sort"), q.Get("dir"))
	if err != nil {
		h.logger.Error("screener list failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	out := make([]ScreenerRow, len(rows))
	symbols := make([]string, len(rows))
	for i, f := range rows {
		out[i] = ScreenerRow{FundamentalsRow: f}
		symbols[i] = f.Symbol
	}

	pricesLive := false
	if h.marketData != nil && len(symbols) > 0 {
		quotes, qerr := h.marketData.Quotes(r.Context(), symbols)
		if qerr != nil {
			// Best-effort by contract: log and serve ratios anyway.
			h.logger.Warn("screener live quotes failed", "err", qerr,
				"request_id", httpx.RequestID(r.Context()))
		} else {
			pricesLive = true
			// The provider drops symbols it doesn't recognise, so map
			// by symbol rather than assuming positions line up.
			bySymbol := make(map[string]int, len(quotes))
			for i, qt := range quotes {
				bySymbol[strings.ToUpper(qt.Symbol)] = i
			}
			for i := range out {
				if j, ok := bySymbol[strings.ToUpper(out[i].Symbol)]; ok {
					qt := quotes[j]
					last, change, pct := qt.Last, qt.Change, qt.ChangePct
					out[i].Last, out[i].Change, out[i].ChangePct = &last, &change, &pct
				}
			}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"rows":       out,
		"pricesLive": pricesLive,
	})
}

// GetFundamental handles GET /kairos/fundamentals/{symbol} — the
// screener detail drawer / fundamentals page source.
func (h *Handler) GetFundamental(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(strings.TrimSpace(r.PathValue("symbol")))
	if symbol == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "symbol is required")
		return
	}
	row, err := h.store.GetFundamental(r.Context(), symbol)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "no fundamentals for symbol")
			return
		}
		h.logger.Error("get fundamental failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}
