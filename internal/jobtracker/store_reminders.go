package jobtracker

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateReminder(ctx context.Context, userID, appID uuid.UUID, in ReminderInput) (*Reminder, error) {
	triggersAt, err := time.Parse(time.RFC3339, in.TriggersAt)
	if err != nil {
		return nil, fmt.Errorf("bad triggersAt: %w", err)
	}
	const q = `
		INSERT INTO job_tracker.reminders (user_id, application_id, triggers_at, note)
		VALUES ($1, $2, $3, $4)
		RETURNING id, application_id, triggers_at, note, fired_at, created_at, updated_at
	`
	row := s.pool.QueryRow(ctx, q, userID, appID, triggersAt, in.Note)
	return scanReminder(row)
}

func (s *Store) ListReminders(ctx context.Context, userID uuid.UUID, appID *uuid.UUID) ([]Reminder, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if appID != nil {
		const q = `
			SELECT id, application_id, triggers_at, note, fired_at, created_at, updated_at
			FROM job_tracker.reminders
			WHERE user_id = $1 AND application_id = $2
			ORDER BY triggers_at ASC
		`
		rows, err = s.pool.Query(ctx, q, userID, *appID)
	} else {
		const q = `
			SELECT id, application_id, triggers_at, note, fired_at, created_at, updated_at
			FROM job_tracker.reminders
			WHERE user_id = $1
			ORDER BY triggers_at ASC
		`
		rows, err = s.pool.Query(ctx, q, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("list reminders: %w", err)
	}
	defer rows.Close()

	var out []Reminder
	for rows.Next() {
		r, e := scanReminderCols(rows.Scan)
		if e != nil {
			return nil, e
		}
		out = append(out, *r)
	}
	if out == nil {
		out = []Reminder{}
	}
	return out, rows.Err()
}

func (s *Store) GetReminder(ctx context.Context, userID, id uuid.UUID) (*Reminder, error) {
	const q = `
		SELECT id, application_id, triggers_at, note, fired_at, created_at, updated_at
		FROM job_tracker.reminders
		WHERE id = $1 AND user_id = $2
	`
	return scanReminder(s.pool.QueryRow(ctx, q, id, userID))
}

func (s *Store) PatchReminder(ctx context.Context, userID, id uuid.UUID, in ReminderEdit) (*Reminder, error) {
	if in.TriggersAt != nil {
		if _, err := time.Parse(time.RFC3339, *in.TriggersAt); err != nil {
			return nil, fmt.Errorf("bad triggersAt: %w", err)
		}
	}
	const q = `
		UPDATE job_tracker.reminders SET
			triggers_at = COALESCE($3::timestamptz, triggers_at),
			note        = CASE WHEN $4::boolean THEN $5 ELSE note END,
			updated_at  = NOW()
		WHERE id = $1 AND user_id = $2
		RETURNING id, application_id, triggers_at, note, fired_at, created_at, updated_at
	`
	noteProvided := in.Note != nil
	return scanReminder(s.pool.QueryRow(ctx, q, id, userID, in.TriggersAt, noteProvided, in.Note))
}

func (s *Store) DeleteReminder(ctx context.Context, userID, id uuid.UUID) error {
	const q = `DELETE FROM job_tracker.reminders WHERE id = $1 AND user_id = $2`
	ct, err := s.pool.Exec(ctx, q, id, userID)
	if err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DueReminders returns all unfired reminders whose triggers_at is in the past.
// Called by the every-5-min cron job. Uses FOR UPDATE SKIP LOCKED so that if
// we ever run two replicas, they don't race on the same rows.
func (s *Store) DueReminders(ctx context.Context) ([]DueReminder, error) {
	const q = `
		SELECT id, user_id, application_id, note
		FROM job_tracker.reminders
		WHERE triggers_at <= NOW() AND fired_at IS NULL
		FOR UPDATE SKIP LOCKED
	`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("due reminders: %w", err)
	}
	defer rows.Close()

	var out []DueReminder
	for rows.Next() {
		var r DueReminder
		if e := rows.Scan(&r.ID, &r.UserID, &r.ApplicationID, &r.Note); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkReminderFired sets fired_at = NOW() for the given reminder row.
func (s *Store) MarkReminderFired(ctx context.Context, id uuid.UUID) error {
	const q = `UPDATE job_tracker.reminders SET fired_at = NOW() WHERE id = $1`
	_, err := s.pool.Exec(ctx, q, id)
	return err
}

// scanReminder scans a single pgx.Row into a Reminder.
func scanReminder(row interface{ Scan(...any) error }) (*Reminder, error) {
	return scanReminderCols(row.Scan)
}

func scanReminderCols(scan func(...any) error) (*Reminder, error) {
	var (
		id, appID          uuid.UUID
		triggersAt, creAt, updAt time.Time
		firedAt            *time.Time
		note               *string
	)
	if err := scan(&id, &appID, &triggersAt, &note, &firedAt, &creAt, &updAt); err != nil {
		return nil, err
	}
	r := &Reminder{
		ID:            id.String(),
		ApplicationID: appID.String(),
		TriggersAt:    triggersAt.UTC().Format(time.RFC3339),
		Note:          note,
		CreatedAt:     creAt.UTC().Format(time.RFC3339),
		UpdatedAt:     updAt.UTC().Format(time.RFC3339),
	}
	if firedAt != nil {
		s := firedAt.UTC().Format(time.RFC3339)
		r.FiredAt = &s
	}
	return r, nil
}
