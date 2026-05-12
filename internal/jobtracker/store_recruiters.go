package jobtracker

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateRecruiter(ctx context.Context, userID uuid.UUID, in RecruiterInput) (*Recruiter, error) {
	const q = `
		INSERT INTO job_tracker.recruiters (user_id, name, email, company, linkedin_url, phone, notes)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, name, email, company, linkedin_url, phone, notes, created_at, updated_at
	`
	row := s.pool.QueryRow(ctx, q, userID, in.Name, in.Email, in.Company, in.LinkedinURL, in.Phone, in.Notes)
	return scanRecruiter(row)
}

func (s *Store) ListRecruiters(ctx context.Context, userID uuid.UUID, search string) ([]Recruiter, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if search != "" {
		const q = `
			SELECT id, name, email, company, linkedin_url, phone, notes, created_at, updated_at
			FROM job_tracker.recruiters
			WHERE user_id = $1
			  AND (name ILIKE '%' || $2 || '%' OR company ILIKE '%' || $2 || '%' OR email ILIKE '%' || $2 || '%')
			ORDER BY name ASC
		`
		rows, err = s.pool.Query(ctx, q, userID, search)
	} else {
		const q = `
			SELECT id, name, email, company, linkedin_url, phone, notes, created_at, updated_at
			FROM job_tracker.recruiters
			WHERE user_id = $1
			ORDER BY name ASC
		`
		rows, err = s.pool.Query(ctx, q, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("list recruiters: %w", err)
	}
	defer rows.Close()

	var out []Recruiter
	for rows.Next() {
		r, e := scanRecruiterCols(rows.Scan)
		if e != nil {
			return nil, e
		}
		out = append(out, *r)
	}
	if out == nil {
		out = []Recruiter{}
	}
	return out, rows.Err()
}

func (s *Store) GetRecruiter(ctx context.Context, userID, id uuid.UUID) (*Recruiter, error) {
	const q = `
		SELECT id, name, email, company, linkedin_url, phone, notes, created_at, updated_at
		FROM job_tracker.recruiters
		WHERE id = $1 AND user_id = $2
	`
	return scanRecruiter(s.pool.QueryRow(ctx, q, id, userID))
}

func (s *Store) PatchRecruiter(ctx context.Context, userID, id uuid.UUID, in RecruiterEdit) (*Recruiter, error) {
	const q = `
		UPDATE job_tracker.recruiters SET
			name         = COALESCE($3, name),
			email        = COALESCE($4, email),
			company      = CASE WHEN $5::boolean THEN $6 ELSE company END,
			linkedin_url = CASE WHEN $7::boolean THEN $8 ELSE linkedin_url END,
			phone        = CASE WHEN $9::boolean THEN $10 ELSE phone END,
			notes        = CASE WHEN $11::boolean THEN $12 ELSE notes END,
			updated_at   = NOW()
		WHERE id = $1 AND user_id = $2
		RETURNING id, name, email, company, linkedin_url, phone, notes, created_at, updated_at
	`
	row := s.pool.QueryRow(ctx, q,
		id, userID,
		in.Name, in.Email,
		in.Company != nil, in.Company,
		in.LinkedinURL != nil, in.LinkedinURL,
		in.Phone != nil, in.Phone,
		in.Notes != nil, in.Notes,
	)
	return scanRecruiter(row)
}

func (s *Store) DeleteRecruiter(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.recruiters WHERE id = $1 AND user_id = $2`
	ct, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return fmt.Errorf("delete recruiter: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func validateRecruiterInput(name, email string, linkedinURL *string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if _, err := parseEmail(email); err != nil {
		return fmt.Errorf("invalid email")
	}
	if linkedinURL != nil {
		if err := ValidateURL(*linkedinURL); err != nil {
			return err
		}
	}
	return nil
}

func scanRecruiter(row interface{ Scan(...any) error }) (*Recruiter, error) {
	return scanRecruiterCols(row.Scan)
}

func scanRecruiterCols(scan func(...any) error) (*Recruiter, error) {
	var (
		id                              uuid.UUID
		name, email                     string
		company, linkedinURL, phone, notes *string
		creAt, updAt                    time.Time
	)
	if err := scan(&id, &name, &email, &company, &linkedinURL, &phone, &notes, &creAt, &updAt); err != nil {
		return nil, err
	}
	return &Recruiter{
		ID:          id.String(),
		Name:        name,
		Email:       email,
		Company:     company,
		LinkedinURL: linkedinURL,
		Phone:       phone,
		Notes:       notes,
		CreatedAt:   creAt.UTC().Format(time.RFC3339),
		UpdatedAt:   updAt.UTC().Format(time.RFC3339),
	}, nil
}
