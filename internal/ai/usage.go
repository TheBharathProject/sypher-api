package ai

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUsageExceeded is returned by EnforceLimit when the caller is over the
// per-month token cap.
var ErrUsageExceeded = errors.New("ai: monthly usage exceeded")

// UsageStore writes/reads the job_tracker.ai_usage table.
type UsageStore struct {
	pool         *pgxpool.Pool
	monthlyLimit int64
}

func NewUsageStore(pool *pgxpool.Pool, monthlyLimit int64) *UsageStore {
	return &UsageStore{pool: pool, monthlyLimit: monthlyLimit}
}

// MonthlyTotal returns the (in + out) token sum for the current calendar
// month for the given user.
func (s *UsageStore) MonthlyTotal(ctx context.Context, userID uuid.UUID) (int64, error) {
	const q = `
		SELECT COALESCE(SUM(tokens_in + tokens_out), 0)
		FROM job_tracker.ai_usage
		WHERE user_id = $1
		  AND created_at >= date_trunc('month', NOW())
	`
	var total int64
	if err := s.pool.QueryRow(ctx, q, userID).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// EnforceLimit returns ErrUsageExceeded if the user has burned their monthly
// budget. Call before every AI request.
func (s *UsageStore) EnforceLimit(ctx context.Context, userID uuid.UUID) error {
	if s.monthlyLimit <= 0 {
		return nil
	}
	total, err := s.MonthlyTotal(ctx, userID)
	if err != nil {
		return err
	}
	if total >= s.monthlyLimit {
		return ErrUsageExceeded
	}
	return nil
}

// Record persists a usage row after a successful AI call.
func (s *UsageStore) Record(ctx context.Context, userID uuid.UUID, endpoint string, tokensIn, tokensOut int) error {
	const q = `
		INSERT INTO job_tracker.ai_usage (user_id, endpoint, tokens_in, tokens_out)
		VALUES ($1, $2, $3, $4)
	`
	_, err := s.pool.Exec(ctx, q, userID, endpoint, tokensIn, tokensOut)
	return err
}

// PeriodWindow returns the start and end timestamps of the current usage
// period (the calendar month) — used by the /ai/usage response.
func PeriodWindow(now time.Time) (start, end time.Time) {
	t := now.UTC()
	start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	end = start.AddDate(0, 1, 0)
	return start, end
}

// Limit returns the configured monthly limit.
func (s *UsageStore) Limit() int64 { return s.monthlyLimit }
