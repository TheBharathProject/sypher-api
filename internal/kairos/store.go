package kairos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// Store is the data-access layer for everything in the kairos schema.
// One concrete struct, methods grouped by domain (chain / strategies /
// backtests / partitions).
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ─────────────────────────────────────────────────────────────────────────
// Strategies
// ─────────────────────────────────────────────────────────────────────────

// SaveStrategy inserts a new strategy for the user and returns the
// populated row. Caller is responsible for validation.
func (s *Store) SaveStrategy(ctx context.Context, uid uuid.UUID, in *Strategy) (*Strategy, error) {
	legsJSON, err := MarshalLegs(in.Legs)
	if err != nil {
		return nil, fmt.Errorf("marshal legs: %w", err)
	}
	out := &Strategy{
		UserID:     uid,
		Name:       in.Name,
		Underlying: in.Underlying,
		Legs:       in.Legs,
		EntryTime:  in.EntryTime,
		ExitTime:   in.ExitTime,
		StopLoss:   in.StopLoss,
		Target:     in.Target,
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO kairos.strategies
			(user_id, name, underlying, legs, entry_time, exit_time, stop_loss, target)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at
	`, uid, in.Name, in.Underlying, legsJSON, in.EntryTime, in.ExitTime, in.StopLoss, in.Target).
		Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListStrategies returns the user's strategies, newest first.
func (s *Store) ListStrategies(ctx context.Context, uid uuid.UUID) ([]Strategy, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, underlying, legs, entry_time, exit_time, stop_loss, target, created_at, updated_at
		FROM kairos.strategies
		WHERE user_id = $1
		ORDER BY created_at DESC
	`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Strategy
	for rows.Next() {
		var (
			st       Strategy
			legsJSON []byte
		)
		if err := rows.Scan(&st.ID, &st.Name, &st.Underlying, &legsJSON, &st.EntryTime, &st.ExitTime, &st.StopLoss, &st.Target, &st.CreatedAt, &st.UpdatedAt); err != nil {
			return nil, err
		}
		legs, err := UnmarshalLegs(legsJSON)
		if err != nil {
			return nil, err
		}
		st.UserID = uid
		st.Legs = legs
		out = append(out, st)
	}
	return out, rows.Err()
}

// DeleteStrategy removes the row if it belongs to the user. Returns
// pgx.ErrNoRows if not found / not owned.
func (s *Store) DeleteStrategy(ctx context.Context, uid, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM kairos.strategies WHERE id = $1 AND user_id = $2
	`, id, uid)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// Backtests
// ─────────────────────────────────────────────────────────────────────────

// CreateBacktest inserts a backtests row with status='pending' and
// returns the new UUID. The handler then enqueues onto the worker
// channel; the row is the durable record (ADR-0010 D4).
func (s *Store) CreateBacktest(ctx context.Context, uid uuid.UUID, req *BacktestRequest, strategyID *uuid.UUID) (uuid.UUID, error) {
	legsJSON, err := MarshalLegs(req.Legs)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO kairos.backtests
			(user_id, strategy_id, name, underlying, legs, from_date, to_date,
			 entry_time, exit_time, stop_loss, target, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'pending')
		RETURNING id
	`, uid, strategyID, req.Name, req.Underlying, legsJSON, req.FromDate, req.ToDate,
		req.EntryTime, req.ExitTime, req.StopLoss, req.Target).Scan(&id)
	return id, err
}

// GetBacktest returns the row by id, scoped to the user.
func (s *Store) GetBacktest(ctx context.Context, uid, id uuid.UUID) (*Backtest, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, user_id, strategy_id, name, underlying, legs,
		       from_date, to_date, entry_time, exit_time, stop_loss, target,
		       status, result, error, started_at, finished_at, created_at
		FROM kairos.backtests
		WHERE id = $1 AND user_id = $2
	`, id, uid)
	return scanBacktest(row)
}

// ListBacktests returns the user's backtest history, newest first.
func (s *Store) ListBacktests(ctx context.Context, uid uuid.UUID, limit int) ([]Backtest, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, strategy_id, name, underlying, legs,
		       from_date, to_date, entry_time, exit_time, stop_loss, target,
		       status, result, error, started_at, finished_at, created_at
		FROM kairos.backtests
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, uid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backtest
	for rows.Next() {
		bt, err := scanBacktest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *bt)
	}
	return out, rows.Err()
}

// ClaimNextBacktest atomically transitions a pending row to running and
// returns it. Returns pgx.ErrNoRows when there's nothing to do.
// ADR-0010 D4: the UPDATE...RETURNING guarantees a row is only claimed
// once even across the channel and the startup sweep.
func (s *Store) ClaimNextBacktest(ctx context.Context, id uuid.UUID) (*Backtest, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE kairos.backtests
		SET status = 'running', started_at = NOW()
		WHERE id = $1 AND status = 'pending'
		RETURNING id, user_id, strategy_id, name, underlying, legs,
		          from_date, to_date, entry_time, exit_time, stop_loss, target,
		          status, result, error, started_at, finished_at, created_at
	`, id)
	return scanBacktest(row)
}

