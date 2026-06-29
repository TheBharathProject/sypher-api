package kairos

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Watchlist / WatchlistItem mirror kairos.watchlists / watchlist_items
// (migration 0027, spec §3.4). JSON tags are camelCase and must match
// ApiWatchlist / ApiWatchlistItem in kairos/lib/kairos-api.ts exactly —
// the FE client file is the contract.
type Watchlist struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	CreatedAt time.Time       `json:"createdAt"`
	Items     []WatchlistItem `json:"items"`
}

type WatchlistItem struct {
	ID       uuid.UUID `json:"id"`
	Symbol   string    `json:"symbol"`
	Exchange string    `json:"exchange"`
	AddedAt  time.Time `json:"addedAt"`
}

// maxWatchlistItems caps a single list. Enforced in AddItem with a
// count-then-insert (no DB constraint — a tiny race overshoot is
// harmless for a soft UX limit).
const maxWatchlistItems = 200

// Sentinel errors the watchlist handlers map onto HTTP statuses.
var (
	errWatchlistDuplicate = errors.New("symbol already on watchlist")
	errWatchlistItemLimit = errors.New("watchlist item limit reached")
)

// EnsureDefault creates the user's default "Watchlist" list when they
// have none (spec §3.4 — auto-created on first GET). Called from
// ListWatchlists so every reader sees at least one list. A concurrent
// first-GET race can in theory create two defaults; there's no unique
// constraint and the UI handles N lists, so we don't guard it.
func (s *Store) EnsureDefault(ctx context.Context, uid uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO kairos.watchlists (user_id, name)
		SELECT $1, 'Watchlist'
		WHERE NOT EXISTS (
			SELECT 1 FROM kairos.watchlists WHERE user_id = $1
		)
	`, uid)
	return err
}

// ListWatchlists returns all of the user's lists with their items,
// oldest list first, items in insertion order. Ensures the default
// list exists first, then runs two queries (lists, then all items
// joined on ownership) and groups in Go — no N+1.
func (s *Store) ListWatchlists(ctx context.Context, uid uuid.UUID) ([]Watchlist, error) {
	if err := s.EnsureDefault(ctx, uid); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, created_at
		FROM kairos.watchlists
		WHERE user_id = $1
		ORDER BY created_at, id
	`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil slices throughout — the FE indexes into .items and
	// .watchlists; nil marshals to JSON null and breaks .length.
	out := []Watchlist{}
	idx := make(map[uuid.UUID]int)
	for rows.Next() {
		var wl Watchlist
		if err := rows.Scan(&wl.ID, &wl.Name, &wl.CreatedAt); err != nil {
			return nil, err
		}
		wl.Items = []WatchlistItem{}
		idx[wl.ID] = len(out)
		out = append(out, wl)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // release the conn before the second query (idempotent)

	itemRows, err := s.pool.Query(ctx, `
		SELECT i.id, i.watchlist_id, i.symbol, i.exchange, i.added_at
		FROM kairos.watchlist_items i
		JOIN kairos.watchlists w ON w.id = i.watchlist_id
		WHERE w.user_id = $1
		ORDER BY i.added_at, i.id
	`, uid)
	if err != nil {
		return nil, err
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var (
			it     WatchlistItem
			listID uuid.UUID
		)
		if err := itemRows.Scan(&it.ID, &listID, &it.Symbol, &it.Exchange, &it.AddedAt); err != nil {
			return nil, err
		}
		if j, ok := idx[listID]; ok {
			out[j].Items = append(out[j].Items, it)
		}
	}
	return out, itemRows.Err()
}

// CreateWatchlist inserts a new empty list and returns it.
func (s *Store) CreateWatchlist(ctx context.Context, uid uuid.UUID, name string) (*Watchlist, error) {
	wl := &Watchlist{Name: name, Items: []WatchlistItem{}}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO kairos.watchlists (user_id, name)
		VALUES ($1, $2)
		RETURNING id, created_at
	`, uid, name).Scan(&wl.ID, &wl.CreatedAt)
	if err != nil {
		return nil, err
	}
	return wl, nil
}

// DeleteWatchlist removes the list (items cascade) if it belongs to
// the user. Returns pgx.ErrNoRows if not found / not owned.
func (s *Store) DeleteWatchlist(ctx context.Context, uid, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM kairos.watchlists WHERE id = $1 AND user_id = $2
	`, id, uid)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// AddItem appends a symbol to the user's list. Ownership is enforced
// twice: a count probe (which doubles as the size-limit check and the
// not-found signal) and a WHERE EXISTS inside the INSERT itself, so a
// list deleted between the two statements can't be written to.
//
// Returns pgx.ErrNoRows (list not found / not owned),
// errWatchlistItemLimit, or errWatchlistDuplicate (unique violation on
// (watchlist_id, symbol), detected via pgconn.PgError 23505).
func (s *Store) AddItem(ctx context.Context, uid, listID uuid.UUID, symbol, exchange string) (*WatchlistItem, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM kairos.watchlist_items WHERE watchlist_id = w.id)
		FROM kairos.watchlists w
		WHERE w.id = $1 AND w.user_id = $2
	`, listID, uid).Scan(&count)
	if err != nil {
		return nil, err // pgx.ErrNoRows → not found / not owned
	}
	if count >= maxWatchlistItems {
		return nil, errWatchlistItemLimit
	}
	it := &WatchlistItem{Symbol: symbol, Exchange: exchange}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO kairos.watchlist_items (watchlist_id, symbol, exchange)
		SELECT $1, $2, $3
		WHERE EXISTS (
			SELECT 1 FROM kairos.watchlists WHERE id = $1 AND user_id = $4
		)
		RETURNING id, added_at
	`, listID, symbol, exchange, uid).Scan(&it.ID, &it.AddedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, errWatchlistDuplicate
		}
		return nil, err // pgx.ErrNoRows → list deleted mid-flight → 404
	}
	return it, nil
}

// RemoveItem deletes one item, scoped to both the list and the owner.
// Returns pgx.ErrNoRows if the item / list / ownership doesn't match.
func (s *Store) RemoveItem(ctx context.Context, uid, listID, itemID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM kairos.watchlist_items i
		USING kairos.watchlists w
		WHERE i.id = $1
		  AND i.watchlist_id = $2
		  AND w.id = i.watchlist_id
		  AND w.user_id = $3
	`, itemID, listID, uid)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
