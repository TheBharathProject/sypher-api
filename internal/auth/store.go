package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the data-access layer for auth.users and auth.api_tokens.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// userCols is the canonical column projection for auth.users — keep it in
// one place so adding/renaming columns (like the 0009 premium fields) is a
// single edit. Order MUST match scanUser below.
const userCols = `id, google_id, email, name, COALESCE(picture_url, ''), timezone, is_premium, email_notifications_enabled`

func scanUser(row pgx.Row, u *User) error {
	return row.Scan(
		&u.ID, &u.GoogleID, &u.Email, &u.Name, &u.PictureURL, &u.Timezone,
		&u.IsPremium, &u.EmailNotificationsEnabled,
	)
}

// UpsertUser inserts a new user (matching on google_id) or updates the
// mutable profile fields (name, email, picture). Returns the resulting User.
//
// Note: is_premium + email_notifications_enabled use schema defaults on
// insert, and ON CONFLICT DO UPDATE deliberately doesn't touch them — a
// returning user keeps whatever opt-in state they previously had.
func (s *Store) UpsertUser(ctx context.Context, googleID, email, name, picture string) (*User, error) {
	const q = `
		INSERT INTO auth.users (google_id, email, name, picture_url)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (google_id) DO UPDATE SET
			email = EXCLUDED.email,
			name = EXCLUDED.name,
			picture_url = EXCLUDED.picture_url,
			updated_at = NOW()
		RETURNING ` + userCols
	var u User
	if err := scanUser(s.pool.QueryRow(ctx, q, googleID, email, name, picture), &u); err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}
	return &u, nil
}

