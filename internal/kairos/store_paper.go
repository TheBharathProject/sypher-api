package kairos

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Store methods for the paper trading engine (spec §3.5) —
// kairos.paper_accounts / paper_orders / paper_positions (migration
// 0027). Fill writes are transactional: FillPaperOrderTx commits order
// + position + cash atomically.

// defaultPaperStartingCapital mirrors the starting_capital column
// default in migration 0027 (₹10,00,000). The INSERT must supply cash
// explicitly (NOT NULL, no default), so the number lives here too.
const defaultPaperStartingCapital = 1000000.00

// PaperAccount is one row of kairos.paper_accounts.
type PaperAccount struct {
	UserID          uuid.UUID
	StartingCapital float64
	Cash            float64
	CreatedAt       time.Time
	ResetAt         *time.Time
}

// ContractQuote is the price snapshot of one option contract — the
// latest chain row for (underlying, expiry, strike, optType).
type ContractQuote struct {
	Bid          float64
	Ask          float64
	LTP          float64
	SnapshotTime time.Time
}

// paperOrderCols is the SELECT list every paper-order scan shares.
const paperOrderCols = `
	id, user_id, kind, symbol,
	COALESCE(underlying, ''), expiry, COALESCE(strike, 0), COALESCE(opt_type, ''),
	side, order_type, qty, limit_price,
	status, COALESCE(reject_reason, ''), fill_price, fees, placed_at, filled_at`

