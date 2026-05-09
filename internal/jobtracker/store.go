package jobtracker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the data-access layer for everything in the job_tracker schema.
// One concrete struct, methods grouped by domain (applications, notes, ...).
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// scanNullableString safely reads a possibly-NULL TEXT column into a Go string.
func nullStr(p *string) any { return nullable(p) }
func nullable(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

// nullDate accepts a "YYYY-MM-DD" string or empty; returns nil for empty.
func nullDate(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// rfc3339 stamps a time.Time as ISO 8601 UTC.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ============================================================================
// Applications
// ============================================================================

// CheckLinkResult is what CheckApplicationByJobLink returns. The id +
// stage are populated only when the link matches an existing row.
type CheckLinkResult struct {
	Exists        bool      `json:"exists"`
	ApplicationID uuid.UUID `json:"applicationId,omitempty"`
	Stage         string    `json:"stage,omitempty"`
}

// FindApplicationIDByJobLink returns the most-recent application row's
// id for a given (user_id, job_link). Used by the upsert path on POST
// /applications: if the link is already on file, we route to update
// instead of insert. Returns pgx.ErrNoRows when no match exists.
//
// Match is exact on the value submitted (the extension strips query
// strings before saving, the same shape we stored last time).
func (s *Store) FindApplicationIDByJobLink(ctx context.Context, userID uuid.UUID, jobLink string) (uuid.UUID, error) {
	jobLink = strings.TrimSpace(jobLink)
	if jobLink == "" {
		return uuid.Nil, pgx.ErrNoRows
	}
	const q = `
		SELECT id
		FROM job_tracker.applications
		WHERE user_id = $1 AND job_link = $2
		ORDER BY created_at DESC
		LIMIT 1
	`
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, q, userID, jobLink).Scan(&id)
	return id, err
}

// CheckApplicationByJobLink reports whether the user already has an
// application row with the given job_link. Used by the browser
// extension's duplicate-detection step before the form even renders.
//
// Match is exact on the value the extension sends (it strips query
// strings on its side). If the user has multiple matches we return the
// most recent — the extension only needs to know "is it in your tracker
// already?" plus a deep link to the row.
func (s *Store) CheckApplicationByJobLink(ctx context.Context, userID uuid.UUID, jobLink string) (*CheckLinkResult, error) {
	jobLink = strings.TrimSpace(jobLink)
	if jobLink == "" {
		return &CheckLinkResult{Exists: false}, nil
	}
	const q = `
		SELECT id, stage
		FROM job_tracker.applications
		WHERE user_id = $1 AND job_link = $2
		ORDER BY created_at DESC
		LIMIT 1
	`
	var (
		id    uuid.UUID
		stage string
	)
	err := s.pool.QueryRow(ctx, q, userID, jobLink).Scan(&id, &stage)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &CheckLinkResult{Exists: false}, nil
		}
		return nil, fmt.Errorf("check-link: %w", err)
	}
	return &CheckLinkResult{Exists: true, ApplicationID: id, Stage: stage}, nil
}

type ListAppsOpts struct {
	Stage  string
	Source string
	Search string
}

// applicationCols is the canonical SELECT list for an Application row.
// Centralised so list/detail/insert-returning use the same projection.
const applicationCols = `
	id, company, role, COALESCE(source,''), COALESCE(location,''), COALESCE(salary_range,''),
	stage,
	COALESCE(to_char(applied_at, 'YYYY-MM-DD'), ''),
	COALESCE(to_char(apply_deadline, 'YYYY-MM-DD'), ''),
	COALESCE(job_link,''), COALESCE(job_description,''), COALESCE(notes,''),
	stale,
	COALESCE(to_char(stage_changed_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
	to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
`

func scanApplication(row pgx.Row, a *Application) error {
	return row.Scan(&a.ID, &a.Company, &a.Role, &a.Source, &a.Location, &a.SalaryRange,
		&a.Stage, &a.AppliedAt, &a.ApplyDeadline, &a.JobLink, &a.JobDescription, &a.Notes,
		&a.Stale, &a.StageChangedAt, &a.CreatedAt, &a.UpdatedAt)
}

