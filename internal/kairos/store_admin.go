package kairos

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// Admin-facing store methods (spec §3.7) plus the backtest gating count.
// JSON tags on the wire types are camelCase and must match the admin
// functions in kairos/lib/kairos-api.ts exactly — the FE client file is
// the contract.

// ─────────────────────────────────────────────────────────────────────────
// Backtest gating
// ─────────────────────────────────────────────────────────────────────────

// CountBacktestsSince counts the user's backtests created after `since`.
// SubmitBacktest uses it for the free-tier 5-per-24h gate (spec §3.6).
func (s *Store) CountBacktestsSince(ctx context.Context, uid uuid.UUID, since time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM kairos.backtests
		WHERE user_id = $1 AND created_at > $2
	`, uid, since).Scan(&n)
	return n, err
}

// ─────────────────────────────────────────────────────────────────────────
// Coverage — the admin heatmap + day drilldown
// ─────────────────────────────────────────────────────────────────────────

// CoverageDayCount is one heatmap cell: how many snapshot ticks and raw
// rows exist for an underlying on one UTC day. `rows` is extra context
// the FE type doesn't declare — harmless on the wire.
type CoverageDayCount struct {
	Date      string `json:"date"` // YYYY-MM-DD (UTC day)
	Snapshots int64  `json:"snapshots"`
	Rows      int64  `json:"rows"`
}

// Coverage returns per-UTC-day snapshot counts for an underlying in
// [from, to) (explicit timestamptz bounds — there is NO snapshot_date
// column; see utcDayBounds). Only days with data come back; the handler
// zero-fills the gaps. Grouping converts to UTC explicitly so the result
// is independent of the session TimeZone, while the outer range filter
// keeps partition pruning.
func (s *Store) Coverage(ctx context.Context, u provider.Underlying, from, to time.Time) ([]CoverageDayCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT (snapshot_time AT TIME ZONE 'UTC')::date AS day,
		       COUNT(DISTINCT snapshot_time),
		       COUNT(*)
		FROM kairos.option_chains
		WHERE underlying = $1 AND snapshot_time >= $2 AND snapshot_time < $3
		GROUP BY 1
		ORDER BY 1
	`, string(u), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CoverageDayCount
	for rows.Next() {
		var (
			day time.Time
			c   CoverageDayCount
		)
		if err := rows.Scan(&day, &c.Snapshots, &c.Rows); err != nil {
			return nil, err
		}
		c.Date = day.Format("2006-01-02")
		out = append(out, c)
	}
	return out, rows.Err()
}

// CoverageExpiry is one row of the day drilldown: per-expiry strike and
// snapshot counts on a single day.
type CoverageExpiry struct {
	Expiry        string `json:"expiry"` // YYYY-MM-DD
	StrikeCount   int64  `json:"strikeCount"`
	SnapshotCount int64  `json:"snapshotCount"`
}