// GetOrCreatePaperAccount auto-provisions the user's virtual cash
// account on first touch (spec §3.5) and returns it.
func (s *Store) GetOrCreatePaperAccount(ctx context.Context, uid uuid.UUID) (*PaperAccount, error) {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO kairos.paper_accounts (user_id, cash)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO NOTHING
	`, uid, defaultPaperStartingCapital)
	if err != nil {
		return nil, err
	}
	acct := &PaperAccount{UserID: uid}
	err = s.pool.QueryRow(ctx, `
		SELECT starting_capital, cash, created_at, reset_at
		FROM kairos.paper_accounts
		WHERE user_id = $1
	`, uid).Scan(&acct.StartingCapital, &acct.Cash, &acct.CreatedAt, &acct.ResetAt)
	if err != nil {
		return nil, err
	}
	return acct, nil
}

// ResetPaperAccount wipes the user's paper history — all orders and
// positions — and restores cash to starting capital, stamping reset_at.
// One tx so a half-reset can't leave positions without funding history.
// Self-provisioning: resetting a never-touched account creates it.
func (s *Store) ResetPaperAccount(ctx context.Context, uid uuid.UUID) (*PaperAccount, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM kairos.paper_orders WHERE user_id = $1`, uid); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM kairos.paper_positions WHERE user_id = $1`, uid); err != nil {
		return nil, err
	}
	acct := &PaperAccount{UserID: uid}
	err = tx.QueryRow(ctx, `
		INSERT INTO kairos.paper_accounts (user_id, cash, reset_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE
		SET cash = kairos.paper_accounts.starting_capital, reset_at = NOW()
		RETURNING starting_capital, cash, created_at, reset_at
	`, uid, defaultPaperStartingCapital).Scan(&acct.StartingCapital, &acct.Cash, &acct.CreatedAt, &acct.ResetAt)
	if err != nil {
		return nil, err
	}
	return acct, tx.Commit(ctx)
}

// InsertPaperOrder persists a non-filling order row — OPEN limit
// orders and REJECTED market orders. Populates ID and PlacedAt.
func (s *Store) InsertPaperOrder(ctx context.Context, ord *PaperOrder) error {
	return s.pool.QueryRow(ctx, `
		INSERT INTO kairos.paper_orders
			(user_id, kind, symbol, underlying, expiry, strike, opt_type,
			 side, order_type, qty, limit_price, status, reject_reason)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, 0), NULLIF($7, ''),
		        $8, $9, $10, $11, $12, NULLIF($13, ''))
		RETURNING id, placed_at
	`, ord.UserID, ord.Kind, ord.Symbol, ord.Underlying, nullDate(ord.Expiry), ord.Strike, ord.OptType,
		ord.Side, ord.OrderType, ord.Qty, ord.LimitPrice, ord.Status, ord.RejectReason).
		Scan(&ord.ID, &ord.PlacedAt)
}

// FillPaperOrderTx executes one fill atomically: order row, position
// upsert (avg-price method via ApplyFill) and the cash leg commit or
// roll back together.
//
//   - ord.ID == uuid.Nil → a fresh MARKET order; the row is INSERTed
//     directly as FILLED.
//   - ord.ID set → a resting LIMIT order; the row is UPDATEd
//     WHERE status='OPEN', so a concurrent cancel wins cleanly
//     (pgx.ErrNoRows comes back and nothing is written).
//
// Returns errInsufficientFunds (tx rolled back, nothing written) when
// the account can't fund the fill: BUY needs cash ≥ notional + fees;
// SELL needs cash ≥ fees + 15% of notional (margin proxy, see paper.go)
// and then credits notional − fees.
//
// The account row is locked FOR UPDATE first, which serialises all of
// a user's concurrent fills (and the position row they touch).
func (s *Store) FillPaperOrderTx(ctx context.Context, ord *PaperOrder, fillPrice, fees float64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var cash float64
	if err := tx.QueryRow(ctx, `
		SELECT cash FROM kairos.paper_accounts WHERE user_id = $1 FOR UPDATE
	`, ord.UserID).Scan(&cash); err != nil {
		return err // ErrNoRows → account vanished; handlers GetOrCreate first
	}

	notional := fillPrice * float64(ord.Qty)
	var newCash float64
	if ord.Side == "BUY" {
		newCash = cash - notional - fees
		if newCash < 0 {
			return errInsufficientFunds
		}
	} else {
		if cash < fees+paperMarginPct*notional {
			return errInsufficientFunds
		}
		newCash = cash + notional - fees
	}

	now := time.Now().UTC()
	if ord.ID == uuid.Nil {
		err = tx.QueryRow(ctx, `
			INSERT INTO kairos.paper_orders
				(user_id, kind, symbol, underlying, expiry, strike, opt_type,
				 side, order_type, qty, limit_price, status, fill_price, fees, filled_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, 0), NULLIF($7, ''),
			        $8, $9, $10, $11, 'FILLED', $12, $13, $14)
			RETURNING id, placed_at
		`, ord.UserID, ord.Kind, ord.Symbol, ord.Underlying, nullDate(ord.Expiry), ord.Strike, ord.OptType,
			ord.Side, ord.OrderType, ord.Qty, ord.LimitPrice, fillPrice, fees, now).
			Scan(&ord.ID, &ord.PlacedAt)
		if err != nil {
			return err
		}
	} else {
		tag, err := tx.Exec(ctx, `
			UPDATE kairos.paper_orders
			SET status = 'FILLED', fill_price = $1, fees = $2, filled_at = $3
			WHERE id = $4 AND status = 'OPEN'
		`, fillPrice, fees, now, ord.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows // cancelled/filled by someone else meanwhile
		}
	}

	// Position upsert. The account lock above serialises this per user,
	// so read-modify-write is safe.
	pos := PaperPosition{
		UserID:     ord.UserID,
		Kind:       ord.Kind,
		Symbol:     ord.Symbol,
		Underlying: ord.Underlying,
		Expiry:     ord.Expiry,
		Strike:     ord.Strike,
		OptType:    ord.OptType,
	}
	var expiry *time.Time
	err = tx.QueryRow(ctx, `
		SELECT qty, avg_price, realized_pnl
		FROM kairos.paper_positions
		WHERE user_id = $1 AND symbol = $2
		FOR UPDATE
	`, ord.UserID, ord.Symbol).Scan(&pos.Qty, &pos.AvgPrice, &pos.RealizedPnL)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	pos = ApplyFill(pos, ord.Side, ord.Qty, fillPrice)
	if ord.Expiry != "" {
		if t, perr := time.Parse("2006-01-02", ord.Expiry); perr == nil {
			expiry = &t
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO kairos.paper_positions
			(user_id, symbol, kind, underlying, expiry, strike, opt_type,
			 qty, avg_price, realized_pnl, updated_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, 0), NULLIF($7, ''),
		        $8, $9, $10, NOW())
		ON CONFLICT (user_id, symbol) DO UPDATE
		SET qty = EXCLUDED.qty,
		    avg_price = EXCLUDED.avg_price,
		    realized_pnl = EXCLUDED.realized_pnl,
		    updated_at = NOW()
	`, ord.UserID, ord.Symbol, ord.Kind, ord.Underlying, expiry, ord.Strike, ord.OptType,
		pos.Qty, round2(pos.AvgPrice), round2(pos.RealizedPnL))
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE kairos.paper_accounts SET cash = $1 WHERE user_id = $2
	`, round2(newCash), ord.UserID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	ord.Status = "FILLED"
	ord.FillPrice = &fillPrice
	ord.Fees = &fees
	ord.FilledAt = &now
	return nil
}

// RejectPaperOrder flips a still-OPEN order to REJECTED with a reason
// (the sweep's insufficient-funds path). pgx.ErrNoRows when the order
// is no longer OPEN.
func (s *Store) RejectPaperOrder(ctx context.Context, id uuid.UUID, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE kairos.paper_orders
		SET status = 'REJECTED', reject_reason = $1
		WHERE id = $2 AND status = 'OPEN'
	`, reason, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ListPaperOrders returns the user's most recent 200 orders, newest
