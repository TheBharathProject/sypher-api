// Command cron-once runs a single named cron job exactly once and exits.
//
// Use during local dev / smoke tests so you don't have to wait for the
// scheduled fire time to verify the job's code path. Same DB, same env,
// same Mailer wiring as the long-running api binary.
//
//	go run ./cmd/cron-once stale-apps
//	go run ./cmd/cron-once daily-digest
//	go run ./cmd/cron-once expire-one-time-premium
//	go run ./cmd/cron-once fire-reminders
//	go run ./cmd/cron-once kairos-options-snapshot
//	go run ./cmd/cron-once kairos-create-partitions
//	go run ./cmd/cron-once kairos-backtest-sweep
//	go run ./cmd/cron-once kairos-alerts
//	go run ./cmd/cron-once kairos-paper-sweep
//	go run ./cmd/cron-once kairos-announcements
//
// Reads the same env vars as cmd/api (DATABASE_URL, RESEND_API_KEY, etc).
// Skip RESEND_API_KEY to dry-run the email leg via the slog mailer.
// The kairos-* jobs additionally need KAIROS_DATA_PROVIDER (and that
// provider's credentials) — same gating as server.routes().
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/billing"
	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/cron/jobs"
	"github.com/TheBharathProject/sypher-api/internal/jobtracker"
	"github.com/TheBharathProject/sypher-api/internal/kairos"
	"github.com/TheBharathProject/sypher-api/internal/kairos/marketdata"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
	"github.com/TheBharathProject/sypher-api/internal/mailer"
)

// jobNames is the usage string — keep in sync with the switch in main.
const jobNames = "stale-apps|daily-digest|expire-one-time-premium|fire-reminders|" +
	"kairos-options-snapshot|kairos-create-partitions|kairos-backtest-sweep|" +
	"kairos-alerts|kairos-paper-sweep|kairos-announcements"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: cron-once <%s>\n", jobNames)
		os.Exit(2)
	}
	jobName := os.Args[1]

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load", "err", err)
		os.Exit(1)
	}

	// Per-run timeout. Generous for digest jobs that fan out emails to
	// many users; tight enough to surface a stuck Postgres or Resend.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	store := jobtracker.NewStore(pool)
	m := mailer.New(cfg, logger)
	n := jobtracker.NewNotifier(store, m, logger)
	urls := jobs.NewURLBuilder(cfg)

	// Kairos deps, mirroring server.routes(): the kairos cron jobs are
	// only registered there when the data provider boots. The
	// long-running api degrades to 503s on provider failure; this
	// binary exists to exercise a job, so a provider that won't boot is
	// a hard error with a pointer at the likely cause.
	newKairosDeps := func() (*kairos.Store, provider.BrokerProvider) {
		kprov, perr := provider.NewProvider(cfg, pool, logger)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "kairos provider init failed: %v\n(check KAIROS_DATA_PROVIDER and the provider's credentials — same env as cmd/api)\n", perr)
			os.Exit(1)
		}
		return kairos.NewStore(pool), kprov
	}

	var run func(context.Context) error
	switch jobName {
	case "stale-apps":
		run = jobs.MarkStaleApplications(store, n, urls, logger)
	case "daily-digest":
		run = jobs.DailyApplicationDigest(store, n, m, urls, logger)
	case "expire-one-time-premium":
		run = jobs.ExpireOneTimePremium(billing.NewStore(pool), logger)
	case "fire-reminders":
		run = jobs.FireDueReminders(store, n, logger)
	case "kairos-options-snapshot":
		kstore, kprov := newKairosDeps()
		run = jobs.KairosOptionsSnapshot(kstore, kprov, logger)
	case "kairos-create-partitions":
		kstore, _ := newKairosDeps()
		run = jobs.KairosCreatePartitions(kstore, logger)
	case "kairos-backtest-sweep":
		kstore, kprov := newKairosDeps()
		// Same construction as server.routes(): the sweep enqueues onto
		// a worker pool. The pool is deliberately NOT started here —
		// the code path under test is SELECT-pending + enqueue, and a
		// started pool would be killed mid-backtest when this one-shot
		// process exits. Rows stay 'pending' for the live api to claim.
		kpool := kairos.NewPool(kstore, kprov, logger)
		run = jobs.KairosBacktestSweep(kstore, kpool.Enqueue, logger)
	case "kairos-alerts":
		// Same construction as server.routes(): one marketdata.Service on
		// top of the active provider feeds the batched quotes call.
		kstore, kprov := newKairosDeps()
		md := marketdata.NewService(kprov, logger)
		run = jobs.KairosAlertsSweep(kstore, md, m, logger)
	case "kairos-paper-sweep":
		kstore, kprov := newKairosDeps()
		md := marketdata.NewService(kprov, logger)
		run = jobs.KairosPaperSweep(kstore, md.Quotes, logger)
	case "kairos-announcements":
		// No provider needed — the NSE feed is public and the job only
		// touches the store, so don't gate on KAIROS_DATA_PROVIDER here.
		run = jobs.KairosAnnouncements(kairos.NewStore(pool), kairos.NewNSEClient(), logger)
	default:
		fmt.Fprintf(os.Stderr, "unknown job: %s (expected %s)\n", jobName, jobNames)
		os.Exit(2)
	}

	logger.Info("cron-once: running", "job", jobName)
	start := time.Now()
	if err := run(ctx); err != nil {
		logger.Error("cron-once: failed", "job", jobName, "err", err, "duration", time.Since(start).Round(time.Millisecond).String())
		os.Exit(1)
	}
	logger.Info("cron-once: done", "job", jobName, "duration", time.Since(start).Round(time.Millisecond).String())
}
