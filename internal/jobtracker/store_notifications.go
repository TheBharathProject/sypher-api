package jobtracker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Notification is the wire shape returned to the frontend. `ReadAt` is a
// pointer so JSON omits it when nil (unread) and emits a string when set.
type Notification struct {
	ID        uuid.UUID  `json:"id"`
	UserID    uuid.UUID  `json:"-"`
	Kind      string     `json:"kind"`
	RefType   *string    `json:"refType,omitempty"`
	RefID     *uuid.UUID `json:"refId,omitempty"`
	Title     string     `json:"title"`
	Body      *string    `json:"body,omitempty"`
	LinkPath  *string    `json:"linkPath,omitempty"`
	ReadAt    *time.Time `json:"readAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// NotificationInput is what handlers / cron jobs pass when creating one.
type NotificationInput struct {
	UserID   uuid.UUID
	Kind     string
	RefType  *string
	RefID    *uuid.UUID
	Title    string
	Body     string
	LinkPath *string
}

// ListNotificationsOpts narrows the feed query. Cursor is the createdAt
// of the last row from the previous page (RFC3339); empty = first page.
type ListNotificationsOpts struct {
	UnreadOnly bool
	Cursor     time.Time
	Limit      int
}

// pgUniqueViolation returns true if the error is a Postgres 23505 code.
// Used by CreateNotificationIdempotent to swallow the dedup-index hit.
func pgUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// CreateNotification inserts and returns the freshly-created row. Used
// by handler-driven event paths (community comments, future synchronous
// triggers). Cron jobs should use CreateNotificationIdempotent so a
// double-fire doesn't double-write.
func (s *Store) CreateNotification(ctx context.Context, in NotificationInput) (*Notification, error) {
	const q = `
		INSERT INTO job_tracker.notifications
			(user_id, kind, ref_type, ref_id, title, body, link_path)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, user_id, kind, ref_type, ref_id, title, body, link_path, read_at, created_at
	`
	row := s.pool.QueryRow(ctx, q,
		in.UserID, in.Kind, in.RefType, in.RefID, in.Title, nilIfEmpty(in.Body), in.LinkPath,
	)
	var n Notification
	if err := scanNotification(row, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// CreateNotificationIdempotent inserts; if the partial unique index
// (user_id, kind, ref_id, day) catches it, returns (nil, false, nil).
// On any other error, returns (nil, false, err). On success, returns
// (notification, true, nil).
//
// dayKey is an artifact of test-friendliness — in prod we always pass
// time.Now(), but tests can roll the clock to verify dedup behaviour.
// The actual day-bucket comparison happens server-side in the index
// expression `((created_at AT TIME ZONE 'UTC')::date)`.
func (s *Store) CreateNotificationIdempotent(ctx context.Context, in NotificationInput, _ time.Time) (*Notification, bool, error) {
	n, err := s.CreateNotification(ctx, in)
	if err == nil {
		return n, true, nil
	}
	if pgUniqueViolation(err) {
		return nil, false, nil
	}
	return nil, false, err
}

// ListNotifications returns one page of notifications for the user,
// newest first. UnreadOnly skips already-read rows; Cursor pages by the
// last row's createdAt. Limit clamps to [1, 100] with a default of 50.
func (s *Store) ListNotifications(ctx context.Context, userID uuid.UUID, opts ListNotificationsOpts) ([]Notification, error) {
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = 50
	case limit > 100:
		limit = 100
	}

	args := []any{userID, limit}
	q := `
		SELECT id, user_id, kind, ref_type, ref_id, title, body, link_path, read_at, created_at
		FROM job_tracker.notifications
		WHERE user_id = $1
	`
	if opts.UnreadOnly {
		q += " AND read_at IS NULL"
	}
	if !opts.Cursor.IsZero() {
		args = append(args, opts.Cursor)
		q += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	q += " ORDER BY created_at DESC LIMIT $2"

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Notification, 0, limit)
	for rows.Next() {
		var n Notification
		if err := scanNotification(rows, &n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UnreadCount is the cheap polling query the bell badge uses (every ~60s
// while the user has Pegasus open). Index-only scan via
// idx_notifications_user_unread.
func (s *Store) UnreadCount(ctx context.Context, userID uuid.UUID) (int, error) {
	const q = `SELECT COUNT(*) FROM job_tracker.notifications WHERE user_id = $1 AND read_at IS NULL`
	var n int
	if err := s.pool.QueryRow(ctx, q, userID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MarkRead stamps read_at = NOW() if the notification is unread and owned
// by this user. Returns pgx.ErrNoRows if the notification doesn't exist
// (or isn't owned), so the handler can map it to a 404.
func (s *Store) MarkRead(ctx context.Context, userID, notifID uuid.UUID) error {
	const q = `
		UPDATE job_tracker.notifications
		SET read_at = NOW()
		WHERE id = $1 AND user_id = $2 AND read_at IS NULL
	`
	tag, err := s.pool.Exec(ctx, q, notifID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Already read or not owned — for "mark read" semantics we treat
		// already-read as success (idempotent) so the caller doesn't have
		// to disambiguate. Owner check is enforced by user_id in WHERE.
		// Verify the row exists at all:
		const checkQ = `SELECT 1 FROM job_tracker.notifications WHERE id = $1 AND user_id = $2`
		var one int
		if err := s.pool.QueryRow(ctx, checkQ, notifID, userID).Scan(&one); err != nil {
			return err // pgx.ErrNoRows bubbles up; handler maps to 404
		}
	}
	return nil
}

// MarkAllRead stamps every unread notification for this user. Returns
// the number of rows newly marked read so the handler can surface it
// (used by the "Mark all read" button toast).
func (s *Store) MarkAllRead(ctx context.Context, userID uuid.UUID) (int, error) {
	const q = `
		UPDATE job_tracker.notifications
		SET read_at = NOW()
		WHERE user_id = $1 AND read_at IS NULL
	`
	tag, err := s.pool.Exec(ctx, q, userID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// scanNotification handles both pgx.Rows.Scan and pgx.Row.Scan via the
// shared interface. Same shape as the SELECTs above.
type pgxRowScanner interface {
	Scan(dest ...any) error
}

func scanNotification(row pgxRowScanner, n *Notification) error {
	return row.Scan(
		&n.ID, &n.UserID, &n.Kind,
		&n.RefType, &n.RefID,
		&n.Title, &n.Body, &n.LinkPath,
		&n.ReadAt, &n.CreatedAt,
	)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Compile-time assertion that pgx.Rows satisfies the scanner interface.
// Cheap insurance against an upgrade quietly breaking the abstraction.
var _ pgxRowScanner = pgx.Rows(nil)

// ===========================================================================
// Cron-driven query types (consumed by internal/cron/jobs/applications.go).
// They live here so the wire definitions stay close to the SQL that
// produces them.
// ===========================================================================

// StaleApplication is what MarkApplicationsStale returns: enough metadata
// to compose an `app_stale` notification without a second round-trip.
type StaleApplication struct {
	ID      uuid.UUID
	UserID  uuid.UUID
	Company string
	Role    string
}

// DigestUser is one user's worth of items for the daily digest. The cron
// job iterates these and pushes one notification per user, then sends an
// email IFF CanReceiveEmail is true (premium + opt-in, see ADR-002 D5).
//
// CanReceiveEmail is computed in the SQL query rather than fetched
// separately so we avoid a second DB round-trip per user.
type DigestUser struct {
	UserID          uuid.UUID
	Email           string
	Name            string
	CanReceiveEmail bool
	Items           []DigestItem
}

// DigestItem is a single line in a user's digest. Reason is one of
// "stale" | "deadline_soon"; Detail is the human-readable explainer.
type DigestItem struct {
	AppID   uuid.UUID
	Company string
	Role    string
	Reason  string
	Detail  string
}

// MarkApplicationsStale flips stale=true on every application whose
// stage hasn't moved since `threshold`, where stale was previously false.
// Returns the rows that flipped — fewer round-trips than UPDATE+SELECT.
//
// Why RETURNING: lets us avoid a second query AND naturally handles the
// "nothing flipped" case (returns []).
func (s *Store) MarkApplicationsStale(ctx context.Context, threshold time.Time) ([]StaleApplication, error) {
	const q = `
		UPDATE job_tracker.applications
		SET stale = true, updated_at = NOW()
		WHERE stale = false
		  AND stage_changed_at IS NOT NULL
		  AND stage_changed_at < $1
		RETURNING id, user_id, company, role
	`
	rows, err := s.pool.Query(ctx, q, threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StaleApplication{}
	for rows.Next() {
		var a StaleApplication
		if err := rows.Scan(&a.ID, &a.UserID, &a.Company, &a.Role); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UsersForDigest returns one entry per user with at least one digest-
// worthy application. `today` is interpreted in IST (caller's choice);
// `deadlineWindowDays` controls how far ahead to look for deadlines.
//
// SQL strategy: one query joins applications + auth.users, filters to
// "stale OR deadline-soon", and returns rows in user_id order so we
// can group in app code with a single pass — no GROUP BY + JSON_AGG
// gymnastics that would obscure intent.
func (s *Store) UsersForDigest(ctx context.Context, today time.Time, deadlineWindowDays int) ([]DigestUser, error) {
	// Note on the deadline window arithmetic: pgx encodes Go int as
	// integer, so we multiply INTERVAL '1 day' by it on the SQL side
	// rather than the older `($2 || ' days')::INTERVAL` string-concat
	// trick — that one expects $2 to be TEXT and pgx (correctly) refuses
	// to cast int→text implicitly.
	// `can_email` = is_premium AND email_notifications_enabled, computed
	// in SQL so the cron loop avoids a second round-trip per user. See
	// docs/adr/0002-premium-email-gating.md (D3).
	const q = `
		SELECT
			a.id, a.user_id, a.company, a.role,
			a.stale, a.apply_deadline, a.stage_changed_at,
			u.email, u.name,
			(u.is_premium AND u.email_notifications_enabled) AS can_email
		FROM job_tracker.applications a
		JOIN auth.users u ON u.id = a.user_id
		WHERE a.stale = true
		   OR (a.apply_deadline IS NOT NULL
		       AND a.apply_deadline >= $1::DATE
		       AND a.apply_deadline <= ($1::DATE + ($2::int * INTERVAL '1 day')))
		ORDER BY a.user_id, a.created_at DESC
	`
	rows, err := s.pool.Query(ctx, q, today, deadlineWindowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	usersByID := map[uuid.UUID]*DigestUser{}
	order := []uuid.UUID{}
	for rows.Next() {
		var (
			appID, userID uuid.UUID
			company, role string
			stale         bool
			applyDeadline *time.Time
			stageChanged  *time.Time
			email, name   string
			canEmail      bool
		)
		if err := rows.Scan(&appID, &userID, &company, &role, &stale, &applyDeadline, &stageChanged, &email, &name, &canEmail); err != nil {
			return nil, err
		}
		u, ok := usersByID[userID]
		if !ok {
			u = &DigestUser{UserID: userID, Email: email, Name: name, CanReceiveEmail: canEmail, Items: []DigestItem{}}
			usersByID[userID] = u
			order = append(order, userID)
		}
		item := DigestItem{AppID: appID, Company: company, Role: role}
		switch {
		case stale && (applyDeadline == nil || applyDeadline.After(today.AddDate(0, 0, deadlineWindowDays))):
			item.Reason = "stale"
			item.Detail = staleDetail(stageChanged, today)
		case applyDeadline != nil:
			item.Reason = "deadline_soon"
			item.Detail = deadlineDetail(*applyDeadline, today)
		default:
			item.Reason = "stale"
			item.Detail = staleDetail(stageChanged, today)
		}
		u.Items = append(u.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]DigestUser, 0, len(order))
	for _, uid := range order {
		out = append(out, *usersByID[uid])
	}
	return out, nil
}

func staleDetail(stageChanged *time.Time, today time.Time) string {
	if stageChanged == nil {
		return "no recent stage change"
	}
	days := int(today.Sub(*stageChanged).Hours() / 24)
	if days < 1 {
		return "no recent stage change"
	}
	return fmt.Sprintf("no movement for %d days", days)
}

func deadlineDetail(deadline, today time.Time) string {
	days := int(deadline.Sub(today).Hours() / 24)
	switch {
	case days < 0:
		return "deadline already passed"
	case days == 0:
		return "deadline today"
	case days == 1:
		return "deadline tomorrow"
	default:
		return fmt.Sprintf("deadline in %d days", days)
	}
}
