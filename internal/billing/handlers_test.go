package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TheBharathProject/sypher-api/internal/auth"
)

// ---------------------------------------------------------------------------
// Fake pgx plumbing — minimal stubs so Store methods work without a real DB
// ---------------------------------------------------------------------------

// fakeRow implements pgx.Row by returning a preset list of values.
type fakeRow struct {
	vals []any
	err  error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if i >= len(r.vals) {
			break
		}
		switch v := d.(type) {
		case *uuid.UUID:
			if u, ok := r.vals[i].(uuid.UUID); ok {
				*v = u
			}
		case *string:
			if s, ok := r.vals[i].(string); ok {
				*v = s
			}
		case **string:
			if s, ok := r.vals[i].(string); ok {
				*v = &s
			} else {
				*v = nil
			}
		case *bool:
			if b, ok := r.vals[i].(bool); ok {
				*v = b
			}
		case *int:
			if n, ok := r.vals[i].(int); ok {
				*v = n
			}
		case **time.Time:
			if t, ok := r.vals[i].(time.Time); ok {
				*v = &t
			} else {
				*v = nil
			}
		case *time.Time:
			if t, ok := r.vals[i].(time.Time); ok {
				*v = t
			}
		}
	}
	return nil
}

// fakeRows implements pgx.Rows by returning zero rows — enough for
// CurrentSubscription (which does QueryRow) to work via fakeRow.
type fakeRows struct{}

func (fakeRows) Close()                               {}
func (fakeRows) Err() error                           { return nil }
func (fakeRows) CommandTag() pgconn.CommandTag        { return pgconn.CommandTag{} }
func (fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (fakeRows) Next() bool                           { return false }
func (fakeRows) Scan(dest ...any) error               { return nil }
func (fakeRows) Values() ([]any, error)               { return nil, nil }
func (fakeRows) RawValues() [][]byte                  { return nil }
func (fakeRows) Conn() *pgx.Conn                      { return nil }

// fakeTx is a do-nothing transaction — Rollback and Commit succeed,
// all query methods return errors (tests that need real txn behaviour
// should use fakePoolTx).
type fakeTx struct{}

func (fakeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return fakeTx{}, nil
}
func (fakeTx) Commit(ctx context.Context) error   { return nil }
func (fakeTx) Rollback(ctx context.Context) error { return nil }
func (fakeTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (fakeTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults { return nil }
func (fakeTx) LargeObjects() pgx.LargeObjects                               { return pgx.LargeObjects{} }
func (fakeTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (fakeTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return fakeRows{}, nil
}
func (fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &fakeRow{err: pgx.ErrNoRows}
}
func (fakeTx) Conn() *pgx.Conn { return nil }

// ---------------------------------------------------------------------------
// fakePoolCurrentSub — for testing CurrentSubscription (used by
// requireNoActivePremium inside Handler).
//
// CurrentSubscription calls pool.QueryRow with a SELECT. We return a row
// that Scan populates with the fields in SubscriptionRow's scan order:
//   id, user_id, kind, plan_tier, status, razorpay_subscription_id,
//   razorpay_order_id, razorpay_payment_id, amount_paise, currency,
//   current_period_end, cancel_at_period_end, created_at, updated_at
// ---------------------------------------------------------------------------

type fakePoolCurrentSub struct {
	sub *SubscriptionRow // nil means pgx.ErrNoRows
}

func (f *fakePoolCurrentSub) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	if f.sub == nil {
		return &fakeRow{err: pgx.ErrNoRows}
	}
	now := time.Now().UTC()
	return &fakeRow{vals: []any{
		f.sub.ID,
		f.sub.UserID,
		f.sub.Kind,
		f.sub.PlanTier,
		f.sub.Status,
		nilStr(f.sub.RazorpaySubscriptionID),
		nilStr(f.sub.RazorpayOrderID),
		nilStr(f.sub.RazorpayPaymentID),
		f.sub.AmountPaise,
		f.sub.Currency,
		nilTime(f.sub.CurrentPeriodEnd),
		f.sub.CancelAtPeriodEnd,
		now,
		now,
	}}
}

func (f *fakePoolCurrentSub) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return fakeRows{}, nil
}
func (f *fakePoolCurrentSub) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakePoolCurrentSub) Begin(_ context.Context) (pgx.Tx, error) {
	return fakeTx{}, nil
}

// nilStr unwraps *string, returning an empty string if nil (the fakeRow
// Scan logic handles **string → nil for us when the value is "").
func nilStr(s *string) any {
	if s == nil {
		return ""
	}
	return *s
}

// nilTime unwraps *time.Time.
func nilTime(t *time.Time) any {
	if t == nil {
		return (*time.Time)(nil)
	}
	return *t
}

// ---------------------------------------------------------------------------
// fakePoolBump — for testing BumpRecurringPeriodEnd (subscription.charged).
//
// The SQL is:
//   UPDATE billing.subscriptions
//   SET current_period_end = $2, updated_at = NOW()
//   WHERE razorpay_subscription_id = $1
//   RETURNING user_id, plan_tier
//
// QueryRow captures $2 (the periodEnd) for assertion.
// RecordEvent (INSERT INTO billing.processed_events) also calls Exec —
// we return RowsAffected=1 so it looks fresh.
// RefreshUserPremium calls Exec — no-op is fine.
// ---------------------------------------------------------------------------

type fakePoolBump struct {
	subID  string
	gotEnd *time.Time // set once BumpRecurringPeriodEnd's QueryRow is called
	uid    uuid.UUID
	tier   string
}

func (f *fakePoolBump) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	// BumpRecurringPeriodEnd: args are ($1=subID, $2=periodEnd)
	if len(args) >= 2 {
		if t, ok := args[1].(time.Time); ok {
			f.gotEnd = &t
		}
	}
	if f.uid == uuid.Nil {
		f.uid = uuid.New()
	}
	if f.tier == "" {
		f.tier = "standard"
	}
	return &fakeRow{vals: []any{f.uid, f.tier}}
}

