package jobs

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/kairos"
)

// KairosCreatePartitions returns a cron func that pre-creates the next
// N weekly partitions of kairos.option_chains. ADR-0009 D4.
//
// Idempotent: uses CREATE TABLE IF NOT EXISTS PARTITION OF. Runs twice
// a month (1st and 15th) so a failed run still has a multi-week
// buffer before inserts start failing for lack of a partition.
//
// Scheduled at 02:30 IST so it doesn't collide with the market-hours
// snapshot cron.
func KairosCreatePartitions(store *kairos.Store, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		const weeksAhead = 8 // ~2 months of buffer
		n, err := store.EnsurePartitions(ctx, weeksAhead)
		if err != nil {
			return err
		}
		logger.Info("kairos partitions ensured", "weeks_ahead", weeksAhead, "created", n)
		return nil
	}
}

// KairosBacktestSweep returns a cron func that re-enqueues any
// kairos.backtests rows stuck in 'pending' that the in-process channel
// missed (e.g. submit happened, channel was full, no live submit since).
//
// Fires every 60s. Cheap — just a SELECT of pending rows + push to
// channel. Channel-full path drops back into the queue.
func KairosBacktestSweep(store *kairos.Store, enqueue func(id uuid.UUID) bool, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		ids, err := store.PendingBacktestIDs(ctx, 50)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if !enqueue(id) {
				logger.Debug("backtest sweep channel full; will retry next tick", "id", id)
				break
			}
		}
		return nil
	}
}
