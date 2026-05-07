// Command migrate applies every .sql file in /app/migrations/ in lex order
// against DATABASE_URL, then exits.
//
// Each file must be idempotent (CREATE ... IF NOT EXISTS, etc.) so re-runs
// are safe. There's no "down" migration support — keep changes additive.
//
// Run as a one-shot via Docker:
//
//	docker run --rm -e DATABASE_URL=... <image> migrate
//
// (entrypoint.sh dispatches to this binary when the first arg is "migrate".)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// migrationsDir defaults to the Docker path. Local dev can override with the
// MIGRATIONS_DIR env var (e.g. MIGRATIONS_DIR=./migrations).
const defaultMigrationsDir = "/app/migrations"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(logger); err != nil {
		logger.Error("migrate failed", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL not set")
	}

	dir := os.Getenv("MIGRATIONS_DIR")
	if dir == "" {
		dir = defaultMigrationsDir
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	if len(files) == 0 {
		logger.Info("no migrations to apply", "dir", dir)
		return nil
	}
	sort.Strings(files) // lex order = numbered order, when files are 0001_*.sql

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	// Bookkeeping table: records which migration files have been applied
	// so re-runs skip already-done files. Migrations are still expected to
	// be idempotent (so a half-applied file can be retried), but this
	// avoids re-parsing later migrations that depend on schema drift from
	// even later migrations (e.g. column renames in 0006 invalidating
	// CREATE INDEX statements in 0003).
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS public._migrations_applied (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("ensure tracking table: %w", err)
	}

	rows, err := conn.Query(ctx, `SELECT filename FROM public._migrations_applied`)
	if err != nil {
		return fmt.Errorf("read tracking table: %w", err)
	}
	applied := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return fmt.Errorf("scan tracking row: %w", err)
		}
		applied[n] = true
	}
	rows.Close()

	count := 0
	for _, f := range files {
		name := filepath.Base(f)
		if applied[name] {
			logger.Info("skipping", "file", name, "reason", "already applied")
			continue
		}
		logger.Info("applying", "file", name)

		sql, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO public._migrations_applied (filename) VALUES ($1)`, name); err != nil {
			return fmt.Errorf("record %s applied: %w", name, err)
		}
		count++
	}
	logger.Info("done", "applied", count, "skipped", len(files)-count)
	return nil
}
