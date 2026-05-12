package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// ctxKey is unexported so callers can't accidentally collide on the type
// (Go requires the *type identity* to match, not just the value).
type ctxKey int

const (
	ctxKeyUserID ctxKey = iota
	ctxKeyEmail
)

// apiTokenPrefix matches the prefix the Pegasus browser extension uses
// for long-lived bearer tokens (issued via /job-tracker/me/api-token).
// We branch on this prefix so a JWT — which is dot-separated base64url
// and never starts with "pg_" — always takes the JWT path.
const apiTokenPrefix = "pg_"

// scopeRouteAllowlist enumerates the routes each scope unlocks. A token
// with scope `S` may only hit routes listed under `S`. This is checked
// against r.Method + " " + r.URL.Path — every entry below is a literal
// route with no path params, which keeps the check trivial.
//
// Adding a new scope: add an entry here AND default-issue tokens with
// that scope for the appropriate flow. Adding a new extension route:
// list it under "extension:capture".
//
// Tokens with empty scopes are legacy "full access" — the gate skips
// the allow-list check for those (existing behavior).
var scopeRouteAllowlist = map[string]map[string]bool{
	"extension:capture": {
		"GET /job-tracker/me":                          true,
		"GET /job-tracker/applications/check-link":     true,
		"POST /job-tracker/applications":               true,
	},
}

// allowedByScopes returns true if any of the token's scopes whitelist
// the requested route. Empty scopes is full access (legacy tokens).
func allowedByScopes(scopes []string, method, path string) bool {
	if len(scopes) == 0 {
		return true
	}
	key := method + " " + path
	for _, s := range scopes {
		if routes, ok := scopeRouteAllowlist[s]; ok && routes[key] {
			return true
		}
	}
	return false
}

// RequireUser is a middleware factory: it returns a function that wraps an
// http.Handler with bearer-token validation. On success, the user's UUID
// (and email when available) are placed on the request context for
// downstream handlers.
//
// The middleware accepts two token shapes:
//   - JWTs (default) — short-lived, browser-session-scoped.
//   - "pg_..." API tokens — long-lived, per-user, issued from Settings
//     and stored in auth.api_tokens. Used by the browser extension.
//
// If `store` is nil, the pg_ path is disabled and only JWTs are accepted
// (kept optional so callers that don't need extension auth can skip the
// dependency).
func RequireUser(secret, issuer, audience string, store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			raw := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
			if raw == "" {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			var (
				id    uuid.UUID
				email string
				err   error
			)
			if store != nil && strings.HasPrefix(raw, apiTokenPrefix) {
				var scopes []string
				id, scopes, err = store.LookupAPIToken(r.Context(), raw)
				if err == nil {
					// Enforce scope before doing anything else. Scope rejection
					// is 403 (you ARE authenticated, you're just not allowed
					// here), distinct from 401 token-invalid.
					if !allowedByScopes(scopes, r.Method, r.URL.Path) {
						httpx.WriteError(w, http.StatusForbidden, "scope_denied",
							"this token isn't authorised for this endpoint")
						return
					}
					// Email is best-effort — a missing row here just means we
					// won't have it on context (downstream handlers that need
					// it should fetch via authStore.GetUserByID anyway).
					if u, uErr := store.GetUserByID(r.Context(), id); uErr == nil {
						email = u.Email
					}
				}
			} else {
				id, email, err = VerifyJWT(secret, issuer, audience, raw)
			}
			if err != nil {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeyUserID, id)
			ctx = context.WithValue(ctx, ctxKeyEmail, email)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserID returns the authenticated user's UUID from the context, or
// uuid.Nil + false if no user was set (i.e. handler is not behind RequireUser).
func UserID(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(ctxKeyUserID).(uuid.UUID)
	return v, ok
}

// MustUserID is the same as UserID but panics if no user is set. Use only
// from handlers that are unconditionally behind RequireUser.
func MustUserID(ctx context.Context) uuid.UUID {
	v, ok := UserID(ctx)
	if !ok {
		panic("auth.MustUserID: no user on context — handler not behind RequireUser?")
	}
	return v
}

// Email returns the authenticated user's email from the context, if set.
func Email(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyEmail).(string)
	return v, ok
}

// WithUserID returns a copy of ctx with the given user id stamped on it.
// Intended for unit tests that exercise handlers which call MustUserID —
// it mirrors what RequireUser does in production without the JWT overhead.
func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, id)
}