func (f *fakePoolBump) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return fakeRows{}, nil
}

func (f *fakePoolBump) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	// RecordEvent INSERT: return RowsAffected=1 so event is treated as fresh.
	// RefreshUserPremium UPDATE: also fine as no-op.
	tag := pgconn.NewCommandTag("INSERT 0 1")
	return tag, nil
}

func (f *fakePoolBump) Begin(_ context.Context) (pgx.Tx, error) {
	return fakeTx{}, nil
}

// ---------------------------------------------------------------------------
// TASK-02 regression: 409 guard on re-subscription
//
// requireNoActivePremium is the guard shared by CheckoutSubscription,
// CheckoutSubscriptionPlus, and CheckoutPremiumPass. We test it directly
// so the nil-client and nil-planID early-returns in the outer handlers
// don't obscure the logic under test.
// ---------------------------------------------------------------------------

// TestRequireNoActivePremiumBlocks409 verifies that a user with an active,
// non-cancelling subscription is refused with 409 + already_subscribed.
func TestRequireNoActivePremiumBlocks409(t *testing.T) {
	uid := uuid.New()

	activeSub := &SubscriptionRow{
		ID:                uuid.New(),
		UserID:            uid,
		Kind:              "recurring",
		PlanTier:          "standard",
		Status:            "active",
		CancelAtPeriodEnd: false,
		AmountPaise:       PremiumMonthlyPaise,
		Currency:          "INR",
	}

	store := &Store{pool: &fakePoolCurrentSub{sub: activeSub}}
	h := &Handler{store: store}

	req := httptest.NewRequest(http.MethodPost, "/billing/checkout/subscription", nil)
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()

	// requireNoActivePremium returns false and writes 409 when blocked.
	allowed := h.requireNoActivePremium(rr, req, uid)

	if allowed {
		t.Fatal("want guard to block (return false), got true")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d; body: %s", rr.Code, rr.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body["error"] != "already_subscribed" {
		t.Errorf("want error=already_subscribed, got %v", body["error"])
	}
}

