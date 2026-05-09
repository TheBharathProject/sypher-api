package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// WebhookHandler is the public POST /webhooks/razorpay endpoint. It is
// NOT auth-gated — Razorpay calls it from the public internet. Trust is
// established via HMAC-SHA256 signature verification against
// RAZORPAY_WEBHOOK_SECRET (cfg.RazorpayWebhookSecret).
//
// Idempotency: every event has a stable `id` we INSERT into
// billing.processed_events; duplicates short-circuit before any
// state changes. Razorpay retries failed deliveries, so duplicate
// arrival is the norm, not the exception.
//
// Dispatch is two-level — first by `event` string, then for
// payment.captured/.failed by `payload.payment.entity.notes.kind`. See
// ADR-0006 D4 for the full table.
type WebhookHandler struct {
	store         *Store
	webhookSecret string
	logger        *slog.Logger
}

func NewWebhookHandler(store *Store, webhookSecret string, logger *slog.Logger) *WebhookHandler {
	return &WebhookHandler{store: store, webhookSecret: webhookSecret, logger: logger}
}

// rzpNotes is the user-defined metadata Razorpay round-trips on orders,
// subscriptions, and payments. Officially typed as `Map<string,string>`
// in their docs, but in practice the wire format ranges across:
//
//   {}                  — empty object
//   []                  — empty PHP-array (their backend serializes empty
//                          assoc-arrays as numerically-indexed arrays;
//                          common in dashboard "test" payloads)
//   null                — sometimes on synthetic events
//   {"k":"v"}           — populated map
//   {"k": 123}          — non-string value (we skip those keys)
//
// A custom UnmarshalJSON makes the webhook robust against all five
// shapes — without it the test webhook 400s with `bad_json` because
// `[]` can't decode into `map[string]string`.
type rzpNotes map[string]string

func (n *rzpNotes) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(data) == 0 || s == "null" || s == "[]" {
		*n = rzpNotes{}
		return nil
	}
	// Try strict map[string]string first — fastest, covers the populated
	// case which is what real production events look like.
	var strict map[string]string
	if err := json.Unmarshal(data, &strict); err == nil {
		*n = strict
		return nil
	}
	// Fallback for mixed-type values: parse loosely and keep only the
	// string ones. We only consume `kind`, `user_id`, `credits` — all of
	// which are stamped as strings on our side, so this is safe.
	var loose map[string]json.RawMessage
	if err := json.Unmarshal(data, &loose); err != nil {
		return fmt.Errorf("notes: %w", err)
	}
	out := rzpNotes{}
	for k, v := range loose {
		var sv string
		if json.Unmarshal(v, &sv) == nil {
			out[k] = sv
		}
	}
	*n = out
	return nil
}

// rzpEvent is the outer envelope of every Razorpay webhook. We only
// pull out the fields we actually use in dispatch — the rest of the
// payload stays as raw JSON we re-decode based on event type.
type rzpEvent struct {
	Event     string         `json:"event"`     // e.g. "subscription.activated"
	AccountID string         `json:"account_id"`
	ID        string         `json:"id"`        // for idempotency dedup
	Payload   rzpEventPayload `json:"payload"`
}

type rzpEventPayload struct {
	Payment      *rzpPaymentEntity      `json:"payment,omitempty"`
	Subscription *rzpSubscriptionEntity `json:"subscription,omitempty"`
}

type rzpPaymentEntity struct {
	Entity rzpPayment `json:"entity"`
}

type rzpPayment struct {
	ID       string   `json:"id"`        // pay_xxx
	OrderID  string   `json:"order_id"`  // order_xxx (the one we created)
	Amount   int      `json:"amount"`    // paise
	Currency string   `json:"currency"`
	Status   string   `json:"status"`    // "captured" | "failed"
	Notes    rzpNotes `json:"notes"`     // our notes — kind, user_id, credits
}

type rzpSubscriptionEntity struct {
	Entity rzpSubscription `json:"entity"`
}

