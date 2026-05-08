// Command cron-once runs a single named cron job exactly once and exits.
//
// Use during local dev / smoke tests so you don't have to wait for the
// scheduled fire time to verify the job's code path. Same DB, same env,
// same Mailer wiring as the long-running api binary.
//
//	go run ./cmd/cron-once stale-apps
//	go run ./cmd/cron-once daily-digest
//
// Reads the same env vars as cmd/api (DATABASE_URL, RESEND_API_KEY, etc).
// Skip RESEND_API_KEY to dry-run the email leg via the slog mailer.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/cron/jobs"
	"github.com/TheBharathProject/sypher-api/internal/jobtracker"
	"github.com/TheBharathProject/sypher-api/internal/mailer"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cron-once <stale-apps|daily-digest>")
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

	var run func(context.Context) error
	switch jobName {
	case "stale-apps":
		run = jobs.MarkStaleApplications(store, n, urls, logger)
	case "daily-digest":
		run = jobs.DailyApplicationDigest(store, n, m, urls, logger)
	default:
		fmt.Fprintf(os.Stderr, "unknown job: %s (expected stale-apps|daily-digest)\n", jobName)
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
