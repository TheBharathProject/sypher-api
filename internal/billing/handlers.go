package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// Handler exposes the auth-gated billing endpoints (everything under
// /billing/* except the public webhook). It hands off to the Razorpay
// HTTP client for outbound calls and to Store for persistence.
//
// If client is nil (no RAZORPAY_KEY_ID / SECRET in env), every checkout
// endpoint responds 503 service_unavailable. This mirrors how R2 and
// Deepseek degrade when their creds are missing — the rest of the app
// keeps booting and serving.
type Handler struct {
	store      *Store
	client     *Client
	planID     string // standard ₹99/mo
	planIDPlus string // plus ₹299/mo with bundle credits
	logger     *slog.Logger
}

// NewHandler accepts both plan ids. Pass empty for planIDPlus when the
// Premium+ tier hasn't been activated in the Razorpay dashboard yet —
// the related endpoint will respond 503 in that case.
func NewHandler(store *Store, client *Client, planID, planIDPlus string, logger *slog.Logger) *Handler {
	return &Handler{store: store, client: client, planID: planID, planIDPlus: planIDPlus, logger: logger}
}

// requireNoActivePremium short-circuits with 409 if the user already
// holds an active premium that isn't on a cancel-at-period-end track.
//
// Reused by the three premium-granting checkout handlers (standard
// recurring, plus recurring, one-time pass). Without this guard, a
// Pro user can mint orphan sub_xxx / order_xxx rows at Razorpay AND
// fresh 'pending' rows in billing.subscriptions just by replaying
// the checkout call — the §8.5 abuse path called out in the audit.
//
// Cancel-at-period-end users ARE allowed through: they've already
// indicated they want to stop, so re-subscribing (or switching tier)
// is a legitimate "changed my mind" flow.
//
// Credits top-ups intentionally skip this check — buying more credits
// while already premium is normal usage.
func (h *Handler) requireNoActivePremium(w http.ResponseWriter, r *http.Request, uid uuid.UUID) bool {
	existing, err := h.store.CurrentSubscription(r.Context(), uid)
	if err != nil {
		h.logger.Error("check existing subscription", "err", err, "user_id", uid)
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return false
	}
	if existing == nil || existing.CancelAtPeriodEnd {
		return true
	}
	body := map[string]any{
		"error":          "already_subscribed",
		"message":        "you already have an active premium subscription; cancel auto-renewal first to switch tiers",
		"subscriptionId": existing.ID,
		"kind":           existing.Kind,
		"planTier":       existing.PlanTier,
	}
	if existing.RazorpaySubscriptionID != nil {
		body["razorpaySubscriptionId"] = *existing.RazorpaySubscriptionID
	}
	if existing.RazorpayOrderID != nil {
		body["razorpayOrderId"] = *existing.RazorpayOrderID
	}
	httpx.WriteJSON(w, http.StatusConflict, body)
	return false
}

// CheckoutSubscription starts a recurring premium flow. Creates a
// Razorpay subscription tied to the configured RAZORPAY_PLAN_ID, mints
// a 'pending' row in billing.subscriptions, and returns the public
// fields the frontend Razorpay Checkout modal needs.
func (h *Handler) CheckoutSubscription(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "billing_unavailable", "billing is not configured on this server")
		return
	}
	if h.planID == "" {
		httpx.WriteError(w, http.StatusServiceUnavailable, "no_plan_id", "RAZORPAY_PLAN_ID is not set")
		return
	}
	uid := auth.MustUserID(r.Context())
	if !h.requireNoActivePremium(w, r, uid) {
		return
	}

	// 12 cycles authorises a year of monthly charges; eMandates have a
	// max-amount and max-cycle limit, and 12 is the comfortable middle.
	notes := map[string]string{
		"kind":    "recurring_premium",
		"user_id": uid.String(),
	}
	sub, err := h.client.CreateSubscription(h.planID, 12, notes)
	if err != nil {
		h.logger.Error("create subscription", "err", err, "user_id", uid)
		httpx.WriteError(w, http.StatusBadGateway, "razorpay_failed", err.Error())
		return
	}

	if _, err := h.store.CreatePendingRecurring(r.Context(), uid, sub.ID, PremiumMonthlyPaise, "standard"); err != nil {
		h.logger.Error("persist pending recurring", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"razorpaySubscriptionId": sub.ID,
		"keyId":                  h.client.KeyID(),
		"amount":                 PremiumMonthlyPaise,
		"currency":               "INR",
	})
}

// CheckoutSubscriptionPlus mirrors CheckoutSubscription but uses the
// ₹299/mo "plus" plan id and stamps plan_tier='plus' on the row so the
// webhook grants 200 bonus credits on every successful charge.
func (h *Handler) CheckoutSubscriptionPlus(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "billing_unavailable", "billing is not configured on this server")
		return
	}
	if h.planIDPlus == "" {
		httpx.WriteError(w, http.StatusServiceUnavailable, "no_plus_plan", "RAZORPAY_PLAN_ID_PLUS is not set")
		return
	}
	uid := auth.MustUserID(r.Context())
	if !h.requireNoActivePremium(w, r, uid) {
		return
	}

	notes := map[string]string{
		"kind":      "recurring_premium",
		"plan_tier": "plus",
		"user_id":   uid.String(),
	}
	sub, err := h.client.CreateSubscription(h.planIDPlus, 12, notes)
	if err != nil {
		h.logger.Error("create plus subscription", "err", err, "user_id", uid)
		httpx.WriteError(w, http.StatusBadGateway, "razorpay_failed", err.Error())
		return
	}

	if _, err := h.store.CreatePendingRecurring(r.Context(), uid, sub.ID, PremiumPlusPaise, "plus"); err != nil {
		h.logger.Error("persist pending plus recurring", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"razorpaySubscriptionId": sub.ID,
		"keyId":                  h.client.KeyID(),
		"amount":                 PremiumPlusPaise,
		"currency":               "INR",
		"planTier":               "plus",
	})
}

