package kairos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// ─────────────────────────────────────────────────────────────────────────
// Paper trading engine (spec §3.5).
//
// Pure fill math (ApplyFill / LimitCrossed) lives at the top; the
// order-execution flow shared by the HTTP handlers (handlers_paper.go)
// and the 60s limit-fill sweep (cron/jobs/kairos_paper.go) follows.
// All persistence goes through store_paper.go; a fill writes order +
// position + cash in ONE pgx tx (FillPaperOrderTx).
//
// Documented simplifications (this is a paper simulator, not a risk
// engine):
//   - SELL orders (short equity and short options alike) credit the
//     premium/proceeds to cash but require cash ≥ fees + 15% of the
//     order's notional (fill price × qty) as a crude margin proxy.
//     Real SPAN/exposure margining is far more involved.
//   - Fees use the shared Indian options cost model (costs.go) for both
//     EQ and OPT orders — close enough in magnitude for equities.
//   - EQ market orders fill at the provider's last quote without a
//     staleness gate (off-hours the provider serves the prior session's
//     last trade, which is the natural paper price). OPT fills require
//     a chain snapshot ≤ paperPriceMaxAge old — chains only exist while
//     the snapshot cron runs, so a stale row means a stale price.
//   - LIMIT orders reserve no cash at placement; funds are checked at
//     fill time inside the tx and the order is REJECTED
//     insufficient_funds if the account can no longer cover it.
// ─────────────────────────────────────────────────────────────────────────

