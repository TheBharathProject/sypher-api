package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// KairosPaperSweep returns a cron func that fills resting paper LIMIT
// orders (spec §3.5). Schedule with EveryN(60 * time.Second).
//
// Each tick during market hours (09:15–15:30 IST weekdays — same gate
// as the snapshot cron): load every OPEN order across all users, batch
// the equity marks into one quote call, fill each order whose limit
// the market has crossed (kairos.SweepPaperLimitOrders — order +
// position + cash in one tx per fill), and append a "paper-sweep" row
// to kairos.ingest_runs.
//
// quotes is the narrow live-quote dependency (same shape as
// provider.QuoteFetcher.FetchQuotes / marketdata.Service.Quotes); nil
// is allowed — equity orders then simply stay OPEN, options orders
// still fill off stored chain snapshots.
//
// Outside market hours the tick is a silent no-op (the snapshot cron
// already logs the market-closed state once per half hour). Ticks that
// found no OPEN orders are not recorded either — at 375 ticks per
// trading day, heartbeat rows would drown the ingest-runs log the
// admin page reads.
func KairosPaperSweep(store *kairos.Store, quotes func(ctx context.Context, symbols []string) ([]provider.Quote, error), logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		if !isWithinMarketHoursIST(time.Now()) {
			return nil
		}
		startedAt := time.Now().UTC()
		res := kairos.SweepPaperLimitOrders(ctx, store, quotes, logger)
		finishedAt := time.Now().UTC()

		if res.Scanned == 0 && len(res.Errs) == 0 {
			return nil
		}
		detail := ""
		if len(res.Errs) > 0 {
			detail = joinTruncated(res.Errs, 500)
		}
		logger.Info("kairos paper sweep",
			"scanned", res.Scanned, "filled", res.Filled,
			"rejected", res.Rejected, "errors", len(res.Errs))

		// Audit row is best-effort and must not fail the cron tick —
		// detached context so it lands even if ctx was cancelled
		// mid-sweep (the fills did happen; log them).
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := store.RecordIngestRun(rctx, "paper-sweep", startedAt, finishedAt,
			len(res.Errs) == 0, res.Filled, detail); err != nil {
			logger.Warn("paper sweep ingest run record failed", "err", err)
		}
		return nil
	}
}

// joinTruncated joins errs with "; " and caps the result at maxLen
// runes — ingest_runs.detail is a log column, not a dumping ground.
func joinTruncated(errs []string, maxLen int) string {
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += "; "
		}
		out += e
	}
	if len(out) > maxLen {
		out = out[:maxLen] + "…"
	}
	return out
}