// CoverageDay returns the per-expiry breakdown plus the snapshot times
// for an underlying on one UTC day.
func (s *Store) CoverageDay(ctx context.Context, u provider.Underlying, date time.Time) ([]CoverageExpiry, []time.Time, error) {
	dayStart, dayEnd := utcDayBounds(date)
	rows, err := s.pool.Query(ctx, `
		SELECT expiry_date,
		       COUNT(DISTINCT strike),
		       COUNT(DISTINCT snapshot_time)
		FROM kairos.option_chains
		WHERE underlying = $1 AND snapshot_time >= $2 AND snapshot_time < $3
		GROUP BY expiry_date
		ORDER BY expiry_date
	`, string(u), dayStart, dayEnd)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var expiries []CoverageExpiry
	for rows.Next() {
		var (
			exp time.Time
			e   CoverageExpiry
		)
		if err := rows.Scan(&exp, &e.StrikeCount, &e.SnapshotCount); err != nil {
			return nil, nil, err
		}
		e.Expiry = exp.Format("2006-01-02")
		expiries = append(expiries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	times, err := s.SnapshotTimes(ctx, u, date)
	if err != nil {
		return nil, nil, err
	}
	return expiries, times, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Health inputs — snapshot ages, cron statuses, sizes
// ─────────────────────────────────────────────────────────────────────────

// LastSnapshotTimes returns the most recent snapshot_time per
// underlying. Underlyings with no data are absent from the map.
func (s *Store) LastSnapshotTimes(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT underlying, MAX(snapshot_time)
		FROM kairos.option_chains
		GROUP BY underlying
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]time.Time)
	for rows.Next() {
		var (
			u string
			t time.Time
		)
		if err := rows.Scan(&u, &t); err != nil {
			return nil, err
		}
		out[u] = t
	}
	return out, rows.Err()
}

// CronRunStatus is the latest ingest_runs row per job — the health
// page's cron cards.
type CronRunStatus struct {
	Job       string     `json:"job"`
	LastRunAt *time.Time `json:"lastRunAt,omitempty"`
	OK        *bool      `json:"ok,omitempty"`
}

// LastIngestRunPerJob returns the newest ingest_runs row for each
// distinct job, alphabetical by job name.
func (s *Store) LastIngestRunPerJob(ctx context.Context) ([]CronRunStatus, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (job) job, started_at, ok
		FROM kairos.ingest_runs
		ORDER BY job, started_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CronRunStatus
	for rows.Next() {
		var (
			c CronRunStatus
			t time.Time
		)
		if err := rows.Scan(&c.Job, &t, &c.OK); err != nil {
			return nil, err
		}
		c.LastRunAt = &t
		out = append(out, c)
	}
	return out, rows.Err()
}

// DatabaseSize returns pg_database_size of the connected database.
func (s *Store) DatabaseSize(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&n)
	return n, err
}

// ─────────────────────────────────────────────────────────────────────────
// Partitions
// ─────────────────────────────────────────────────────────────────────────

// Partition mirrors ApiPartition in kairos-api.ts.
type Partition struct {
	Name        string `json:"name"`
	Range       string `json:"range"`
	SizeBytes   int64  `json:"sizeBytes"`
	RowEstimate int64  `json:"rowEstimate"`
}

