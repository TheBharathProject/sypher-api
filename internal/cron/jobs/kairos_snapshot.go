package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// KairosOptionsSnapshot returns a cron func that snapshots the chain
// for each (underlying, near-expiry) pair and inserts into
// kairos.option_chains.
//
// Skips cleanly (with a visible log line, not silently) when:
//   - the provider's IsReady() returns ErrAuthExpired / ErrNotConfigured /
//     ErrNotImplemented
//   - outside market hours 09:15–15:30 IST (any weekday)
//   - on weekends
//
// Manual / one-off ingest (e.g. seeding data outside market hours for
// testing) goes through POST /kairos/admin/snapshot, which bypasses
// these gates and calls kairos.SnapshotChains directly.
//
// Fires every 60s; see ADR-0013 D3.
func KairosOptionsSnapshot(store *kairos.Store, prov provider.BrokerProvider, logger *slog.Logger) func(context.Context) error {
	// Log throttling: outside-market-hours skip fires once per minute
	// while the API is idle. That's 24×60×60 / 60 = 1440 log lines/day
	// from this one cron, none of which are interesting after the
	// first. Log every Nth skip so the line is visible without
	// flooding journals.
	var skipCount int
	const skipLogEvery = 30 // log once every 30 minutes during off-hours
	return func(ctx context.Context) error {
		if !isWithinMarketHoursIST(time.Now()) {
			skipCount++
			if skipCount%skipLogEvery == 1 {
				logger.Info("kairos snapshot skipped",
					"reason", "market closed",
					"next_log_in_minutes", skipLogEvery)
			}
			return nil
		}
		// Reset throttle when we re-enter market hours.
		skipCount = 0

		if err := prov.IsReady(ctx); err != nil {
			if errors.Is(err, provider.ErrAuthExpired) ||
				errors.Is(err, provider.ErrNotConfigured) ||
				errors.Is(err, provider.ErrNotImplemented) {
				logger.Info("kairos snapshot skipped", "reason", err.Error())
				return nil
			}
			return fmt.Errorf("provider not ready: %w", err)
		}
		_ = kairos.SnapshotChains(ctx, store, prov, logger)
		return nil
	}
}

// isWithinMarketHoursIST returns true when the time is a weekday and
// between 09:15 and 15:30 IST. Holiday handling is a separate future
// job (ADR-0013 D3 notes this is a TODO).
func isWithinMarketHoursIST(now time.Time) bool {
	t := now.In(IST)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	openMin := 9*60 + 15
	closeMin := 15*60 + 30
	cur := t.Hour()*60 + t.Minute()
	return cur >= openMin && cur <= closeMin
}
