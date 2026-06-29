package kairos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// Pool is the backtest worker pool. ADR-0010: in-process goroutines
// pulling from a buffered channel; one Pool per server process.
type Pool struct {
	ch     chan uuid.UUID
	store  *Store
	prov   provider.BrokerProvider
	logger *slog.Logger
	size   int

	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}
}

// NewPool constructs a pool. Size 3 because the active provider's
// historical-data rate limit (Kite: 3 rps) is the gating factor, not
// CPU. Buffered channel size 100 is the spillover for bursty submit
// traffic — overflow goes to the next sweep, not lost.
func NewPool(store *Store, prov provider.BrokerProvider, logger *slog.Logger) *Pool {
	return &Pool{
		ch:     make(chan uuid.UUID, 100),
		store:  store,
		prov:   prov,
		logger: logger,
		size:   3,
		done:   make(chan struct{}),
	}
}

// Start spawns the workers and runs the startup sweep (ADR-0010 D5).
// Idempotent.
func (p *Pool) Start(ctx context.Context) {
	p.startOnce.Do(func() {
		// 1) Reset anything stuck "running" for too long.
		if n, err := p.store.SweepStaleRunning(ctx); err != nil {
			p.logger.Warn("backtest sweep stale failed", "err", err)
		} else if n > 0 {
			p.logger.Info("backtest sweep reset", "rows", n)
		}
		// 2) Push existing pending rows onto the channel.
		ids, err := p.store.PendingBacktestIDs(ctx, cap(p.ch))
		if err != nil {
			p.logger.Warn("backtest pending sweep failed", "err", err)
		}
		for _, id := range ids {
			select {
			case p.ch <- id:
			default:
				// Channel full — next sweep will pick it up.
			}
		}
		// 3) Workers.
		for i := 0; i < p.size; i++ {
			i := i
			go p.worker(ctx, i)
		}
		p.logger.Info("backtest pool started", "size", p.size, "pending_enqueued", len(ids))
	})
}

// Enqueue is the non-blocking submit path called by the HTTP handler.
// Returns true if the channel accepted the id, false if the buffer is
// full (in which case the row stays pending and the next periodic
// sweep — invoked from the cron, see jobs/kairos_sweep.go — will pick
// it up).
func (p *Pool) Enqueue(id uuid.UUID) bool {
	select {
	case p.ch <- id:
		return true
	default:
		return false
	}
}

// Depth reports how many backtest ids are currently buffered on the
// worker channel — the admin health page's "queue depth" card. A point-
// in-time read; jobs already claimed by a worker don't count.
func (p *Pool) Depth() int {
	return len(p.ch)
}

// Stop closes the channel and waits for workers to finish their
// current job. Called by server.Stop.
func (p *Pool) Stop() {
	p.stopOnce.Do(func() {
		close(p.done)
	})
}

// worker is one goroutine. Loops until ctx is done or Stop is called.
func (p *Pool) worker(ctx context.Context, idx int) {
	p.logger.Debug("backtest worker started", "idx", idx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case id := <-p.ch:
			p.runJob(ctx, id)
		}
	}
}

// runJob claims the row, executes, persists. Errors are logged + stored
// on the row; the worker keeps going.
func (p *Pool) runJob(ctx context.Context, id uuid.UUID) {
	bt, err := p.store.ClaimNextBacktest(ctx, id)
	if err != nil {
		// pgx.ErrNoRows is expected when another worker already claimed it.
		p.logger.Debug("backtest claim skipped", "id", id, "err", err)
		return
	}
	start := time.Now()
	res, err := p.simulate(ctx, bt)
	if err != nil {
		p.logger.Warn("backtest failed", "id", id, "err", err, "elapsed", time.Since(start))
		_ = p.store.FailBacktest(ctx, id, err.Error())
		return
	}
	if err := p.store.FinishBacktest(ctx, id, res); err != nil {
		p.logger.Warn("backtest persist failed", "id", id, "err", err)
		return
	}
	p.logger.Info("backtest done", "id", id, "trades", len(res.Trades), "pnl", res.TotalPnL, "elapsed", time.Since(start))
}

// ─────────────────────────────────────────────────────────────────────────
// Simulation
// ─────────────────────────────────────────────────────────────────────────

// Cost model constants live in costs.go (shared with the paper-trading
// engine); see ComputeOrderCosts, SlippageTicks, TickSize.

// lotSizes mirror handler.go's lookups; duplicated here so simulation
// is self-contained.
var simLotSizes = map[provider.Underlying]int{
	provider.UnderlyingNIFTY:     75,
	provider.UnderlyingBANKNIFTY: 35,
	provider.UnderlyingSENSEX:    20,
}
var simStrikeSteps = map[provider.Underlying]int{
	provider.UnderlyingNIFTY:     50,
	provider.UnderlyingBANKNIFTY: 100,
	provider.UnderlyingSENSEX:    200,
}

