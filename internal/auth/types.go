// Package auth owns identity for sypher-api.
//
//	google.go      OAuth URL build + code exchange
//	jwt.go         issue + verify HS256 JWTs
//	store.go       SQL — auth.users + auth.api_tokens
//	middleware.go  RequireUser — gates protected endpoints, puts userID in ctx
//	handler.go     /auth/google, /auth/google/callback, /auth/logout
//	types.go       request/response shapes shared across handlers
//
// Other tool packages depend on auth.RequireUser to gate per-user endpoints
// and on auth.UserID(ctx) to retrieve the authenticated user's UUID.
package auth

import "github.com/google/uuid"

// User is the identity record stored in auth.users.
type User struct {
	ID         uuid.UUID `json:"id"`
	GoogleID   string    `json:"-"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	PictureURL string    `json:"pictureUrl,omitempty"`
	Timezone   string    `json:"timezone"`
}

// GoogleProfile is the subset of fields we read from Google's userinfo
// endpoint after exchanging an OAuth code.
type GoogleProfile struct {
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

// APIToken is a long-lived per-user token (browser extension).
// The plain `token` value is only available at issue time; we store the hash.
type APIToken struct {
	ID         uuid.UUID `json:"id"`
	Prefix     string    `json:"prefix"`
	Label      string    `json:"label,omitempty"`
	CreatedAt  string    `json:"createdAt"`
	LastUsedAt string    `json:"lastUsedAt,omitempty"`
}