func (s *Store) ListApplications(ctx context.Context, userID uuid.UUID, opts ListAppsOpts) ([]Application, error) {
	q := `SELECT ` + applicationCols + ` FROM job_tracker.applications WHERE user_id = $1`
	args := []any{userID}
	if opts.Stage != "" {
		args = append(args, opts.Stage)
		q += fmt.Sprintf(" AND stage = $%d", len(args))
	}
	if opts.Source != "" {
		args = append(args, opts.Source)
		q += fmt.Sprintf(" AND source = $%d", len(args))
	}
	if opts.Search != "" {
		args = append(args, "%"+opts.Search+"%")
		q += fmt.Sprintf(" AND (company ILIKE $%d OR role ILIKE $%d OR COALESCE(location,'') ILIKE $%d)", len(args), len(args), len(args))
	}
	q += " ORDER BY created_at DESC"

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Application, 0)
	for rows.Next() {
		var a Application
		if err := scanApplication(rows, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetApplication(ctx context.Context, userID, id uuid.UUID) (*Application, error) {
	q := `SELECT ` + applicationCols + ` FROM job_tracker.applications WHERE id = $1 AND user_id = $2`
	var a Application
	if err := scanApplication(s.pool.QueryRow(ctx, q, id, userID), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) CreateApplication(ctx context.Context, userID uuid.UUID, in ApplicationInput) (*Application, error) {
	const q = `
		INSERT INTO job_tracker.applications
			(user_id, company, role, source, location, salary_range, stage, applied_at, apply_deadline, job_link, job_description, notes, stale, stage_changed_at)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,$8::DATE,$9::DATE,NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),$13,NOW())
		RETURNING id
	`
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, q, userID, in.Company, in.Role, in.Source, in.Location, in.SalaryRange,
		in.Stage, nullDate(in.AppliedAt), nullDate(in.ApplyDeadline), in.JobLink, in.JobDescription, in.Notes, in.Stale).Scan(&id)
	if err != nil {
		return nil, err
	}
	// Seed history with an "initial stage" entry. from_stage stays NULL so
	// the timeline UI can render this as "Application added" rather than a
	// transition.
	const insertHistory = `
		INSERT INTO job_tracker.application_stage_history (user_id, application_id, from_stage, to_stage)
		VALUES ($1, $2, NULL, $3)
	`
	if _, err := s.pool.Exec(ctx, insertHistory, userID, id, in.Stage); err != nil {
		// Don't fail the create over a history-write hiccup.
		// (logging is the handler's responsibility — caller has it.)
	}
	return s.GetApplication(ctx, userID, id)
}

func (s *Store) UpdateApplication(ctx context.Context, userID, id uuid.UUID, in ApplicationInput) (*Application, error) {
	// One CTE roundtrip captures the *previous* stage and performs the
	// update. We then conditionally insert a history row outside the CTE
	// (DML-in-CTE that conditionally inserts gets messy; two queries is
	// clearer at this scale).
	const q = `
		WITH prev AS (
			SELECT stage AS old_stage
			FROM job_tracker.applications
			WHERE id = $13 AND user_id = $14
		),
		upd AS (
			UPDATE job_tracker.applications SET
				company = $1, role = $2,
				source = NULLIF($3,''), location = NULLIF($4,''), salary_range = NULLIF($5,''),
				stage_changed_at = CASE WHEN stage <> $6 THEN NOW() ELSE stage_changed_at END,
				stage = $6,
				applied_at = $7::DATE, apply_deadline = $8::DATE,
				job_link = NULLIF($9,''), job_description = NULLIF($10,''), notes = NULLIF($11,''),
				stale = $12, updated_at = NOW()
			WHERE id = $13 AND user_id = $14
			RETURNING stage AS new_stage
		)
		SELECT prev.old_stage, upd.new_stage FROM prev, upd
	`
	var oldStage, newStage string
	err := s.pool.QueryRow(ctx, q, in.Company, in.Role, in.Source, in.Location, in.SalaryRange,
		in.Stage, nullDate(in.AppliedAt), nullDate(in.ApplyDeadline), in.JobLink, in.JobDescription, in.Notes, in.Stale, id, userID).
		Scan(&oldStage, &newStage)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, pgx.ErrNoRows
		}
		return nil, err
	}

	if oldStage != newStage {
		const insertHistory = `
			INSERT INTO job_tracker.application_stage_history (user_id, application_id, from_stage, to_stage)
			VALUES ($1, $2, $3, $4)
		`
		// Best-effort — if the history write fails we still return the
		// updated app rather than rolling back the whole edit.
		_, _ = s.pool.Exec(ctx, insertHistory, userID, id, oldStage, newStage)
	}

	return s.GetApplication(ctx, userID, id)
}

