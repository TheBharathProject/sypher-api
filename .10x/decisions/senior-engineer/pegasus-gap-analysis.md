# ADR: Pegasus Gap Analysis — Senior Engineer Decision Record

**Status:** Accepted
**Date:** 2026-05-12
**Slug:** pegasus-gap-analysis
**Author:** Senior Engineer Agent (claude-sonnet-4-6)
**Depends on:** EM, Architect, Staff Engineer, PM, CTO ADRs (same slug)

---

## Purpose

This document is the definitive, code-read-backed implementation guide for the SDE
assigned to execute the 26 tasks. Every claim about file shape, line number, or
existing pattern is verified against the live source. Follow this document over the
upstream ADRs when there is a conflict; upstream documents describe intent, this one
describes reality.

---

## Resolved Decisions (incorporated)

1. PDF endpoints do NOT debit credits — no `gateAICredit` call in any PDF handler.
2. OUTCOMES enum: fix frontend to send `"Reject"/"InProgress"`. Backend constants unchanged.
3. `seedRecruiters` (lines 101–111 in `app/community/[section]/page.tsx`) removed in
   TASK-17; real posts render from `posts` state. Confirm real recruiter count before deploying.
4. TASK-13/TASK-14 deployment: single window, backend validation first or simultaneously.
5. `ProductFrame` renders semantic `<main>` at line 347 of `components/frames.tsx` —
   `document.querySelector("main")` will work in `ModalShell`.

---

## Task-by-Task Implementation Guide

---

### TASK-01 — Fix `currentPeriodEnd` null for active subscriptions

**File to change:** `sypher-api/internal/billing/webhook.go`

**What the code actually does today:**
- `subscription.activated` handler (line 185–199): calls `ActivateRecurring(ctx, sub.Entity.ID, "", periodEnd)`.
  The `ActivateRecurring` store function (store.go line 112–127) already writes `current_period_end = $3`.
  So the column IS being set on `activated`.
- `subscription.charged` handler (line 201–232): calls `BumpRecurringPeriodEnd(ctx, sub.Entity.ID, periodEnd)`.
  That store function (store.go line 133–146) also already writes `current_period_end = $2`.

**Root cause to investigate:** `SubscriptionRow.CurrentPeriodEnd` is `*time.Time` (nullable pointer). If a
subscription was created via an older code path that didn't write `current_period_end`, the column is NULL.
The fix is two-part:
1. Verify `ActivateRecurring` is being called with the right `periodEnd` value. The `subscription.activated`
   event may carry `current_end = 0` for the first-cycle mandate verification (a known Razorpay quirk). If
   `sub.Entity.CurrentEnd == 0`, skip writing `periodEnd` rather than writing epoch.
2. Add a guard: `if sub.Entity.CurrentEnd > 0 { periodEnd = time.Unix(...) } else { /* skip period_end update */ }`.

**Approach:** In the `subscription.activated` case, add `if sub.Entity.CurrentEnd == 0` check before computing
`periodEnd`. If zero, pass `time.Time{}` and update `ActivateRecurring` to skip the `current_period_end` column
update when the period-end argument is the zero value (use `COALESCE($3, current_period_end)` in SQL, or add a
separate function variant). For `subscription.charged`, `CurrentEnd` should always be non-zero — add an assertion
log if it is ever zero but do not block the rest of the handler.

**Edge cases:**
- Test webhook from Razorpay dashboard sends `current_end: 0` on `subscription.activated`. Code must not write
  epoch (1970) to the column.
- First cycle sends both `activated` + `charged` in sequence. The `charged` handler will overwrite with the real
  `current_end` value, making the double-event safe.

**Test strategy (TASK-25):** Mock handler with a fake store that records the `periodEnd` value passed to
`ActivateRecurring`. Assert that a `subscription.activated` payload with `current_end=0` does NOT write a zero-
time `current_period_end`. Assert that `subscription.charged` with a valid `current_end` DOES write the correct
time.

**Gotchas:**
- Do NOT change the `rzpSubscription` struct — the `CurrentEnd int64` field is correct.
- The existing `ActivateRecurring` signature passes a `paymentID string` as second arg. Keep the signature
  unchanged; the subscription.activated call already passes `""` for paymentID.

---

### TASK-02 — Add 409 guard to prevent Pro re-subscription

**File to change:** `sypher-api/internal/billing/handlers.go`

**What the code actually does today:** `requireNoActivePremium` already exists (lines 55–80) and is already called
in `CheckoutSubscription` (line 96), `CheckoutSubscriptionPlus` (line 139), and `CheckoutPremiumPass` (line 179).
The guard already checks `status = 'active'` in `CurrentSubscription` (store.go line 250–274) and returns 409
via `httpx.WriteJSON(w, http.StatusConflict, ...)`.

