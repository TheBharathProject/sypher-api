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

const migrationsDir = "/app/migrations"

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

	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	if len(files) == 0 {
		logger.Info("no migrations to apply", "dir", migrationsDir)
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

	for _, f := range files {
		name := filepath.Base(f)
		logger.Info("applying", "file", name)

		sql, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	logger.Info("done", "applied", len(files))
	return nil
}
