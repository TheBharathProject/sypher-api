package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos"
	"github.com/TheBharathProject/sypher-api/internal/kairos/marketdata"
	"github.com/TheBharathProject/sypher-api/internal/mailer"
)

// KairosAlertsSweep returns a cron func that evaluates every user's
// active price/pct-change alerts against one batched quotes call and
// flips breached rules to triggered (+ email). Scheduled with EveryN(60s)
// alongside the options snapshot (spec §3.4).
//
// Gates on the same market-hours window as KairosOptionsSnapshot
// (09:15–15:30 IST, weekdays — isWithinMarketHoursIST in
// kairos_snapshot.go): quotes don't move outside the session, so a
// closed-market sweep could only re-trigger on stale data.
//
// Provider-capability gating lives below the call: a provider without
// quotes support (null/dhan/upstox/angel) makes kairos.EvaluateAlerts
// log + record a skipped ingest run and return nil, so this job never
// error-spams the cron runner.
func KairosAlertsSweep(store *kairos.Store, md *marketdata.Service, mail mailer.Mailer, logger *slog.Logger) func(context.Context) error {
	// Log throttling — same arithmetic as KairosOptionsSnapshot: a 60s
	// job skipping off-hours would otherwise emit ~1000 identical lines
	// a day.
	var skipCount int
	const skipLogEvery = 30 // log once every 30 minutes during off-hours
	return func(ctx context.Context) error {
		if !isWithinMarketHoursIST(time.Now()) {
			skipCount++
			if skipCount%skipLogEvery == 1 {
				logger.Info("kairos alerts sweep skipped",
					"reason", "market closed",
					"next_log_in_minutes", skipLogEvery)
			}
			return nil
		}
		// Reset throttle when we re-enter market hours.
		skipCount = 0
		return kairos.EvaluateAlerts(ctx, store, md, mail, logger)
	}
}
