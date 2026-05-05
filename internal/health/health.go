// Package health exposes a /health endpoint that confirms the service is
// alive AND can reach Postgres. Used by the Docker HEALTHCHECK directive
// and by deploy.sh to confirm a fresh container is serving traffic.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// NewHandler returns an http.Handler that pings the DB and reports liveness.
// The pgxpool.Pool is injected — keeps this package free of global state.
func NewHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok":  false,
				"err": err.Error(),
			})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}
