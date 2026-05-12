package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbPool is the minimal interface of *pgxpool.Pool that Store uses. The
// concrete type satisfies this automatically; the interface exists so
// unit tests can inject a fake without a real database connection.
type dbPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store is the data-access layer for the billing.* schema + the cached
// `is_premium` and `credits_balance` columns on auth.users. All writes
// go through here; the webhook handler and the checkout handlers don't
// touch SQL directly.
//
// Concurrency note: SpendCredits, RecordCreditPurchase, and the helpers
// they call run inside an explicit BEGIN/COMMIT so the ledger row and
// the cached balance update can't drift.
type Store struct {
	pool dbPool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SubscriptionRow is the persisted shape of billing.subscriptions.
type SubscriptionRow struct {
	ID                       uuid.UUID
	UserID                   uuid.UUID
	Kind                     string // 'recurring' | 'one_time'
	// PlanTier discriminates pricing within `recurring`.
	//   'standard' — ₹99/mo, premium only.
	//   'plus'     — ₹299/mo, premium + 200 credits granted per charge.
	// One-time rows always carry 'standard'.
	PlanTier                 string
	Status                   string // 'pending' | 'active' | 'halted' | 'cancelled' | 'expired'
	RazorpaySubscriptionID   *string
	RazorpayOrderID          *string
	RazorpayPaymentID        *string
	AmountPaise              int
	Currency                 string
	CurrentPeriodEnd         *time.Time
	CancelAtPeriodEnd        bool
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// CreditTransactionRow is the persisted shape of billing.credit_transactions.
type CreditTransactionRow struct {
	ID                  uuid.UUID
	UserID              uuid.UUID
	Delta               int
	Reason              string
	BalanceAfter        int
	RazorpayPaymentID   *string
	RazorpayOrderID     *string
	RefType             *string
	RefID               *uuid.UUID
	CreatedAt           time.Time
}

// ============================================================================
// Subscriptions
// ============================================================================

// CreatePendingRecurring inserts a 'pending' recurring sub immediately
// after we ask Razorpay for a subscription_id. Status flips to 'active'
// when subscription.activated webhook lands.
//
// `planTier` is "standard" (₹99/mo, premium only) or "plus" (₹299/mo,
// premium + bundle credits per charge). Pass an empty string to default
// to "standard" — keeps the call site quiet for the common case.
func (s *Store) CreatePendingRecurring(ctx context.Context, userID uuid.UUID, razorpaySubID string, amountPaise int, planTier string) (uuid.UUID, error) {
	if planTier == "" {
		planTier = "standard"
	}
	const q = `
		INSERT INTO billing.subscriptions
			(user_id, kind, plan_tier, status, razorpay_subscription_id, amount_paise, currency)
		VALUES ($1, 'recurring', $2, 'pending', $3, $4, 'INR')
		RETURNING id
	`
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, q, userID, planTier, razorpaySubID, amountPaise).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("create pending recurring: %w", err)
	}
	return id, nil
}

// CreatePendingOneTime is the same shape for a one-time premium pass.
func (s *Store) CreatePendingOneTime(ctx context.Context, userID uuid.UUID, razorpayOrderID string, amountPaise int) (uuid.UUID, error) {
	const q = `
		INSERT INTO billing.subscriptions
			(user_id, kind, status, razorpay_order_id, amount_paise, currency)
		VALUES ($1, 'one_time', 'pending', $2, $3, 'INR')
		RETURNING id
	`
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, q, userID, razorpayOrderID, amountPaise).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("create pending one_time: %w", err)
	}
	return id, nil
}

// ActivateRecurring is called from subscription.activated. Sets status,
// stamps payment_id, sets current_period_end. Idempotent — calling
// twice with the same values is a no-op.
func (s *Store) ActivateRecurring(ctx context.Context, razorpaySubID, paymentID string, periodEnd time.Time) (uuid.UUID, error) {
	const q = `
		UPDATE billing.subscriptions
		SET status = 'active',
			razorpay_payment_id = $2,
			current_period_end = $3,
			updated_at = NOW()
		WHERE razorpay_subscription_id = $1
		RETURNING user_id
	`
	var uid uuid.UUID
	if err := s.pool.QueryRow(ctx, q, razorpaySubID, paymentID, periodEnd).Scan(&uid); err != nil {
		return uuid.Nil, fmt.Errorf("activate recurring: %w", err)
	}
	return uid, nil
}

