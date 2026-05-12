package server

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// userLimiter holds a token-bucket limiter per user.
type userLimiter struct {
	lim     *rate.Limiter
	lastSee time.Time
}

// limiterMap is a map from user ID → userLimiter with a RW mutex.
// We GC stale entries (unseen for > 5 min) on each access so the map
// doesn't grow unboundedly.
type limiterMap struct {
	mu      sync.Mutex
	entries map[string]*userLimiter
	r       rate.Limit
	b       int
}

func newLimiterMap(r rate.Limit, b int) *limiterMap {
	return &limiterMap{
		entries: make(map[string]*userLimiter),
		r:       r,
		b:       b,
	}
}

func (m *limiterMap) get(uid string) *rate.Limiter {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	// GC entries not seen for > 5 minutes to cap memory.
	for k, v := range m.entries {
		if now.Sub(v.lastSee) > 5*time.Minute {
			delete(m.entries, k)
		}
	}

	e, ok := m.entries[uid]
	if !ok {
		e = &userLimiter{lim: rate.NewLimiter(m.r, m.b)}
		m.entries[uid] = e
	}
	e.lastSee = now
	return e.lim
}

// withPathRateLimit wraps next so that requests whose path starts with any of
// the given prefixes are gated by the rl middleware; all others pass straight
// through. This lets us apply different limiters to different route groups
// without threading limiters through RegisterRoutes.
func withPathRateLimit(next http.Handler, rl func(http.Handler) http.Handler, prefixes ...string) http.Handler {
	limited := rl(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range prefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				limited.ServeHTTP(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// withUserRateLimit returns a middleware that enforces the given limiter map
// on authenticated requests, keyed by user ID. Unauthenticated requests pass
// through (the auth middleware will reject them anyway).
func withUserRateLimit(m *limiterMap) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, ok := auth.UserID(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			if !m.get(uid.String()).Allow() {
				w.Header().Set("Retry-After", "60")
				httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests — slow down")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
