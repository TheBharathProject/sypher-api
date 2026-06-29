package server

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// Middleware in Go is just a function that wraps an http.Handler and returns
// another http.Handler. Composition is plain function-of-function calls;
// no "decorator" mechanism is needed.
//
//   final := withRecover(withRequestID(withLogging(withCORS(mux, origins), logger)), logger)
//
// Order matters — the outermost wrapper sees the request first.

// withRecover converts any panic in a handler into a 500 response and an
// error log, instead of crashing the server.
func withRecover(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// withRequestID sits just inside us and sets the response
				// header before any handler runs, so reading it back here
				// gives us the ID even though our r predates the context
				// value.
				logger.Error("panic in handler",
					"err", rec,
					"path", r.URL.Path,
					"request_id", w.Header().Get("X-Request-ID"),
					"stack", string(debug.Stack()),
				)
				httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withRequestID tags every request with an ID — an inbound X-Request-ID
// header is honoured (so upstream proxies/clients can correlate), else we
// mint a UUID. The ID is echoed on the response header and stashed in the
// request context so handlers can include it in error logs via
// httpx.RequestID.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context(), id)))
	})
}

// withLogging emits one structured log line per request. We wrap the
// ResponseWriter so we can capture the status code that was written.
func withLogging(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", strconv.FormatInt(time.Since(start).Milliseconds(), 10),
			"remote", httpx.ClientIP(r),
			"request_id", httpx.RequestID(r.Context()),
		)
	})
}

// statusRecorder is a tiny adapter — Go's http.ResponseWriter doesn't
// expose the status after WriteHeader, so we capture it ourselves.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// withCORS handles CORS preflight + sets the right headers on actual
// responses. Allow-list comes from config; we never use the wildcard "*"
// because we want credentials-bearing requests to work in the future.
func withCORS(next http.Handler, allowed []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(allowed, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAPIKey is a middleware *factory* — it returns a middleware
// configured with a specific key. Closures are how we inject config into
// middleware in Go.
//
// We use crypto/subtle.ConstantTimeCompare to avoid timing attacks on the
// comparison. Plain == is fine in 99% of cases; for shared-secret comparisons
// it's the right reflex to keep.
func requireAPIKey(expected string) func(http.Handler) http.Handler {
	expectedBytes := []byte(expected)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-API-Key")
			if subtle.ConstantTimeCompare([]byte(got), expectedBytes) != 1 {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid api key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
