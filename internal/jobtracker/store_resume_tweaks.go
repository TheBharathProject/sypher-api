package jobtracker

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ============================================================================
// Resume Tweaks — versioned AI rewrites with revision-chain history.
// Schema lives in migrations/0015_resume_tweaks.sql.
// Pricing lives in internal/billing/costs.go (CostResumeTweak = 20).
// ============================================================================

// CreateResumeTweakArgs bundles everything CreateResumeTweak needs.
// Keeping it a struct (vs many positional args) keeps callers readable
// and lets us add optional fields later without churning the signature.
type CreateResumeTweakArgs struct {
	UserID        uuid.UUID
	ParentID      *uuid.UUID
	ApplicationID *uuid.UUID
	SourceFileID  *uuid.UUID
	Title         string
	SourceText    string
	Prompt        string
	TweakedText   string
	TokensIn      int
	TokensOut     int
}

// CreateResumeTweak inserts a new revision and returns the full row.
// Caller is responsible for credit debiting (see gateAICredit in
// handlers_ai.go) — this method only persists.
func (s *Store) CreateResumeTweak(ctx context.Context, a CreateResumeTweakArgs) (*ResumeTweak, error) {
	const q = `
		INSERT INTO job_tracker.resume_tweaks (
			user_id, parent_id, application_id, source_file_id,
			title, source_text, prompt, tweaked_text,
			tokens_in, tokens_out
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, parent_id, application_id, source_file_id,
		          title, source_text, prompt, tweaked_text,
		          COALESCE(user_edits, ''),
		          tokens_in, tokens_out,
		          to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		          to_char(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	`
	var r ResumeTweak
	var parent, app, file *uuid.UUID
	err := s.pool.QueryRow(ctx, q,
		a.UserID, a.ParentID, a.ApplicationID, a.SourceFileID,
		a.Title, a.SourceText, a.Prompt, a.TweakedText,
		a.TokensIn, a.TokensOut,
	).Scan(
		&r.ID, &parent, &app, &file,
		&r.Title, &r.SourceText, &r.Prompt, &r.TweakedText,
		&r.UserEdits, &r.TokensIn, &r.TokensOut,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert resume tweak: %w", err)
	}
	if parent != nil {
		v := parent.String()
		r.ParentID = &v
	}
	if app != nil {
		v := app.String()
		r.ApplicationID = &v
	}
	if file != nil {
		v := file.String()
		r.SourceFileID = &v
	}
	return &r, nil
}

// GetResumeTweak fetches one row by id, tenant-isolated. Returns
// pgx.ErrNoRows when missing or not the caller's.
func (s *Store) GetResumeTweak(ctx context.Context, userID, id uuid.UUID) (*ResumeTweak, error) {
	const q = `
		SELECT id, parent_id, application_id, source_file_id,
		       title, source_text, prompt, tweaked_text,
		       COALESCE(user_edits, ''),
		       tokens_in, tokens_out,
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       to_char(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM job_tracker.resume_tweaks
		WHERE id = $1 AND user_id = $2
	`
	var r ResumeTweak
	var parent, app, file *uuid.UUID
	err := s.pool.QueryRow(ctx, q, id, userID).Scan(
		&r.ID, &parent, &app, &file,
		&r.Title, &r.SourceText, &r.Prompt, &r.TweakedText,
		&r.UserEdits, &r.TokensIn, &r.TokensOut,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if parent != nil {
		v := parent.String()
		r.ParentID = &v
	}
	if app != nil {
		v := app.String()
		r.ApplicationID = &v
	}
	if file != nil {
		v := file.String()
		r.SourceFileID = &v
	}
	return &r, nil
}

// ListResumeTweaks returns summaries (no heavy text blobs) newest first.
// Optionally filtered to a single application; pass uuid.Nil to disable.
func (s *Store) ListResumeTweaks(ctx context.Context, userID uuid.UUID, applicationID uuid.UUID, limit int) ([]ResumeTweakSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows pgx.Rows
	var err error
	if applicationID == uuid.Nil {
		rows, err = s.pool.Query(ctx, `
			SELECT id, parent_id, application_id, title,
			       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			       to_char(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
			FROM job_tracker.resume_tweaks
			WHERE user_id = $1
			ORDER BY created_at DESC
			LIMIT $2
		`, userID, limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT id, parent_id, application_id, title,
			       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			       to_char(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
			FROM job_tracker.resume_tweaks
			WHERE user_id = $1 AND application_id = $2
			ORDER BY created_at DESC
			LIMIT $3
		`, userID, applicationID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ResumeTweakSummary
	for rows.Next() {
		var s ResumeTweakSummary
		var parent, app *uuid.UUID
		if err := rows.Scan(&s.ID, &parent, &app, &s.Title, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		if parent != nil {
			v := parent.String()
			s.ParentID = &v
		}
		if app != nil {
			v := app.String()
			s.ApplicationID = &v
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateResumeTweak applies non-nil fields from `edit` and returns the
// fresh row. No-op (returns current row) when both fields are nil.
// No credits charged — editing existing rows is free.
func (s *Store) UpdateResumeTweak(ctx context.Context, userID, id uuid.UUID, edit ResumeTweakEdit) (*ResumeTweak, error) {
	if edit.Title == nil && edit.UserEdits == nil {
		return s.GetResumeTweak(ctx, userID, id)
	}
	// COALESCE the nullable params so callers can patch one field at a
	// time without sending the other. updated_at bumps unconditionally
	// when we hit this path so the FE can show a "last edited" timestamp.
	const q = `
		UPDATE job_tracker.resume_tweaks
		SET title      = COALESCE($3, title),
		    user_edits = COALESCE($4, user_edits),
		    updated_at = NOW()
		WHERE id = $1 AND user_id = $2
	`
	tag, err := s.pool.Exec(ctx, q, id, userID, edit.Title, edit.UserEdits)
	if err != nil {
		return nil, fmt.Errorf("update resume tweak: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, pgx.ErrNoRows
	}
	return s.GetResumeTweak(ctx, userID, id)
}

// DeleteResumeTweak removes one row. Children's parent_id becomes NULL
// via the ON DELETE SET NULL constraint — the rest of the chain stays.
func (s *Store) DeleteResumeTweak(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM job_tracker.resume_tweaks WHERE id = $1 AND user_id = $2`,
		id, userID)
	if err != nil {
		return fmt.Errorf("delete resume tweak: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// resolveTweakSource picks the source resume text for a new tweak,
// honouring the precedence: explicit sourceText > parent's tweaked
// output (continuation flow) > extracted text from a Vault file.
// Returns ("", "", nil) when nothing usable was provided — caller maps to 400.
func (s *Store) resolveTweakSource(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID, sourceFileID *uuid.UUID, explicitText string) (text string, fileRef *uuid.UUID, err error) {
	if explicitText != "" {
		return explicitText, sourceFileID, nil
	}
	if parentID != nil {
		parent, err := s.GetResumeTweak(ctx, userID, *parentID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", nil, fmt.Errorf("parent tweak not found")
			}
			return "", nil, err
		}
		// Honour user_edits over raw tweaked_text when present, so the
		// continuation flows from "the version the user actually accepted".
		txt := parent.UserEdits
		if txt == "" {
			txt = parent.TweakedText
		}
		// File ref carries forward only when caller didn't override.
		if sourceFileID == nil && parent.SourceFileID != nil {
			fid, perr := uuid.Parse(*parent.SourceFileID)
			if perr == nil {
				return txt, &fid, nil
			}
		}
		return txt, sourceFileID, nil
	}
	return "", sourceFileID, nil
}