// CheckoutPremiumPass starts a one-time premium order. Creates a
// Razorpay order with notes.kind='one_time_premium' so payment.captured
// dispatches to the right handler.
func (h *Handler) CheckoutPremiumPass(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "billing_unavailable", "billing is not configured on this server")
		return
	}
	uid := auth.MustUserID(r.Context())
	if !h.requireNoActivePremium(w, r, uid) {
		return
	}

	notes := map[string]string{
		"kind":    "one_time_premium",
		"user_id": uid.String(),
	}
	order, err := h.client.CreateOrder(PremiumMonthlyPaise, "INR", notes)
	if err != nil {
		h.logger.Error("create premium order", "err", err, "user_id", uid)
		httpx.WriteError(w, http.StatusBadGateway, "razorpay_failed", err.Error())
		return
	}

	if _, err := h.store.CreatePendingOneTime(r.Context(), uid, order.ID, PremiumMonthlyPaise); err != nil {
		h.logger.Error("persist pending one_time", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"razorpayOrderId": order.ID,
		"keyId":           h.client.KeyID(),
		"amount":          PremiumMonthlyPaise,
		"currency":        "INR",
		"kind":            "one_time_premium",
	})
}

type creditsCheckoutInput struct {
	PackID string `json:"packId"`
}

// CheckoutCredits starts a credits-pack order. Pack pricing is
// authoritative server-side via packs.go; the body only carries the
// pack id. notes.kind='credits' + notes.credits=N drives the webhook.
func (h *Handler) CheckoutCredits(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "billing_unavailable", "billing is not configured on this server")
		return
	}
	uid := auth.MustUserID(r.Context())

	var in creditsCheckoutInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	pack, err := PackByID(in.PackID)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", err.Error())
		return
	}

	notes := map[string]string{
		"kind":    "credits",
		"user_id": uid.String(),
		"credits": fmt.Sprintf("%d", pack.Credits),
		"pack_id": pack.ID,
	}
	order, err := h.client.CreateOrder(pack.AmountPaise, "INR", notes)
	if err != nil {
		h.logger.Error("create credits order", "err", err, "user_id", uid, "pack", pack.ID)
		httpx.WriteError(w, http.StatusBadGateway, "razorpay_failed", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"razorpayOrderId": order.ID,
		"keyId":           h.client.KeyID(),
		"amount":          pack.AmountPaise,
		"currency":        "INR",
		"credits":         pack.Credits,
		"packId":          pack.ID,
		"kind":            "credits",
	})
}

// CancelSubscription is the user-initiated cancellation. We honour
// cancel-at-period-end: Razorpay stops renewing, but the user keeps
// premium until current_period_end. The webhook subscription.cancelled
// (which fires when the period elapses) does the final state flip.
func (h *Handler) CancelSubscription(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "billing_unavailable", "billing is not configured on this server")
		return
	}
	uid := auth.MustUserID(r.Context())
	subID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_id", "invalid subscription id")
		return
	}

	row, err := h.store.SubscriptionByID(r.Context(), uid, subID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "subscription not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if row.Kind != "recurring" {
		httpx.WriteError(w, http.StatusBadRequest, "not_recurring", "only recurring subs can be cancelled")
		return
	}
	if row.RazorpaySubscriptionID == nil || *row.RazorpaySubscriptionID == "" {
		httpx.WriteError(w, http.StatusInternalServerError, "data_corrupt", "missing razorpay subscription id")
		return
	}

	if err := h.client.CancelSubscription(*row.RazorpaySubscriptionID); err != nil {
		h.logger.Error("razorpay cancel", "err", err, "sub_id", subID)
		httpx.WriteError(w, http.StatusBadGateway, "razorpay_failed", err.Error())
		return
	}
	if err := h.store.MarkCancelAtPeriodEnd(r.Context(), uid, subID); err != nil {
		h.logger.Error("mark cancel at period end", "err", err, "sub_id", subID)
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetMe returns the current user's billing snapshot — premium status,
// credits balance, and recent activity. The frontend renders the Billing
// section in /settings off this single payload.
func (h *Handler) GetMe(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())

	sub, err := h.store.CurrentSubscription(r.Context(), uid)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	balance, err := h.store.CreditsBalance(r.Context(), uid)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	activity, err := h.store.RecentActivity(r.Context(), uid, 6)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	out := map[string]any{
		"creditsBalance": balance,
		"recentActivity": activity,
		"creditPacks":    CreditPacks(),
	}
	if sub != nil {
		out["premium"] = map[string]any{
			"id":                  sub.ID,
			"kind":                sub.Kind,
			"planTier":            sub.PlanTier,
			"status":              sub.Status,
			"currentPeriodEnd":    sub.CurrentPeriodEnd,
			"cancelAtPeriodEnd":   sub.CancelAtPeriodEnd,
			"amountPaise":         sub.AmountPaise,
		}
	} else {
		out["premium"] = nil
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
