// Package db wraps Postgres connection pooling.
//
// We use github.com/jackc/pgx/v5/pgxpool — pgx is the standard for Go +
// Postgres. It's faster than database/sql + lib/pq, supports Postgres-
// specific types properly, and pgxpool gives us a connection pool out of
// the box.
//
// Why a *pgxpool.Pool, not a single *pgx.Conn:
//
//	HTTP servers handle concurrent requests in separate goroutines. Each
//	needs its own DB connection while it's working. The pool hands out
//	connections from a shared set and recycles them. We size it small (1-4
//	connections) because the OCI VM is shared infra; tune up if needed.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool builds the pool, pings the DB to confirm it's actually reachable,
// and returns the live pool. Callers are responsible for calling pool.Close()
// at shutdown — see cmd/api/main.go for the defer pattern.
//
// ctx carries the deadline. We pass it both to pool construction (which is
// instant) and to the ping. If the ctx is cancelled mid-ping, the function
// returns the cancellation error.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// fmt.Errorf with %w wraps the original error — callers can still
		// inspect it via errors.Is / errors.As. Plain error strings lose context.
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	// Sensible defaults for a small backend on a small VM.
	cfg.MinConns = 1
	cfg.MaxConns = 4
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	// Confirm we can actually reach Postgres before returning.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	return pool, nil
}
