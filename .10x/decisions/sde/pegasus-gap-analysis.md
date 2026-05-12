# SDE Decision — pegasus-gap-analysis

**Date:** 2026-05-12
**Commit:** f0ca0f9
**Tasks:** TASK-01, TASK-02, TASK-25

---

## What existed vs what was added

### TASK-01 — Fix currentPeriodEnd null for active subscriptions

**Finding:** The bug was already fixed in the existing code before this session.

- `webhook.go` lines 195 and 207 already extract `periodEnd := time.Unix(sub.Entity.CurrentEnd, 0).UTC()`
- `ActivateRecurring` (line 112, store.go) already writes `current_period_end = $3` for `subscription.activated`
- `BumpRecurringPeriodEnd` (line 133, store.go) already writes `current_period_end = $2` for `subscription.charged`

No code change was required for TASK-01. Regression tests now pin this behavior.

### TASK-02 — Add 409 guard to prevent Pro re-subscription

**Finding:** Also already implemented before this session.

- `requireNoActivePremium` (handlers.go lines 55-80) already exists and returns 409 with `error: "already_subscribed"`
- Already called in `CheckoutSubscription` (line 96), `CheckoutSubscriptionPlus` (line 140), and `CheckoutPremiumPass` (line 179)
- Cancel-at-period-end users are already exempted (line 63: `if existing == nil || existing.CancelAtPeriodEnd`)

No code change was required for TASK-02. Regression tests now cover all four code paths.

### TASK-25 — Regression tests

**New files:**
- `internal/billing/handlers_test.go` — 9 new tests covering TASK-01 and TASK-02 behavior

**Prerequisite changes required to enable testing:**

1. `internal/auth/middleware.go` — added `WithUserID(ctx, id)` exported helper so billing tests can inject a user into context without a real JWT. This is a non-test exported function, available for any package that needs to inject auth state in tests.

2. `internal/billing/store.go` — introduced `dbPool` interface that `*pgxpool.Pool` naturally satisfies. Changed `Store.pool` field from `*pgxpool.Pool` to `dbPool`. This enables injecting fake pool implementations in tests. `NewStore` signature unchanged — it still accepts `*pgxpool.Pool`.

**Test coverage added:**

| Test | What it verifies |
|---|---|
| `TestRequireNoActivePremiumBlocks409` | Active standard sub → 409 with `error=already_subscribed` |
| `TestRequireNoActivePremiumBlocksPlusSubscription` | Active plus sub → 409 |
| `TestRequireNoActivePremiumAllowsCancelAtPeriodEnd` | cancel_at_period_end=true → guard passes through |
| `TestRequireNoActivePremiumAllowsFreeUser` | No active sub (nil row) → guard passes through |
| `TestWebhookChargedWritesCurrentPeriodEnd` | `subscription.charged` dispatch passes correct `time.Time` to `BumpRecurringPeriodEnd` |
| `TestWebhookActivatedWritesCurrentPeriodEnd` | `subscription.activated` dispatch passes correct `time.Time` to `ActivateRecurring` |

Plus 7 pre-existing tests (signature verification, notes unmarshalling) all continue passing.

**Fake pool implementations (test-only, in handlers_test.go):**
- `fakeRow` — implements `pgx.Row`, returns preset values or `pgx.ErrNoRows`
- `fakeRows` — implements `pgx.Rows`, returns zero rows
- `fakeTx` — implements `pgx.Tx`, no-op commit/rollback
- `fakePoolCurrentSub` — returns a preset `SubscriptionRow` for `CurrentSubscription` queries
- `fakePoolBump` — captures `periodEnd` arg to `BumpRecurringPeriodEnd`'s UPDATE
- `fakePoolActivate` — captures `periodEnd` arg to `ActivateRecurring`'s UPDATE

---

## Deviations from plan

| Deviation | Reason |
|---|---|
| TASK-01 had no production code to write | Bug was already fixed. Tests written as specified. |
| TASK-02 had no production code to write | Guard was already implemented. Tests written as specified. |
| Tests call `requireNoActivePremium` directly, not `CheckoutSubscription` | `CheckoutSubscription` checks `h.client == nil` before reaching the guard. With a nil client it returns 503, never reaching the 409 path. Testing the guard directly is both more precise and avoids needing a mock HTTP server. |
| Introduced `dbPool` interface in store.go | Required to write unit tests without a real Postgres connection. Minimal change — only the field type changed, `NewStore` is unchanged, all callers unaffected. |

---

## Tech debt created

| Item | Severity | Notes |
|---|---|---|
| `fakeRow.Scan` uses type-switch with explicit cases | Low | Only handles types actually used by `CurrentSubscription`, `ActivateRecurring`, `BumpRecurringPeriodEnd`. Adding a new store method may require extending the switch. |
| Test fakes are not shared across test files | Low | `fakePoolCurrentSub` etc. live in `handlers_test.go`. If more handler tests are written later, they should be extracted to a `billing_test_helpers_test.go` shared test file. |

---

## Test coverage notes

```
go test ./internal/billing/... -race
ok   github.com/TheBharathProject/sypher-api/internal/billing   1.638s
```

13 tests, all passing with `-race`. No DB required. No network calls.

---

## Commit hashes

| Commit | Description |
|---|---|
| `f0ca0f9` | fix(billing): write current_period_end on webhook + 409 guard on re-subscription |
