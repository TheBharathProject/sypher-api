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

// IssueAPIToken creates a new token for the user. Returns the *plain* token
// once (caller must show it to the user immediately) plus the row metadata.
// Storage holds only the hash.
func (s *Store) IssueAPIToken(ctx context.Context, userID uuid.UUID, label string) (plainToken string, meta *APIToken, err error) {
	raw, err := RandomState() // re-use random helper; produces ~32 chars
	if err != nil {
		return "", nil, fmt.Errorf("rand: %w", err)
	}
	plain := "pg_" + raw
	sum := sha256.Sum256([]byte(plain))
	hash := hex.EncodeToString(sum[:])
	prefix := plain[:8]

	const q = `
		INSERT INTO auth.api_tokens (user_id, token_hash, prefix, label)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		RETURNING id, prefix, COALESCE(label, ''), to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	`
	m := &APIToken{}
	err = s.pool.QueryRow(ctx, q, userID, hash, prefix, label).
		Scan(&m.ID, &m.Prefix, &m.Label, &m.CreatedAt)
	if err != nil {
		return "", nil, fmt.Errorf("insert token: %w", err)
	}
	return plain, m, nil
}

// LookupAPIToken returns the user_id behind a plain token, or pgx.ErrNoRows.
func (s *Store) LookupAPIToken(ctx context.Context, plain string) (uuid.UUID, error) {
	sum := sha256.Sum256([]byte(plain))
	hash := hex.EncodeToString(sum[:])
	const q = `
		UPDATE auth.api_tokens SET last_used_at = NOW()
		WHERE token_hash = $1
		RETURNING user_id
	`
	var uid uuid.UUID
	err := s.pool.QueryRow(ctx, q, hash).Scan(&uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, pgx.ErrNoRows
		}
		return uuid.Nil, err
	}
	return uid, nil
}
