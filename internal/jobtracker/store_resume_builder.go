package jobtracker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ============================================================================
// Resume Builder Drafts — CRUD on job_tracker.resume_builder_drafts.
// Schema: migrations/0022_resume_builder_drafts.sql.
// Content is stored as JSONB; we marshal/unmarshal at the boundary so the
// rest of the codebase works with strongly-typed DraftContent.
// ============================================================================

// classic-v2 is the parse-friendly template. New drafts use it so the
// LaTeX-mode editor can auto-sync edits back into the form fields.
// classic-v1 is kept registered for existing drafts (no parser).
const defaultResumeBuilderTemplate = "classic-v2"

// CreateResumeBuilderDraft inserts a new draft. Title is trimmed but not
// validated for length here — the handler enforces 1..200 chars.
func (s *Store) CreateResumeBuilderDraft(ctx context.Context, userID uuid.UUID, in ResumeBuilderDraftInput) (*ResumeBuilderDraft, error) {
	tmpl := in.TemplateID
	if tmpl == "" {
		tmpl = defaultResumeBuilderTemplate
	}
	contentBytes, err := json.Marshal(in.Content)
	if err != nil {
		return nil, fmt.Errorf("marshal content: %w", err)
	}
	const q = `
		INSERT INTO job_tracker.resume_builder_drafts (user_id, title, template_id, content)
		VALUES ($1, $2, $3, $4)
		RETURNING id, title, template_id, content, created_at, updated_at
	`
	row := s.pool.QueryRow(ctx, q, userID, in.Title, tmpl, contentBytes)
	return scanResumeBuilderDraft(row)
}

// ListResumeBuilderDrafts returns the user's drafts in updated-at DESC
// order. Drops the content blob so the rail stays cheap; callers needing
// full content use GetResumeBuilderDraft.
func (s *Store) ListResumeBuilderDrafts(ctx context.Context, userID uuid.UUID) ([]ResumeBuilderDraftSummary, error) {
	const q = `
		SELECT id, title, template_id, created_at, updated_at
		FROM job_tracker.resume_builder_drafts
		WHERE user_id = $1
		ORDER BY updated_at DESC
	`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list drafts: %w", err)
	}
	defer rows.Close()

	out := make([]ResumeBuilderDraftSummary, 0)
	for rows.Next() {
		var (
			id, tmpl   string
			title      string
			creAt, upd time.Time
		)
		if err := rows.Scan(&id, &title, &tmpl, &creAt, &upd); err != nil {
			return nil, err
		}
		out = append(out, ResumeBuilderDraftSummary{
			ID:         id,
			Title:      title,
			TemplateID: tmpl,
			CreatedAt:  creAt.UTC().Format(time.RFC3339),
			UpdatedAt:  upd.UTC().Format(time.RFC3339),
		})
	}
	return out, rows.Err()
}

// GetResumeBuilderDraft returns the full draft if owned by the user;
// otherwise pgx.ErrNoRows so the handler can map to 404 without leaking
// existence.
func (s *Store) GetResumeBuilderDraft(ctx context.Context, userID, id uuid.UUID) (*ResumeBuilderDraft, error) {
	const q = `
		SELECT id, title, template_id, content, created_at, updated_at
		FROM job_tracker.resume_builder_drafts
		WHERE id = $1 AND user_id = $2
	`
	return scanResumeBuilderDraft(s.pool.QueryRow(ctx, q, id, userID))
}

// PatchResumeBuilderDraft updates the fields the caller passed and leaves
// the rest untouched. content is replaced wholesale when present — partial
// patching inside the JSON shape isn't a use case worth complicating the
// query for (the FE always sends the full content object).
func (s *Store) PatchResumeBuilderDraft(ctx context.Context, userID, id uuid.UUID, in ResumeBuilderDraftEdit) (*ResumeBuilderDraft, error) {
	var contentBytes []byte
	if in.Content != nil {
		b, err := json.Marshal(*in.Content)
		if err != nil {
			return nil, fmt.Errorf("marshal content: %w", err)
		}
		contentBytes = b
	}
	const q = `
		UPDATE job_tracker.resume_builder_drafts SET
			title       = COALESCE($3, title),
			template_id = COALESCE($4, template_id),
			content     = CASE WHEN $5::boolean THEN $6::jsonb ELSE content END,
			updated_at  = NOW()
		WHERE id = $1 AND user_id = $2
		RETURNING id, title, template_id, content, created_at, updated_at
	`
	row := s.pool.QueryRow(ctx, q,
		id, userID,
		in.Title, in.TemplateID,
		in.Content != nil, contentBytes,
	)
	return scanResumeBuilderDraft(row)
}

// DeleteResumeBuilderDraft hard-deletes the row (no soft-delete in MVP).
// Returns pgx.ErrNoRows if the draft doesn't exist or isn't owned.
func (s *Store) DeleteResumeBuilderDraft(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.resume_builder_drafts WHERE id = $1 AND user_id = $2`
	ct, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return fmt.Errorf("delete draft: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func scanResumeBuilderDraft(row interface{ Scan(...any) error }) (*ResumeBuilderDraft, error) {
	var (
		id, tmpl     string
		title        string
		contentBytes []byte
		creAt, upd   time.Time
	)
	if err := row.Scan(&id, &title, &tmpl, &contentBytes, &creAt, &upd); err != nil {
		return nil, err
	}
	var content DraftContent
	if len(contentBytes) > 0 {
		if err := json.Unmarshal(contentBytes, &content); err != nil {
			return nil, fmt.Errorf("unmarshal content: %w", err)
		}
	}
	return &ResumeBuilderDraft{
		ID:         id,
		Title:      title,
		TemplateID: tmpl,
		Content:    content,
		CreatedAt:  creAt.UTC().Format(time.RFC3339),
		UpdatedAt:  upd.UTC().Format(time.RFC3339),
	}, nil
}