// PartitionInfo lists the child partitions of kairos.option_chains
// (named kairos.option_chains_<isoyear>_w<week>, per migration 0025 and
// EnsurePartitions) via pg_inherits/pg_class. RowEstimate comes from
// reltuples, which is -1 until the first ANALYZE/VACUUM — clamped to 0.
func (s *Store) PartitionInfo(ctx context.Context) ([]Partition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname,
		       COALESCE(pg_get_expr(c.relpartbound, c.oid), ''),
		       pg_total_relation_size(c.oid),
		       GREATEST(c.reltuples::BIGINT, 0)
		FROM pg_inherits i
		JOIN pg_class c      ON c.oid = i.inhrelid
		JOIN pg_class parent ON parent.oid = i.inhparent
		JOIN pg_namespace n  ON n.oid = parent.relnamespace
		WHERE n.nspname = 'kairos' AND parent.relname = 'option_chains'
		ORDER BY c.relname
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Partition
	for rows.Next() {
		var p Partition
		if err := rows.Scan(&p.Name, &p.Range, &p.SizeBytes, &p.RowEstimate); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────
// Backtests (admin view)
// ─────────────────────────────────────────────────────────────────────────

// AdminBacktest is a Backtest plus the owner's identity. The embedded
// Backtest's UserID is json:"-", so the explicit fields below carry it
// on the wire (ApiAdminBacktest = ApiBacktest & {userId?, userEmail?}).
type AdminBacktest struct {
	Backtest
	BacktestUserID uuid.UUID `json:"userId"`
	UserEmail      string    `json:"userEmail"`
}

// AdminListBacktests returns recent backtests across all users, newest
// first, optionally filtered by status. status == "" lists all.
func (s *Store) AdminListBacktests(ctx context.Context, status string, limit int) ([]AdminBacktest, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.user_id, b.strategy_id, b.name, b.underlying, b.legs,
		       b.from_date, b.to_date, b.entry_time, b.exit_time, b.stop_loss, b.target,
		       b.status, b.result, b.error, b.started_at, b.finished_at, b.created_at,
		       u.email
		FROM kairos.backtests b
		JOIN auth.users u ON u.id = b.user_id
		WHERE ($1 = '' OR b.status = $1)
		ORDER BY b.created_at DESC
		LIMIT $2
	`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminBacktest
	for rows.Next() {
		var (
			ab       AdminBacktest
			legs     []byte
			result   []byte
			strID    *uuid.UUID
			sl, tg   *float64
			errMsg   *string
			started  *time.Time
			finished *time.Time
			fromDate time.Time
			toDate   time.Time
		)
		if err := rows.Scan(&ab.ID, &ab.UserID, &strID, &ab.Name, &ab.Underlying, &legs,
			&fromDate, &toDate, &ab.EntryTime, &ab.ExitTime, &sl, &tg,
			&ab.Status, &result, &errMsg, &started, &finished, &ab.CreatedAt,
			&ab.UserEmail); err != nil {
			return nil, err
		}
		ab.BacktestUserID = ab.UserID
		ab.FromDate = fromDate.Format("2006-01-02")
		ab.ToDate = toDate.Format("2006-01-02")
		if l, err := UnmarshalLegs(legs); err == nil {
			ab.Legs = l
		}
		if len(result) > 0 {
			var r BacktestResult
			if err := json.Unmarshal(result, &r); err == nil {
				ab.Result = &r
			}
		}
		ab.StrategyID = strID
		ab.StopLoss = sl
		ab.Target = tg
		if errMsg != nil {
			ab.Error = *errMsg
		}
		ab.StartedAt = started
		ab.FinishedAt = finished
		out = append(out, ab)
	}
	return out, rows.Err()
}

// RetryBacktest flips a failed backtest back to pending, clearing the
// old error/result/timing so the worker reruns it from scratch. Returns
// pgx.ErrNoRows when the row doesn't exist or isn't failed.
func (s *Store) RetryBacktest(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE kairos.backtests
		SET status = 'pending', error = NULL, result = NULL,
		    started_at = NULL, finished_at = NULL
		WHERE id = $1 AND status = 'failed'
	`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// Users (admin view) — keyset pagination, jobtracker pattern
// ─────────────────────────────────────────────────────────────────────────

// AdminUser mirrors ApiAdminUser in kairos-api.ts.
type AdminUser struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	IsPremium bool      `json:"isPremium"`
	IsAdmin   bool      `json:"isAdmin"`
	CreatedAt time.Time `json:"createdAt"`
}

// errBadCursor marks a malformed pagination cursor so the handler can
// answer 400 instead of 500.
var errBadCursor = errors.New("bad cursor")

// userCursor is a (created_at, id) keyset cursor — copied from
// jobtracker's AppCursor for stable, gap-free pagination.
type userCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// encodeUserCursor base64-encodes "created_at|id" as an opaque cursor.
func encodeUserCursor(createdAt time.Time, id uuid.UUID) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeUserCursor reverses encodeUserCursor.
func decodeUserCursor(encoded string) (*userCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("bad cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, fmt.Errorf("bad cursor time: %w", err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return nil, fmt.Errorf("bad cursor id: %w", err)
	}
	return &userCursor{CreatedAt: t, ID: id}, nil
}

// AdminListUsers pages through auth.users newest-first, optionally
// filtered by a case-insensitive email/name substring. cursor is the
// opaque value from the previous page ("" = first page); nextCursor is
// "" on the last page. Returns an error on a malformed cursor — the
// handler maps it to 400.
func (s *Store) AdminListUsers(ctx context.Context, q, cursor string, limit int) ([]AdminUser, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	sql := `
		SELECT id, email, name, is_premium, is_admin, created_at
		FROM auth.users
		WHERE ($1 = '' OR email ILIKE '%' || $1 || '%' OR name ILIKE '%' || $1 || '%')`
	args := []any{q}
	if cursor != "" {
		cur, err := decodeUserCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", errBadCursor, err)
		}
		args = append(args, cur.CreatedAt, cur.ID)
		sql += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	sql += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := make([]AdminUser, 0, limit)
	for rows.Next() {
		var u AdminUser
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.IsPremium, &u.IsAdmin, &u.CreatedAt); err != nil {
			return nil, "", err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = encodeUserCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// AdminGetUser returns one user row in admin shape (auth.Store has no
// created_at in its projection, so this reads the table directly).
func (s *Store) AdminGetUser(ctx context.Context, id uuid.UUID) (*AdminUser, error) {
	var u AdminUser
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, name, is_premium, is_admin, created_at
		FROM auth.users
		WHERE id = $1
	`, id).Scan(&u.ID, &u.Email, &u.Name, &u.IsPremium, &u.IsAdmin, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Fundamentals upload
// ─────────────────────────────────────────────────────────────────────────

// FundamentalUpsert is one parsed CSV row of the admin fundamentals
// upload. Numeric pointers are nil when the CSV cell was empty (→ NULL).
type FundamentalUpsert struct {
	Symbol      string
	Name        string
	Sector      string
	MktCapCr    *float64
	PE          *float64
	ROCE        *float64
	ROE         *float64
	DE          *float64
	NPY1        *float64
	NPY2        *float64
	OPM         *float64
	CAGR3Profit *float64
	CAGR3Sales  *float64
}

// AdminUpsertFundamental upserts one kairos.equity_fundamentals row by
// symbol, stamping updated_at.
func (s *Store) AdminUpsertFundamental(ctx context.Context, f FundamentalUpsert) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO kairos.equity_fundamentals
			(symbol, name, sector, mkt_cap_cr, pe, roce, roe, de,
			 np_y1, np_y2, opm, cagr3_profit, cagr3_sales, updated_at)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NOW())
		ON CONFLICT (symbol) DO UPDATE SET
			name = EXCLUDED.name,
			sector = EXCLUDED.sector,
			mkt_cap_cr = EXCLUDED.mkt_cap_cr,
			pe = EXCLUDED.pe,
			roce = EXCLUDED.roce,
			roe = EXCLUDED.roe,
			de = EXCLUDED.de,
			np_y1 = EXCLUDED.np_y1,
			np_y2 = EXCLUDED.np_y2,
			opm = EXCLUDED.opm,
			cagr3_profit = EXCLUDED.cagr3_profit,
			cagr3_sales = EXCLUDED.cagr3_sales,
			updated_at = NOW()
	`, f.Symbol, f.Name, f.Sector, f.MktCapCr, f.PE, f.ROCE, f.ROE, f.DE,
		f.NPY1, f.NPY2, f.OPM, f.CAGR3Profit, f.CAGR3Sales)
	return err
}

// ─────────────────────────────────────────────────────────────────────────
// Chain export
// ─────────────────────────────────────────────────────────────────────────

// ChainRowSource adapts pgx.Rows to the export RowSource. Callers must
// Close it (also safe after exhaustion).
type ChainRowSource struct {
	rows pgx.Rows
}

func (c *ChainRowSource) Next() (ExportRow, bool, error) {
	if !c.rows.Next() {
		return ExportRow{}, false, c.rows.Err()
	}
	var r ExportRow
	if err := c.rows.Scan(&r.Underlying, &r.ExpiryDate, &r.Strike, &r.OptionType, &r.SnapshotTime,
		&r.Spot, &r.LTP, &r.Bid, &r.Ask, &r.OI, &r.OIChange, &r.Volume); err != nil {
		return ExportRow{}, false, err
	}
	return r, true, nil
}

func (c *ChainRowSource) Close() { c.rows.Close() }

// ChainExportRows opens a streaming cursor over option_chains rows for
// an underlying in [from, to) (explicit timestamptz bounds — partition
// pruning applies), optionally filtered to a single expiry. Rows come
// back in (snapshot_time, expiry, strike, side) order, ready for CSV.
func (s *Store) ChainExportRows(ctx context.Context, u provider.Underlying, from, to time.Time, expiry *time.Time) (*ChainRowSource, error) {
	sql := `
		SELECT underlying, expiry_date, strike, option_type, snapshot_time,
		       COALESCE(spot, 0), COALESCE(ltp, 0), COALESCE(bid, 0), COALESCE(ask, 0),
		       COALESCE(oi, 0), COALESCE(oi_change, 0), COALESCE(volume, 0)
		FROM kairos.option_chains
		WHERE underlying = $1 AND snapshot_time >= $2 AND snapshot_time < $3`
	args := []any{string(u), from, to}
	if expiry != nil {
		args = append(args, *expiry)
		sql += fmt.Sprintf(" AND expiry_date = $%d", len(args))
	}
	sql += " ORDER BY snapshot_time ASC, expiry_date ASC, strike ASC, option_type ASC"
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &ChainRowSource{rows: rows}, nil
}