// PaperPosition is one net position per (user, symbol) —
// kairos.paper_positions. Qty is signed (negative = short); JSON tags
// match ApiPaperPosition in kairos/lib/kairos-api.ts exactly.
type PaperPosition struct {
	ID          uuid.UUID  `json:"id"`
	UserID      uuid.UUID  `json:"-"`
	Kind        string     `json:"kind"` // "EQ" | "OPT"
	Symbol      string     `json:"symbol"`
	Underlying  string     `json:"underlying,omitempty"`
	Expiry      string     `json:"expiry,omitempty"` // YYYY-MM-DD
	Strike      int        `json:"strike,omitempty"`
	OptType     string     `json:"optType,omitempty"` // "CE" | "PE"
	Qty         int        `json:"qty"`
	AvgPrice    float64    `json:"avgPrice"`
	RealizedPnL float64    `json:"realizedPnl"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

// PaperOrder is one row of kairos.paper_orders. JSON tags match
// ApiPaperOrder in kairos/lib/kairos-api.ts exactly.
type PaperOrder struct {
	ID           uuid.UUID  `json:"id"`
	UserID       uuid.UUID  `json:"-"`
	Kind         string     `json:"kind"` // "EQ" | "OPT"
	Symbol       string     `json:"symbol"`
	Underlying   string     `json:"underlying,omitempty"`
	Expiry       string     `json:"expiry,omitempty"` // YYYY-MM-DD
	Strike       int        `json:"strike,omitempty"`
	OptType      string     `json:"optType,omitempty"` // "CE" | "PE"
	Side         string     `json:"side"`              // "BUY" | "SELL"
	OrderType    string     `json:"orderType"`         // "MARKET" | "LIMIT"
	Qty          int        `json:"qty"`
	LimitPrice   *float64   `json:"limitPrice,omitempty"`
	Status       string     `json:"status"` // OPEN | FILLED | CANCELLED | REJECTED
	RejectReason string     `json:"rejectReason,omitempty"`
	FillPrice    *float64   `json:"fillPrice,omitempty"`
	Fees         *float64   `json:"fees,omitempty"`
	PlacedAt     time.Time  `json:"placedAt"`
	FilledAt     *time.Time `json:"filledAt,omitempty"`
}

// Reject reasons / engine sentinels.
const (
	rejectNoFreshPrice      = "no_fresh_price"
	rejectInsufficientFunds = "insufficient_funds"
)

var (
	// errNoFreshPrice — no executable price (provider down, no quote,
	// or the latest chain snapshot is older than paperPriceMaxAge).
	errNoFreshPrice = errors.New("paper: no fresh price")
	// errInsufficientFunds — the account can't fund the fill. Returned
	// by FillPaperOrderTx; the whole tx rolls back.
	errInsufficientFunds = errors.New("paper: insufficient funds")
)

// paperPriceMaxAge is how old an option-chain snapshot may be and still
// count as an executable price (spec §3.5: reject when >120s stale).
const paperPriceMaxAge = 120 * time.Second

// paperMarginPct is the SELL-side margin proxy: cash must cover
// fees + paperMarginPct × notional before a sell fills. Documented
// simplification — see the package comment above.
const paperMarginPct = 0.15

// markLookback bounds how far back position marks / sweep prices may
// search the chain table. Keeps the per-contract lookup inside a few
// weekly partitions instead of scanning all of them.
const markLookback = 7 * 24 * time.Hour

// quotesFunc is the narrow live-quote dependency the engine needs for
// equity prices — satisfied by marketdata.Service.Quotes or
// provider.QuoteFetcher.FetchQuotes.
type quotesFunc func(ctx context.Context, symbols []string) ([]provider.Quote, error)

// ─────────────────────────────────────────────────────────────────────────
// Fill math
// ─────────────────────────────────────────────────────────────────────────

// ApplyFill folds one fill into a position using the avg-price method:
//
//   - extending the position (same direction, or from flat) re-weights
//     the average price;
//   - reducing it realizes (closed qty × favourable price move) into
//     RealizedPnL and leaves the average untouched;
//   - crossing zero realizes the P&L on the whole closed portion and
//     opens the remainder at the fill price (avg resets).
//
// side is "BUY" or "SELL"; qty is the (positive) fill quantity; price
// the per-unit fill price. Pure function — identity fields pass through.
func ApplyFill(pos PaperPosition, side string, qty int, price float64) PaperPosition {
	signed := qty
	if side == "SELL" {
		signed = -qty
	}

	switch {
	case pos.Qty == 0 || (pos.Qty > 0) == (signed > 0):
		// Flat or extending: weighted average.
		oldAbs := math.Abs(float64(pos.Qty))
		newAbs := oldAbs + float64(qty)
		pos.AvgPrice = (oldAbs*pos.AvgPrice + float64(qty)*price) / newAbs
		pos.Qty += signed

	default:
		// Reducing / flipping.
		closeQty := min(qty, abs(pos.Qty))
		if pos.Qty > 0 {
			pos.RealizedPnL += float64(closeQty) * (price - pos.AvgPrice)
		} else {
			pos.RealizedPnL += float64(closeQty) * (pos.AvgPrice - price)
		}
		pos.Qty += signed
		if pos.Qty == 0 {
			pos.AvgPrice = 0
		} else if qty > closeQty {
			// Crossed zero — the remainder opens at the fill price.
			pos.AvgPrice = price
		}
		// Partial close: average price is untouched.
	}
	return pos
}

// LimitCrossed reports whether a resting limit order is executable at
// the last seen price: a BUY fills at or below its limit, a SELL at or
// above.
func LimitCrossed(side string, limit, last float64) bool {
	if side == "BUY" {
		return last <= limit
	}
	return last >= limit
}

// applySlippage moves a fill price one configured tick against the
// order (BUY pays more, SELL receives less), floored at one tick so a
// near-zero premium can't go non-positive.
func applySlippage(side string, price float64) float64 {
	slip := float64(SlippageTicks) * TickSize
	if side == "BUY" {
		return price + slip
	}
	if out := price - slip; out >= TickSize {
		return out
	}
	return TickSize
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// paperContractSymbol builds the deterministic symbol that keys an
// option contract in paper_orders/paper_positions (UNIQUE(user_id,
// symbol)). E.g. "NIFTY 2026-06-25 24000 CE".
func paperContractSymbol(underlying string, expiry string, strike int, optType string) string {
	return fmt.Sprintf("%s %s %d %s", underlying, expiry, strike, optType)
}

// ─────────────────────────────────────────────────────────────────────────
// Order execution
// ─────────────────────────────────────────────────────────────────────────

// ExecutePaperMarketOrder runs the MARKET fill flow for a validated,
// not-yet-persisted order: resolve a fresh price, apply 1-tick adverse
// slippage, compute fees via the shared cost model, then write order +
// position + cash in one tx. Price-less or unfundable orders are
// persisted as REJECTED with a reason — that's a successful API
// outcome, not an error; err is non-nil only for infrastructure
// failures (DB down etc.).
func ExecutePaperMarketOrder(ctx context.Context, store *Store, quotes quotesFunc, ord *PaperOrder) error {
	price, err := resolvePaperPrice(ctx, store, quotes, ord, time.Now().Add(-paperPriceMaxAge))
	if err != nil {
		if errors.Is(err, errNoFreshPrice) {
			ord.Status = "REJECTED"
			ord.RejectReason = rejectNoFreshPrice
			return store.InsertPaperOrder(ctx, ord)
		}
		return err
	}
	price = applySlippage(ord.Side, price)
	fees := round2(ComputeOrderCosts(OrderCosts{Side: ord.Side, Premium: price, Qty: float64(ord.Qty)}).Total)

	err = store.FillPaperOrderTx(ctx, ord, round2(price), fees)
	if errors.Is(err, errInsufficientFunds) {
		ord.Status = "REJECTED"
		ord.RejectReason = rejectInsufficientFunds
		return store.InsertPaperOrder(ctx, ord)
	}
	return err
}

// resolvePaperPrice finds the executable per-unit price for an order:
//
//	EQ:  live quote last (via the quotes dependency)
//	OPT: latest chain row newer than `since` — BUY hits the Ask, SELL
//	     hits the Bid; LTP is the fallback when that side of the book
//	     wasn't captured.
//
// errNoFreshPrice when nothing usable exists.
func resolvePaperPrice(ctx context.Context, store *Store, quotes quotesFunc, ord *PaperOrder, since time.Time) (float64, error) {
	if ord.Kind == "EQ" {
		if quotes == nil {
			return 0, errNoFreshPrice
		}
		qs, err := quotes(ctx, []string{ord.Symbol})
		if err != nil || len(qs) == 0 || qs[0].Last <= 0 {
			return 0, errNoFreshPrice
		}
		return qs[0].Last, nil
	}

	expiry, err := time.Parse("2006-01-02", ord.Expiry)
	if err != nil {
		return 0, errNoFreshPrice
	}
	row, err := store.LatestContractQuote(ctx, ord.Underlying, expiry, ord.Strike, ord.OptType, since)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errNoFreshPrice
		}
		return 0, err
	}
	price := row.Ask
	if ord.Side == "SELL" {
		price = row.Bid
	}
	if price <= 0 {
		price = row.LTP
	}
	if price <= 0 {
		return 0, errNoFreshPrice
	}
	return price, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Limit-order sweep (called by cron/jobs/kairos_paper.go every 60s
// during market hours)
// ─────────────────────────────────────────────────────────────────────────

// PaperSweepResult summarises one sweep pass for ingest-run logging.
type PaperSweepResult struct {
	Scanned  int
	Filled   int
	Rejected int
	Errs     []string
}

// SweepPaperLimitOrders loads every OPEN limit order (all users),
// batches the equity marks into one quote call, and fills each order
// whose limit the market has crossed via FillPaperOrderTx. Orders
// without a fresh price simply stay OPEN for the next tick; orders the
// account can no longer fund flip to REJECTED insufficient_funds.
//
// Limit fills execute at the observed market price (which is at-or-
// better than the limit by definition of the cross) with no extra
// slippage — the limit price already bounds the fill.
func SweepPaperLimitOrders(ctx context.Context, store *Store, quotes quotesFunc, logger *slog.Logger) PaperSweepResult {
	var res PaperSweepResult
	orders, err := store.OpenLimitOrders(ctx)
	if err != nil {
		res.Errs = append(res.Errs, "load open orders: "+err.Error())
		return res
	}
	res.Scanned = len(orders)
	if len(orders) == 0 {
		return res
	}

	// One batched quote call for all distinct EQ symbols.
	eqLast := make(map[string]float64)
	var eqSymbols []string
	seen := make(map[string]bool)
	for _, o := range orders {
		if o.Kind == "EQ" && !seen[o.Symbol] {
			seen[o.Symbol] = true
			eqSymbols = append(eqSymbols, o.Symbol)
		}
	}
	if len(eqSymbols) > 0 && quotes != nil {
		qs, err := quotes(ctx, eqSymbols)
		if err != nil {
			// Provider down — EQ orders stay OPEN this tick.
			logger.Warn("paper sweep: equity quotes failed", "err", err)
		} else {
			for _, q := range qs {
				eqLast[q.Symbol] = q.Last
			}
		}
	}

	since := time.Now().Add(-paperPriceMaxAge)
	for i := range orders {
		ord := &orders[i]
		if ord.LimitPrice == nil {
			continue // defensive: only LIMIT orders rest OPEN
		}
		var last float64
		if ord.Kind == "EQ" {
			last = eqLast[ord.Symbol]
		} else {
			p, err := resolvePaperPrice(ctx, store, nil, ord, since)
			if err != nil {
				if !errors.Is(err, errNoFreshPrice) {
					res.Errs = append(res.Errs, ord.ID.String()+": "+err.Error())
				}
				continue
			}
			last = p
		}
		if last <= 0 || !LimitCrossed(ord.Side, *ord.LimitPrice, last) {
			continue
		}

		fees := round2(ComputeOrderCosts(OrderCosts{Side: ord.Side, Premium: last, Qty: float64(ord.Qty)}).Total)
		err := store.FillPaperOrderTx(ctx, ord, round2(last), fees)
		switch {
		case err == nil:
			res.Filled++
		case errors.Is(err, errInsufficientFunds):
			if rerr := store.RejectPaperOrder(ctx, ord.ID, rejectInsufficientFunds); rerr != nil {
				res.Errs = append(res.Errs, ord.ID.String()+": reject: "+rerr.Error())
			} else {
				res.Rejected++
			}
		case errors.Is(err, pgx.ErrNoRows):
			// Cancelled between load and fill — nothing to do.
		default:
			res.Errs = append(res.Errs, ord.ID.String()+": "+err.Error())
		}
	}
	return res
}