// BumpRecurringPeriodEnd is called from subscription.charged (auto-
// renewal succeeded). It pushes current_period_end forward and returns
// the user id + plan_tier so the webhook caller can grant bundle
// credits when the tier is 'plus'.
func (s *Store) BumpRecurringPeriodEnd(ctx context.Context, razorpaySubID string, periodEnd time.Time) (uuid.UUID, string, error) {
	const q = `
		UPDATE billing.subscriptions
		SET current_period_end = $2, updated_at = NOW()
		WHERE razorpay_subscription_id = $1
		RETURNING user_id, plan_tier
	`
	var uid uuid.UUID
	var tier string
	if err := s.pool.QueryRow(ctx, q, razorpaySubID, periodEnd).Scan(&uid, &tier); err != nil {
		return uuid.Nil, "", fmt.Errorf("bump period_end: %w", err)
	}
	return uid, tier, nil
}

// MarkRecurringStatus flips the status field — used by webhook handlers
// for subscription.halted and subscription.cancelled events.
func (s *Store) MarkRecurringStatus(ctx context.Context, razorpaySubID, status string) (uuid.UUID, error) {
	const q = `
		UPDATE billing.subscriptions
		SET status = $2, updated_at = NOW()
		WHERE razorpay_subscription_id = $1
		RETURNING user_id
	`
	var uid uuid.UUID
	if err := s.pool.QueryRow(ctx, q, razorpaySubID, status).Scan(&uid); err != nil {
		return uuid.Nil, fmt.Errorf("mark status: %w", err)
	}
	return uid, nil
}