// first.
func (s *Store) ListPaperOrders(ctx context.Context, uid uuid.UUID) ([]PaperOrder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+paperOrderCols+`
		FROM kairos.paper_orders
		WHERE user_id = $1
		ORDER BY placed_at DESC
		LIMIT 200
	`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPaperOrders(rows)
}

// CancelPaperOrder cancels a still-OPEN order owned by the user.
// pgx.ErrNoRows when it doesn't exist, isn't theirs, or already left
// the OPEN state.
func (s *Store) CancelPaperOrder(ctx context.Context, uid, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE kairos.paper_orders
		SET status = 'CANCELLED'
		WHERE id = $1 AND user_id = $2 AND status = 'OPEN'
	`, id, uid)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// OpenLimitOrders returns OPEN orders across ALL users, oldest first —
// the 60s sweep's worklist. Capped at 500 per tick; a deeper book
// drains over successive ticks.
func (s *Store) OpenLimitOrders(ctx context.Context) ([]PaperOrder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+paperOrderCols+`
		FROM kairos.paper_orders
		WHERE status = 'OPEN'
		ORDER BY placed_at ASC
		LIMIT 500
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPaperOrders(rows)
}

// ListPaperPositions returns all the user's positions (including
// closed, qty=0 rows — they carry realized P&L), most recently touched
// first.
func (s *Store) ListPaperPositions(ctx context.Context, uid uuid.UUID) ([]PaperPosition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, kind, symbol,
		       COALESCE(underlying, ''), expiry, COALESCE(strike, 0), COALESCE(opt_type, ''),
		       qty, avg_price, realized_pnl, updated_at
		FROM kairos.paper_positions
		WHERE user_id = $1
		ORDER BY updated_at DESC
	`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PaperPosition{}
	for rows.Next() {
		p, err := scanPaperPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetPaperPosition returns one position by id, scoped to the user.
func (s *Store) GetPaperPosition(ctx context.Context, uid, id uuid.UUID) (*PaperPosition, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, user_id, kind, symbol,
		       COALESCE(underlying, ''), expiry, COALESCE(strike, 0), COALESCE(opt_type, ''),
		       qty, avg_price, realized_pnl, updated_at
		FROM kairos.paper_positions
		WHERE id = $1 AND user_id = $2
	`, id, uid)
	return scanPaperPosition(row)
}