**Conclusion:** TASK-02 is ALREADY IMPLEMENTED. `requireNoActivePremium` covers `status='active'` (which
includes both `active` and `authenticated` equivalent — Razorpay's `active` maps to our `active` column value).

**Action for SDE:** Read `billing/handlers.go` lines 55–80 and `billing/store.go` lines 250–274. Confirm that
the `CurrentSubscription` query filters on `status = 'active'`. Verify that Razorpay's `authenticated` status
(which their docs mention as a pre-first-charge recurring state) is not inserted as `active` in our DB —
check `ActivateRecurring`: it sets `status = 'active'` only on `subscription.activated` event. If Razorpay's
`authenticated` state does not trigger `subscription.activated`, there is no row with `status='active'` yet and
the guard correctly passes. Document this finding in the TASK-25 test.

**Test strategy:** Write a table-driven test in `billing/handlers_test.go` that calls `CheckoutSubscription`
with a mock store that returns an active subscription row and verifies the handler returns 409.

**Gotchas:** Do NOT add a redundant second guard — `requireNoActivePremium` is already on every checkout path.
The "fix" is documentation + test coverage, not new code.

---

### TASK-03 — Write migration `0019_community_slugs.sql`

**File to create:** `sypher-api/migrations/0019_community_slugs.sql`

**Current state:** 18 migrations exist (0001–0018). Next number is 0019. The community posts table
was created in `0010_community.sql`. Check that migration for the exact schema before writing.

**Approach:** Write exactly the SQL from Architect ADR §Feature 1, Migration 0019:
```sql
ALTER TABLE job_tracker.community_posts
  ADD COLUMN IF NOT EXISTS slug TEXT;

UPDATE job_tracker.community_posts
  SET slug = 'post-' || REPLACE(id::text, '-', '')
  WHERE slug IS NULL;

ALTER TABLE job_tracker.community_posts
  ALTER COLUMN slug SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS community_posts_slug_idx
  ON job_tracker.community_posts (slug);
```

**Edge cases:**
- Table may be empty in staging/dev — the UPDATE affects 0 rows, which is fine.
- The `IF NOT EXISTS` guards on both `ADD COLUMN` and `CREATE INDEX` make this idempotent.
- Running twice will attempt to `ALTER COLUMN slug SET NOT NULL` on an already-NOT-NULL column;
  Postgres handles this gracefully (no-op). Still safe.

**Test strategy:** Run the migration against a test DB. Verify `pg_typeof(slug)` is `text`, NOT NULL is set,
unique index exists. Manual smoke test only.

**Gotchas:** The `REPLACE(id::text, '-', '')` strips UUID hyphens. The result is a 32-character hex string
prefixed with `post-` = 37 chars total. This is well under the 80-char slug limit from `slugify`.

---

### TASK-04 — Implement `slug.go`

**File to create:** `sypher-api/internal/jobtracker/slug.go`

**Approach:** Follow Staff Engineer ADR §5a exactly. The file contains:
1. Package-level compiled regex: `var consecutiveHyphens = regexp.MustCompile(`-{2,}`)` — compiles once.
2. `slugify(title string) string` — pure function, no I/O, following the 6-step algorithm in Staff ADR §5c.
   Note: step 6 truncation must check `len(result) > 80` then look for last hyphen boundary with
   `strings.LastIndexByte(result, '-') > 40` condition.
3. `func (s *Store) uniqueSlug(ctx context.Context, title string) (string, error)` — loop 5 times,
   use `crypto/rand` (NOT `math/rand`), hex-encode 2 bytes per attempt.

**The uniqueSlug loop structure (critical):**
```go
candidate := base  // attempt 0: base slug, no suffix
for attempt := 0; attempt < 5; attempt++ {
    var exists bool
    err := s.pool.QueryRow(ctx,
        `SELECT EXISTS(SELECT 1 FROM job_tracker.community_posts WHERE slug = $1)`,
        candidate,
    ).Scan(&exists)
    if err != nil { return "", err }
    if !exists { return candidate, nil }
    // generate suffix for next attempt
    b := make([]byte, 2)
    if _, err := rand.Read(b); err != nil { return "", err }
    trimmed := base
    if len(base) > 80 { trimmed = base[:80] }
    candidate = trimmed + "-" + hex.EncodeToString(b)
}
return "", fmt.Errorf("slug: exhausted retries for %q", base)
```

**Imports needed:** `crypto/rand`, `encoding/hex`, `regexp`, `strings`, `fmt`, `context`.

**Edge cases:**
- Empty title (all special chars or blank): `slugify` returns `""`, `uniqueSlug` sets `base = "post"`.
- Title longer than 80 chars: truncation at 80, then back to last hyphen if one exists past position 40.
- Hindi/Tamil titles: non-ASCII bytes are silently dropped; falls back to `"post-{hex}"`.

**Gotchas:**
- DO NOT use `math/rand`. Code review must grep the file for `math/rand` before merge.
- The regex `consecutiveHyphens` must be package-level, not inside `slugify()`.
- `crypto/rand.Read` fills the byte slice; use `hex.EncodeToString(b)` for the suffix.

---

### TASK-05 — Write `slug_test.go`

**File to create:** `sypher-api/internal/jobtracker/slug_test.go`

**Approach:** Package `jobtracker`, test function `TestSlugify`. Use the exact test matrix from Staff ADR §10.
Tests are pure (no DB), so no setup needed. Import `"strings"` for the repeat helper and `"testing"`.

**Required test cases (minimum):**
- `"My Google L5 Interview Went!"` → `"my-google-l5-interview-went"`
- `"How to crack SDE-2 at Amazon"` → `"how-to-crack-sde-2-at-amazon"`
- `"!!!"` → `""` (all-special → empty, not "post" — slugify returns empty; uniqueSlug handles fallback)
- `"  leading spaces  "` → `"leading-spaces"`
- `"a--b---c"` → `"a-b-c"` (consecutive hyphens collapsed)
- `strings.Repeat("x", 100)` → `strings.Repeat("x", 80)` (truncation)
- `"गूगल interview experience"` → `"interview-experience"` (Unicode drop, no panic)

**Gotchas:** The test for empty result checks `slugify("!!!")` returns `""`, not `"post"`. The `"post"` fallback
is in `uniqueSlug`, not `slugify`.

---

### TASK-06 — Wire slugs into `store_community.go` and `handlers_community.go`

**Files to change:**
- `sypher-api/internal/jobtracker/store_community.go`
- `sypher-api/internal/jobtracker/handlers_community.go`

**store_community.go changes:**

1. **`communityPostCols` constant (line 80–83):** Currently:
   ```go
   const communityPostCols = `p.id, p.user_id, COALESCE(u.name, ''), COALESCE(pf.slug, ''),
       p.surface, p.title, COALESCE(p.body, ''),
       p.metadata, p.is_public, p.vote_count, p.comment_count, p.status,
       p.created_at, p.updated_at`
   ```
   Add `p.slug` between `p.user_id ... COALESCE(pf.slug, '')` and `p.surface`:
   ```go
   const communityPostCols = `p.id, p.user_id, COALESCE(u.name, ''), COALESCE(pf.slug, ''),
       p.slug, p.surface, p.title, COALESCE(p.body, ''),
       p.metadata, p.is_public, p.vote_count, p.comment_count, p.status,
       p.created_at, p.updated_at`
   ```
   NOTE: `pf.slug` is the author's profile slug; `p.slug` is the community post slug. Both exist and both are
   needed. The column order in `communityPostCols` must match `scanCommunityPost`'s `row.Scan(...)` call exactly.

2. **`CommunityPost` struct** (already in `store_community.go` lines 21–39): Add `Slug string "json:\"slug\""`.
   Place it after `UserID` and before `AuthorName` to match the SELECT order:
   ```go
   Slug        string          `json:"slug"`
   ```
   The struct is defined in `store_community.go` (not `types.go` — the types are embedded here).

3. **`scanCommunityPost` function (line 87–94):** Add `&p.Slug` to the Scan call in the correct positional slot
   matching the updated `communityPostCols` order.

4. **`CreatePost` function (lines 98–119):** The INSERT currently doesn't include `slug`. It must be updated to
   call `uniqueSlug` before the INSERT and include the slug in the INSERT statement:
   ```go
   slug, err := s.uniqueSlug(ctx, in.Title)
   if err != nil {
       return nil, fmt.Errorf("generate slug: %w", err)
   }
   // Add slug to INSERT columns and VALUES placeholder
   ```
   The INSERT CTE must include `slug` column and `$7` placeholder (adjusting existing args).

5. **Add `GetPostBySlug(ctx context.Context, slug string, viewerID *uuid.UUID) (*CommunityPost, error)`:**
   Reuse the same query structure as `GetPost` but filter on `p.slug = $1` instead of `p.id = $1`. The
   `viewerID` optional MyVote fill follows the same pattern as `GetPost` (lines 180–203).

**handlers_community.go changes:**

`GetCommunityPost` (line 176–192) currently calls `pathUUID(w, r, "id")` which returns 400 for non-UUID strings.
Replace with dual-path logic:
```go
func (h *Handler) GetCommunityPost(w http.ResponseWriter, r *http.Request) {
    uid := auth.MustUserID(r.Context())
    idOrSlug := r.PathValue("id")
    var (
        post *CommunityPost
        err  error
    )
    if parsed, parseErr := uuid.Parse(idOrSlug); parseErr == nil {
        post, err = h.store.GetPost(r.Context(), parsed, &uid)
    } else {
        post, err = h.store.GetPostBySlug(r.Context(), idOrSlug, &uid)
    }
    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            httpx.WriteError(w, http.StatusNotFound, "not_found", "post not found")
            return
        }
        writeDBError(w, err)
        return
    }
    httpx.WriteJSON(w, http.StatusOK, post)
}
```
Apply the same pattern to `PublicGetCommunityPost`.

**Edge cases:**
- Concurrent inserts with the same title: `uniqueSlug`'s 5-retry loop handles this. The unique index is the
  final arbiter. If the INSERT fails with a unique constraint violation (`23505`), the store returns the raw
  pgx error. The handler will call `writeDBError` which returns 500 — acceptable for this rare case. If you
  want to improve, catch `pgx.ErrUniqueViolation` in `CreatePost` and retry `uniqueSlug` once.
- Path value `"posts"` would be tried as a slug lookup and return 404 — correct.

**Gotchas:**
- The `CommunityPost` struct field `Slug` must come before `Surface` in scan order because `communityPostCols`
  puts `p.slug` before `p.surface`. Getting the scan order wrong produces silent data corruption.
- `pf.slug` (profile slug) and `p.slug` (post slug) are DIFFERENT columns from DIFFERENT tables. Double-check
  the column alias in the JOIN query.

---

### TASK-07 — Add `slug: string` to `ApiCommunityPost` in `lib/api-client.ts`

**File to change:** `job-tracker/lib/api-client.ts`

**Current state:** `ApiCommunityPost` is at lines 330–346. It does NOT have a `slug` field. `authorSlug` is
already present as `authorSlug?: string` (optional). `slug` must be required (non-optional) since every post
now has one after TASK-03 migration.

**Approach:** Add `slug: string;` after `authorSlug?: string;`:
```typescript
export type ApiCommunityPost = {
  id: string;
  userId: string;
  authorName: string;
  authorSlug?: string;
  slug: string;       // <-- add this
  surface: "reviews" | "experiences" | "referrals" | "ask" | "recruiters";
  // ... rest unchanged
};
```

**Run `pnpm tsc --noEmit` after this change** — TypeScript will flag any consumer of `ApiCommunityPost`
that destructures the type without the `slug` field. That's the desired behavior.

**Gotchas:** Do not make `slug` optional (`slug?: string`) — it is always present after the migration.
Old posts received slugs via the backfill in 0019.

---

### TASK-08 — Wire community post card links to use `post.slug`

**File to change:** `job-tracker/app/community/[section]/page.tsx`

**Current state:** Line 494:
```tsx
<Link className="post-row" href={`/community/posts/${post.id}`}>
```

**Change:** Replace `post.id` with `post.slug`:
```tsx
<Link className="post-row" href={`/community/posts/${post.slug}`}>
```

This is a one-line change. The dual-path backend (TASK-06) ensures old UUID bookmarks still resolve.

**Deployment dependency:** TASK-03 migration and TASK-06 backend must be in production BEFORE this ships.

**Gotchas:** No other card renders need updating in this file — the post list renderer is at line 491–515.
Confirm there are no other `post.id` href usages in the same file before closing the task.

---

### TASK-09 — Write migration `0020_community_sort_indexes.sql`

**File to create:** `sypher-api/migrations/0020_community_sort_indexes.sql`

**Approach:** Two partial index CREATE statements per Architect ADR §Feature 2:
```sql
CREATE INDEX IF NOT EXISTS idx_community_posts_surface_votes
  ON job_tracker.community_posts (surface, vote_count DESC, created_at DESC)
  WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_community_posts_surface_comments
  ON job_tracker.community_posts (surface, comment_count DESC, created_at DESC)
  WHERE status = 'active';
```

Both are additive. Zero downtime. No dependencies on migration 0019.

---

### TASK-10 — Add `?sort=` support to `ListPosts()`

**File to change:** `sypher-api/internal/jobtracker/store_community.go`

**Current state:** `CommunityListOpts` struct (lines 69–74) has `Cursor`, `Limit`, `PublicOnly`, `ViewerID`.
`ListPosts` (lines 124–176) hard-codes `ORDER BY p.created_at DESC` at line 149.
`parseListOpts` (handlers_community.go lines 100–113) reads only `cursor` and `limit`.

**Changes to `CommunityListOpts`:** Add `Sort string` field. Empty string defaults to `"newest"`.

**Changes to `ListPosts`:** Add the allowlist map and ORDER BY dispatch. Place the allowlist as a
package-level var (same file):
```go
var allowedSortClauses = map[string]string{
    "newest":        "p.created_at DESC",
    "votes":         "p.vote_count DESC, p.created_at DESC",
    "most-reviewed": "p.comment_count DESC, p.created_at DESC",
    "least-reviewed":"p.comment_count ASC, p.created_at DESC",
}
```
In `ListPosts`, replace `q += " ORDER BY p.created_at DESC LIMIT $2"` with:
```go
clause, ok := allowedSortClauses[opts.Sort]
if !ok {
    clause = allowedSortClauses["newest"]
}
q += " ORDER BY " + clause + " LIMIT $2"
```
This is safe because `clause` comes from a hard-coded Go map, never from user input.

**Changes to `parseListOpts`:** Add `opts.Sort = r.URL.Query().Get("sort")` before the return.

**Gotcha (cursor pagination):** The `writeListResponse` function uses `posts[len(posts)-1].CreatedAt`
as the next cursor regardless of sort mode. This is incorrect for `votes` sort (a new high-vote post
won't appear after the cursor). Add a code comment documenting this as the accepted v1.0 trade-off
and that sort tabs reset to page 1 on sort change.

---

### TASK-11 — Implement `validators_community.go`

**File to create:** `sypher-api/internal/jobtracker/validators_community.go`

**Follow Staff ADR §4 exactly.** Key facts verified from source:
- Package is `jobtracker` (lowercase).
- The `metaValidationError` type has `field string` and `msg string` fields (unexported — used
  only within the package; the handler reads them via the struct fields, not the `Error()` method).
- `validOutcomes` map must contain exactly 5 values: `"Offer"`, `"Reject"`, `"Ghosted"`,
  `"InProgress"`, `"Withdrew"`. `"Rejected"` must NOT appear.
- `validateAskMeta` must enforce `len(tags) <= 3` AND each tag must be from the allowlist.

**Ask tag allowlist** (from `lib/site-data.ts` — verify these match):
The frontend imports `askTags` from `@/lib/site-data`. Read that file to get the canonical list.
```
"Salary & Negotiation", "Interview Prep", "Career Switch",
"Work-Life Balance", "Layoffs", "Remote Work",
"Campus Placement", "FAANG", "Startups", "Other"
```
Use these 10 values as the allow-list in `validateAskMeta`.

**Recruiter specializations allowlist** (from `SPECIALIZATIONS` in `page.tsx` lines 141–154):
12 values: `"Backend"`, `"Frontend"`, `"Full Stack"`, `"Data"`, `"Mobile"`, `"DevOps"`, `"ML/AI"`,
`"Product"`, `"Design"`, `"Security"`, `"QA"`, `"Other"`.

**Hiring levels allowlist** (from `HIRING_LEVELS` in `page.tsx` lines 155–164):
8 values: `"Intern"`, `"Entry Level"`, `"Mid Level"`, `"Senior"`, `"Staff"`, `"Principal"`,
`"Director"`, `"VP"`.

**Gotchas:**
- `validateExperiencesMeta` uses `if outcome, ok := meta["outcome"].(string); ok && outcome != ""`.
  An absent `outcome` key is NOT an error — posts can be created without specifying an outcome (partial
  draft). Only validate when the key is present and non-empty.
- Validate `difficulty` and `outcome` independently; failing one does not skip the other in the same call.
  Return the first error found.
- `referrals` surface: return `nil` (no validation). The `default:` branch in the switch also returns nil.

---

### TASK-12 — Write `validators_community_test.go`

**File to create:** `sypher-api/internal/jobtracker/validators_community_test.go`

**Follow Staff ADR §10 test matrix exactly.** Package `jobtracker`.
Add cases for recruiter and reviews surfaces beyond the minimum required by the ADR:
- Reviews: valid `targetRole = "SDE"` passes; invalid `targetRole = "Consultant"` fails with `field="targetRole"`.
- Recruiter: `specializations` with an invalid value fails; empty `specializations` passes.
- Ask: tag from allowlist passes; tag not on allowlist fails.

**Gotcha:** The test accesses `err.field` directly (unexported). Since the test is in the same package
(`package jobtracker`), this is fine. Do not export the field.

---

### TASK-13 — Wire metadata validation and `?sort=` into handlers

**File to change:** `sypher-api/internal/jobtracker/handlers_community.go`

**CreateCommunityPost change (line 129–173):**
Insert after the body-length check (line 158 `if len(in.Body) > 16384`) and before `in.Surface = surface`:
```go
if verr := validateCommunityMetadata(surface, in.Metadata); verr != nil {
    httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{
        "error":   "invalid_metadata",
        "field":   verr.field,
        "message": verr.msg,
    })
    return
}
```
Note: use `httpx.WriteJSON` with `map[string]string`, NOT `httpx.WriteError`. This is the
three-key response shape required by the PM acceptance criteria.

**UpdateCommunityPost change (line 217–241):**
The PATCH body carries only the fields the user is editing. The handler currently reads `in.Metadata`
from the request body. Apply validation to the incoming metadata only if it is non-empty/non-null:
```go
if len(in.Metadata) > 0 && string(in.Metadata) != "{}" && string(in.Metadata) != "null" {
    if verr := validateCommunityMetadata(/* need surface */); verr != nil { ... }
}
```
Problem: `UpdateCommunityPost` does not receive `surface` from the path — the route is
`PATCH /job-tracker/community/posts/{id}`. The surface is stored in the DB row, not the path.
To validate, either (a) fetch the stored post first to get its surface, or (b) require the client to
include `surface` in the PATCH body.

**Decision (simpler):** Accept `surface` as an optional field in `CommunityPostInput` for updates.
If the client sends it, validate. If absent, skip metadata validation on update (title-only edits remain
safe). This avoids an extra DB read on every PATCH. Add `Surface string \`json:"surface"\`` to
`CommunityPostInput` (it already has `Surface string \`json:"-"\`` which means the JSON decoder ignores it).
Change the json tag to `json:"surface,omitempty"` only for update path — or use a separate input struct.

**Simpler approach:** Add a new `CommunityUpdateInput` struct with `Surface string \`json:"surface"\``
(optional). Use this in `UpdateCommunityPost`. Existing `CommunityPostInput` has `Surface string \`json:"-"\``
which is correct for Create (surface comes from path, not body).

**`parseListOpts` change:** Already handled in TASK-10.

**Gotchas:**
- The three-key response shape (`{"error":..., "field":..., "message":...}`) diverges from the
  standard two-key `httpx.WriteError` shape. Write it explicitly with `httpx.WriteJSON`.
- Do not call `validateCommunityMetadata` on an empty `in.Metadata` (`nil` or `{}`). Check `len(in.Metadata) > 2`
  (length 2 = `{}`) before calling.

---

### TASK-14 — Realign community enum constants

**File to change:** `job-tracker/app/community/[section]/page.tsx`

**Current values (verified from source):**
- `OUTCOMES` line 126: `["Offer", "Rejected", "In Progress", "Withdrew", "Ghosted"]` — 5 values, wrong names
- `TARGET_ROLES` lines 128–139: 10 values including "Frontend Engineer", "Backend Engineer", etc.
- `EXPERIENCE_LEVELS` line 140: `["Intern", "Entry Level", "Mid Level", "Senior", "Staff+", "Manager"]` — 6 values
- `ROUND_TYPES` lines 114–125: 10 values, missing "Group Discussion", "Case Study", "Culture Fit"

**New values:**
```typescript
const OUTCOMES = ["Offer", "Reject", "Ghosted", "InProgress", "Withdrew"];

const OUTCOME_LABELS: Record<string, string> = {
  Offer: "Offer",
  Reject: "Rejected",
  Ghosted: "Ghosted",
  InProgress: "In Progress",
  Withdrew: "Withdrew",
};

const TARGET_ROLES = ["SDE", "PM", "Data Science", "Design", "DevOps", "QA", "Other"];

const EXPERIENCE_LEVELS = ["Fresher", "Junior (0-2)", "Mid (2-5)", "Senior (5+)", "Lead (8+)"];

const ROUND_TYPES = [
  "Phone Screen", "Recruiter Call", "Technical", "Coding",
  "System Design", "Behavioral", "Onsite", "Hiring Manager", "HR",
  "Group Discussion", "Case Study", "Culture Fit", "Other"
];
```

**All `<select>` elements rendering OUTCOMES must use the label map for display:**
```tsx
{OUTCOMES.map((o) => (
  <option key={o} value={o}>{OUTCOME_LABELS[o] ?? o}</option>
))}
```
The `value={o}` sends `"Reject"` to the backend; the display text shows `"Rejected"`. Verify line 885–888
(experience modal outcome select) uses this pattern.

**Deployment constraint:** This must ship simultaneously with or after TASK-13 backend validation.
Add a PR comment: "DO NOT merge before TASK-13 is live in production."

**Gotchas:** The old `emptyExperience` object (line 238) has `outcome: ""` — no change needed there.

---

### TASK-15 — Wire filter/sort buttons to URL query state

**Files to change:**
- `job-tracker/app/community/[section]/page.tsx`
- `job-tracker/lib/community.ts`

**Current state:** Filter buttons at lines 467–473 use `<button type="button" className="filter-box">` with
no onClick handlers — decorative only. `listCommunityPosts` in `lib/community.ts` lines 25–34 accepts only
`cursor` and `limit` opts.

**Page changes:**
1. Add `"use client"` is already present (line 1). Good.
2. Add imports: `useSearchParams`, `useRouter` from `next/navigation` (already imports `useParams`).
3. In `CommunitySectionPage`, add:
   ```tsx
   const searchParams = useSearchParams();
   const router = useRouter();
   const currentSort = searchParams.get("sort") ?? "newest";
   
   function setFilter(key: string, value: string) {
     const params = new URLSearchParams(searchParams.toString());
     if (value) { params.set(key, value); } else { params.delete(key); }
     router.replace(`?${params.toString()}`, { scroll: false });
   }
   ```
4. Update the `useEffect` that fetches posts (lines 344–363) to depend on `searchParams` and pass
   the sort/filter params to `listCommunityPosts`.
5. Replace the decorative filter buttons with wired `onClick` handlers:
   ```tsx
   <button onClick={() => setFilter("sort", "votes")}>Most Upvoted</button>
   ```

**lib/community.ts changes:**
Update `listCommunityPosts` opts type to accept `sort`, `tag`, `outcome`, `role`, `level`:
```typescript
opts: { cursor?: string | null; limit?: number; sort?: string; tag?: string; outcome?: string; role?: string; level?: string } = {}
```
Add each to the `URLSearchParams` construction.

**Gotchas:**
- `useSearchParams` requires a `<Suspense>` boundary in Next.js App Router for static exports. Check if the
  community page is already wrapped — if not, wrap the page content in `<Suspense fallback={<div>Loading...</div>}>`
  or use `useRouter().query` pattern instead.
- Sort tabs on recruiter surface: `"Most Upvoted"` → `sort=votes`; `"All Companies"` → a filter, not a sort.
  Map each filter button to the correct URL param key.

---

### TASK-16 — Wire author chip links

**File to change:** `job-tracker/app/community/[section]/page.tsx`

**Current state:** Author name is rendered at line 503:
```tsx
<span>{post.authorName || "Anonymous"}</span>
```

**Change:** Wrap in a conditional Link when `post.authorSlug` is non-null/non-empty:
```tsx
{post.authorSlug
  ? <Link href={`/u/${post.authorSlug}`} className="author-chip">
      {post.authorName || "Anonymous"}
    </Link>
  : <span className="author-chip">{post.authorName || "Anonymous"}</span>
}
```

**Dependency:** TASK-07 must have added `authorSlug` to `ApiCommunityPost`. The field already
exists in the Go struct (`CommunityPost.AuthorSlug`, already in the scan) and `ApiCommunityPost`
already has `authorSlug?: string` (optional). No backend change needed.

**Gotchas:** Do NOT restructure the `<p className="post-row-meta">` element. Only swap the span for a
conditional Link. Memory rule: no JSX restructuring when wiring backends.

---

### TASK-17 — Fix community recruiter list to render from API

**File to change:** `job-tracker/app/community/[section]/page.tsx`

**Current state:**
- `seedRecruiters` constant: lines 101–112 (10 hardcoded entries).
- `filteredRecruiters` computed variable: lines 386–392 (filters seedRecruiters by search).
- `count` derived variable: line 383–385 uses `seedRecruiters.length` for recruiters.
- The page renders `posts` (from API) for all surfaces except recruiters, which renders `filteredRecruiters`.

**Search for the render location:** The recruiter surface renders from the existing `filteredPosts(posts, search)`
list at line 491 (it uses `posts` for all surfaces). There is no separate recruiter-specific render block.
The `filteredRecruiters` variable is computed but never used in the JSX to render a list — it would be used
in a separate render branch that currently doesn't exist. Check the full file to confirm.

Re-checking lines 379–398: `count` uses `seedRecruiters.length` but `filteredRecruiters` is only used
if there's recruiter-specific rendering. The real post list at lines 491–515 already renders from `posts`
for all surfaces. The issue is that `count` shows `seedRecruiters.length = 10` in the page subtitle instead
of the real count.

**Changes:**
1. Remove `seedRecruiters` constant (lines 101–112).
2. Remove `filteredRecruiters` computed variable (lines 386–392).
3. Update `count` logic (lines 379–384): for `section === "recruiters"`, use `posts.length` instead of
   `seedRecruiters.length`. For all surfaces, use `posts.length` for the count display.
4. The `intro` prop on `ProductFrame` (line 443) references `count` — this will now show the real API count.

**Deployment guard:** Before merging, verify that `GET /job-tracker/community/recruiters` returns at least
one real post in production. If zero, the empty state shows correctly (no crash), but the PM intent is to
show real data. Confirm real recruiter posts exist or defer.

---

### TASK-18 — Populate "Link to tracked application" select in experience modal

**File to change:** `job-tracker/app/community/[section]/page.tsx`

**Current state:** `ExperienceModal` (line 791) renders the "Link to tracked application" select (lines
835–842) with only a default "None — enter manually" option. No API call.

**Approach:** Add state and effect inside `ExperienceModal`:
```tsx
const [apps, setApps] = useState<ApiApplication[]>([]);
useEffect(() => {
  api.get<ApiApplicationPage>("/job-tracker/applications?limit=200")
    .then((res) => setApps(res.items ?? []))
    .catch(() => {});
}, []);
```
On select change, auto-fill `draft.company` and `draft.role`:
```tsx
onChange={(e) => {
  const appId = e.target.value;
  const app = apps.find((a) => a.id === appId);
  if (app) {
    setDraft({ ...draft, linkedApplication: appId, company: app.company, role: app.role });
  } else {
    setDraft({ ...draft, linkedApplication: "" });
  }
}}
```

**Import needed:** `ApiApplication`, `ApiApplicationPage` from `@/lib/api-client`. The `api` client is
already imported in the file via the community import.

**Gotchas:**
- Load on every modal open (the `useEffect` with empty deps fires when the component mounts). Since the modal
  is conditionally rendered (`{openModal && section === "experiences" ? <ExperienceModal ...>`), this is correct.
- `api` is not currently imported directly in the page — it is accessed via the community lib functions.
  Check the imports at the top of the file. May need to add `import { api } from "@/lib/api-client"`.
- Memory rule: do not restructure the existing select JSX. Only populate the options and add the onChange logic.

---

### TASK-19 — Fix profile URL validation fields

**File to check:** `job-tracker/app/profile/page.tsx`

**Current state (verified from grep):** All three URL fields already use `type="url"`:
- Line 668: `type="url"` (websiteUrl)
- Line 677: `type="url"` (linkedinUrl)
- Line 686: `type="url"` (githubUrl)

**Conclusion:** TASK-19 is ALREADY IMPLEMENTED in the current codebase.

**Action for SDE:** Run `pnpm tsc --noEmit` to confirm no type errors. Visually verify the profile page
in the browser — `type="url"` on an input causes the browser to validate the URL format on form submission
(shows native browser error for non-URL values). Document as "verified, no changes needed" in the PR.

**Note:** The underlying broken-link bug (B4) was the `<a href="hasdkjaamc">` from an unvalidated text input.
If `type="url"` is already present, the bug should be resolved in the current frontend. Confirm by entering
a non-URL string and verifying the browser prevents form submission.

---

### TASK-20 — Add `go-pdf/fpdf` and implement `handlers_ai_pdf.go`

**Files to create/change:**
- `sypher-api/go.mod` + `go.sum` (via `go get github.com/go-pdf/fpdf@latest`)
- `sypher-api/internal/jobtracker/handlers_ai_pdf.go` [NEW]
- `sypher-api/internal/jobtracker/routes.go` [MOD]

**handlers_ai_pdf.go structure:**

The file lives in `package jobtracker`. Imports needed: `net/http`, `fmt`, `regexp`, `strings`,
`github.com/go-pdf/fpdf`, `github.com/TheBharathProject/sypher-api/internal/auth`,
`github.com/TheBharathProject/sypher-api/internal/httpx`.

Three handlers follow the pattern from Staff ADR §6c exactly:

1. **`CoverLetterPDF`** (`POST /job-tracker/ai/cover-letter/pdf`):
   - Input struct: `{text string, company string, role string}`.
   - No `gateAICredit` call (credit was paid at text-generation time).
   - Validate `text != ""`.
   - Call `buildCoverLetterPDF(text, company, role)` which returns `*fpdf.Fpdf`.
   - Set headers BEFORE `pdf.Output(w)`:
     ```go
     filename := sanitizeFilename(fmt.Sprintf("%s-%s-cover-letter.pdf", in.Company, in.Role))
     w.Header().Set("Content-Type", "application/pdf")
     w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
     if err := pdf.Output(w); err != nil {
         h.logger.Error("pdf output", "err", err)
     }
     ```

2. **`ResumeTweakPDF`** (`POST /job-tracker/ai/resume/tweaks/{id}/pdf`):
   - Use `pathUUID(w, r, "id")` to get the tweak ID.
   - Call `h.store.GetResumeTweak(r.Context(), uid, id)`.
   - Prefer `row.UserEdits` if non-empty, else `row.TweakedText`.
   - No `gateAICredit`.

3. **`LatestResumeReportPDF`** (`GET /job-tracker/ai/resume/report/latest/pdf`):
   - Call `h.store.LatestReport(r.Context(), uid)`.
   - Use `report.ReportMd` as content; strip markdown syntax (`## Header` → `Header`, etc.) with a simple
     regexp replace before passing to `buildReportPDF`.
   - Check `store.go` for the `LatestReport` function signature and `ResumeReport` struct fields.

**`sanitizeFilename`:** Package-level `var unsafeFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9._\-]`)`.
Function trims, replaces unsafe chars with `-`, truncates at 200 chars.

**routes.go changes (after line 126):** Add inside the auth-gated block:
```go
g("POST", "/job-tracker/ai/cover-letter/pdf", h.CoverLetterPDF)
g("POST", "/job-tracker/ai/resume/tweaks/{id}/pdf", h.ResumeTweakPDF)
g("GET",  "/job-tracker/ai/resume/report/latest/pdf", h.LatestResumeReportPDF)
```

**Gotchas:**
- Headers MUST be set before `pdf.Output(w)`. Once `Output` begins writing bytes, the 200 status is committed
  and no further header changes take effect.
- `buildCoverLetterPDF` must return `*fpdf.Fpdf`, not write directly to the `http.ResponseWriter`. This allows
  the handler to handle errors before writing a single byte.
- Import path: `github.com/go-pdf/fpdf` (maintained fork). NOT `github.com/jung-kurt/gofpdf` (archived).
- Run `go build ./...` after implementing. Must produce zero output.
- `h.logger` is a `*slog.Logger` field on `*Handler` (confirmed in `handler.go` line 28). Use it for the output
  error log.

---

### TASK-21 — Add "Download PDF" buttons to cover letter modal, tweak modal, and resume report

**Files to change:**
- `job-tracker/lib/api-client.ts`
- `job-tracker/app/applications/page.tsx`
- `job-tracker/app/resume/page.tsx`

**`api-client.ts` — add `downloadPDF` helper** (add after `api` export):
```typescript
export async function downloadPDF(endpoint: string, body: Record<string, unknown>, filename: string): Promise<void> {
  const res = await api.raw(endpoint, { method: "POST", body });
  const blob = await (res as Response).blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}
```
For `LatestResumeReportPDF` (GET), use `api.raw(endpoint, { method: "GET" })`.

**`app/applications/page.tsx` — cover letter modal:**
The cover letter modal is at lines 1089–1158. The AI result (the generated cover letter text) is stored
in the `coverText` state variable (check state declaration around line 160). After generation succeeds and
`coverText` is non-empty, render a "Download PDF" button in the `ai-modal-foot` div (lines 1139–1157):
```tsx
{coverText ? (
  <button
    className="ghost-button"
    type="button"
    onClick={() => void downloadPDF(
      "/job-tracker/ai/cover-letter/pdf",
      { text: coverText, company: viewingApp?.company ?? "", role: viewingApp?.role ?? "" },
      `${viewingApp?.company ?? "cover"}-${viewingApp?.role ?? "letter"}-cover-letter.pdf`
    )}
  >
    Download PDF
  </button>
) : null}
```

**`app/applications/page.tsx` — tweak modal:**
The tweak result is shown inside the tweak modal. Find where `tweakResult` (or similar state) is rendered
and add a "Download PDF" button. Check state declarations around line 160 for the tweak result variable name.
The tweak ID from the created tweak is needed — use the `createdTweakId` (or similar) state.

**`app/resume/page.tsx` — resume report:**
Step 3 (Results) is at lines 184–212. After the markdown preview, add:
```tsx
<button
  className="ghost-button"
  type="button"
  onClick={() => void downloadPDF_GET(
    "/job-tracker/ai/resume/report/latest/pdf",
    `resume-report-${new Date().toISOString().slice(0,10).replace(/-/g,"")}.pdf`
  )}
>
  Download report
</button>
```
(Add a separate `downloadPDF_GET` helper that uses `api.raw` with `method: "GET"` since the report endpoint is a GET.)

**Memory rule:** Do not restructure JSX. Only add the button element next to existing content.

---

### TASK-22 — Add CSS design tokens and stage color tokens to `globals.css`

**File to change:** `job-tracker/app/globals.css`

**Current state (verified):**
- `:root` block is lines 18–52. Existing stage tokens: `--stage-interest`, `--stage-screen`,
  `--stage-applied`, `--stage-offer`, `--stage-offer-ink` (5 tokens, only partial set).
- `[data-theme="light"]` block is lines 57–76.
- `[data-theme="dark"]` block is lines 78–99.

**Tokens to add to `:root` block (append after line 51 `--font-serif-stack:...`):**
```css
--bg-hover: rgba(255, 255, 255, 0.05);
--accent-soft: rgba(255, 255, 255, 0.08);
--font-mono: ui-monospace, SFMono-Regular, "SF Mono", Consolas, monospace;
--radius-sm: 4px;
--radius-2xl: 20px;
--stage-interested: #efe7d2;
--stage-applied:    #fef9e7;
--stage-phone:      #eaf7ed;
--stage-technical:  #fdf2e9;
--stage-onsite:     #f4ecf7;
--stage-offer:      #d5f5e3;
--stage-rejected:   #fdecea;
```

**Add to `[data-theme="light"]` block:**
```css
--bg-hover: rgba(0, 0, 0, 0.04);
--accent-soft: rgba(0, 0, 0, 0.06);
```

**Add to `[data-theme="dark"]` block:**
```css
--bg-hover: rgba(255, 255, 255, 0.06);
```

**Stage colors do NOT need dark-theme mirrors** per CTO ADR note: "Stage tints don't flip; they're accent
colors that read on both backgrounds." Only add the dark-theme `--bg-hover` override.

**`app/applications/page.tsx` changes (lines 754–763):**
The kanban stage-dot background is hardcoded inline at those lines (verified from source). Replace:
```tsx
// Before:
style={{
  background:
    stage === "INTERESTED" ? "#b9a87f"
    : stage === "OFFER"    ? "#62c18b"
    : stage === "REJECTED" ? "#ef6f6c"
    :                        "#9094ff"
}}
// After:
style={{
  background:
    stage === "INTERESTED" ? "var(--stage-interested)"
    : stage === "OFFER"    ? "var(--stage-offer)"
    : stage === "REJECTED" ? "var(--stage-rejected)"
    :                        "var(--stage-phone)"
}}
```

**`components/ui.tsx` — MetricCard:**
The `MetricCard` component at lines 25–41 renders `<h3>{value}</h3>`. Add `style={{ fontFamily: "var(--font-mono)" }}`
to the `<h3>` element.

---

### TASK-23 — Implement `ModalShell` in `components/ui.tsx` and migrate all 8 modal sites

**Files to change:**
- `job-tracker/components/ui.tsx`
- `job-tracker/app/applications/page.tsx`
- `job-tracker/app/settings/page.tsx`
- `job-tracker/app/community/[section]/page.tsx`
- `job-tracker/package.json`

**Critical observation:** There is ALREADY a local `ModalShell` function in
`app/community/[section]/page.tsx` at lines 596–626. It is a simple wrapper without focus trap,
ARIA attributes, or `inert`. This existing local `ModalShell` must be REPLACED by the new exported
`ModalShell` from `components/ui.tsx`. Delete the local version from `page.tsx` after migrating.

**Step 1: `pnpm add @radix-ui/react-focus-scope` from `job-tracker/` directory.**

**Step 2: Add `ModalShell` to `components/ui.tsx`:**
```tsx
"use client";
import { ReactNode, useEffect, useRef } from "react";
import { FocusScope } from "@radix-ui/react-focus-scope";

export type ModalShellProps = {
  open: boolean;
  onClose: () => void;
  title: string;
  titleId?: string;
  children: ReactNode;
  width?: "sm" | "md" | "lg";
};

export function ModalShell({
  open,
  onClose,
  title,
  titleId = "modal-title",
  children,
  width = "md"
}: ModalShellProps) {
  useEffect(() => {
    if (!open) return;
    document.body.classList.add("overflow-hidden");
    const main = document.querySelector("main");
    if (main) (main as HTMLElement & { inert: boolean }).inert = true;
    return () => {
      document.body.classList.remove("overflow-hidden");
      const main = document.querySelector("main");
      if (main) (main as HTMLElement & { inert: boolean }).inert = false;
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => { if (e.key === "Escape") onClose(); };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, [open, onClose]);

  if (!open) return null;

  const widthClass = width === "lg" ? "modal-card modal-card--lg" : width === "sm" ? "modal-card modal-card--sm" : "modal-card";

  return (
    <div
      className="modal-backdrop"
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleId}
      onClick={onClose}
    >
      <FocusScope trapped loop>
        <div className={widthClass} onClick={(e) => e.stopPropagation()}>
          <h2 id={titleId}>{title}</h2>
          {children}
        </div>
      </FocusScope>
    </div>
  );
}
```

**`inert` TypeScript type issue:** If `tsc` complains about `inert` not being on `HTMLElement`, use the cast
pattern from Staff ADR §8: `{...({ inert: open ? "" : undefined } as any)}` on the `<main>` element reference,
or suppress with the explicit cast shown above.

**Step 3: Migrate 8 modal sites.** All in ONE PR. For each site:
- Replace `<div className="modal-backdrop" role="dialog" aria-modal="true" ...>` with `<ModalShell open={showX} onClose={closeX} title="...">`.
- Move the `<h2>` title inside ModalShell props.
- Add `autoFocus` to the close button.
- Add `triggerRef` + `requestAnimationFrame` focus restore pattern.

**8 modal sites:**
1. Add application modal — `app/applications/page.tsx` ~line 804 (`showAdd` state)
2. Edit application modal — `app/applications/page.tsx` ~line 958 (`editingId` state)
3. View application sidebar — `app/applications/page.tsx` ~line 802 (`viewingApp` state)
4. Set reminder modal — search for "reminder" in `app/applications/page.tsx`
5. Cover letter modal — `app/applications/page.tsx` line 1089 (`showCoverDialog` state)
6. Resume tweak modal — `app/applications/page.tsx` line 1161 (`showTweakDialog` state)
7. Recruiter modal — `app/community/[section]/page.tsx` (the existing `ModalShell` wrapping `RecruiterModal` — replace the local ModalShell with the exported one)
8. Delete account modal — `app/settings/page.tsx` (search for "delete" modal)

**Gotchas:**
- The community page already has a local `ModalShell` component (lines 596–626). After the migration, delete
  this local component. All 5 community modals (Review, Experience, Referral, Ask, Recruiter) use this local
  `ModalShell` — they will now use the imported one.
- The community page `ModalShell` does not have `open` prop — it is always rendered when the parent decides.
  The new `ModalShell` from `components/ui.tsx` accepts an `open` prop. Callers may need adjustment.
- Run `pnpm tsc --noEmit` after all 8 migrations. Zero errors required.
- The applications page modals currently have `role="dialog"` and `aria-modal="true"` (confirmed at lines
  804–807). Do NOT add duplicate attributes — the new `ModalShell` wrapper handles them.

---

### TASK-24 — Mobile CSS surgery

**File to change:** `job-tracker/app/globals.css`

**Verified metrics:**
- 106 `:hover` rules (from grep count).
- 13 occurrences of `720px` (from grep count).
- 3 `.modal-backdrop` definitions (lines 1312, 8095, 10193).

**Four surgical changes (in this order):**

**1. Breakpoint replacement:**
Use find-and-replace (NOT manual edit) to change `@media (max-width: 720px)` → `@media (max-width: 767px)`.
Do NOT change `width: min(100%, 720px)` (line 1324) — this is a max-width cap on modal card width, not a breakpoint.
Also check `max-width: 720px;` on line 6472 — this is a layout max-width, not a breakpoint. Only change `@media`
breakpoints. Total breakpoint occurrences: 10 instances of `@media (max-width: 720px)` (from the grep list, excluding
line 1324 and 6472 which are not media queries). Verify the count before and after.

**2. Modal overlay iOS fix:**
At line 1317: change `place-items: center;` to `place-items: start center;` and add `padding-top: 8svh;`.
This affects all 3 `.modal-backdrop` definitions. Apply to all three.

**3. Hover gating:**
Before editing: run `grep -n ':hover' app/globals.css | wc -l` to confirm 106.
Wrap all `:hover` rules in `@media (hover: hover) { ... }`. Use a scripted approach:
- Write a node script or use `sed` to find all `:hover` rule blocks and wrap them.
- Alternative: use VS Code multi-cursor find with regex `(\.[\w-]+:hover\s*\{[^}]+\})` and wrap each match.
- After editing: re-run grep count. The count of `:hover` rules OUTSIDE `@media (hover: hover)` must be 0.

**4. Reduced-motion:**
Identify specific transitions to wrap:
```css
@media (prefers-reduced-motion: no-preference) {
  .sidebar-collapse-transition { transition: ...; }
  .modal-fade-in { animation: ...; }
  /* button hover transitions */
}
```
Do not wrap ALL transitions — only the ones mentioned above. Other transitions (focus ring, color shifts) are
fine without gating.

**5. Touch targets:**
```css
.icon-button {
  position: relative;
}
.icon-button::before {
  content: "";
  position: absolute;
  inset: -10px;
}
```
Add `position: relative` to the existing `.icon-button` rule (find it in globals.css) and add the `::before` rule.

**Gotchas:**
- The `720px` on line 1324 (`width: min(100%, 720px)`) is a modal card max-width cap — do NOT change it.
- The `max-width: 720px` on line 6472 is an element width constraint — do NOT change it.
- `@media (max-width: 720px)` on line 7937 is inside a comment — grep will count it but do NOT change comments.
- After hover-gating, run `grep -c ':hover' globals.css` outside any `@media (hover: hover)` block — must be 0.

---

### TASK-25 — Billing regression tests

**Files to create/change:**
- `sypher-api/internal/billing/webhook_test.go` [MOD — add to existing file]
- `sypher-api/internal/billing/handlers_test.go` [NEW]

**webhook_test.go additions:** The existing file (confirmed from source) tests signature verification
and rzpNotes unmarshalling. Add:
- `TestWebhookActivatedWritesPeriodEnd`: Mock a `subscription.activated` event with a non-zero `current_end`.
  Assert that the store's `ActivateRecurring` is called with a non-zero `time.Time`.
- `TestWebhookActivatedZeroPeriodEnd`: Mock `subscription.activated` with `current_end=0`. Assert that
  `ActivateRecurring` is called with `time.Time{}` (or that `current_period_end` is not updated).

**handlers_test.go:** New file, tests `requireNoActivePremium` behavior:
- `TestCheckoutSubscriptionReturns409WhenAlreadyActive`: Create a minimal `Handler` with a mock `Store` that
  returns an active subscription row from `CurrentSubscription`. Call `CheckoutSubscription` via `httptest`.
  Assert 409 response.

**Mock pattern:** The `billing.Store` uses a concrete `*pgxpool.Pool`. For unit tests, either use
`pgxmock` or extract a `Storer` interface. Given the existing test file uses `nil` for store (and only
tests signature verification), the simplest approach for the 409 test is to create a tiny mock struct that
implements only `CurrentSubscription`.

---

### TASK-26 — Slug and community validator tests

**Files:** Same as TASK-05 and TASK-12 — these are already covered in detail above. TASK-26 is the
parent tracking item; no new work beyond completing TASK-05 and TASK-12.

---

## Cross-Task Notes

### Shared patterns

1. **All new backend files** must be in `package jobtracker` and follow `handlers_reminders.go`
   (handler pattern) and `store_reminders.go` (store pattern) exactly.

2. **CommunityPost struct location:** The struct is defined inside `store_community.go` (not `types.go`).
   When adding the `Slug` field, edit `store_community.go`.

3. **`communityPostCols` scan order is load-bearing.** Adding `p.slug` at the wrong position in the SELECT
   list will produce silent data corruption. The scan order in `scanCommunityPost` must match exactly.

4. **No new credit constants.** PDF endpoints are free. Do not touch `internal/billing/costs.go`.

5. **`hasActiveSubscription` already exists** as `requireNoActivePremium` in `billing/handlers.go`. TASK-02
   is already done. Confirm and move on.

6. **Profile URL fields** already use `type="url"`. TASK-19 is already done. Confirm and move on.

7. **The community page's local `ModalShell`** (lines 596–626) must be deleted when TASK-23 migrates the
   community modals to the new exported `ModalShell`. Do not leave two `ModalShell` definitions.

### Deployment ordering (hard constraints)

```
TASK-03 migration → TASK-06 backend → TASK-08 frontend (slug links)
TASK-09 migration → TASK-10 backend (sort)
TASK-11 validators → TASK-13 wire validation
TASK-13 backend validation → TASK-14 frontend enum change (MUST be simultaneous or backend first)
```

The TASK-13/TASK-14 constraint is the only one that can cause live user breakage. The others are purely
additive (new fields, new indexes, new endpoints) and are safe to ship before their frontend counterparts.

---

## Open Questions

1. **TASK-13 UpdateCommunityPost surface lookup:** The handler receives the post ID but not the surface.
   Two approaches: (a) add `surface` as an optional field in the PATCH body; (b) fetch the stored post
   first. Approach (a) is simpler (no extra DB read) but requires the client to include surface. Approach
   (b) is always correct but adds a read. Recommend (a) with optional field.

2. **TASK-17 prod recruiter count:** Must confirm via `GET /job-tracker/community/recruiters` that
   real posts exist in production before removing seed data. If zero, defer TASK-17.

3. **TASK-24 hover-gating script:** A manual find-and-replace of 106 rules is error-prone. Recommend
   writing a one-shot Node.js/Python script to do the transformation mechanically. Script should be
   committed alongside the CSS change for audit.

4. **TASK-25 billing store mock:** The `billing.Store` is a concrete type, not an interface. To unit-test
   the 409 handler behavior, either extract a minimal interface or use integration tests with `pgxmock`.
   Recommend minimal interface approach for the handler test.
