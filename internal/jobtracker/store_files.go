package jobtracker

import (
	"context"
	"errors"

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
type AIReport struct {
	ID           uuid.UUID `json:"id"`
	ResumeFileID *string   `json:"resumeFileId,omitempty"`
	ReportMD     string    `json:"reportMd"`
	Score        int       `json:"score"`
	CreatedAt    string    `json:"createdAt"`
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

// SaveReport persists an AI resume report for later retrieval.
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
	return &r, nil
}

// LatestReport returns the most recent report for the user, or pgx.ErrNoRows.
func (s *Store) LatestReport(ctx context.Context, userID uuid.UUID) (*AIReport, error) {
	const q = `
		SELECT id, resume_file_id, report_md, COALESCE(score, 0),
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM job_tracker.ai_reports
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`
	var r AIReport
	var fileRef *uuid.UUID
	err := s.pool.QueryRow(ctx, q, userID).Scan(&r.ID, &fileRef, &r.ReportMD, &r.Score, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	if fileRef != nil {
		fid := fileRef.String()
		r.ResumeFileID = &fid
	}
	return &r, nil
}
