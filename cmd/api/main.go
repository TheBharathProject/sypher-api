// Command api is the sypher-api server binary.
//
// It boots the config, the DB pool, the HTTP server, and traps SIGINT /
// SIGTERM so it can shut down cleanly when the container is stopped.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/db"
	"github.com/TheBharathProject/sypher-api/internal/server"
)

func main() {
	// Structured JSON logging to stdout — Docker captures it; you can grep
	// it with `docker logs sypher-api | jq`.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// run does the actual work. We split it out from main() so we can return an
// error and have main() handle the exit code — the standard Go pattern.
func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// signal.NotifyContext gives us a context that cancels when SIGINT or
	// SIGTERM arrives. Pass it down; everything propagates cancellation
	// properly. This is THE pattern for graceful shutdown in modern Go.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Info("db pool ready")

	srv := server.New(cfg, pool, logger)
	return srv.Start(ctx)
}
