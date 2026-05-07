package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims are the bits we put inside a JWT. We embed RegisteredClaims so
// jwt-go validates iss/aud/exp/nbf for us.
type Claims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

// IssueJWT returns a signed HS256 token for the given user.
func IssueJWT(secret, issuer, audience string, userID uuid.UUID, email string, ttl time.Duration) (string, error) {
	now := time.Now().UTC()
	claims := Claims{
		Email: email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// VerifyJWT validates the signature, audience, issuer, and expiry, returning
// the user UUID and email. Returns a non-nil error on any validation failure.
func VerifyJWT(secret, issuer, audience, raw string) (uuid.UUID, string, error) {
	parsed, err := jwt.ParseWithClaims(raw, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithIssuer(issuer), jwt.WithAudience(audience))
	if err != nil {
		return uuid.Nil, "", err
	}
	c, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return uuid.Nil, "", errors.New("invalid token")
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("bad subject: %w", err)
	}
	return id, c.Email, nil
}
