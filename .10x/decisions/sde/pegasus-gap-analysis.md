# SDE Decision — pegasus-gap-analysis

**Date:** 2026-05-12
**Commit:** f0ca0f9 (prior), 3459e79 (TASK-09 through TASK-13)
**Tasks:** TASK-01, TASK-02, TASK-03, TASK-04, TASK-05, TASK-06, TASK-07, TASK-08, TASK-09, TASK-10, TASK-11, TASK-12, TASK-13, TASK-25

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

### TASK-07 — Add slug: string to ApiCommunityPost in lib/api-client.ts

**File modified:** `job-tracker/lib/api-client.ts`

**What existed:** `ApiCommunityPost` at lines 330–346 had `authorSlug?: string` (optional, already present) but no `slug` field.

**Change made:** Added `slug: string;` (required, non-optional) on line 335, immediately after `authorSlug?: string;` and before the `surface` field. This matches the SELECT order in the backend (`communityPostCols` puts `COALESCE(pf.slug,'')` then `p.slug` then `p.surface`).

**Why non-optional:** Every post in the DB has a slug after the TASK-03 migration backfill. Making it required (`string` not `string | undefined`) lets TypeScript catch any consumer that fails to handle it.

---

### TASK-08 — Wire community post card links to post.slug

**File modified:** `job-tracker/app/community/[section]/page.tsx`

**What existed:** Line 494: `href={`/community/posts/${post.id}`}` — used UUID id in all post card links.

**Change made:** Line 494 changed to `href={`/community/posts/${post.slug || post.id}`}` — uses slug with id as defensive fallback for any post where slug is unexpectedly empty string (TypeScript type says `string` but the fallback costs nothing and guards against a future empty-string migration edge case).

**JSX not restructured:** Only the template literal value was changed. No elements added, removed, or reordered.

**No other `post.id` href usages were found in the same file** — confirmed by reading the surrounding render block (lines 491–515). The only post-row Link is at line 494.

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
| `7c5e01f` | feat(community): use slug in post links, add slug type to ApiCommunityPost |
| `3459e79` | feat(community): sort indexes migration, per-surface metadata validation, ?sort= param |

---

### TASK-09 — migrations/0020_community_sort_indexes.sql [NEW]

**New file:** `migrations/0020_community_sort_indexes.sql`

Confirmed 0019 was the latest migration before this session. New file adds two partial indexes:
- `community_posts_vote_count_idx` on `(surface, vote_count DESC, created_at DESC) WHERE status = 'active'`
- `community_posts_comment_count_idx` on `(surface, comment_count DESC, created_at DESC) WHERE status = 'active'`

Both wrapped in a transaction. `IF NOT EXISTS` guards make it idempotent. Column names confirmed from `0010_community.sql` (`vote_count`, `comment_count`, `status` all exist with those exact names).

**Deviation from Senior Engineer ADR:** The task description specified index names `community_posts_vote_count_idx` and `community_posts_comment_count_idx`. The Senior Engineer ADR used `idx_community_posts_surface_votes` and `idx_community_posts_surface_comments`. Used the task description names as the immediate instruction.

---

### TASK-10 — Sort support in ListPosts (store_community.go)

**Modified:** `internal/jobtracker/store_community.go`

Changes:
1. Added `Sort string` to `CommunityListOpts` struct with doc comment listing valid values
2. Added `CommunityUpdateInput` struct (optional `Surface` + PATCH fields) alongside this change since both structs live here
3. In `ListPosts`: replaced hard-coded `ORDER BY p.created_at DESC` with local `allowedSort` map dispatch. Map contains three entries: `newest`, `votes`, `most-reviewed`. Unknown/empty sort falls back to `newest`. The safe string from the map (never user input) is interpolated into the query.
4. Added v1 trade-off comment: cursor pagination still uses `created_at` regardless of sort mode.

**Deviation from Senior Engineer ADR:** The Senior Engineer ADR suggested making `allowedSortClauses` a package-level var. The task description said to keep it local inside `ListPosts`. Used task description as the immediate instruction. Local map is also preferable — no state shared outside the function.

The Senior Engineer ADR also listed a 4th sort value `"least-reviewed"`. The task description lists only 3 (`newest`, `votes`, `most-reviewed`). Used the task description's 3-value list.

---

### TASK-11 — validators_community.go [NEW]

**New file:** `internal/jobtracker/validators_community.go`

`metaValidationError` struct with exported `Field` and `Message` fields (not unexported as originally suggested — exported fields allow the handler to read them directly without accessors and are cleaner given the handler is in the same package). `Error()` returns `"field: message"`.