// GetUserByID returns the user with the given UUID, or pgx.ErrNoRows.
func (s *Store) GetUserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	q := `SELECT ` + userCols + ` FROM auth.users WHERE id = $1`
	var u User
	if err := scanUser(s.pool.QueryRow(ctx, q, id), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// CanReceiveEmail returns true iff the user is premium AND has opt-in
// enabled. Single round-trip; used at every email-send call site per
// ADR-002 D3. Returns (false, nil) on pgx.ErrNoRows so a deleted user
// silently fails-closed without bubbling the error.
func (s *Store) CanReceiveEmail(ctx context.Context, id uuid.UUID) (bool, error) {
	const q = `SELECT is_premium AND email_notifications_enabled FROM auth.users WHERE id = $1`
	var ok bool
	err := s.pool.QueryRow(ctx, q, id).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// SetEmailPref flips the per-user opt-in. Only callable for premium
// users; the handler enforces that. Returns the updated User so callers
// can echo it without a second SELECT.
func (s *Store) SetEmailPref(ctx context.Context, id uuid.UUID, enabled bool) (*User, error) {
	q := `
		UPDATE auth.users
		SET email_notifications_enabled = $1, updated_at = NOW()
		WHERE id = $2
		RETURNING ` + userCols
	var u User
	if err := scanUser(s.pool.QueryRow(ctx, q, enabled, id), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// UpdateUserName sets the user's display name.
func (s *Store) UpdateUserName(ctx context.Context, id uuid.UUID, name string) error {
	const q = `UPDATE auth.users SET name = $1, updated_at = NOW() WHERE id = $2`
	_, err := s.pool.Exec(ctx, q, name, id)
	return err
}

// UpdateUserTimezone sets the user's IANA timezone.
func (s *Store) UpdateUserTimezone(ctx context.Context, id uuid.UUID, tz string) error {
	const q = `UPDATE auth.users SET timezone = $1, updated_at = NOW() WHERE id = $2`
	_, err := s.pool.Exec(ctx, q, tz, id)
	return err
}

// DeleteUser deletes the auth.users row for the given id. Every product table
// (applications, notes, profile, files, ai_*, etc.) has ON DELETE CASCADE on
// its user_id foreign key per migration 0002+, so the cascade tears down the
// row tree atomically. There's no soft-delete — once gone, gone.
func (s *Store) DeleteUser(ctx context.Context, id uuid.UUID) error {
	const q = `DELETE FROM auth.users WHERE id = $1`
	_, err := s.pool.Exec(ctx, q, id)
	return err
}

// UpsertToolAccess records that a user has accessed a given Sypher tool.
// On first login it inserts a row; on subsequent logins it bumps last_seen.
// The tool string should match a known product slug ("pegasus", "kairos").
func (s *Store) UpsertToolAccess(ctx context.Context, userID uuid.UUID, tool string) error {
	const q = `
		INSERT INTO auth.user_tool_access (user_id, tool)
		VALUES ($1, $2)
		ON CONFLICT (user_id, tool) DO UPDATE SET last_seen = NOW()
	`
	_, err := s.pool.Exec(ctx, q, userID, tool)
	return err
}

// IssueAPIToken creates a new token for the user. Returns the *plain*
// token once (caller must show it to the user immediately) plus the row
// metadata. Storage holds only the hash.
//
// `scopes` defaults to ["extension:capture"] when nil — handlers that
// want a full-access "personal" token can pass an empty slice explicitly.
// See middleware.go for the scope→routes allow-list.
func (s *Store) IssueAPIToken(ctx context.Context, userID uuid.UUID, label string, scopes []string) (plainToken string, meta *APIToken, err error) {
	raw, err := RandomState() // re-use random helper; produces ~32 chars
	if err != nil {
		return "", nil, fmt.Errorf("rand: %w", err)
	}
	plain := "pg_" + raw
	sum := sha256.Sum256([]byte(plain))
	hash := hex.EncodeToString(sum[:])
	prefix := plain[:8]

	if scopes == nil {
		scopes = []string{ScopeExtensionCapture}
	}

	const q = `
		INSERT INTO auth.api_tokens (user_id, token_hash, prefix, label, scopes)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		RETURNING id, prefix, COALESCE(label, ''), scopes,
			to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	`
	m := &APIToken{}
	err = s.pool.QueryRow(ctx, q, userID, hash, prefix, label, scopes).
		Scan(&m.ID, &m.Prefix, &m.Label, &m.Scopes, &m.CreatedAt)
	if err != nil {
		return "", nil, fmt.Errorf("insert token: %w", err)
	}
	return plain, m, nil
}

// LookupAPIToken returns the user_id + scopes behind a plain token, or
// pgx.ErrNoRows when the hash is unknown OR the row has been revoked.
// Bumps last_used_at as part of the same UPDATE so a successful lookup
// always advances the audit timestamp.
//
// Scopes is empty for legacy tokens (full access) or a list like
// ["extension:capture"] for new ones. The caller is responsible for
// gating the request based on the returned scopes.
func (s *Store) LookupAPIToken(ctx context.Context, plain string) (uuid.UUID, []string, error) {
	sum := sha256.Sum256([]byte(plain))
	hash := hex.EncodeToString(sum[:])
	const q = `
		UPDATE auth.api_tokens SET last_used_at = NOW()
		WHERE token_hash = $1 AND revoked_at IS NULL
		RETURNING user_id, scopes
	`
	var (
		uid    uuid.UUID
		scopes []string
	)
	err := s.pool.QueryRow(ctx, q, hash).Scan(&uid, &scopes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil, pgx.ErrNoRows
		}
		return uuid.Nil, nil, err
	}
	return uid, scopes, nil
}

// ListAPITokens returns the user's tokens (active + revoked) most-recent
// first. Plaintext is never available — only the prefix + metadata.
func (s *Store) ListAPITokens(ctx context.Context, userID uuid.UUID) ([]APIToken, error) {
	const q = `
		SELECT id, prefix, COALESCE(label, ''), scopes,
			to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(to_char(last_used_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(to_char(revoked_at,   'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		FROM auth.api_tokens
		WHERE user_id = $1
		ORDER BY created_at DESC
	`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list api_tokens: %w", err)
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Prefix, &t.Label, &t.Scopes, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken flips revoked_at on a token owned by the given user.
// Returns pgx.ErrNoRows if the token doesn't exist, isn't theirs, or is
// already revoked — the handler maps that to 404.
func (s *Store) RevokeAPIToken(ctx context.Context, userID, tokenID uuid.UUID) error {
	const q = `
		UPDATE auth.api_tokens
		SET revoked_at = NOW()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
	`
	tag, err := s.pool.Exec(ctx, q, tokenID, userID)
	if err != nil {
		return fmt.Errorf("revoke api_token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
