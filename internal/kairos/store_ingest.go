package kairos

import (
	"context"
	"time"
)

// IngestRun is one row of kairos.ingest_runs — the audit log written by
// every kairos cron / manual snapshot (spec §3.8). Surfaced by the
// admin ingest-runs endpoint, hence camelCase JSON tags (spec §1).
//
// FinishedAt and OK are pointers because the schema allows NULL (a row
// can in principle be opened at start and closed at finish; today
// RecordIngestRun writes complete rows only, but readers must not
// assume that).
type IngestRun struct {
	ID          int64      `json:"id"`
	Job         string     `json:"job"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt"`
	OK          *bool      `json:"ok"`
	RowsWritten int        `json:"rowsWritten"`
	Detail      string     `json:"detail"`
}

// RecordIngestRun inserts one completed-run row into kairos.ingest_runs.
// detail carries the error summary on failure ("" → NULL in the table).
func (s *Store) RecordIngestRun(ctx context.Context, job string, startedAt, finishedAt time.Time, ok bool, rowsWritten int, detail string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO kairos.ingest_runs
			(job, started_at, finished_at, ok, rows_written, detail)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
	`, job, startedAt, finishedAt, ok, rowsWritten, detail)
	return err
}

// ListIngestRuns returns recent runs, newest first. job == "" lists all
// jobs. limit defaults to 50 and is capped at 200.
func (s *Store) ListIngestRuns(ctx context.Context, job string, limit int) ([]IngestRun, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, job, started_at, finished_at, ok,
		       COALESCE(rows_written, 0), COALESCE(detail, '')
		FROM kairos.ingest_runs
		WHERE ($1 = '' OR job = $1)
		ORDER BY started_at DESC
		LIMIT $2
	`, job, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IngestRun
	for rows.Next() {
		var r IngestRun
		if err := rows.Scan(&r.ID, &r.Job, &r.StartedAt, &r.FinishedAt, &r.OK,
			&r.RowsWritten, &r.Detail); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
