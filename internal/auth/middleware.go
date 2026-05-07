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

// RequireUser is a middleware factory: it returns a function that wraps an
// http.Handler with JWT validation. On success, the user's UUID and email
// are placed on the request context for downstream handlers.
func RequireUser(secret, issuer, audience string) func(http.Handler) http.Handler {
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
			id, email, err := VerifyJWT(secret, issuer, audience, raw)
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