// FinishBacktest writes the result JSONB and flips status to done.
func (s *Store) FinishBacktest(ctx context.Context, id uuid.UUID, res *BacktestResult) error {
	resJSON, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE kairos.backtests
		SET status = 'done', result = $1, finished_at = NOW()
		WHERE id = $2
	`, resJSON, id)
	return err
}

// FailBacktest records an error message and flips status to failed.
func (s *Store) FailBacktest(ctx context.Context, id uuid.UUID, msg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE kairos.backtests
		SET status = 'failed', error = $1, finished_at = NOW()
		WHERE id = $2
	`, msg, id)
	return err
}

// SweepStaleRunning resets any running rows older than 5 minutes back
// to pending. Called from worker.Pool.Start on boot per ADR-0010 D5.
func (s *Store) SweepStaleRunning(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE kairos.backtests
		SET status = 'pending', started_at = NULL
		WHERE status = 'running' AND started_at < NOW() - INTERVAL '5 minutes'
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PendingBacktestIDs returns up to `limit` oldest pending rows. The
// startup sweep pushes these onto the worker channel.
func (s *Store) PendingBacktestIDs(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM kairos.backtests
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────
// Option chains — ingestion + query
// ─────────────────────────────────────────────────────────────────────────

// BulkInsertChain writes a batch of ChainRows. Used by the snapshot
// cron. Per ADR-0009 we use pgx.CopyFrom for speed.
func (s *Store) BulkInsertChain(ctx context.Context, providerName string, rows []provider.ChainRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		return []any{
			providerName,
			string(r.Underlying),
			r.ExpiryDate,
			r.Strike,
			string(r.OptionType),
			r.SnapshotTime,
			nullF(r.Spot),
			nullF(r.LTP),
			nullF(r.Bid),
			nullF(r.Ask),
			nullI(r.OI),
			nullI(r.OIChange),
			nullI(r.Volume),
		}, nil
	})
	cols := []string{"provider", "underlying", "expiry_date", "strike", "option_type",
		"snapshot_time", "spot", "ltp", "bid", "ask", "oi", "oi_change", "volume"}
	n, err := s.pool.CopyFrom(ctx, pgx.Identifier{"kairos", "option_chains"}, cols, src)
	return n, err
}

// LatestChain returns the most recent snapshot for an (underlying, expiry)
// plus the snapshot's age in seconds. Empty rows + zero time when no
// data — distinguished from a query error by err==nil and len(rows)==0.
//
// MAX() over a filter that matches no rows returns NULL, not zero rows,
// so we scan into a *time.Time and treat nil as "no data yet."
func (s *Store) LatestChain(ctx context.Context, u provider.Underlying, expiry time.Time) ([]provider.ChainRow, time.Time, error) {
	var snapshot *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT MAX(snapshot_time)
		FROM kairos.option_chains
		WHERE underlying = $1 AND expiry_date = $2
	`, string(u), expiry).Scan(&snapshot)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, err
	}
	if snapshot == nil {
		// No snapshots collected for this (underlying, expiry) yet.
		// Caller should serve sample data / show "no data" banner.
		return nil, time.Time{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT underlying, expiry_date, strike, option_type, snapshot_time,
		       COALESCE(spot, 0), COALESCE(ltp, 0), COALESCE(bid, 0), COALESCE(ask, 0),
		       COALESCE(oi, 0), COALESCE(oi_change, 0), COALESCE(volume, 0)
		FROM kairos.option_chains
		WHERE underlying = $1 AND expiry_date = $2 AND snapshot_time = $3
		ORDER BY strike ASC, option_type ASC
	`, string(u), expiry, *snapshot)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var out []provider.ChainRow
	for rows.Next() {
		var r provider.ChainRow
		var und, optType string
		if err := rows.Scan(&und, &r.ExpiryDate, &r.Strike, &optType, &r.SnapshotTime,
			&r.Spot, &r.LTP, &r.Bid, &r.Ask, &r.OI, &r.OIChange, &r.Volume); err != nil {
			return nil, time.Time{}, err
		}
		r.Underlying = provider.Underlying(und)
		r.OptionType = provider.OptionType(optType)
		out = append(out, r)
	}
	return out, *snapshot, rows.Err()
}

