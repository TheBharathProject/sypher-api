package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/health"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/waitlist"
)

// Server bundles the things we need to serve HTTP. It's a plain struct;
// dependency injection in Go is just "pass things explicitly," not a
// framework feature.
type Server struct {
	cfg    *config.Config
	pool   *pgxpool.Pool
	logger *slog.Logger
	httpd  *http.Server
}

func New(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) *Server {
	s := &Server{cfg: cfg, pool: pool, logger: logger}
	s.httpd = &http.Server{
		Addr:              cfg.HTTPListenAddr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// routes is where every endpoint gets registered. Adding a new tool's API
// is a single block here — keeps the surface visible at a glance.
//
// The mux is stdlib net/http (since Go 1.22 it supports method-and-pattern
// matching like "POST /waitlist", which is all we need at this scale).
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Meta — public, no auth
	mux.HandleFunc("GET /", s.handleRoot)
	mux.Handle("GET /health", health.NewHandler(s.pool))

	// Waitlist — auth gated by X-API-Key
	apiKey := requireAPIKey(s.cfg.WaitlistAPIKey)
	wlStore := waitlist.NewStore(s.pool)
	wlHandler := waitlist.NewHandler(wlStore, s.cfg.IPSalt, s.logger)
	mux.Handle("POST /waitlist", apiKey(wlHandler))

	// Compose middleware. Outer wrappers run first.
	var h http.Handler = mux
	h = withCORS(h, s.cfg.CORSOrigins)
	h = withLogging(h, s.logger)
	h = withRecover(h, s.logger)
	return h
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "sypher-api",
		"version": "0.1.0",
		"status":  "ok",
	})
}

// Start blocks until the server stops, returning the cause.
// Cancellation of ctx triggers a graceful shutdown.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		s.logger.Info("http server starting", "addr", s.cfg.HTTPListenAddr, "env", s.cfg.Env)
		if err := s.httpd.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := s.httpd.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		return err
	}
}