// simulate runs the strategy over each trading day in [from, to],
// reading entry-time and exit-time chains from our DB, and applies the
// cost model. Returns a BacktestResult with aggregate stats and a
// trade-by-trade log.
//
// Approach (ADR-0010): we don't call the provider during simulation —
// all data comes from kairos.option_chains. If a particular day has
// no snapshots near the entry time (chain was missing because the
// market was closed, ingest was down, etc.), that day is skipped with
// outcome="skipped" and zero P&L.
func (p *Pool) simulate(ctx context.Context, bt *Backtest) (*BacktestResult, error) {
	u := provider.Underlying(bt.Underlying)
	lotSize, ok := simLotSizes[u]
	if !ok {
		return nil, fmt.Errorf("unsupported underlying: %s", bt.Underlying)
	}
	from, err := time.Parse("2006-01-02", bt.FromDate)
	if err != nil {
		return nil, err
	}
	to, err := time.Parse("2006-01-02", bt.ToDate)
	if err != nil {
		return nil, err
	}
	entryHM, err := parseHHMM(bt.EntryTime)
	if err != nil {
		return nil, err
	}
	exitHM, err := parseHHMM(bt.ExitTime)
	if err != nil {
		return nil, err
	}
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		ist = time.FixedZone("IST", 5*3600+30*60)
	}

	res := &BacktestResult{}
	var equityCurve []float64
	cumulative := 0.0

	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		// Skip weekends. NSE doesn't trade Saturday/Sunday.
		switch d.Weekday() {
		case time.Saturday, time.Sunday:
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		entryAt := time.Date(d.Year(), d.Month(), d.Day(), entryHM.h, entryHM.m, 0, 0, ist)
		exitAt := time.Date(d.Year(), d.Month(), d.Day(), exitHM.h, exitHM.m, 0, 0, ist)

		// Resolve the closest expiry that's tradeable on this day.
		// The strategy's expiryRule (WEEKLY|NEXT_WEEKLY|MONTHLY) picks
		// which one. For now we just pick the nearest Thursday ≥ d as
		// "weekly" — a full implementation would derive from the
		// instruments table.
		weekly := nextWeeklyExpiry(d)
		expiryByRule := map[string]time.Time{
			"WEEKLY":      weekly,
			"NEXT_WEEKLY": weekly.AddDate(0, 0, 7),
			"MONTHLY":     lastThursdayOfMonth(d),
		}

		entryChain, err := p.store.ChainAtTime(ctx, u, weekly, entryAt)
		if err != nil {
			p.logger.Debug("backtest entry-chain miss", "day", d.Format("2006-01-02"), "err", err)
			continue
		}
		if len(entryChain) == 0 {
			continue
		}
		exitChain, err := p.store.ChainAtTime(ctx, u, weekly, exitAt)
		if err != nil || len(exitChain) == 0 {
			continue
		}
		spot := entryChain[0].Spot
		if spot == 0 {
			continue
		}
		atm := atmStrike(spot, simStrikeSteps[u])

		// Sum entry premium received/paid and exit premium for all legs.
		entryPrem, exitPrem := 0.0, 0.0
		valid := true
		for _, leg := range bt.Legs {
			expiryFor := expiryByRule[leg.ExpiryRule]
			if expiryFor.IsZero() {
				expiryFor = weekly
			}
			strike := atm + strikeOffsetForRule(leg.StrikeRule, simStrikeSteps[u])
			entryRow := findRow(entryChain, strike, provider.OptionType(leg.OptType))
			exitRow := findRow(exitChain, strike, provider.OptionType(leg.OptType))
			if entryRow == nil || exitRow == nil {
				valid = false
				break
			}
			// BUY = pay entry, receive exit. SELL = receive entry, pay exit.
			// Per ADR-0011 D7 we already have realistic LTPs; apply 1-tick
			// slippage to the unfavourable side.
			entryFill := entryRow.LTP
			exitFill := exitRow.LTP
			if leg.Side == "BUY" {
				entryFill += SlippageTicks * TickSize
				exitFill -= SlippageTicks * TickSize
				entryPrem -= entryFill * float64(leg.Lots) * float64(lotSize)
				exitPrem -= exitFill * float64(leg.Lots) * float64(lotSize)
			} else {
				entryFill -= SlippageTicks * TickSize
				exitFill += SlippageTicks * TickSize
				entryPrem += entryFill * float64(leg.Lots) * float64(lotSize)
				exitPrem += exitFill * float64(leg.Lots) * float64(lotSize)
			}
		}
		if !valid {
			continue
		}

		gross := entryPrem - exitPrem
		costs := computeCosts(bt.Legs, lotSize, entryChain, exitChain, atm)
		net := gross - costs
		cumulative += net
		equityCurve = append(equityCurve, cumulative)
		outcome := "flat"
		switch {
		case net > 0:
			outcome = "win"
		case net < 0:
			outcome = "loss"
		}
		res.Trades = append(res.Trades, Trade{
			Date:     d.Format("2006-01-02"),
			Entry:    round2(entryPrem),
			Exit:     round2(exitPrem),
			GrossPnL: round2(gross),
			Costs:    round2(costs),
			NetPnL:   round2(net),
			Outcome:  outcome,
		})
	}

	res.TotalPnL = round2(cumulative)
	res.WinRate = winRate(res.Trades)
	res.Sharpe = sharpe(res.Trades)
	res.MaxDrawdown = round2(maxDrawdown(equityCurve))
	if len(res.Trades) == 0 {
		return res, errors.New("no data available in the requested date range — try a more recent range once historical ingest has accumulated")
	}
	return res, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Simulation helpers
