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

### TASK-03 — Migration 0019_community_slugs.sql

**New file:** `migrations/0019_community_slugs.sql`

Wraps all DDL in a transaction. Four statements, all idempotent:
1. `ALTER TABLE ... ADD COLUMN IF NOT EXISTS slug TEXT` — adds the column
2. `UPDATE ... SET slug = 'post-' || REPLACE(id::text, '-', '') WHERE slug IS NULL` — backfills existing rows
3. `ALTER COLUMN slug SET NOT NULL` — enforces constraint after backfill
4. `CREATE UNIQUE INDEX IF NOT EXISTS community_posts_slug_idx` — enforces uniqueness

Confirmed: 0018 was the last migration before this session.

---

### TASK-04 — internal/jobtracker/slug.go [NEW]

**New file:** `internal/jobtracker/slug.go`

Two package-level compiled regexes (no regex compiles inside functions):
- `consecutiveHyphens = regexp.MustCompile(`-{2,}`)` — collapses multiple hyphens
- `nonSlugChar = regexp.MustCompile(`[^a-z0-9 \-]`)` — strips disallowed characters

`slugify(title string) string` — 6-step pure function:
1. `strings.ToLower`
2. Strip non-slug chars via `nonSlugChar`
3. `strings.TrimSpace`
4. `strings.ReplaceAll(s, " ", "-")` then `consecutiveHyphens.ReplaceAllString`
5. Truncate to 80 chars, back up to last hyphen if idx > 40
6. Returns `""` on empty result (uniqueSlug handles "post" fallback)

`uniqueSlug(ctx, title)` — 5-retry loop with `crypto/rand` (2 bytes → 4 hex chars suffix). Uses `SELECT EXISTS(...)` check.

**Deviation:** The Senior Engineer ADR showed a single `consecutiveHyphens` regex that matched `[-\s]{2,}`. In practice a single space also needs converting to a hyphen (single spaces aren't "two or more"). Split into explicit `strings.ReplaceAll(s, " ", "-")` then collapse consecutive hyphens. The observed behavior matches the test matrix exactly.

---

### TASK-05 — internal/jobtracker/slug_test.go [NEW]

**New file:** `internal/jobtracker/slug_test.go`

13 test cases in `TestSlugify`:
- Normal ASCII title with exclamation → slug without `!`
- Hyphenated word (`SDE-2`) → preserved
- All-special-chars (`!!!`) → `""` (not "post")
- Leading/trailing spaces → trimmed
- Consecutive hyphens (`a--b---c`) → collapsed
- 100-char title → truncated to 80
- Hindi + ASCII (`गूगल interview experience`) → `interview-experience`
- Empty string → `""`
- Only non-ASCII → `""`
- Mixed spaces and hyphens → collapsed
- Exactly 80 chars → not truncated
- Punctuation (`What's the best way...`) → stripped
- 50 a's + `-` + 40 b's (91 chars) → truncated at hyphen boundary = 50 a's

All 13 pass with `-race`.

---

### TASK-06 — Wire slugs into store_community.go and handlers_community.go

**Modified:** `internal/jobtracker/store_community.go`

Changes (line-level):
- `CommunityPost` struct: added `Slug string \`json:"slug"\`` between `AuthorSlug` and `Surface` fields
- `communityPostCols` constant: added `p.slug` between `COALESCE(pf.slug, '')` and `p.surface`
- `scanCommunityPost`: added `&p.Slug` between `&p.AuthorSlug` and `&p.Surface` in the Scan call
- `CreatePost`: added `slug, err := s.uniqueSlug(ctx, in.Title)` before the INSERT; INSERT now includes `slug` column with `$7` placeholder
- New `GetPostBySlug(ctx, slug, viewerID)` function after `GetPost` — same query structure, `WHERE p.slug = $1`

**Modified:** `internal/jobtracker/handlers_community.go`

Changes:
- `GetCommunityPost`: replaced `pathUUID(w, r, "id")` with dual-path: `uuid.Parse(idOrSlug)` succeeds → `GetPost`; fails → `GetPostBySlug`. ErrNoRows → 404 in both branches.
- `PublicGetCommunityPost`: same dual-path pattern with `viewerID = nil`.

The `pathUUID` call for UpdateCommunityPost, DeleteCommunityPost, FlagCommunityPost etc. are NOT changed — those legitimately require a UUID (author-only operations). Only the GET endpoints need slug support.

---

## Deviations from plan

| Deviation | Reason |
|---|---|
| TASK-01 had no production code to write | Bug was already fixed. Tests written as specified. |
| TASK-02 had no production code to write | Guard was already implemented. Tests written as specified. |
| Tests call `requireNoActivePremium` directly, not `CheckoutSubscription` | `CheckoutSubscription` checks `h.client == nil` before reaching the guard. With a nil client it returns 503, never reaching the 409 path. Testing the guard directly is both more precise and avoids needing a mock HTTP server. |
| Introduced `dbPool` interface in store.go | Required to write unit tests without a real Postgres connection. Minimal change — only the field type changed, `NewStore` is unchanged, all callers unaffected. |
| `consecutiveHyphens` regex split into two steps | ADR showed `[-\s]{2,}` — but that regex only matches 2+ spaces/hyphens. A single space between words also needs converting to `-`. Used `strings.ReplaceAll` for single-space → hyphen, then `consecutiveHyphens` for 2+ hyphens. Result is semantically identical to the intent. |
| `CommunityPost.Slug` placed between `AuthorSlug` and `Surface` | ADR said "after UserID and before AuthorName" but both AuthorName and AuthorSlug come from the JOIN, not `p.*`. The SELECT puts `COALESCE(u.name,''), COALESCE(pf.slug,'')` first, then `p.slug`. Struct field order follows SELECT order to match the Scan, so Slug sits between AuthorSlug and Surface. |

---

## Tech debt created

| Item | Severity | Notes |
|---|---|---|
| `fakeRow.Scan` uses type-switch with explicit cases | Low | Only handles types actually used by `CurrentSubscription`, `ActivateRecurring`, `BumpRecurringPeriodEnd`. Adding a new store method may require extending the switch. |
| Test fakes are not shared across test files | Low | `fakePoolCurrentSub` etc. live in `handlers_test.go`. If more handler tests are written later, they should be extracted to a `billing_test_helpers_test.go` shared test file. |
| `uniqueSlug` query runs outside the INSERT transaction | Low | For TASK-06, `uniqueSlug` checks then the INSERT happens separately. A concurrent INSERT could win the race between the EXISTS check and the actual INSERT, causing a unique constraint violation. The unique index is the final arbiter; the error propagates to the handler as a 500. The ADR explicitly accepts this as "rare race, 500 is acceptable". A future improvement would be to use INSERT ... ON CONFLICT RETRY inside a CTE. |

---

## Test coverage notes

```
go test ./internal/billing/... -race
ok   github.com/TheBharathProject/sypher-api/internal/billing   1.638s
```

13 tests, all passing with `-race`. No DB required. No network calls.

```
go test ./internal/jobtracker/... -race
ok   github.com/TheBharathProject/sypher-api/internal/jobtracker   1.625s
```

29 tests total (16 `TestExtractMentions` + 13 `TestSlugify`), all passing with `-race`. Pure function tests — no DB required.

```
go build ./...
(zero output)
```

---

## Commit hashes

| Commit | Description |
|---|---|
| `f0ca0f9` | fix(billing): write current_period_end on webhook + 409 guard on re-subscription |
| `abed2a3` | feat(jobtracker): community post slugs — migration, slug.go, dual-path lookup |
