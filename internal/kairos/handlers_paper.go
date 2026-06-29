package kairos

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// ─────────────────────────────────────────────────────────────────────────
// /kairos/paper/*  (spec §3.5)
//
// Response shapes are dictated by kairos/lib/kairos-api.ts:
//   GET    /kairos/paper/account              → ApiPaperAccount (bare)
//   POST   /kairos/paper/reset                → ApiPaperAccount (bare)
//   POST   /kairos/paper/orders               → ApiPaperOrder (bare)
//   GET    /kairos/paper/orders               → {orders:[ApiPaperOrder]}
//   DELETE /kairos/paper/orders/{id}          → 204
//   GET    /kairos/paper/positions            → {positions:[ApiPaperPosition]}
//   POST   /kairos/paper/positions/{id}/close → ApiPaperOrder (bare)
//
// Dark launch (spec §3.5): POST /kairos/orders exists but refuses —
// 403 execution_disabled while KAIROS_LIVE_TRADING is off (default),
// 501 not_implemented once it's on (no live routing is built yet).
//
// Routes are registered by the wiring task; handlers read {id} via
// r.PathValue.
// ─────────────────────────────────────────────────────────────────────────

// PaperAccountResponse is the ApiPaperAccount wire shape. EquityValue
// and TotalPnl are pointers: omitted (not zeroed) when position marks
// are unavailable — e.g. the data provider is down — so the FE can
// tell "no data" from "flat".
type PaperAccountResponse struct {
	Cash            float64  `json:"cash"`
	StartingCapital float64  `json:"startingCapital"`
	EquityValue     *float64 `json:"equityValue,omitempty"`
	TotalPnl        *float64 `json:"totalPnl,omitempty"`
}

// paperQuotes returns the live-quote dependency for equity fills and
// marks: the active provider's optional QuoteFetcher capability, nil
// when the provider doesn't quote (stubs) — callers degrade to
// REJECTED no_fresh_price / omitted marks.
func (h *Handler) paperQuotes() quotesFunc {
	if qf, ok := h.provider.(provider.QuoteFetcher); ok {
		return qf.FetchQuotes
	}
	return nil
}

