# ADR-0006 — Razorpay billing (subscriptions, one-time premium, credits)

**Status:** Accepted
**Date:** 2026-05-09

## Context

ADR-0002 (premium-email-gating, 2026-05-09) flipped the email layer
behind an `is_premium` boolean and deferred a real billing integration
("Stripe deferred — premium status is admin-flippable via SQL until a
paid user shows up"). Phase 6b makes billing real. The provider is
**Razorpay** because Pegasus is India-first (₹99/month, INR-only,
Indian users predominantly).

Three charge types coexist as parallel paths through the same provider:

1. **Recurring premium subscription** — eMandate / UPI Autopay,
   ₹99/month. Default UX for users willing to set up autopay.
2. **One-time premium pass** — ₹99, 30 days of premium, then expires.
   Fallback for users who refuse autopay (~10–15% of Indian users
   reject UPI Autopay; eMandate has notable abandonment).
3. **Credits top-up** — variable amount, unlocks paid AI features.
   Lives on a separate ledger from premium (a user can have credits
   without being premium and vice versa).

These are distinct user intents. Conflating them ("premium gives
unlimited credits") would muddle pricing later when AI features get
their own per-call cost dial.

## Decisions

### D1 — New schema `billing` with two tables + a column on `auth.users`

Migration `0013_billing.sql`:

- `billing.subscriptions` — premium-tier state. `kind` discriminates
  recurring vs one_time. Status moves through `pending → active → halted | cancelled | expired`.
- `billing.credit_transactions` — append-only ledger. Positive deltas
  for purchases, negative for AI consumption. Reason field is
  free-text (not enum'd) so adding a new AI feature doesn't need a
  migration.
- `billing.processed_events` — webhook idempotency: every Razorpay
  event_id is INSERTed before processing; duplicates short-circuit.
- `auth.users.credits_balance` — denormalised running total of the
  ledger, updated atomically inside the same transaction as each
  ledger row.

**Rejected:**

- One ledger table for both subs + credits — query patterns differ;
  `reason` and `ref_type` would be doing too much.
- Computing balance from the ledger on every read — fine at small
  scale but adds latency for AI handlers that gate before each call.

### D2 — `is_premium` is a cached boolean refreshed on every state change

`auth.users.is_premium` stays as ADR-0002 D1 originally intended — a
cached derivation. `Store.RefreshUserPremium(uid)` recomputes it from
`billing.subscriptions` (any active row with `current_period_end IS NULL OR > NOW()` → true). The webhook handler and the cron job both call
this after any state change. The email-gate path
(`Store.UserCanReceiveEmail`) stays untouched.

### D3 — Three checkout endpoints

- `POST /billing/checkout/subscription` — creates a Razorpay
  subscription tied to `RAZORPAY_PLAN_ID`, mints a `pending` row,
  returns `{razorpaySubscriptionId, keyId, amount, currency}`.
- `POST /billing/checkout/premium-pass` — creates a one-time order with
  `notes.kind='one_time_premium'`, returns `{razorpayOrderId, keyId, amount, currency, kind}`.
- `POST /billing/checkout/credits` — body `{packId}` resolves against
  `internal/billing/packs.go`, creates an order with
  `notes.kind='credits'` + `notes.credits=N`, returns `{razorpayOrderId, keyId, amount, credits, currency, kind}`.

Pack pricing is server-authoritative — clients can only pick a pack id;
they can't request custom amounts. Hard-coded to start (3 packs:
₹49/100, ₹199/500, ₹449/1500). DB rows can come later if pricing
needs to vary.

### D4 — Single webhook endpoint, two-level dispatch

`POST /webhooks/razorpay` is public (no auth middleware) but verified
via HMAC-SHA256(`RAZORPAY_WEBHOOK_SECRET`) over the raw body, compared
in constant time via `crypto/hmac.Equal`. Dispatch is two-level:
first by `event` string, then for `payment.captured` / `payment.failed`
by `payload.payment.entity.notes.kind`.

| event                       | notes.kind         | effect                                                       |
|-----------------------------|--------------------|--------------------------------------------------------------|
| `subscription.activated`    | —                  | Mark sub `active`, set `current_period_end`, refresh premium. |
| `subscription.charged`      | —                  | Bump `current_period_end`. (renewal succeeded)               |
| `subscription.halted`       | —                  | Mark `halted`, refresh premium.                              |
| `subscription.cancelled`    | —                  | Mark `cancelled`, refresh premium.                            |
| `payment.captured`          | `one_time_premium` | Mark order's sub row `active`, period_end = now+30d, refresh.|
| `payment.captured`          | `credits`          | Insert positive ledger row, update cached balance atomically.|
| `payment.failed`            | `one_time_premium` | Mark order's sub row `expired`, refresh.                     |
| `payment.failed`            | `credits`          | No-op (no credits awarded).                                  |
| any other                   | —                  | Logged at debug, 200 returned (Razorpay stops retrying).      |

Idempotency: every event has a stable `id`. `RecordEvent(eventID)`
INSERTs into `billing.processed_events` and returns whether the row
was fresh. Duplicates short-circuit before any state changes.

### D5 — Cron job for one-time expiry

Recurring subs auto-renew via webhook (`subscription.charged`). One-time
passes get no renewal event — they sit there with `current_period_end`
ticking down. New cron job `expire-one-time-premium` runs at 04:00 IST
daily, scans for `kind='one_time' AND status='active' AND current_period_end < NOW()`,
flips status to `expired`, and refreshes `is_premium` for each affected
user. Local testing via `go run ./cmd/cron-once expire-one-time-premium`.

### D6 — Cancel-at-period-end semantics

User clicks "Cancel auto-renewal" in `/settings`. Backend calls
`POST /v1/subscriptions/{id}/cancel` with `cancel_at_cycle_end=1` and
flips `cancel_at_period_end=true` on the local row. The user keeps
premium until `current_period_end` passes, then Razorpay fires
`subscription.cancelled` which marks the row `cancelled` and the next
`RefreshUserPremium` flips `is_premium` to false.

### D7 — Atomic credit purchase + spend

Both `RecordCreditPurchase(uid, credits, ...)` and `SpendCredits(uid, amount, reason, ...)` run inside a single SQL transaction:

1. `UPDATE auth.users SET credits_balance = credits_balance ± N WHERE id = $1` — for spend, `AND credits_balance >= N` to prevent going negative; the SQL path returns no row when the balance check fails, which the helper maps to `ErrInsufficientCredits`.
2. `INSERT INTO billing.credit_transactions ... VALUES (..., balance_after, ...)` — using the value returned from step 1.
3. `COMMIT`.

A crash mid-transaction cannot leave the cached balance out of sync
with the ledger.

### D8 — Razorpay credentials live in env

- `RAZORPAY_KEY_ID` (sent to frontend Checkout).
- `RAZORPAY_KEY_SECRET` (server-only).
- `RAZORPAY_WEBHOOK_SECRET` (server-only, set per-endpoint in dashboard).
- `RAZORPAY_PLAN_ID` (the ₹99/mo Plan, created once in dashboard).

If `RAZORPAY_KEY_ID` or `RAZORPAY_KEY_SECRET` is empty, `billing.NewClient` returns `nil` and every checkout endpoint responds `503 service_unavailable`. Mirrors how R2 / Deepseek degrade with missing creds.

Local dev uses test mode (`rzp_test_*`); prod uses live mode. The
deploy.sh on OCI VM gets live keys via the existing `pg.secret` env file.

### D9 — Separate from AI usage tracking (Phase 7)

`internal/ai/usage_store.go` already tracks token usage per user with a
monthly cap. Phase 6b ships the credits **mechanism** (table, helpers,
checkout) but does NOT yet wire spending into AI handlers. That's
Phase 7 work, gated on per-feature pricing decisions ("how many credits
does a cover letter cost?"). Until then:

- AI handlers stay on the existing `AIUsageMonthlyTokenLimit` gate.
- Users can buy credits today; balance just sits there ready for Phase 7.
- The `Store.SpendCredits` helper is shipped + unit-tested against
  insufficient-balance.

## Consequences

**Positive:**

- Three billing models from day one. Recurring gives predictable MRR;
  one-time captures users who refuse autopay; credits decouples AI
  pricing from premium pricing.
- Webhook-driven `is_premium` is correct by construction; the cron
  is only for one-time expiry, which has no upstream signal.
- Single source of truth for premium (`billing.subscriptions`) and
  credits (`credit_transactions` ledger). The cached columns on
  `auth.users` are derived state, refreshed by single helpers.
- Adding a new charge type later (e.g. annual plan, gift codes) is a
  new `notes.kind` + a webhook branch — no schema change.

**Negative:**

- eMandate setup in India has notable abandonment (~20–30%). The
  one-time fallback partially absorbs this.
- Razorpay live mode requires Indian-business KYC (PAN, GSTIN if
  applicable, bank account). Real-world ops, not code; flag before
  flipping live env vars.
- `billing.processed_events` grows monotonically. A monthly cleanup
  (delete rows older than 90 days) is fine, deferred until volume
  warrants.
- Currently no email or in-app notification on failed renewal — the
  in-app billing status is the only signal. If subscription.halted
  rates climb, add a follow-up nudge.