// MarkCancelAtPeriodEnd is set when the user clicks "Cancel" in /settings.
// We've already called Razorpay's cancel API; this just stamps our
// cached row so the UI reflects it without waiting for the webhook.
func (s *Store) MarkCancelAtPeriodEnd(ctx context.Context, userID, subID uuid.UUID) error {
	const q = `
		UPDATE billing.subscriptions
		SET cancel_at_period_end = TRUE, updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND kind = 'recurring' AND status = 'active'
	`
	tag, err := s.pool.Exec(ctx, q, subID, userID)
	if err != nil {
		return fmt.Errorf("mark cancel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ActivateOneTime is called from payment.captured for one_time premium.
// Sets status, current_period_end = NOW() + 30d (configurable via
// PremiumPassDays).
func (s *Store) ActivateOneTime(ctx context.Context, razorpayOrderID, paymentID string) (uuid.UUID, error) {
	periodEnd := time.Now().UTC().Add(time.Duration(PremiumPassDays) * 24 * time.Hour)
	const q = `
		UPDATE billing.subscriptions
		SET status = 'active',
			razorpay_payment_id = $2,
			current_period_end = $3,
			updated_at = NOW()
		WHERE razorpay_order_id = $1
		RETURNING user_id
	`
	var uid uuid.UUID
	if err := s.pool.QueryRow(ctx, q, razorpayOrderID, paymentID, periodEnd).Scan(&uid); err != nil {
		return uuid.Nil, fmt.Errorf("activate one_time: %w", err)
	}
	return uid, nil
}

// MarkOneTimeStatus flips an order-backed row's status. Used for
// payment.failed (→ expired) and the cron expiry job.
func (s *Store) MarkOneTimeStatus(ctx context.Context, razorpayOrderID, status string) (uuid.UUID, error) {
	const q = `
		UPDATE billing.subscriptions
		SET status = $2, updated_at = NOW()
		WHERE razorpay_order_id = $1
		RETURNING user_id
	`
	var uid uuid.UUID
	if err := s.pool.QueryRow(ctx, q, razorpayOrderID, status).Scan(&uid); err != nil {
		return uuid.Nil, fmt.Errorf("mark one_time status: %w", err)
	}
	return uid, nil
}

// ExpireOldOneTime is the cron-driven sweep. Scans for active one-time
// rows whose period has passed, marks them expired, returns the user_ids
// so the caller can refresh is_premium for each.
func (s *Store) ExpireOldOneTime(ctx context.Context) ([]uuid.UUID, error) {
	const q = `
		UPDATE billing.subscriptions
		SET status = 'expired', updated_at = NOW()
		WHERE kind = 'one_time'
			AND status = 'active'
			AND current_period_end < NOW()
		RETURNING user_id
	`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("expire one_time: %w", err)
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var uid uuid.UUID
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}

// CurrentSubscription returns the user's active sub (any kind) or nil if
// they're on the free tier. Used by GET /billing/me.
func (s *Store) CurrentSubscription(ctx context.Context, userID uuid.UUID) (*SubscriptionRow, error) {
	const q = `
		SELECT id, user_id, kind, plan_tier, status, razorpay_subscription_id, razorpay_order_id,
			razorpay_payment_id, amount_paise, currency, current_period_end,
			cancel_at_period_end, created_at, updated_at
		FROM billing.subscriptions
		WHERE user_id = $1 AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`
	var r SubscriptionRow
	err := s.pool.QueryRow(ctx, q, userID).Scan(
		&r.ID, &r.UserID, &r.Kind, &r.PlanTier, &r.Status,
		&r.RazorpaySubscriptionID, &r.RazorpayOrderID, &r.RazorpayPaymentID,
		&r.AmountPaise, &r.Currency, &r.CurrentPeriodEnd,
		&r.CancelAtPeriodEnd, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("current subscription: %w", err)
	}
	return &r, nil
}

// SubscriptionByID is used by the cancel handler to verify ownership
// before forwarding to Razorpay.
func (s *Store) SubscriptionByID(ctx context.Context, userID, subID uuid.UUID) (*SubscriptionRow, error) {
	const q = `
		SELECT id, user_id, kind, plan_tier, status, razorpay_subscription_id, razorpay_order_id,
			razorpay_payment_id, amount_paise, currency, current_period_end,
			cancel_at_period_end, created_at, updated_at
		FROM billing.subscriptions
		WHERE id = $1 AND user_id = $2
	`
	var r SubscriptionRow
	err := s.pool.QueryRow(ctx, q, subID, userID).Scan(
		&r.ID, &r.UserID, &r.Kind, &r.PlanTier, &r.Status,
		&r.RazorpaySubscriptionID, &r.RazorpayOrderID, &r.RazorpayPaymentID,
		&r.AmountPaise, &r.Currency, &r.CurrentPeriodEnd,
		&r.CancelAtPeriodEnd, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ============================================================================
// Credits
// ============================================================================

// RecordCreditPurchase is the webhook-driven top-up path: payment.captured
// with notes.kind='credits'. Inserts a +delta ledger row AND increments
// the cached balance, all inside one transaction so a crash mid-update
// can't leave the cached balance out of sync with the ledger.
//
// Returns the new balance.
func (s *Store) RecordCreditPurchase(ctx context.Context, userID uuid.UUID, credits int, razorpayOrderID, razorpayPaymentID string) (int, error) {
	if credits <= 0 {
		return 0, fmt.Errorf("credits must be positive, got %d", credits)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Atomic running-total: bump the cached balance and capture the new
	// value in a single round-trip. We pass that into the ledger insert
	// so balance_after stays consistent with the cached value.
	var newBalance int
	if err := tx.QueryRow(ctx, `
		UPDATE auth.users
		SET credits_balance = credits_balance + $2
		WHERE id = $1
		RETURNING credits_balance
	`, userID, credits).Scan(&newBalance); err != nil {
		return 0, fmt.Errorf("update balance: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO billing.credit_transactions
			(user_id, delta, reason, balance_after, razorpay_payment_id, razorpay_order_id)
		VALUES ($1, $2, 'razorpay_purchase', $3, $4, $5)
	`, userID, credits, newBalance, razorpayPaymentID, razorpayOrderID); err != nil {
		return 0, fmt.Errorf("insert ledger: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return newBalance, nil
}

// SpendCredits is the AI-handler entry point. Atomically deducts
// `amount` credits and writes a negative ledger row. Returns the new
// balance, or ErrInsufficientCredits if the user doesn't have enough.
//
// `reason` is the free-text label (e.g. "ai_cover_letter"). refType +
// refID correlate the spend to the thing it produced (an application
// id, a resume id, etc.) for audit. Both can be empty/uuid.Nil.
//
// Phase 6 ships this helper but doesn't yet wire it into AI handlers —
// that's Phase 7, gated on per-feature pricing decisions.
func (s *Store) SpendCredits(ctx context.Context, userID uuid.UUID, amount int, reason string, refType string, refID uuid.UUID) (int, error) {
	if amount <= 0 {
		return 0, fmt.Errorf("spend amount must be positive, got %d", amount)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the user row + check balance + decrement in one round-trip.
	// The CHECK constraint at the SQL level means an insufficient
	// balance bounces with a constraint failure rather than going
	// negative — which is what we want.
	var newBalance int
	err = tx.QueryRow(ctx, `
		UPDATE auth.users
		SET credits_balance = credits_balance - $2
		WHERE id = $1 AND credits_balance >= $2
		RETURNING credits_balance
	`, userID, amount).Scan(&newBalance)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrInsufficientCredits
		}
		return 0, fmt.Errorf("decrement balance: %w", err)
	}

	var refTypePtr any = nil
	var refIDPtr any = nil
	if refType != "" {
		refTypePtr = refType
	}
	if refID != uuid.Nil {
		refIDPtr = refID
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO billing.credit_transactions
			(user_id, delta, reason, balance_after, ref_type, ref_id)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, userID, -amount, reason, newBalance, refTypePtr, refIDPtr); err != nil {
		return 0, fmt.Errorf("insert ledger: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return newBalance, nil
}

// ErrInsufficientCredits — returned by SpendCredits when the user has
// fewer credits than the requested amount. Handlers map to 402.
var ErrInsufficientCredits = errors.New("insufficient credits")

// CreditsBalance reads the cached column directly. Used by GET /billing/me
// and any AI handler that wants to gate before a paid call.
func (s *Store) CreditsBalance(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT credits_balance FROM auth.users WHERE id = $1`, userID).Scan(&n)
	return n, err
}

// RecentActivity merges sub events + credit txns for the /billing/me
// "recent activity" widget. Limit is the row cap (typically 6).
//
// JSON tags are camelCase so the frontend reads `row.summary` etc.
// — without them Go's encoding/json defaults to PascalCase field
// names and the UI shows blanks.
type ActivityItem struct {
	When    time.Time `json:"when"`
	Kind    string    `json:"kind"`    // 'subscription' | 'credits'
	Summary string    `json:"summary"` // human-readable
	Amount  int       `json:"amount"`  // paise (positive for charges, negative for credit spends)
}

func (s *Store) RecentActivity(ctx context.Context, userID uuid.UUID, limit int) ([]ActivityItem, error) {
	if limit <= 0 {
		limit = 6
	}
	const q = `
		SELECT created_at, 'credits' AS kind,
			CASE WHEN delta > 0 THEN 'Credits purchased' ELSE 'Credits used' END
			|| ' (' || delta || ')' AS summary,
			delta * 100 AS amount_paise_signed
		FROM billing.credit_transactions
		WHERE user_id = $1
		UNION ALL
		SELECT created_at, 'subscription' AS kind,
			CASE WHEN kind = 'recurring' THEN 'Premium · monthly' ELSE 'Premium · 30-day pass' END
			|| ' — ' || status AS summary,
			amount_paise
		FROM billing.subscriptions
		WHERE user_id = $1 AND status IN ('active','expired','cancelled')
		ORDER BY 1 DESC
		LIMIT $2
	`
	rows, err := s.pool.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent activity: %w", err)
	}
	defer rows.Close()
	out := []ActivityItem{}
	for rows.Next() {
		var it ActivityItem
		if err := rows.Scan(&it.When, &it.Kind, &it.Summary, &it.Amount); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ============================================================================
// Webhook idempotency
// ============================================================================

// RecordEvent inserts the event_id into processed_events. Returns true
// if this is a fresh event (we should process it), false if Razorpay is
// retrying an event we've already handled.
func (s *Store) RecordEvent(ctx context.Context, eventID string) (bool, error) {
	if eventID == "" {
		// No event id means we can't dedupe — process every time. This
		// shouldn't happen with real Razorpay payloads but the fallback
		// is safer than refusing to process.
		return true, nil
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO billing.processed_events (event_id) VALUES ($1) ON CONFLICT DO NOTHING`,
		eventID)
	if err != nil {
		return false, fmt.Errorf("record event: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ============================================================================
// is_premium cache refresh
// ============================================================================

// RefreshUserPremium recomputes auth.users.is_premium from the current
// active subscription state and writes it. Called from every webhook
// branch and the cron expiry job — the is_premium column is purely a
// cached derivation of billing.subscriptions.status.
//
// Logic: if there's any active subscription whose current_period_end is
// in the future (or NULL for a freshly-activated recurring), is_premium
// becomes true. Otherwise false.
func (s *Store) RefreshUserPremium(ctx context.Context, userID uuid.UUID) error {
	const q = `
		UPDATE auth.users
		SET is_premium = EXISTS (
			SELECT 1 FROM billing.subscriptions
			WHERE user_id = $1
			  AND status = 'active'
			  AND (current_period_end IS NULL OR current_period_end > NOW())
		)
		WHERE id = $1
	`
	_, err := s.pool.Exec(ctx, q, userID)
	if err != nil {
		return fmt.Errorf("refresh is_premium: %w", err)
	}
	return nil
}