// LatestContractQuote returns the most recent chain row for one option
// contract at or after `since`. The explicit lower bound keeps the
// query inside a few weekly partitions (the chain table has no
// snapshot_date column — see utcDayBounds). pgx.ErrNoRows when no
// fresh-enough row exists.
func (s *Store) LatestContractQuote(ctx context.Context, underlying string, expiry time.Time, strike int, optType string, since time.Time) (*ContractQuote, error) {
	q := &ContractQuote{}
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(bid, 0), COALESCE(ask, 0), COALESCE(ltp, 0), snapshot_time
		FROM kairos.option_chains
		WHERE underlying = $1 AND expiry_date = $2 AND strike = $3 AND option_type = $4
		  AND snapshot_time >= $5
		ORDER BY snapshot_time DESC
		LIMIT 1
	`, underlying, expiry, strike, optType, since).Scan(&q.Bid, &q.Ask, &q.LTP, &q.SnapshotTime)
	if err != nil {
		return nil, err
	}
	return q, nil
}

// ─────────────────────────────────────────────────────────────────────────
// scan helpers
// ─────────────────────────────────────────────────────────────────────────

func scanPaperOrders(rows pgx.Rows) ([]PaperOrder, error) {
	out := []PaperOrder{}
	for rows.Next() {
		var (
			o      PaperOrder
			expiry *time.Time
		)
		if err := rows.Scan(&o.ID, &o.UserID, &o.Kind, &o.Symbol,
			&o.Underlying, &expiry, &o.Strike, &o.OptType,
			&o.Side, &o.OrderType, &o.Qty, &o.LimitPrice,
			&o.Status, &o.RejectReason, &o.FillPrice, &o.Fees, &o.PlacedAt, &o.FilledAt); err != nil {
			return nil, err
		}
		if expiry != nil {
			o.Expiry = expiry.Format("2006-01-02")
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func scanPaperPosition(r rowScanner) (*PaperPosition, error) {
	var (
		p         PaperPosition
		expiry    *time.Time
		updatedAt time.Time
	)
	if err := r.Scan(&p.ID, &p.UserID, &p.Kind, &p.Symbol,
		&p.Underlying, &expiry, &p.Strike, &p.OptType,
		&p.Qty, &p.AvgPrice, &p.RealizedPnL, &updatedAt); err != nil {
		return nil, err
	}
	if expiry != nil {
		p.Expiry = expiry.Format("2006-01-02")
	}
	p.UpdatedAt = &updatedAt
	return &p, nil
}

// nullDate maps "" → NULL and "YYYY-MM-DD" → a DATE param.
func nullDate(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// quoteForPosition resolves a best-effort mark for one position: OPT
// via the latest chain row inside markLookback (LTP, falling back to
// the bid/ask mid). EQ marks come from the batched quote call in
// handlers_paper.go, not here.
func (s *Store) quoteForPosition(ctx context.Context, p *PaperPosition) (float64, bool) {
	if p.Kind != "OPT" || p.Expiry == "" {
		return 0, false
	}
	expiry, err := time.Parse("2006-01-02", p.Expiry)
	if err != nil {
		return 0, false
	}
	q, err := s.LatestContractQuote(ctx, p.Underlying, expiry, p.Strike, p.OptType, time.Now().Add(-markLookback))
	if err != nil {
		return 0, false
	}
	if q.LTP > 0 {
		return q.LTP, true
	}
	if q.Bid > 0 && q.Ask > 0 {
		return (q.Bid + q.Ask) / 2, true
	}
	return 0, false
}