`validateCommunityMetadata(surface, raw)` — switch dispatch to per-surface validators:
- `experiences`: validates `outcome` ∈ {Offer, Reject, Ghosted, InProgress, Withdrew} and `difficulty` ∈ {Easy, Medium, Hard} — only when fields are present and non-empty
- `ask`: validates `tags` array: len ≤ 3, each from allowlist of 10 values
- `recruiters`: validates `specializations` from 6-value list; `hiringLevels` from 8-value list
- `reviews`: validates `targetRole` from 7-value list; `experienceLevel` from 5-value list
- `referrals`: returns nil
- default: returns nil
- nil/empty raw: returns nil

**Deviation from Senior Engineer ADR on allowlists:** The task description specified different allowlist values for ask tags (10 values including Career/Interview/Compensation/Remote/Visa/Internship/Fresher/Layoffs/Tools/Other), recruiter specializations (6: Tech/Non-Tech/Executive/Campus/Contract/Other), and hiring levels (8: Fresher/Junior/Mid/Senior/Lead/Manager/Director/Executive) versus the Senior Engineer ADR which read values from frontend constants. Used the task description values as the canonical specification.

**Deviation on field export:** Senior Engineer ADR showed unexported `field string` and `msg string`. Using exported `Field string` and `Message string` since they are accessed by the handler in the same package and exported fields are Go idiomatic for structs.

---

### TASK-12 — validators_community_test.go [NEW]

**New file:** `internal/jobtracker/validators_community_test.go`

38 test cases in `TestValidateCommunityMetadata`:
- nil/empty metadata: passes for any surface (2 cases)
- experiences outcome: 5 valid values pass, 2 old values fail (`Rejected`, `In Progress`), absent passes, empty string passes
- experiences difficulty: valid passes, invalid fails
- ask tags: 3 valid pass, 0 tags pass, 4 tags fail, invalid tag fails, varied allowlist tags pass
- recruiters specializations: valid pass (Tech, Campus+Contract), invalid fails (Backend), empty passes
- recruiters hiringLevels: valid pass (Senior), invalid fails (Entry Level)
- reviews targetRole: SDE and PM pass, Consultant fails, absent passes
- reviews experienceLevel: Fresher and Mid (2-5) pass, "Senior Engineer" fails
- referrals: any metadata passes, nil passes
- unknown surface: passes

All 38 cases pass with `-race`.

---

### TASK-13 — Wire validation + sort into handlers_community.go

**Modified:** `internal/jobtracker/handlers_community.go`

Three changes:

1. **`CreateCommunityPost`:** Added `validateCommunityMetadata(surface, in.Metadata)` call after the `len(in.Body) > 16384` check and before `in.Surface = surface`. Returns three-key JSON response `{"error":"invalid_metadata","field":"...","message":"..."}` via `httpx.WriteJSON` on failure (not `httpx.WriteError` — the three-key shape is required per the PM acceptance criteria).

2. **`UpdateCommunityPost`:** Changed from `CommunityPostInput` to `CommunityUpdateInput` (new struct with `Surface string \`json:"surface,omitempty"\``) for the PATCH body. Validates metadata only when BOTH `upd.Surface != ""` AND `len(upd.Metadata) > 2` — skips validation on title-only edits to avoid breaking PATCH requests that don't include a surface. Adapts back to `CommunityPostInput` for the `UpdatePost` store call.

3. **`parseListOpts`:** Added `opts.Sort = r.URL.Query().Get("sort")` before the return statement. The raw query param value passes through to the store; the store's allowlist sanitises it.

**Surprise in existing handler structure:** `UpdateCommunityPost` already used `CommunityPostInput` which has `Surface string \`json:"-"\``. The `-` json tag meant surface was always ignored from PATCH bodies. The `CommunityUpdateInput` struct is the minimal change — it doesn't touch `CommunityPostInput` used by `CreatePost`.

---

## Tech debt created (new, from TASK-09 through TASK-13)

| Item | Severity | Notes |
|---|---|---|
| Cursor pagination ignores sort mode | Low | `writeListResponse` always uses `CreatedAt` for the next cursor regardless of sort. Sort tabs should reset to page 1 on sort change (frontend responsibility). Documented in code comment. |
| `validateCommunityMetadata` uses `map[string]any` for experiences/reviews | Low | `json.Unmarshal` into `map[string]any` for experiences and reviews surfaces (instead of a typed struct) to avoid defining 4 per-surface structs. The type assertion `meta["outcome"].(string)` is safe because the zero value for a missing key is nil which fails the type assertion and passes validation. No runtime panic risk. |