// ChainAtTime returns the chain rows nearest to a specific time. Used
// by the backtest worker to look up entry/exit prices.
func (s *Store) ChainAtTime(ctx context.Context, u provider.Underlying, expiry, at time.Time) ([]provider.ChainRow, error) {
	rows, err := s.pool.Query(ctx, `
		WITH t AS (
			SELECT snapshot_time
			FROM kairos.option_chains
			WHERE underlying = $1 AND expiry_date = $2 AND snapshot_time <= $3
			ORDER BY snapshot_time DESC
			LIMIT 1
		)
		SELECT underlying, expiry_date, strike, option_type, snapshot_time,
		       COALESCE(spot, 0), COALESCE(ltp, 0), COALESCE(bid, 0), COALESCE(ask, 0),
		       COALESCE(oi, 0), COALESCE(oi_change, 0), COALESCE(volume, 0)
		FROM kairos.option_chains, t
		WHERE underlying = $1 AND expiry_date = $2 AND snapshot_time = t.snapshot_time
		ORDER BY strike ASC, option_type ASC
	`, string(u), expiry, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []provider.ChainRow
	for rows.Next() {
		var r provider.ChainRow
		var und, optType string
		if err := rows.Scan(&und, &r.ExpiryDate, &r.Strike, &optType, &r.SnapshotTime,
			&r.Spot, &r.LTP, &r.Bid, &r.Ask, &r.OI, &r.OIChange, &r.Volume); err != nil {
			return nil, err
		}
		r.Underlying = provider.Underlying(und)
		r.OptionType = provider.OptionType(optType)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountBacktestsLast24h is used for the free-tier rate limit
// (ADR-0010 D7).
func (s *Store) CountBacktestsLast24h(ctx context.Context, uid uuid.UUID) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM kairos.backtests
		WHERE user_id = $1 AND created_at > NOW() - INTERVAL '24 hours'
	`, uid).Scan(&n)
	return n, err
}

// ─────────────────────────────────────────────────────────────────────────
// Partitions — ADR-0009 D4
// ─────────────────────────────────────────────────────────────────────────

// EnsurePartitions creates weekly child partitions covering the next
// `weeks` calendar weeks (including the current week if not present).
// Idempotent — uses CREATE TABLE IF NOT EXISTS PARTITION OF.
//
// Boundaries are TIMESTAMPTZ literals in explicit UTC. Postgres
// partitions snapshot_time, so the FOR VALUES clause has to be a
// timestamp, not a date — the latter would cast through session
// TimeZone and break on a DB with a non-UTC default.
//
// Returns the count of partitions actually created (for log lines).
func (s *Store) EnsurePartitions(ctx context.Context, weeks int) (int, error) {
	if weeks <= 0 {
		weeks = 8
	}
	weekStart := mondayOf(time.Now().UTC())
	created := 0
	for i := 0; i < weeks; i++ {
		start := weekStart.AddDate(0, 0, 7*i)
		end := start.AddDate(0, 0, 7)
		year, week := start.ISOWeek()
		child := fmt.Sprintf("kairos.option_chains_%d_w%02d", year, week)
		// Format as 2026-05-18 00:00:00+00 — explicit UTC offset means
		// session TimeZone doesn't shift the boundary.
		startLit := start.UTC().Format("2006-01-02 15:04:05-07")
		endLit := end.UTC().Format("2006-01-02 15:04:05-07")
		stmt := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF kairos.option_chains FOR VALUES FROM ('%s') TO ('%s')",
			child, startLit, endLit,
		)
		tag, err := s.pool.Exec(ctx, stmt)
		if err != nil {
			return created, fmt.Errorf("create partition %s: %w", child, err)
		}
		// pg returns "CREATE TABLE" for new partitions.
		if strings.Contains(tag.String(), "CREATE") {
			created++
		}
	}
	return created, nil
}

func mondayOf(t time.Time) time.Time {
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	offset := wd - 1
	return time.Date(t.Year(), t.Month(), t.Day()-offset, 0, 0, 0, 0, time.UTC)
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

// rowScanner is a tiny interface satisfied by both pgx.Row and pgx.Rows so
// scanBacktest can serve both QueryRow and Query.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanBacktest(r rowScanner) (*Backtest, error) {
	var (
		bt       Backtest
		legs     []byte
		result   []byte
		strID    *uuid.UUID
		sl       *float64
		tg       *float64
		errMsg   *string
		started  *time.Time
		finished *time.Time
		fromDate time.Time
		toDate   time.Time
	)
	if err := r.Scan(&bt.ID, &bt.UserID, &strID, &bt.Name, &bt.Underlying, &legs,
		&fromDate, &toDate, &bt.EntryTime, &bt.ExitTime, &sl, &tg,
		&bt.Status, &result, &errMsg, &started, &finished, &bt.CreatedAt); err != nil {
		return nil, err
	}
	bt.FromDate = fromDate.Format("2006-01-02")
	bt.ToDate = toDate.Format("2006-01-02")
	if l, err := UnmarshalLegs(legs); err == nil {
		bt.Legs = l
	}
	if len(result) > 0 {
		var r BacktestResult
		if err := json.Unmarshal(result, &r); err == nil {
			bt.Result = &r
		}
	}
	if strID != nil {
		bt.StrategyID = strID
	}
	if sl != nil {
		bt.StopLoss = sl
	}
	if tg != nil {
		bt.Target = tg
	}
	if errMsg != nil {
		bt.Error = *errMsg
	}
	if started != nil {
		bt.StartedAt = started
	}
	if finished != nil {
		bt.FinishedAt = finished
	}
	return &bt, nil
}

func nullF(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullI(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