// ─────────────────────────────────────────────────────────────────────────

type hhmm struct{ h, m int }

func parseHHMM(s string) (hhmm, error) {
	if !isHHMM(s) {
		return hhmm{}, fmt.Errorf("invalid time %q", s)
	}
	var hm hhmm
	fmt.Sscanf(s, "%d:%d", &hm.h, &hm.m)
	return hm, nil
}

func atmStrike(spot float64, step int) int {
	if step == 0 {
		return int(spot)
	}
	return int(math.Round(spot/float64(step)) * float64(step))
}

func strikeOffsetForRule(rule string, step int) int {
	switch rule {
	case "ATM":
		return 0
	case "ATM+1":
		return step
	case "ATM+2":
		return 2 * step
	case "ATM+3":
		return 3 * step
	case "ATM-1":
		return -step
	case "ATM-2":
		return -2 * step
	case "ATM-3":
		return -3 * step
	}
	return 0
}

func findRow(rows []provider.ChainRow, strike int, optType provider.OptionType) *provider.ChainRow {
	for i := range rows {
		if rows[i].Strike == strike && rows[i].OptionType == optType {
			return &rows[i]
		}
	}
	return nil
}

// computeCosts applies STT + brokerage + exchange + SEBI + GST via the
// shared per-order model in costs.go. Each leg executes two orders
// (entry + exit); the exit order takes the opposite side, so a SELL leg
// pays STT on its entry fill and a BUY leg on its exit fill.
func computeCosts(legs []Leg, lotSize int, entry, exit []provider.ChainRow, atm int) float64 {
	totalCost := 0.0
	for _, l := range legs {
		strike := atm + strikeOffsetForRule(l.StrikeRule, simStrikeSteps[provider.UnderlyingNIFTY]) // step is best-effort here
		eRow := findRow(entry, strike, provider.OptionType(l.OptType))
		xRow := findRow(exit, strike, provider.OptionType(l.OptType))
		if eRow == nil || xRow == nil {
			continue
		}
		qty := float64(l.Lots * lotSize)
		entrySide, exitSide := "BUY", "SELL"
		if l.Side == "SELL" {
			entrySide, exitSide = "SELL", "BUY"
		}
		ec := ComputeOrderCosts(OrderCosts{Side: entrySide, Premium: eRow.LTP, Qty: qty})
		xc := ComputeOrderCosts(OrderCosts{Side: exitSide, Premium: xRow.LTP, Qty: qty})
		totalCost += ec.Total + xc.Total
	}
	return totalCost
}

func winRate(trades []Trade) float64 {
	if len(trades) == 0 {
		return 0
	}
	wins := 0
	for _, t := range trades {
		if t.NetPnL > 0 {
			wins++
		}
	}
	return round2(float64(wins) / float64(len(trades)))
}

// sharpe is a simple per-trade Sharpe (no annualisation). Good enough
// for relative comparison between strategy variants.
func sharpe(trades []Trade) float64 {
	if len(trades) < 2 {
		return 0
	}
	var sum float64
	for _, t := range trades {
		sum += t.NetPnL
	}
	mean := sum / float64(len(trades))
	var ss float64
	for _, t := range trades {
		d := t.NetPnL - mean
		ss += d * d
	}
	std := math.Sqrt(ss / float64(len(trades)-1))
	if std == 0 {
		return 0
	}
	return round2(mean / std)
}

func maxDrawdown(equity []float64) float64 {
	if len(equity) == 0 {
		return 0
	}
	peak := equity[0]
	maxDD := 0.0
	for _, v := range equity {
		if v > peak {
			peak = v
		}
		dd := v - peak
		if dd < maxDD {
			maxDD = dd
		}
	}
	return maxDD
}

func nextWeeklyExpiry(d time.Time) time.Time {
	t := d
	for t.Weekday() != time.Thursday {
		t = t.AddDate(0, 0, 1)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 15, 30, 0, 0, t.Location())
}

func lastThursdayOfMonth(d time.Time) time.Time {
	first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, d.Location())
	nextMonth := first.AddDate(0, 1, 0)
	t := nextMonth.AddDate(0, 0, -1)
	for t.Weekday() != time.Thursday {
		t = t.AddDate(0, 0, -1)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 15, 30, 0, 0, t.Location())
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// _ keeps sort imported even when worker doesn't call it directly —
// future enhancements (sorting result.Trades by date) will use it.
var _ = sort.Slice