// GetPaperAccount returns cash plus best-effort equity value / total
// P&L. Marks: EQ positions from one batched live-quote call, OPT
// positions from the latest stored chain row. If ANY open position
// can't be marked, equityValue/totalPnl are omitted rather than served
// wrong.
func (h *Handler) GetPaperAccount(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	acct, err := h.store.GetOrCreatePaperAccount(r.Context(), uid)
	if err != nil {
		h.logger.Error("paper account load failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	positions, err := h.store.ListPaperPositions(r.Context(), uid)
	if err != nil {
		h.logger.Error("paper positions load failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.paperAccountResponse(r, acct, positions))
}

// ResetPaperAccount wipes orders + positions and restores starting
// capital.
func (h *Handler) ResetPaperAccount(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	acct, err := h.store.ResetPaperAccount(r.Context(), uid)
	if err != nil {
		h.logger.Error("paper account reset failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	// Fresh account: equity == cash, P&L == 0 — computable without marks.
	equity := acct.Cash
	pnl := 0.0
	httpx.WriteJSON(w, http.StatusOK, PaperAccountResponse{
		Cash:            acct.Cash,
		StartingCapital: acct.StartingCapital,
		EquityValue:     &equity,
		TotalPnl:        &pnl,
	})
}

// paperOrderRequest is the union body of POST /kairos/paper/orders
// (ApiPaperOrderRequest): EQ orders carry symbol; OPT orders carry
// underlying/expiry/strike/optType.
type paperOrderRequest struct {
	Kind       string   `json:"kind"`
	Symbol     string   `json:"symbol"`
	Underlying string   `json:"underlying"`
	Expiry     string   `json:"expiry"`
	Strike     int      `json:"strike"`
	OptType    string   `json:"optType"`
	Side       string   `json:"side"`
	OrderType  string   `json:"orderType"`
	Qty        int      `json:"qty"`
	LimitPrice *float64 `json:"limitPrice"`
}

// maxPaperQty / maxPaperLimitPrice bound inputs well inside the
// NUMERIC(12,2)/(14,2) columns they multiply into.
const (
	maxPaperQty        = 1_000_000
	maxPaperLimitPrice = 10_000_000.0
)

// PlacePaperOrder validates and executes (MARKET) or parks (LIMIT) a
// paper order. The response is always the order row — REJECTED orders
// (no fresh price, insufficient funds) come back with status REJECTED
// + rejectReason rather than an HTTP error, mirroring how a broker
// reports rejections.
func (h *Handler) PlacePaperOrder(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in paperOrderRequest
	if !readJSON(w, r, &in) {
		return
	}
	ord, errMsg := buildPaperOrder(uid, &in)
	if errMsg != "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", errMsg)
		return
	}

	// Ensure the account row exists — FillPaperOrderTx locks it.
	if _, err := h.store.GetOrCreatePaperAccount(r.Context(), uid); err != nil {
		h.logger.Error("paper account provision failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}

	var err error
	if ord.OrderType == "MARKET" {
		err = ExecutePaperMarketOrder(r.Context(), h.store, h.paperQuotes(), ord)
	} else {
		ord.Status = "OPEN"
		err = h.store.InsertPaperOrder(r.Context(), ord)
	}
	if err != nil {
		h.logger.Error("paper order failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, ord)
}

// buildPaperOrder validates the request and shapes a PaperOrder ready
// for the engine. Returns a non-empty message on validation failure.
func buildPaperOrder(uid uuid.UUID, in *paperOrderRequest) (*PaperOrder, string) {
	side := strings.ToUpper(strings.TrimSpace(in.Side))
	if side != "BUY" && side != "SELL" {
		return nil, "side must be BUY or SELL"
	}
	orderType := strings.ToUpper(strings.TrimSpace(in.OrderType))
	if orderType != "MARKET" && orderType != "LIMIT" {
		return nil, "orderType must be MARKET or LIMIT"
	}
	if in.Qty <= 0 || in.Qty > maxPaperQty {
		return nil, "qty must be between 1 and 1000000"
	}
	ord := &PaperOrder{
		UserID:    uid,
		Side:      side,
		OrderType: orderType,
		Qty:       in.Qty,
	}
	if orderType == "LIMIT" {
		if in.LimitPrice == nil || *in.LimitPrice <= 0 || *in.LimitPrice > maxPaperLimitPrice {
			return nil, "limitPrice must be a positive price for LIMIT orders"
		}
		ord.LimitPrice = in.LimitPrice
	}

	switch strings.ToUpper(strings.TrimSpace(in.Kind)) {
	case "EQ":
		symbol := strings.ToUpper(strings.TrimSpace(in.Symbol))
		if symbol == "" || utf8.RuneCountInString(symbol) > 32 {
			return nil, "symbol must be 1-32 characters"
		}
		ord.Kind = "EQ"
		ord.Symbol = symbol
	case "OPT":
		underlying := strings.ToUpper(strings.TrimSpace(in.Underlying))
		if !validUnderlyings[underlying] {
			return nil, "underlying must be NIFTY|BANKNIFTY|SENSEX"
		}
		if _, err := time.Parse("2006-01-02", in.Expiry); err != nil {
			return nil, "expiry must be YYYY-MM-DD"
		}
		if in.Strike <= 0 {
			return nil, "strike must be a positive integer"
		}
		optType := strings.ToUpper(strings.TrimSpace(in.OptType))
		if optType != "CE" && optType != "PE" {
			return nil, "optType must be CE or PE"
		}
		ord.Kind = "OPT"
		ord.Underlying = underlying
		ord.Expiry = in.Expiry
		ord.Strike = in.Strike
		ord.OptType = optType
		ord.Symbol = paperContractSymbol(underlying, in.Expiry, in.Strike, optType)
	default:
		return nil, "kind must be EQ or OPT"
	}
	return ord, ""
}

// ListPaperOrders returns the user's most recent 200 orders.
func (h *Handler) ListPaperOrders(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListPaperOrders(r.Context(), uid)
	if err != nil {
		h.logger.Error("paper orders list failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"orders": out})
}

// CancelPaperOrder cancels a still-OPEN order.
func (h *Handler) CancelPaperOrder(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	if err := h.store.CancelPaperOrder(r.Context(), uid, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "open order not found")
			return
		}
		h.logger.Error("paper order cancel failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListPaperPositions returns all the user's positions.
func (h *Handler) ListPaperPositions(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListPaperPositions(r.Context(), uid)
	if err != nil {
		h.logger.Error("paper positions list failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"positions": out})
}

// ClosePaperPosition squares off a position with a MARKET order on the
// opposite side for |qty|, and returns the generated order (which may
// itself come back REJECTED, e.g. no_fresh_price after hours).
func (h *Handler) ClosePaperPosition(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	pos, err := h.store.GetPaperPosition(r.Context(), uid, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "position not found")
			return
		}
		h.logger.Error("paper position load failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	if pos.Qty == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "position is already flat")
		return
	}
	side := "SELL"
	if pos.Qty < 0 {
		side = "BUY"
	}
	ord := &PaperOrder{
		UserID:     uid,
		Kind:       pos.Kind,
		Symbol:     pos.Symbol,
		Underlying: pos.Underlying,
		Expiry:     pos.Expiry,
		Strike:     pos.Strike,
		OptType:    pos.OptType,
		Side:       side,
		OrderType:  "MARKET",
		Qty:        abs(pos.Qty),
	}
	if err := ExecutePaperMarketOrder(r.Context(), h.store, h.paperQuotes(), ord); err != nil {
		h.logger.Error("paper position close failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ord)
}

// RealOrders is the dark-launched live-trading endpoint
// (POST /kairos/orders). While KAIROS_LIVE_TRADING is off (the
// default) it refuses with 403 execution_disabled; flipping the flag
// only changes the refusal to 501 — no live routing exists yet.
func (h *Handler) RealOrders(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.KairosLiveTrading {
		httpx.WriteError(w, http.StatusForbidden, "execution_disabled",
			"live order execution is disabled by the operator")
		return
	}
	httpx.WriteError(w, http.StatusNotImplemented, "not_implemented",
		"live order routing is not implemented yet")
}

// paperAccountResponse assembles the account payload: cash always;
// equityValue/totalPnl only when every open (qty≠0) position has a
// mark. equity = cash + Σ qty×mark (signed qty makes shorts a
// liability); totalPnl = equity − startingCapital, which folds in
// realized P&L, unrealized P&L and fees without double counting.
func (h *Handler) paperAccountResponse(r *http.Request, acct *PaperAccount, positions []PaperPosition) PaperAccountResponse {
	resp := PaperAccountResponse{
		Cash:            acct.Cash,
		StartingCapital: acct.StartingCapital,
	}

	open := make([]*PaperPosition, 0, len(positions))
	var eqSymbols []string
	seen := map[string]bool{}
	for i := range positions {
		p := &positions[i]
		if p.Qty == 0 {
			continue
		}
		open = append(open, p)
		if p.Kind == "EQ" && !seen[p.Symbol] {
			seen[p.Symbol] = true
			eqSymbols = append(eqSymbols, p.Symbol)
		}
	}

	eqLast := map[string]float64{}
	if len(eqSymbols) > 0 {
		quotes := h.paperQuotes()
		if quotes == nil {
			return resp // provider can't quote → marks unavailable
		}
		qs, err := quotes(r.Context(), eqSymbols)
		if err != nil {
			// Provider down — serve cash, omit marks (FE shows "—").
			h.logger.Warn("paper account marks unavailable", "err", err, "request_id", httpx.RequestID(r.Context()))
			return resp
		}
		for _, q := range qs {
			eqLast[q.Symbol] = q.Last
		}
	}

	value := acct.Cash
	for _, p := range open {
		var mark float64
		if p.Kind == "EQ" {
			mark = eqLast[p.Symbol]
		} else if m, ok := h.store.quoteForPosition(r.Context(), p); ok {
			mark = m
		}
		if mark <= 0 {
			return resp // one unmarkable position → don't fake a total
		}
		value += float64(p.Qty) * mark
	}
	equity := round2(value)
	pnl := round2(value - acct.StartingCapital)
	resp.EquityValue = &equity
	resp.TotalPnl = &pnl
	return resp
}
