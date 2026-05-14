package jobtracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// File is the row + JSON shape for /resumes and /cover-letters.
type File struct {
	ID         uuid.UUID `json:"id"`
	Kind       string    `json:"kind"`
	Slot       *int      `json:"slot,omitempty"`
	Label      string    `json:"label,omitempty"`
	FileName   string    `json:"fileName"`
	FileSize   int64     `json:"fileSize"`
	MimeType   string    `json:"mimeType,omitempty"`
	StorageKey string    `json:"-"` // internal — never expose
	UploadedAt string    `json:"uploadedAt,omitempty"`
	CreatedAt  string    `json:"createdAt"`
}

// FileInput is what the client posts when starting an upload.
type FileInput struct {
	Kind     string `json:"kind"`
	Slot     *int   `json:"slot,omitempty"`
	Label    string `json:"label"`
	FileName string `json:"fileName"`
	FileSize int64  `json:"fileSize"`
	MimeType string `json:"mimeType"`
}

// AIReport is the row + JSON shape for the resume report.
//
// Format dispatch: a row has EITHER report_md (legacy Markdown) OR
// report_json (new structured score report). The handler picks based on
// which field is populated — never both. New writes always go through
// report_json; report_md is preserved for historical reads only.
type AIReport struct {
	ID           uuid.UUID `json:"id"`
	ResumeFileID *string   `json:"resumeFileId,omitempty"`
	DraftID      *string   `json:"draftId,omitempty"`
	// Format is "json" when report_json is set, "md" when legacy
	// report_md is set. The FE keys its renderer off this.
	Format     string          `json:"format"`
	ReportMD   string          `json:"reportMd,omitempty"`
	ReportJSON json.RawMessage `json:"reportJson,omitempty"`
	Score      int             `json:"score"`
	CreatedAt  string          `json:"createdAt"`
}

// AIReportSummary is the row + JSON shape for GET /ai/resume/reports.
// Drops the body so list responses stay small; FE re-fetches per id.
// ResumeFilename is joined from job_tracker.files and is nullable because
// the FK is ON DELETE SET NULL (resume can be deleted after generating a report).
type AIReportSummary struct {
	ID             uuid.UUID `json:"id"`
	ResumeFileID   *string   `json:"resumeFileId,omitempty"`
	ResumeFilename *string   `json:"resumeFilename,omitempty"`
	DraftID        *string   `json:"draftId,omitempty"`
	Format         string    `json:"format"`
	Score          int       `json:"score"`
	CreatedAt      string    `json:"createdAt"`
}

// ListAIReportsOpts mirrors ListNotificationsOpts: cursor is the createdAt of
// the last row from the prior page (exclusive); empty = first page.
type ListAIReportsOpts struct {
	Cursor time.Time
	Limit  int
}

// CreateFile inserts a metadata row before the actual upload happens.
// Returns the row so the handler can build the storage key + presigned URL.
// If slot is non-nil, any existing row in (user, kind, slot) is deleted first
// so the new file replaces it — and the caller must also clean up R2.
func (s *Store) CreateFile(ctx context.Context, userID uuid.UUID, kind, label, fileName, mimeType, storageKey string, fileSize int64, slot *int) (*File, []string, error) {
	var orphanedKeys []string
	if slot != nil {
		// Replace semantics: clear the slot first, returning storage_key(s)
		// so the caller can delete the R2 objects.
		const clearQ = `
			DELETE FROM job_tracker.files
			WHERE user_id = $1 AND kind = $2 AND slot = $3
			RETURNING storage_key
		`
		rows, err := s.pool.Query(ctx, clearQ, userID, kind, *slot)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err == nil {
				orphanedKeys = append(orphanedKeys, key)
			}
		}
		rows.Close()
	}
	const q = `
		INSERT INTO job_tracker.files (user_id, kind, slot, label, file_name, file_size, mime_type, storage_key)
		VALUES ($1, $2, $3, NULLIF($4,''), $5, $6, NULLIF($7,''), $8)
		RETURNING id, kind, slot, COALESCE(label,''), file_name, file_size, COALESCE(mime_type,''), storage_key,
		          COALESCE(to_char(uploaded_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
		          to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	`
	var f File
	err := s.pool.QueryRow(ctx, q, userID, kind, slot, label, fileName, fileSize, mimeType, storageKey).
		Scan(&f.ID, &f.Kind, &f.Slot, &f.Label, &f.FileName, &f.FileSize, &f.MimeType, &f.StorageKey, &f.UploadedAt, &f.CreatedAt)
	if err != nil {
		return nil, orphanedKeys, err
	}
	return &f, orphanedKeys, nil
}