type rzpSubscription struct {
	ID                 string   `json:"id"`                   // sub_xxx
	Status             string   `json:"status"`               // "active" | "halted" | ...
	CurrentEnd         int64    `json:"current_end"`          // unix seconds
	Notes              rzpNotes `json:"notes"`
	LatestPaymentID    string            `json:"-"`                    // we'll fill from event payload payment if present
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// MUST read the raw body BEFORE unmarshalling — signature is
	// computed over the exact bytes Razorpay sent.
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_body", "could not read body")
		return
	}

	if !VerifyWebhookSignature(raw, r.Header.Get("X-Razorpay-Signature"), h.webhookSecret) {
		// Don't leak which check failed — just refuse.
		h.logger.Warn("webhook signature mismatch", "remote", r.RemoteAddr)
		httpx.WriteError(w, http.StatusUnauthorized, "bad_signature", "signature did not verify")
		return
	}

	var ev rzpEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	fresh, err := h.store.RecordEvent(r.Context(), ev.ID)
	if err != nil {
		h.logger.Error("webhook record event", "err", err, "event_id", ev.ID)
		httpx.WriteError(w, http.StatusInternalServerError, "record_event", err.Error())
		return
	}
	if !fresh {
		// Already processed — Razorpay can stop retrying. 200 is the
		// success signal it expects.
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := h.dispatch(r, &ev); err != nil {
		// Internal failure during dispatch — log + return 500 so
		// Razorpay retries. The processed_events row was already
		// inserted but if dispatch failed mid-way, we'd want a manual
		// reconciliation. We accept that operational reality for now;
		// the alternative (rolling back processed_events on dispatch
		// failure) opens up duplicate-event windows.
		h.logger.Error("webhook dispatch failed", "err", err, "event", ev.Event, "event_id", ev.ID)
		httpx.WriteError(w, http.StatusInternalServerError, "dispatch_failed", err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
}

// dispatch routes a verified event to the right store helper. Switch
// table mirrors the matrix in ADR-0006 D4.
func (h *WebhookHandler) dispatch(r *http.Request, ev *rzpEvent) error {
	ctx := r.Context()

	switch ev.Event {

	case "subscription.activated":
		sub := ev.Payload.Subscription
		if sub == nil {
			return fmt.Errorf("missing subscription payload")
		}
		periodEnd := time.Unix(sub.Entity.CurrentEnd, 0).UTC()
		// subscription.activated doesn't itself carry a payment; the
		// associated payment (the ₹0 mandate-verification or the first
		// ₹99 charge) lands as a separate event. We pass empty for now;
		// subscription.charged updates this later.
		uid, err := h.store.ActivateRecurring(ctx, sub.Entity.ID, "", periodEnd)
		if err != nil {
			return err
		}
		return h.store.RefreshUserPremium(ctx, uid)

	case "subscription.charged":
		sub := ev.Payload.Subscription
		pay := ev.Payload.Payment
		if sub == nil {
			return fmt.Errorf("missing subscription payload on charged")
		}
		periodEnd := time.Unix(sub.Entity.CurrentEnd, 0).UTC()
		uid, tier, err := h.store.BumpRecurringPeriodEnd(ctx, sub.Entity.ID, periodEnd)
		if err != nil {
			return err
		}
		// Best-effort: stamp the latest payment_id if the event included
		// the payment entity. Only used for display in /settings.
		if pay != nil && pay.Entity.ID != "" {
			_, _ = h.store.ActivateRecurring(ctx, sub.Entity.ID, pay.Entity.ID, periodEnd)
		}
		// Plus tier grants bundle credits on every successful charge
		// (including the first one — Razorpay sends activated + charged
		// for the initial cycle). Idempotency on the event_id ensures we
		// don't double-grant when Razorpay retries.
		if tier == "plus" && pay != nil && pay.Entity.ID != "" {
			if _, err := h.store.RecordCreditPurchase(
				ctx, uid, PremiumPlusBundleCred,
				sub.Entity.ID, // use sub_id as the order correlation (no order on subscription charges)
				pay.Entity.ID,
			); err != nil {
				h.logger.Error("plus tier credit grant failed", "err", err, "user_id", uid)
				// Don't bubble — we still want is_premium refreshed even if
				// the credit grant hiccupped. Manual reconciliation possible.
			}
		}
		return h.store.RefreshUserPremium(ctx, uid)

	case "subscription.halted":
		sub := ev.Payload.Subscription
		if sub == nil {
			return fmt.Errorf("missing subscription payload")
		}
		uid, err := h.store.MarkRecurringStatus(ctx, sub.Entity.ID, "halted")
		if err != nil {
			return err
		}
		return h.store.RefreshUserPremium(ctx, uid)

	case "subscription.cancelled":
		sub := ev.Payload.Subscription
		if sub == nil {
			return fmt.Errorf("missing subscription payload")
		}
		uid, err := h.store.MarkRecurringStatus(ctx, sub.Entity.ID, "cancelled")
		if err != nil {
			return err
		}
		return h.store.RefreshUserPremium(ctx, uid)

	case "payment.captured":
		return h.dispatchPaymentCaptured(ctx, ev)

	case "payment.failed":
		return h.dispatchPaymentFailed(ctx, ev)

	default:
		// Razorpay sends many event types we don't care about (refund.*,
		// invoice.*, etc.). 200 + no-op is the right response for
		// unhandled events.
		h.logger.Debug("webhook unhandled event", "event", ev.Event, "id", ev.ID)
		return nil
	}
}

// dispatchPaymentCaptured branches on notes.kind set when we created
// the order. Three cases today:
//   - 'one_time_premium' → activate the order's sub row, refresh premium.
//   - 'credits'          → record the credit purchase, refresh balance.
//   - missing/unknown    → no-op; might be a payment from a future flow.
func (h *WebhookHandler) dispatchPaymentCaptured(ctx context.Context, ev *rzpEvent) error {
	pay := ev.Payload.Payment
	if pay == nil {
		return fmt.Errorf("missing payment payload")
	}
	notes := pay.Entity.Notes
	kind := notes["kind"]
	switch kind {
	case "one_time_premium":
		uid, err := h.store.ActivateOneTime(ctx, pay.Entity.OrderID, pay.Entity.ID)
		if err != nil {
			return err
		}
		return h.store.RefreshUserPremium(ctx, uid)
	case "credits":
		userIDStr := notes["user_id"]
		creditsStr := notes["credits"]
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return fmt.Errorf("credits payment without valid user_id note: %w", err)
		}
		var credits int
		if _, err := fmt.Sscanf(creditsStr, "%d", &credits); err != nil || credits <= 0 {
			return fmt.Errorf("credits payment without valid credits note: %q", creditsStr)
		}
		_, err = h.store.RecordCreditPurchase(ctx, userID, credits, pay.Entity.OrderID, pay.Entity.ID)
		return err
	default:
		h.logger.Debug("payment.captured with unknown kind",
			"kind", kind, "payment_id", pay.Entity.ID)
		return nil
	}
}

// dispatchPaymentFailed mirrors the captured path but only matters for
// one_time_premium (the order's sub row needs to flip to 'expired' so
// the user can retry). Credits failures are no-ops since we never
// awarded any.
func (h *WebhookHandler) dispatchPaymentFailed(ctx context.Context, ev *rzpEvent) error {
	pay := ev.Payload.Payment
	if pay == nil {
		return fmt.Errorf("missing payment payload")
	}
	if pay.Entity.Notes["kind"] == "one_time_premium" {
		uid, err := h.store.MarkOneTimeStatus(ctx, pay.Entity.OrderID, "expired")
		if err != nil {
			return err
		}
		return h.store.RefreshUserPremium(ctx, uid)
	}
	return nil
}
