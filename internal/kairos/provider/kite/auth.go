package kite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// providerToken is the in-memory and on-disk shape of a row in
// kairos.provider_tokens. Encrypted-at-rest in the DB; plaintext here.
//
// Encryption note: this scaffolding stores plaintext for the first cut
// so the round-trip works end-to-end. Wrap with internal/security/aead
// in a follow-up — see ADR-0011 D5. The Put/Get hooks are isolated so
// the swap is a one-file change.
type providerToken struct {
	Provider     string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Metadata     map[string]any
	UpdatedAt    time.Time
}

// tokenStore is a tiny cache-on-top-of-DB. Reads hit memory; writes go
// to the DB then invalidate the cache. The cache prevents the per-call
// auth header from being a DB query.
type tokenStore struct {
	pool *pgxpool.Pool
	mu   sync.RWMutex
	mem  map[string]providerToken
}

func newTokenStore(pool *pgxpool.Pool) *tokenStore {
	return &tokenStore{pool: pool, mem: map[string]providerToken{}}
}

// Get reads from cache or, on miss, from kairos.provider_tokens.
// Returns the zero value with a nil error if the row doesn't exist
// — callers check AccessToken=="" to decide if auth is missing.
//
// nil pool is tolerated so init in tests (which pass a nil pool) works.
func (t *tokenStore) Get(ctx context.Context, name string) (providerToken, error) {
	t.mu.RLock()
	if v, ok := t.mem[name]; ok {
		t.mu.RUnlock()
		return v, nil
	}
	t.mu.RUnlock()
	if t.pool == nil {
		return providerToken{Provider: name}, nil
	}
	// refresh_token + expires_at are nullable: Kite has no refresh
	// token, and some providers (Dhan) have non-expiring tokens. Scan
	// into pointers and dereference defensively.
	var (
		acc       string
		ref       *string
		exp       *time.Time
		metaRaw   []byte
		updatedAt time.Time
	)
	err := t.pool.QueryRow(ctx, `
		SELECT access_token, refresh_token, expires_at, metadata, updated_at
		FROM kairos.provider_tokens
		WHERE provider = $1`, name).Scan(&acc, &ref, &exp, &metaRaw, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return providerToken{Provider: name}, nil
		}
		return providerToken{}, err
	}
	meta := map[string]any{}
	_ = json.Unmarshal(metaRaw, &meta)
	tok := providerToken{
		Provider:    name,
		AccessToken: acc,
		Metadata:    meta,
		UpdatedAt:   updatedAt,
	}
	if ref != nil {
		tok.RefreshToken = *ref
	}
	if exp != nil {
		tok.ExpiresAt = *exp
	}
	t.mu.Lock()
	t.mem[name] = tok
	t.mu.Unlock()
	return tok, nil
}

// Put upserts a token row and refreshes the cache. Called by the OAuth
// callback handler after exchanging a request_token.
func (t *tokenStore) Put(ctx context.Context, tok providerToken) error {
	if t.pool == nil {
		t.mu.Lock()
		tok.UpdatedAt = time.Now()
		t.mem[tok.Provider] = tok
		t.mu.Unlock()
		return nil
	}
	metaJSON, err := json.Marshal(tok.Metadata)
	if err != nil {
		return err
	}
	if metaJSON == nil {
		metaJSON = []byte("{}")
	}
	var expVal any
	if !tok.ExpiresAt.IsZero() {
		expVal = tok.ExpiresAt
	}
	var refVal any
	if tok.RefreshToken != "" {
		refVal = tok.RefreshToken
	}
	_, err = t.pool.Exec(ctx, `
		INSERT INTO kairos.provider_tokens
			(provider, access_token, refresh_token, expires_at, metadata, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (provider) DO UPDATE SET
			access_token  = EXCLUDED.access_token,
			refresh_token = EXCLUDED.refresh_token,
			expires_at    = EXCLUDED.expires_at,
			metadata      = EXCLUDED.metadata,
			updated_at    = NOW()
	`, tok.Provider, tok.AccessToken, refVal, expVal, metaJSON)
	if err != nil {
		return err
	}
	// Refresh cache.
	t.mu.Lock()
	tok.UpdatedAt = time.Now()
	t.mem[tok.Provider] = tok
	t.mu.Unlock()
	return nil
}

// sha256Hex is Kite's checksum input for /session/token. They specify
// hex-encoded SHA-256 of (api_key + request_token + api_secret).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