// TestRequireNoActivePremiumBlocksPlusSubscription applies the 409 guard
// test to a plus-tier active subscription — both tiers must be blocked.
func TestRequireNoActivePremiumBlocksPlusSubscription(t *testing.T) {
	uid := uuid.New()

	activeSub := &SubscriptionRow{
		ID:                uuid.New(),
		UserID:            uid,
		Kind:              "recurring",
		PlanTier:          "plus",
		Status:            "active",
		CancelAtPeriodEnd: false,
		AmountPaise:       PremiumPlusPaise,
		Currency:          "INR",
	}

	store := &Store{pool: &fakePoolCurrentSub{sub: activeSub}}
	h := &Handler{store: store}

	req := httptest.NewRequest(http.MethodPost, "/billing/checkout/subscription-plus", nil)
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()

	allowed := h.requireNoActivePremium(rr, req, uid)

	if allowed {
		t.Fatal("want guard to block (return false), got true")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

// TestRequireNoActivePremiumAllowsCancelAtPeriodEnd verifies that a user
// whose subscription is active but cancel_at_period_end=true passes through
// the guard — they've signalled intent to stop, so re-subscribing is valid.
func TestRequireNoActivePremiumAllowsCancelAtPeriodEnd(t *testing.T) {
	uid := uuid.New()

	cancelSub := &SubscriptionRow{
		ID:                uuid.New(),
		UserID:            uid,
		Kind:              "recurring",
		PlanTier:          "standard",
		Status:            "active",
		CancelAtPeriodEnd: true, // user cancelled — should be allowed through
		AmountPaise:       PremiumMonthlyPaise,
		Currency:          "INR",
	}

	store := &Store{pool: &fakePoolCurrentSub{sub: cancelSub}}
	h := &Handler{store: store}

	req := httptest.NewRequest(http.MethodPost, "/billing/checkout/subscription", nil)
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()

	allowed := h.requireNoActivePremium(rr, req, uid)

	if !allowed {
		t.Fatalf("cancel-at-period-end user should pass the guard; got blocked. body: %s", rr.Body.String())
	}
}

// TestRequireNoActivePremiumAllowsFreeUser verifies that a user with no
// active subscription (nil row) is allowed through.
func TestRequireNoActivePremiumAllowsFreeUser(t *testing.T) {
	uid := uuid.New()

	// nil sub → CurrentSubscription returns nil (no active row)
	store := &Store{pool: &fakePoolCurrentSub{sub: nil}}
	h := &Handler{store: store}

	req := httptest.NewRequest(http.MethodPost, "/billing/checkout/subscription", nil)
	req = req.WithContext(auth.WithUserID(req.Context(), uid))
	rr := httptest.NewRecorder()

	allowed := h.requireNoActivePremium(rr, req, uid)

	if !allowed {
		t.Fatalf("free user should pass the guard; got blocked. body: %s", rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// TASK-01 regression: subscription.charged writes current_period_end
// ---------------------------------------------------------------------------

// TestWebhookChargedWritesCurrentPeriodEnd verifies that a
// subscription.charged event correctly extracts current_end from the
// payload and passes the corresponding time.Time to BumpRecurringPeriodEnd.
//
// We call dispatch directly (bypassing ServeHTTP and the RecordEvent
// idempotency insert) so the test doesn't require a real database.
func TestWebhookChargedWritesCurrentPeriodEnd(t *testing.T) {
	wantEnd := time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC)

	subID := "sub_test_bumped_123"
	fb := &fakePoolBump{subID: subID}
	store := &Store{pool: fb}
	h := &WebhookHandler{store: store, webhookSecret: "x"}

	ev := &rzpEvent{
		Event: "subscription.charged",
		ID:    "evt_charged_001",
		Payload: rzpEventPayload{
			Subscription: &rzpSubscriptionEntity{
				Entity: rzpSubscription{
					ID:         subID,
					Status:     "active",
					CurrentEnd: wantEnd.Unix(),
				},
			},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", nil)
	// dispatch may return an error from RefreshUserPremium (Exec returns
	// a no-op CommandTag), but what matters is that gotEnd was set.
	_ = h.dispatch(req, ev)

	if fb.gotEnd == nil {
		t.Fatal("BumpRecurringPeriodEnd was never called for subscription.charged")
	}
	if !fb.gotEnd.Equal(wantEnd) {
		t.Errorf("current_period_end forwarded: got %v, want %v", fb.gotEnd, wantEnd)
	}
}

// TestWebhookActivatedWritesCurrentPeriodEnd mirrors the charged test for
// subscription.activated, which also sets current_period_end via
// ActivateRecurring.
func TestWebhookActivatedWritesCurrentPeriodEnd(t *testing.T) {
	wantEnd := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	subID := "sub_test_activated_456"

	fp := &fakePoolActivate{subID: subID}
	store := &Store{pool: fp}
	h := &WebhookHandler{store: store, webhookSecret: "x"}

	ev := &rzpEvent{
		Event: "subscription.activated",
		ID:    "evt_activated_001",
		Payload: rzpEventPayload{
			Subscription: &rzpSubscriptionEntity{
				Entity: rzpSubscription{
					ID:         subID,
					Status:     "active",
					CurrentEnd: wantEnd.Unix(),
				},
			},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", nil)
	_ = h.dispatch(req, ev)

	if fp.gotEnd == nil {
		t.Fatal("ActivateRecurring was never called for subscription.activated")
	}
	if !fp.gotEnd.Equal(wantEnd) {
		t.Errorf("current_period_end forwarded: got %v, want %v", fp.gotEnd, wantEnd)
	}
}

// fakePoolActivate captures the periodEnd arg passed to ActivateRecurring's
// UPDATE:  SET status='active', razorpay_payment_id=$2, current_period_end=$3
// args: ($1=subID, $2=paymentID, $3=periodEnd)
type fakePoolActivate struct {
	subID  string
	gotEnd *time.Time
	uid    uuid.UUID
}

func (f *fakePoolActivate) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	// ActivateRecurring passes (subID, paymentID, periodEnd) — index 2 is the time.
	if len(args) >= 3 {
		if t, ok := args[2].(time.Time); ok {
			f.gotEnd = &t
		}
	}
	if f.uid == uuid.Nil {
		f.uid = uuid.New()
	}
	return &fakeRow{vals: []any{f.uid}}
}

func (f *fakePoolActivate) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return fakeRows{}, nil
}
func (f *fakePoolActivate) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (f *fakePoolActivate) Begin(_ context.Context) (pgx.Tx, error) {
	return fakeTx{}, nil
}
