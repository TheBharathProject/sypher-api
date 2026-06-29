package kairos

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ─────────────────────────────────────────────────────────────────────────
// kairos.alerts store (migration 0027, spec §3.4).
//
// CRUD methods are user-scoped (WHERE user_id = $uid everywhere); the
// evaluator methods (ActiveAlerts / TriggerAlert / StampEvaluated) run
// across all users from the cron and are never reachable from HTTP.
// ─────────────────────────────────────────────────────────────────────────

// maxActiveAlerts caps active rules per user. Enforced in CreateAlert
// with a count-then-insert (no DB constraint — a tiny race overshoot is
// harmless for a soft UX limit, same stance as maxWatchlistItems).
const maxActiveAlerts = 50

// errAlertLimit is mapped onto 400 limit_reached by the create handler.
var errAlertLimit = errors.New("active alert limit reached")

// alertCols is the SELECT list every Alert scan uses. symbol / rule /
// threshold are nullable in the schema (the handler never inserts NULLs,
// but readers must not assume that), hence the COALESCEs.
const alertCols = `id, COALESCE(symbol, ''), COALESCE(rule, ''), COALESCE(threshold, 0),
	       status, triggered_at, last_evaluated_at, created_at`

func scanAlert(row pgx.Row, a *Alert) error {
	return row.Scan(&a.ID, &a.Symbol, &a.Rule, &a.Threshold,
		&a.Status, &a.TriggeredAt, &a.LastEvaluatedAt, &a.CreatedAt)
}

// ListAlerts returns all of the user's alerts (every status — the FE
// renders the triggered feed from the same list), newest first, riding
// the (user_id, created_at DESC) index.
func (s *Store) ListAlerts(ctx context.Context, uid uuid.UUID) ([]Alert, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+alertCols+`
		FROM kairos.alerts
		WHERE user_id = $1
		ORDER BY created_at DESC, id
	`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil slice — the FE indexes into .alerts; nil marshals to JSON
	// null and breaks .length.
	out := []Alert{}
	for rows.Next() {
		var a Alert
		if err := scanAlert(rows, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateAlert inserts a new active alert and returns it. Returns
// errAlertLimit when the user already has maxActiveAlerts active rules.
func (s *Store) CreateAlert(ctx context.Context, uid uuid.UUID, symbol, rule string, threshold float64) (*Alert, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM kairos.alerts
		WHERE user_id = $1 AND status = 'active'
	`, uid).Scan(&count)
	if err != nil {
		return nil, err
	}
	if count >= maxActiveAlerts {
		return nil, errAlertLimit
	}
	a := &Alert{Symbol: symbol, Rule: rule}
	// RETURNING threshold (not echoing the input): NUMERIC(12,2) rounds
	// on insert, and the response must match what later GETs will serve.
	err = s.pool.QueryRow(ctx, `
		INSERT INTO kairos.alerts (user_id, symbol, rule, threshold)
		VALUES ($1, $2, $3, $4)
		RETURNING id, COALESCE(threshold, 0), status, created_at
	`, uid, symbol, rule, threshold).Scan(&a.ID, &a.Threshold, &a.Status, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// UpdateAlertStatus sets the alert's status and returns the updated row.
// Reactivating (status='active') clears triggered_at so a re-armed rule
// doesn't carry a stale trigger stamp into the feed. Returns
// pgx.ErrNoRows if the alert isn't found / isn't owned by uid.
func (s *Store) UpdateAlertStatus(ctx context.Context, uid, id uuid.UUID, status string) (*Alert, error) {
	var a Alert
	err := scanAlert(s.pool.QueryRow(ctx, `
		UPDATE kairos.alerts
		SET status = $3,
		    triggered_at = CASE WHEN $3 = 'active' THEN NULL ELSE triggered_at END
		WHERE id = $1 AND user_id = $2
		RETURNING `+alertCols+`
	`, id, uid, status), &a)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// DeleteAlert removes the alert if it belongs to the user. Returns
// pgx.ErrNoRows if not found / not owned.
func (s *Store) DeleteAlert(ctx context.Context, uid, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM kairos.alerts WHERE id = $1 AND user_id = $2
	`, id, uid)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ActiveAlerts returns every user's active alerts joined with the
// owner's email + email_notifications_enabled — one query feeds the
// whole evaluator sweep (partial index alerts_active_idx). Ordered by
// symbol so the sweep's grouping is deterministic.
func (s *Store) ActiveAlerts(ctx context.Context) ([]ActiveAlert, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.user_id, COALESCE(a.symbol, ''), COALESCE(a.rule, ''),
		       COALESCE(a.threshold, 0), u.email, u.email_notifications_enabled
		FROM kairos.alerts a
		JOIN auth.users u ON u.id = a.user_id
		WHERE a.status = 'active'
		ORDER BY a.symbol, a.created_at, a.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveAlert
	for rows.Next() {
		var a ActiveAlert
		if err := rows.Scan(&a.ID, &a.UserID, &a.Symbol, &a.Rule,
			&a.Threshold, &a.Email, &a.EmailEnabled); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TriggerAlert flips one alert to triggered and stamps triggered_at.
// Guarded on status='active' so a user pausing/deleting mid-sweep wins
// the race; a no-op update is not an error.
func (s *Store) TriggerAlert(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE kairos.alerts
		SET status = 'triggered', triggered_at = NOW()
		WHERE id = $1 AND status = 'active'
	`, id)
	return err
}

// StampEvaluated sets last_evaluated_at = NOW() on every id in one
// statement — the sweep's "I looked at these" mark, breached or not.
func (s *Store) StampEvaluated(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE kairos.alerts SET last_evaluated_at = NOW() WHERE id = ANY($1)
	`, ids)
	return err
}
