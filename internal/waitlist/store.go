package waitlist

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the data-access layer for waitlist signups.
//
// "Store" instead of "Repository" — Go community convention is plain English
// names for data layers (Store, Repo, both fine; Store is shorter).
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Insert tries to record a new signup. Returns:
//
//	(true,  nil) — new row inserted
//	(false, nil) — email already on the list (ON CONFLICT DO NOTHING fired)
//	(_, err)     — actual DB error
//
// Note that we lower-case the email here, not in the handler. The store
// owns "what does a row look like in the DB" — keeping that knowledge in
// one place means the handler can't accidentally insert a mixed-case email.
func (s *Store) Insert(ctx context.Context, email, source, referrer, userAgent, ipHash string) (bool, error) {
	const q = `
		INSERT INTO waitlist.signups (email, source, referrer, user_agent, ip_hash)
		VALUES (lower($1), $2, $3, $4, $5)
		ON CONFLICT (lower(email)) DO NOTHING
		RETURNING id
	`
	var id int64
	err := s.pool.QueryRow(ctx, q, email, source, referrer, userAgent, ipHash).Scan(&id)

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		// ON CONFLICT DO NOTHING returns no row — that's the "duplicate" path.
		return false, nil
	default:
		return false, err
	}
}