// FinalizeFile marks the upload as complete (after the browser PUT to R2 succeeds).
func (s *Store) FinalizeFile(ctx context.Context, userID, id uuid.UUID, fileSize int64) error {
	const q = `
		UPDATE job_tracker.files
		SET uploaded_at = NOW(),
		    file_size = CASE WHEN $1 > 0 THEN $1 ELSE file_size END
		WHERE id = $2 AND user_id = $3
	`
	tag, err := s.pool.Exec(ctx, q, fileSize, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) ListFiles(ctx context.Context, userID uuid.UUID, kind string) ([]File, error) {
	const q = `
		SELECT id, kind, slot, COALESCE(label,''), file_name, file_size, COALESCE(mime_type,''), storage_key,
		       COALESCE(to_char(uploaded_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM job_tracker.files
		WHERE user_id = $1 AND kind = $2
		ORDER BY slot NULLS LAST, created_at DESC
	`
	rows, err := s.pool.Query(ctx, q, userID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]File, 0)
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.Kind, &f.Slot, &f.Label, &f.FileName, &f.FileSize, &f.MimeType, &f.StorageKey,
			&f.UploadedAt, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) GetFile(ctx context.Context, userID, id uuid.UUID) (*File, error) {
	const q = `
		SELECT id, kind, slot, COALESCE(label,''), file_name, file_size, COALESCE(mime_type,''), storage_key,
		       COALESCE(to_char(uploaded_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM job_tracker.files
		WHERE id = $1 AND user_id = $2
	`
	var f File
	err := s.pool.QueryRow(ctx, q, id, userID).
		Scan(&f.ID, &f.Kind, &f.Slot, &f.Label, &f.FileName, &f.FileSize, &f.MimeType, &f.StorageKey, &f.UploadedAt, &f.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// UpdateFileLabel changes the label on an existing file row. Idempotent.
// Empty label clears it (NULL in DB).
func (s *Store) UpdateFileLabel(ctx context.Context, userID, id uuid.UUID, label string) error {
	const q = `
		UPDATE job_tracker.files
		SET label = NULLIF($1,'')
		WHERE id = $2 AND user_id = $3
	`
	tag, err := s.pool.Exec(ctx, q, label, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteFile(ctx context.Context, userID, id uuid.UUID) (string, error) {
	// Returns the storage_key so the caller can also delete the R2 object.
	const q = `
		DELETE FROM job_tracker.files WHERE id = $1 AND user_id = $2
		RETURNING storage_key
	`
	var key string
	err := s.pool.QueryRow(ctx, q, id, userID).Scan(&key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", pgx.ErrNoRows
		}
		return "", err
	}
	return key, nil
}

// SaveReport persists a legacy Markdown AI resume report. Kept for
// back-compat with any caller still on the markdown path — new code
// should use SaveScoreReport.
func (s *Store) SaveReport(ctx context.Context, userID uuid.UUID, fileID *uuid.UUID, reportMD string, score int) (*AIReport, error) {
	const q = `
		INSERT INTO job_tracker.ai_reports (user_id, resume_file_id, report_md, score)
		VALUES ($1, $2, $3, $4)
		RETURNING id, resume_file_id, report_md, COALESCE(score, 0),
		          to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	`
	var r AIReport
	var fileRef *uuid.UUID
	err := s.pool.QueryRow(ctx, q, userID, fileID, reportMD, score).
		Scan(&r.ID, &fileRef, &r.ReportMD, &r.Score, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	if fileRef != nil {
		s := fileRef.String()
		r.ResumeFileID = &s
	}
	r.Format = "md"
	return &r, nil
}

// CreatePendingScoreReport inserts an "empty" report row reserved for a
// score generation that's still running in a background goroutine.
// Both report_md and report_json stay NULL — that absence is how the
// FE / GetReport detect pending state.
//
// Returns the new row's id so the handler can return it immediately
// (202 Accepted) and the FE can poll for completion.
func (s *Store) CreatePendingScoreReport(ctx context.Context, userID uuid.UUID, fileID, draftID *uuid.UUID) (uuid.UUID, error) {
	const q = `
		INSERT INTO job_tracker.ai_reports (user_id, resume_file_id, draft_id, score)
		VALUES ($1, $2, $3, 0)
		RETURNING id
	`
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, q, userID, fileID, draftID).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateScoreReport fills in the JSON body + final score on a row that
// was previously inserted via CreatePendingScoreReport. Idempotent if
// called twice (last write wins) — but the handler only calls it once
// per goroutine.
func (s *Store) UpdateScoreReport(ctx context.Context, userID, id uuid.UUID, reportJSON []byte, score int) error {
	const q = `
		UPDATE job_tracker.ai_reports
		SET report_json = $3, score = $4
		WHERE id = $1 AND user_id = $2
	`
	_, err := s.pool.Exec(ctx, q, id, userID, reportJSON, score)
	return err
}

// SaveScoreReport persists a structured (JSON) resume score report.
// Either fileID or draftID may be non-nil (or both, e.g. when a
// builder-sourced report also happened to write a Vault PDF). Both
// can also be nil when the user pasted raw text.
func (s *Store) SaveScoreReport(ctx context.Context, userID uuid.UUID, fileID, draftID *uuid.UUID, reportJSON []byte, score int) (*AIReport, error) {
	const q = `
		INSERT INTO job_tracker.ai_reports (user_id, resume_file_id, draft_id, report_json, score)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, resume_file_id, draft_id, report_json, COALESCE(score, 0),
		          to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	`
	var r AIReport
	var fileRef, draftRef *uuid.UUID
	err := s.pool.QueryRow(ctx, q, userID, fileID, draftID, reportJSON, score).
		Scan(&r.ID, &fileRef, &draftRef, &r.ReportJSON, &r.Score, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	if fileRef != nil {
		v := fileRef.String()
		r.ResumeFileID = &v
	}
	if draftRef != nil {
		v := draftRef.String()
		r.DraftID = &v
	}
	r.Format = "json"
	return &r, nil
}

// ResumeUsage counts how often this resume has been referenced — today that
// only means how many AI reports were generated against it. Once applications
// gain a per-row resume picker, that count will join in. Tenant-isolated:
// only counts the user's own rows, returns (0, nil) if the resume isn't theirs.
func (s *Store) ResumeUsage(ctx context.Context, userID, resumeID uuid.UUID) (int, error) {
	// Cheap ownership check first — saves a join when the resume isn't ours.
	const ownQ = `SELECT 1 FROM job_tracker.files WHERE id = $1 AND user_id = $2 AND kind = 'resume'`
	var x int
	if err := s.pool.QueryRow(ctx, ownQ, resumeID, userID).Scan(&x); err != nil {
		return 0, err // pgx.ErrNoRows bubbles up; handler maps to 404.
	}
	const q = `
		SELECT COUNT(*)
		FROM job_tracker.ai_reports
		WHERE user_id = $1 AND resume_file_id = $2
	`
	var n int
	if err := s.pool.QueryRow(ctx, q, userID, resumeID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// scanAIReportRow reads both legacy (report_md) and new (report_json)
// columns and sets Format = "json" | "md" so the handler knows which
// body to surface. Used by LatestReport + GetReport.
func scanAIReportRow(row pgx.Row) (*AIReport, error) {
	var (
		r          AIReport
		fileRef    *uuid.UUID
		draftRef   *uuid.UUID
		reportMD   *string
		reportJSON []byte
	)
	if err := row.Scan(
		&r.ID, &fileRef, &draftRef, &reportMD, &reportJSON, &r.Score, &r.CreatedAt,
	); err != nil {
		return nil, err
	}
	if fileRef != nil {
		v := fileRef.String()
		r.ResumeFileID = &v
	}
	if draftRef != nil {
		v := draftRef.String()
		r.DraftID = &v
	}
	switch {
	case len(reportJSON) > 0:
		r.ReportJSON = json.RawMessage(reportJSON)
		r.Format = "json"
	case reportMD != nil:
		r.ReportMD = *reportMD
		r.Format = "md"
	default:
		// Both columns null = pending async generation. FE polls this
		// id every 10s until format flips to "json".
		r.Format = "pending"
	}
	return &r, nil
}

// LatestReport returns the most recent report for the user, or pgx.ErrNoRows.
func (s *Store) LatestReport(ctx context.Context, userID uuid.UUID) (*AIReport, error) {
	const q = `
		SELECT id, resume_file_id, draft_id, report_md, report_json, COALESCE(score, 0),
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM job_tracker.ai_reports
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`
	return scanAIReportRow(s.pool.QueryRow(ctx, q, userID))
}

// GetReport returns one report by id if the user owns it, else pgx.ErrNoRows.
func (s *Store) GetReport(ctx context.Context, userID, id uuid.UUID) (*AIReport, error) {
	const q = `
		SELECT id, resume_file_id, draft_id, report_md, report_json, COALESCE(score, 0),
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM job_tracker.ai_reports
		WHERE id = $1 AND user_id = $2
	`
	return scanAIReportRow(s.pool.QueryRow(ctx, q, id, userID))
}

// ListAIReports returns one page of summary rows for the user, newest first.
// Markdown is intentionally NOT selected — list views render the score chip
// and a filename, and a 50-row page with full bodies would balloon to MBs.
// Cursor pages by created_at (exclusive). Limit clamps to [1, 100], default 50.
// resume_file_id is FK with ON DELETE SET NULL, so the LEFT JOIN preserves
// orphan rows (deleted resume) — Filename comes back as nil, score still shows.
func (s *Store) ListAIReports(ctx context.Context, userID uuid.UUID, opts ListAIReportsOpts) ([]AIReportSummary, time.Time, error) {
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = 50
	case limit > 100:
		limit = 100
	}

	args := []any{userID, limit}
	q := `
		SELECT r.id, r.resume_file_id, r.draft_id, f.file_name,
		       COALESCE(r.score, 0),
		       CASE
		         WHEN r.report_json IS NOT NULL THEN 'json'
		         WHEN r.report_md IS NOT NULL THEN 'md'
		         ELSE 'pending'
		       END,
		       to_char(r.created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       r.created_at
		FROM job_tracker.ai_reports r
		LEFT JOIN job_tracker.files f ON f.id = r.resume_file_id
		WHERE r.user_id = $1
	`
	if !opts.Cursor.IsZero() {
		args = append(args, opts.Cursor)
		q += fmt.Sprintf(" AND r.created_at < $%d", len(args))
	}
	q += " ORDER BY r.created_at DESC LIMIT $2"

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()

	out := make([]AIReportSummary, 0, limit)
	var lastTS time.Time
	for rows.Next() {
		var (
			row      AIReportSummary
			fileRef  *uuid.UUID
			draftRef *uuid.UUID
			fname    *string
			ts       time.Time
		)
		if err := rows.Scan(&row.ID, &fileRef, &draftRef, &fname, &row.Score, &row.Format, &row.CreatedAt, &ts); err != nil {
			return nil, time.Time{}, err
		}
		if fileRef != nil {
			v := fileRef.String()
			row.ResumeFileID = &v
		}
		if draftRef != nil {
			v := draftRef.String()
			row.DraftID = &v
		}
		row.ResumeFilename = fname
		out = append(out, row)
		lastTS = ts
	}
	return out, lastTS, rows.Err()
}