// ApplicationTimeline returns the stage history for an application in
// chronological order (oldest first). The first row will typically have
// from_stage = NULL representing the initial create.
func (s *Store) ApplicationTimeline(ctx context.Context, userID, appID uuid.UUID) ([]StageChange, error) {
	// Confirm the app belongs to this user — without this an attacker
	// who guesses an id could read someone else's history.
	var ownerCheck uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM job_tracker.applications WHERE id = $1`, appID,
	).Scan(&ownerCheck)
	if err != nil {
		return nil, err
	}
	if ownerCheck != userID {
		return nil, pgx.ErrNoRows
	}

	const q = `
		SELECT COALESCE(from_stage, ''), to_stage,
		       to_char(changed_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM job_tracker.application_stage_history
		WHERE application_id = $1 AND user_id = $2
		ORDER BY changed_at, id
	`
	rows, err := s.pool.Query(ctx, q, appID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]StageChange, 0)
	for rows.Next() {
		var c StageChange
		if err := rows.Scan(&c.From, &c.To, &c.ChangedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteApplication(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.applications WHERE id = $1 AND user_id = $2`
	tag, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// Dashboard aggregates per-stage counts in a single query.
func (s *Store) Dashboard(ctx context.Context, userID uuid.UUID) (*DashboardMetrics, error) {
	const q = `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE stage NOT IN ('OFFER','REJECTED')),
			COUNT(*) FILTER (WHERE stage IN ('PHONE_SCREEN','TECHNICAL','ONSITE')),
			COUNT(*) FILTER (WHERE stage = 'OFFER'),
			COUNT(*) FILTER (WHERE created_at > NOW() - INTERVAL '7 days'),
			COUNT(*) FILTER (WHERE stage <> 'INTERESTED'),
			COUNT(*) FILTER (WHERE stage IN ('PHONE_SCREEN','TECHNICAL','ONSITE','OFFER','REJECTED'))
		FROM job_tracker.applications
		WHERE user_id = $1
	`
	var total, inPipeline, interviews, offers, addedThisWeek, applied, anyResponse int
	err := s.pool.QueryRow(ctx, q, userID).
		Scan(&total, &inPipeline, &interviews, &offers, &addedThisWeek, &applied, &anyResponse)
	if err != nil {
		return nil, err
	}
	m := &DashboardMetrics{
		Total: total, InPipeline: inPipeline, Interviews: interviews, Offers: offers,
		AddedThisWeek: addedThisWeek,
	}
	if applied > 0 {
		m.ResponseRate = round2(float64(anyResponse) / float64(applied))
	}
	if total > 0 {
		m.ConversionRate = round2(float64(offers) / float64(total))
	}
	return m, nil
}

// Funnel returns per-stage application counts in canonical order.
// Used by /public/analytics/{slug}.
func (s *Store) Funnel(ctx context.Context, userID uuid.UUID) ([]FunnelEntry, error) {
	const q = `
		SELECT stage, COUNT(*)::int
		FROM job_tracker.applications
		WHERE user_id = $1
		GROUP BY stage
	`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var stage string
		var count int
		if err := rows.Scan(&stage, &count); err != nil {
			return nil, err
		}
		counts[stage] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	stages := []string{"INTERESTED", "APPLIED", "PHONE_SCREEN", "TECHNICAL", "ONSITE", "OFFER", "REJECTED"}
	out := make([]FunnelEntry, 0, len(stages))
	for _, st := range stages {
		out = append(out, FunnelEntry{Stage: st, Count: counts[st]})
	}
	return out, nil
}

// WeeklyActivity returns the count of applications created per ISO week
// (Monday-anchored, UTC) for the last `weeks` weeks, ascending by week.
// Uses created_at since not every app has applied_at set.
func (s *Store) WeeklyActivity(ctx context.Context, userID uuid.UUID, weeks int) ([]WeeklyEntry, error) {
	if weeks <= 0 {
		weeks = 8
	}
	const q = `
		SELECT to_char(date_trunc('week', created_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD') AS week_start,
		       COUNT(*)::int
		FROM job_tracker.applications
		WHERE user_id = $1
		  AND created_at >= date_trunc('week', NOW() AT TIME ZONE 'UTC') - ($2::int - 1) * INTERVAL '1 week'
		GROUP BY 1
		ORDER BY 1
	`
	rows, err := s.pool.Query(ctx, q, userID, weeks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var ws string
		var c int
		if err := rows.Scan(&ws, &c); err != nil {
			return nil, err
		}
		got[ws] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Build the full series so missing weeks render as 0
	out := make([]WeeklyEntry, 0, weeks)
	now := time.Now().UTC()
	// Anchor to Monday of the current week
	monday := now.AddDate(0, 0, -int((now.Weekday()+6)%7))
	monday = time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.UTC)
	for i := weeks - 1; i >= 0; i-- {
		w := monday.AddDate(0, 0, -7*i).Format("2006-01-02")
		out = append(out, WeeklyEntry{WeekStart: w, Count: got[w]})
	}
	return out, nil
}

func round2(f float64) float64 {
	return float64(int(f*10000+0.5)) / 10000
}

// ============================================================================
// Notes + Categories
// ============================================================================

const noteListExcerptLen = 200

func (s *Store) ListNotes(ctx context.Context, userID uuid.UUID, categoryID *uuid.UUID, search string) ([]NoteListItem, error) {
	q := fmt.Sprintf(`
		SELECT id, category_id, title, COALESCE(LEFT(content, %d), ''), pinned,
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       to_char(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM job_tracker.notes
		WHERE user_id = $1
	`, noteListExcerptLen)
	args := []any{userID}
	if categoryID != nil {
		args = append(args, *categoryID)
		q += fmt.Sprintf(" AND category_id = $%d", len(args))
	}
	if search != "" {
		args = append(args, "%"+search+"%")
		q += fmt.Sprintf(" AND (title ILIKE $%d OR content ILIKE $%d)", len(args), len(args))
	}
	q += " ORDER BY pinned DESC, updated_at DESC"

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NoteListItem, 0)
	for rows.Next() {
		var n NoteListItem
		var cat *uuid.UUID
		if err := rows.Scan(&n.ID, &cat, &n.Title, &n.Excerpt, &n.Pinned, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		n.CategoryID = cat
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) GetNote(ctx context.Context, userID, id uuid.UUID) (*Note, error) {
	const q = `
		SELECT id, category_id, title, content, pinned,
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       to_char(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM job_tracker.notes WHERE id = $1 AND user_id = $2
	`
	var n Note
	var cat *uuid.UUID
	err := s.pool.QueryRow(ctx, q, id, userID).Scan(&n.ID, &cat, &n.Title, &n.Content, &n.Pinned, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, err
	}
	n.CategoryID = cat
	return &n, nil
}

func (s *Store) CreateNote(ctx context.Context, userID uuid.UUID, in NoteInput) (*Note, error) {
	var catID any
	if in.CategoryID != nil && *in.CategoryID != "" {
		parsed, err := uuid.Parse(*in.CategoryID)
		if err != nil {
			return nil, fmt.Errorf("bad categoryId: %w", err)
		}
		catID = parsed
	}
	const q = `
		INSERT INTO job_tracker.notes (user_id, category_id, title, content, pinned)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, q, userID, catID, in.Title, in.Content, in.Pinned).Scan(&id); err != nil {
		return nil, err
	}
	return s.GetNote(ctx, userID, id)
}

func (s *Store) UpdateNote(ctx context.Context, userID, id uuid.UUID, in NoteInput) (*Note, error) {
	var catID any
	if in.CategoryID != nil && *in.CategoryID != "" {
		parsed, err := uuid.Parse(*in.CategoryID)
		if err != nil {
			return nil, fmt.Errorf("bad categoryId: %w", err)
		}
		catID = parsed
	}
	const q = `
		UPDATE job_tracker.notes
		SET category_id = $1, title = $2, content = $3, pinned = $4, updated_at = NOW()
		WHERE id = $5 AND user_id = $6
	`
	tag, err := s.pool.Exec(ctx, q, catID, in.Title, in.Content, in.Pinned, id, userID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, pgx.ErrNoRows
	}
	return s.GetNote(ctx, userID, id)
}

func (s *Store) DeleteNote(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.notes WHERE id = $1 AND user_id = $2`
	tag, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) ListCategories(ctx context.Context, userID uuid.UUID) ([]Category, error) {
	if err := s.seedDefaultCategories(ctx, userID); err != nil {
		return nil, err
	}
	const q = `
		SELECT id, name, COALESCE(color,'')
		FROM job_tracker.note_categories
		WHERE user_id = $1
		ORDER BY name
	`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Category, 0)
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Color); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// seedDefaultCategories ensures the user has the 6 default categories the
// frontend expects. Idempotent (does nothing if rows already exist).
func (s *Store) seedDefaultCategories(ctx context.Context, userID uuid.UUID) error {
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM job_tracker.note_categories WHERE user_id = $1`, userID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	defaults := []struct {
		name, color string
	}{
		{"General", "#94a3b8"},
		{"Job Search", "#60a5fa"},
		{"Interview Prep", "#f97316"},
		{"Company Research", "#a78bfa"},
		{"Networking", "#34d399"},
		{"Learning", "#facc15"},
	}
	for _, d := range defaults {
		if _, err := s.pool.Exec(ctx, `INSERT INTO job_tracker.note_categories (user_id, name, color) VALUES ($1,$2,$3)`, userID, d.name, d.color); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreateCategory(ctx context.Context, userID uuid.UUID, in CategoryInput) (*Category, error) {
	const q = `
		INSERT INTO job_tracker.note_categories (user_id, name, color)
		VALUES ($1, $2, NULLIF($3,''))
		RETURNING id, name, COALESCE(color,'')
	`
	var c Category
	if err := s.pool.QueryRow(ctx, q, userID, in.Name, in.Color).Scan(&c.ID, &c.Name, &c.Color); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) DeleteCategory(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.note_categories WHERE id = $1 AND user_id = $2`
	tag, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ============================================================================
// Profile + sub-resources
// ============================================================================

func (s *Store) ensureProfile(ctx context.Context, userID uuid.UUID) error {
	const q = `INSERT INTO job_tracker.profiles (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`
	_, err := s.pool.Exec(ctx, q, userID)
	return err
}

// slugifyAlphaNum normalises any string to a slug-shape token: lowercase,
// non-[a-z0-9] runs collapse to `-`, leading/trailing `-` trimmed, length
// capped at 38 chars. Returns "" if the result is shorter than 3 chars.
func slugifyAlphaNum(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 38 {
		out = strings.TrimRight(out[:38], "-")
	}
	if len(out) < 3 {
		return ""
	}
	return out
}

// slugFromName turns "Shubham Dixit" → "shubham-dixit", "Md Rizabul " →
// "md-rizabul". Returns "" if nothing usable — caller falls back to email.
func slugFromName(name string) string {
	return slugifyAlphaNum(name)
}

// slugFromEmail derives a slug from an email's local part. Always returns
// a non-empty value — the last-resort fallback is "user".
func slugFromEmail(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		local = email[:i]
	}
	if out := slugifyAlphaNum(local); out != "" {
		return out
	}
	return "user"
}

// ensureSlug picks a slug for the user if they don't have one yet.
// Idempotent: no-op if `slug IS NOT NULL`. Prefers the user's name
// ("Shubham Dixit" → "shubham-dixit"); falls back to the email local part
// when the name is empty/unusable. Resolves uniqueness with -2, -3, ...
// suffixes (capped at 100 attempts).
func (s *Store) ensureSlug(ctx context.Context, userID uuid.UUID) error {
	// 1. Cheap probe — most calls bail here.
	var current *string
	if err := s.pool.QueryRow(ctx,
		`SELECT slug FROM job_tracker.profiles WHERE user_id = $1`, userID,
	).Scan(&current); err != nil {
		return err
	}
	if current != nil && *current != "" {
		return nil
	}

	// 2. Pull name + email together.
	var name, email string
	if err := s.pool.QueryRow(ctx,
		`SELECT name, email FROM auth.users WHERE id = $1`, userID,
	).Scan(&name, &email); err != nil {
		return fmt.Errorf("ensureSlug: lookup user: %w", err)
	}

	// 3. Seed: prefer name, fall back to email.
	base := slugFromName(name)
	if base == "" {
		base = slugFromEmail(email)
	}

	// 4. Find an available variant. Cap at 100 attempts so a runaway
	//    can't loop forever on a pathological seed.
	candidate := base
	for i := 2; i < 100; i++ {
		avail, err := s.SlugAvailable(ctx, userID, candidate)
		if err != nil {
			return err
		}
		if avail {
			break
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
		if len(candidate) > 40 {
			candidate = candidate[:40]
		}
	}

	return s.UpdateSlug(ctx, userID, candidate)
}

// ProvisionProfile is the eager bootstrap path called from the auth
// OAuth callback once we know the user's identity. Idempotent:
//   - creates the empty profile row if missing
//   - sets a slug from name/email if none yet
//
// Does not touch is_public — the schema default (FALSE) keeps the public
// profile route returning 404 until the user explicitly toggles visibility.
func (s *Store) ProvisionProfile(ctx context.Context, userID uuid.UUID) error {
	if err := s.ensureProfile(ctx, userID); err != nil {
		return err
	}
	return s.ensureSlug(ctx, userID)
}

func (s *Store) GetProfile(ctx context.Context, userID uuid.UUID) (*Profile, error) {
	if err := s.ensureProfile(ctx, userID); err != nil {
		return nil, err
	}
	if err := s.ensureSlug(ctx, userID); err != nil {
		return nil, err
	}
	const root = `
		SELECT COALESCE(slug,''), is_public,
		       COALESCE(headline,''), COALESCE(about,''), COALESCE(location,''),
		       COALESCE(linkedin_url,''), COALESCE(github_url,''), COALESCE(website_url,'')
		FROM job_tracker.profiles WHERE user_id = $1
	`
	p := &Profile{Experiences: []Experience{}, Educations: []Education{}, Projects: []Project{}, Skills: []Skill{}}
	if err := s.pool.QueryRow(ctx, root, userID).Scan(
		&p.Slug, &p.IsPublic,
		&p.Headline, &p.About, &p.Location,
		&p.LinkedinURL, &p.GithubURL, &p.WebsiteURL); err != nil {
		return nil, err
	}

	if exps, err := s.ListExperiences(ctx, userID); err != nil {
		return nil, err
	} else {
		p.Experiences = exps
	}
	if edus, err := s.ListEducations(ctx, userID); err != nil {
		return nil, err
	} else {
		p.Educations = edus
	}
	if projs, err := s.ListProjects(ctx, userID); err != nil {
		return nil, err
	} else {
		p.Projects = projs
	}
	if skills, err := s.ListSkills(ctx, userID); err != nil {
		return nil, err
	} else {
		p.Skills = skills
	}
	return p, nil
}

func (s *Store) UpdateProfile(ctx context.Context, userID uuid.UUID, in ProfileInput) error {
	if err := s.ensureProfile(ctx, userID); err != nil {
		return err
	}
	const q = `
		UPDATE job_tracker.profiles
		SET headline = NULLIF($1,''), about = NULLIF($2,''), location = NULLIF($3,''),
		    linkedin_url = NULLIF($4,''), github_url = NULLIF($5,''), website_url = NULLIF($6,''),
		    updated_at = NOW()
		WHERE user_id = $7
	`
	_, err := s.pool.Exec(ctx, q, in.Headline, in.About, in.Location,
		in.LinkedinURL, in.GithubURL, in.WebsiteURL, userID)
	return err
}

// SlugAvailable returns true iff no other user owns the given slug.
func (s *Store) SlugAvailable(ctx context.Context, userID uuid.UUID, slug string) (bool, error) {
	if slug == "" {
		return false, errors.New("empty slug")
	}
	const q = `SELECT user_id FROM job_tracker.profiles WHERE lower(slug) = lower($1)`
	var owner uuid.UUID
	err := s.pool.QueryRow(ctx, q, slug).Scan(&owner)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return false, err
	}
	return owner == userID, nil
}

func (s *Store) UpdateSlug(ctx context.Context, userID uuid.UUID, slug string) error {
	if err := s.ensureProfile(ctx, userID); err != nil {
		return err
	}
	const q = `UPDATE job_tracker.profiles SET slug = NULLIF($1,''), updated_at = NOW() WHERE user_id = $2`
	_, err := s.pool.Exec(ctx, q, slug, userID)
	return err
}

func (s *Store) UpdateVisibility(ctx context.Context, userID uuid.UUID, public bool) error {
	if err := s.ensureProfile(ctx, userID); err != nil {
		return err
	}
	const q = `UPDATE job_tracker.profiles SET is_public = $1, updated_at = NOW() WHERE user_id = $2`
	_, err := s.pool.Exec(ctx, q, public, userID)
	return err
}

// --- Experiences ---

const experienceCols = `
	id, company, title, COALESCE(location,''),
	COALESCE(to_char(start_date, 'YYYY-MM-DD'), ''),
	COALESCE(to_char(end_date,   'YYYY-MM-DD'), ''),
	COALESCE(current, FALSE),
	COALESCE(description,''),
	sort_order
`

func scanExperience(row pgx.Row, e *Experience) error {
	return row.Scan(&e.ID, &e.Company, &e.Title, &e.Location,
		&e.StartDate, &e.EndDate, &e.Current, &e.Description, &e.SortOrder)
}

func (s *Store) ListExperiences(ctx context.Context, userID uuid.UUID) ([]Experience, error) {
	q := `SELECT ` + experienceCols + ` FROM job_tracker.profile_experiences WHERE user_id = $1 ORDER BY sort_order, company`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Experience, 0)
	for rows.Next() {
		var e Experience
		if err := scanExperience(rows, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CreateExperience(ctx context.Context, userID uuid.UUID, in ExperienceInput) (*Experience, error) {
	q := `
		INSERT INTO job_tracker.profile_experiences
			(user_id, company, title, location, start_date, end_date, current, description, sort_order)
		VALUES ($1, $2, $3, NULLIF($4,''), $5::DATE, $6::DATE, $7, NULLIF($8,''), $9)
		RETURNING ` + experienceCols
	var e Experience
	err := scanExperience(s.pool.QueryRow(ctx, q, userID, in.Company, in.Title, in.Location,
		nullDate(in.StartDate), nullDate(in.EndDate), in.Current, in.Description, in.SortOrder), &e)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) UpdateExperience(ctx context.Context, userID, id uuid.UUID, in ExperienceInput) (*Experience, error) {
	q := `
		UPDATE job_tracker.profile_experiences SET
			company = $1, title = $2, location = NULLIF($3,''),
			start_date = $4::DATE, end_date = $5::DATE, current = $6,
			description = NULLIF($7,''), sort_order = $8
		WHERE id = $9 AND user_id = $10
		RETURNING ` + experienceCols
	var e Experience
	err := scanExperience(s.pool.QueryRow(ctx, q, in.Company, in.Title, in.Location,
		nullDate(in.StartDate), nullDate(in.EndDate), in.Current, in.Description, in.SortOrder, id, userID), &e)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) DeleteExperience(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.profile_experiences WHERE id = $1 AND user_id = $2`
	tag, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// --- Educations ---

const educationCols = `
	id, school, COALESCE(degree,''), COALESCE(field,''),
	COALESCE(to_char(start_date, 'YYYY-MM-DD'), ''),
	COALESCE(to_char(end_date,   'YYYY-MM-DD'), ''),
	COALESCE(gpa,''),
	COALESCE(description,''),
	sort_order
`

func scanEducation(row pgx.Row, e *Education) error {
	return row.Scan(&e.ID, &e.School, &e.Degree, &e.Field,
		&e.StartDate, &e.EndDate, &e.GPA, &e.Description, &e.SortOrder)
}

func (s *Store) ListEducations(ctx context.Context, userID uuid.UUID) ([]Education, error) {
	q := `SELECT ` + educationCols + ` FROM job_tracker.profile_educations WHERE user_id = $1 ORDER BY sort_order, school`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Education, 0)
	for rows.Next() {
		var e Education
		if err := scanEducation(rows, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CreateEducation(ctx context.Context, userID uuid.UUID, in EducationInput) (*Education, error) {
	q := `
		INSERT INTO job_tracker.profile_educations
			(user_id, school, degree, field, start_date, end_date, gpa, description, sort_order)
		VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5::DATE, $6::DATE, NULLIF($7,''), NULLIF($8,''), $9)
		RETURNING ` + educationCols
	var e Education
	err := scanEducation(s.pool.QueryRow(ctx, q, userID, in.School, in.Degree, in.Field,
		nullDate(in.StartDate), nullDate(in.EndDate), in.GPA, in.Description, in.SortOrder), &e)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) UpdateEducation(ctx context.Context, userID, id uuid.UUID, in EducationInput) (*Education, error) {
	q := `
		UPDATE job_tracker.profile_educations SET
			school = $1, degree = NULLIF($2,''), field = NULLIF($3,''),
			start_date = $4::DATE, end_date = $5::DATE, gpa = NULLIF($6,''),
			description = NULLIF($7,''), sort_order = $8
		WHERE id = $9 AND user_id = $10
		RETURNING ` + educationCols
	var e Education
	err := scanEducation(s.pool.QueryRow(ctx, q, in.School, in.Degree, in.Field,
		nullDate(in.StartDate), nullDate(in.EndDate), in.GPA, in.Description, in.SortOrder, id, userID), &e)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) DeleteEducation(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.profile_educations WHERE id = $1 AND user_id = $2`
	tag, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// --- Projects ---

const projectCols = `
	id, name, COALESCE(description,''),
	COALESCE(tech_stack,''), COALESCE(link,''),
	sort_order
`

func scanProject(row pgx.Row, p *Project) error {
	return row.Scan(&p.ID, &p.Name, &p.Description, &p.TechStack, &p.Link, &p.SortOrder)
}

func (s *Store) ListProjects(ctx context.Context, userID uuid.UUID) ([]Project, error) {
	q := `SELECT ` + projectCols + ` FROM job_tracker.profile_projects WHERE user_id = $1 ORDER BY sort_order, name`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Project, 0)
	for rows.Next() {
		var p Project
		if err := scanProject(rows, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) CreateProject(ctx context.Context, userID uuid.UUID, in ProjectInput) (*Project, error) {
	q := `
		INSERT INTO job_tracker.profile_projects
			(user_id, name, description, tech_stack, link, sort_order)
		VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), $6)
		RETURNING ` + projectCols
	var p Project
	err := scanProject(s.pool.QueryRow(ctx, q, userID, in.Name, in.Description, in.TechStack, in.Link, in.SortOrder), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) UpdateProject(ctx context.Context, userID, id uuid.UUID, in ProjectInput) (*Project, error) {
	q := `
		UPDATE job_tracker.profile_projects SET
			name = $1, description = NULLIF($2,''),
			tech_stack = NULLIF($3,''), link = NULLIF($4,''), sort_order = $5
		WHERE id = $6 AND user_id = $7
		RETURNING ` + projectCols
	var p Project
	err := scanProject(s.pool.QueryRow(ctx, q, in.Name, in.Description, in.TechStack, in.Link, in.SortOrder, id, userID), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) DeleteProject(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.profile_projects WHERE id = $1 AND user_id = $2`
	tag, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// --- Skills ---

const skillCols = `id, name, COALESCE(category,''), sort_order`

func scanSkill(row pgx.Row, sk *Skill) error {
	return row.Scan(&sk.ID, &sk.Name, &sk.Category, &sk.SortOrder)
}

func (s *Store) ListSkills(ctx context.Context, userID uuid.UUID) ([]Skill, error) {
	q := `SELECT ` + skillCols + ` FROM job_tracker.profile_skills WHERE user_id = $1 ORDER BY sort_order, name`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Skill, 0)
	for rows.Next() {
		var sk Skill
		if err := scanSkill(rows, &sk); err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

func (s *Store) CreateSkill(ctx context.Context, userID uuid.UUID, in SkillInput) (*Skill, error) {
	q := `
		INSERT INTO job_tracker.profile_skills (user_id, name, category, sort_order)
		VALUES ($1, $2, NULLIF($3,''), $4)
		RETURNING ` + skillCols
	var sk Skill
	err := scanSkill(s.pool.QueryRow(ctx, q, userID, in.Name, in.Category, in.SortOrder), &sk)
	if err != nil {
		return nil, err
	}
	return &sk, nil
}

func (s *Store) DeleteSkill(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.profile_skills WHERE id = $1 AND user_id = $2`
	tag, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ============================================================================
// Public profile + analytics (no auth)
// ============================================================================

// resolvePublicSlug returns the user_id behind a slug iff the profile is
// marked public. pgx.ErrNoRows on miss/private — caller should map to 404.
func (s *Store) resolvePublicSlug(ctx context.Context, slug string) (uuid.UUID, error) {
	const q = `
		SELECT user_id FROM job_tracker.profiles
		WHERE lower(slug) = lower($1) AND is_public = TRUE
	`
	var uid uuid.UUID
	err := s.pool.QueryRow(ctx, q, slug).Scan(&uid)
	return uid, err
}

func (s *Store) PublicProfile(ctx context.Context, slug string) (*PublicProfile, error) {
	const q = `
		SELECT u.id, u.name, COALESCE(u.picture_url,''),
		       COALESCE(p.headline,''), COALESCE(p.about,''), COALESCE(p.location,''),
		       COALESCE(p.linkedin_url,''), COALESCE(p.github_url,''), COALESCE(p.website_url,''),
		       p.slug
		FROM job_tracker.profiles p
		JOIN auth.users u ON u.id = p.user_id
		WHERE lower(p.slug) = lower($1) AND p.is_public = TRUE
	`
	var userID uuid.UUID
	pp := &PublicProfile{
		Experiences: []Experience{}, Educations: []Education{},
		Projects: []Project{}, Skills: []Skill{},
	}
	err := s.pool.QueryRow(ctx, q, slug).Scan(
		&userID, &pp.Name, &pp.PictureURL,
		&pp.Headline, &pp.About, &pp.Location,
		&pp.LinkedinURL, &pp.GithubURL, &pp.WebsiteURL,
		&pp.Slug,
	)
	if err != nil {
		return nil, err
	}

	if exps, err := s.ListExperiences(ctx, userID); err != nil {
		return nil, err
	} else {
		pp.Experiences = exps
	}
	if edus, err := s.ListEducations(ctx, userID); err != nil {
		return nil, err
	} else {
		pp.Educations = edus
	}
	if projs, err := s.ListProjects(ctx, userID); err != nil {
		return nil, err
	} else {
		pp.Projects = projs
	}
	if skills, err := s.ListSkills(ctx, userID); err != nil {
		return nil, err
	} else {
		pp.Skills = skills
	}
	return pp, nil
}

// PublicAnalyticsBySlug returns the public dashboard for the user behind
// the given slug. Errors with pgx.ErrNoRows if the profile isn't public.
func (s *Store) PublicAnalyticsBySlug(ctx context.Context, slug string) (*PublicAnalytics, error) {
	uid, err := s.resolvePublicSlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	summary, err := s.Dashboard(ctx, uid)
	if err != nil {
		return nil, err
	}
	funnel, err := s.Funnel(ctx, uid)
	if err != nil {
		return nil, err
	}
	weekly, err := s.WeeklyActivity(ctx, uid, 8)
	if err != nil {
		return nil, err
	}
	return &PublicAnalytics{
		Summary:        *summary,
		Funnel:         funnel,
		WeeklyActivity: weekly,
	}, nil
}

// ============================================================================
// Feedback
// ============================================================================

func (s *Store) InsertFeedback(ctx context.Context, userID *uuid.UUID, in FeedbackInput) error {
	const q = `
		INSERT INTO job_tracker.feedback (user_id, message, context)
		VALUES ($1, $2, $3)
	`
	var ctxJSON any
	if in.Context != nil {
		ctxJSON = in.Context
	}
	_, err := s.pool.Exec(ctx, q, userID, in.Message, ctxJSON)
	return err
}

// silence unused-warnings for helpers kept for future use
var _ = nullStr
var _ = rfc3339
var _ time.Duration
