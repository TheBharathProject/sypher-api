# ADR: Pegasus Gap Analysis — Engineering Manager Decision Record

**Status:** Accepted
**Date:** 2026-05-12
**Slug:** pegasus-gap-analysis
**Author:** Engineering Manager Agent (claude-sonnet-4-6)
**Depends on:** CTO, PM, Architect, Staff Engineer ADRs (same slug)

---

## User Decision Applied

The OUTCOMES enum question is resolved: **fix the frontend to send `"Reject"` and `"InProgress"` to match existing backend constants.** Backend enum values are not renamed and no DB rows are migrated. Display labels in the UI must use a `OUTCOME_LABELS` map to surface user-friendly text (`"Reject" → "Rejected"`, `"InProgress" → "In Progress"`) without changing the wire format.

---

## Scope Summary

The remaining Pegasus work decomposes into **7 feature groups** (CTO sequencing), **2 critical bug fixes** (B2, B3 from PM ADR), **1 bug fix batch** (B4/B5 are subsumed into Phase 12 frontend), and a **parallel test-coverage track**. Total discrete tasks: **26** (excluding buffer tasks). Estimated total effort with buffer: **8–14 engineer-days** for a single full-stack engineer, or **5–9 days** with a backend/frontend split.

---

## Task List — Ordered by Dependency

### Milestone 0 — Bug Fixes (Backend, no dependencies, ship first)

These two backend bugs are P0 and have zero dependencies on other phases. Ship them before any feature work begins.

---

**TASK-01**
- Title: Fix `currentPeriodEnd` null for active subscriptions (B2)
- File: `internal/billing/webhook.go`
- Work: On `subscription.activated` and `subscription.charged` Razorpay webhook events, write `current_end` from `payload.subscription.entity.current_end` to the `current_period_end` column. The Settings billing UI already handles the null display case; this is a pure backend data-integrity fix.
- Size: S
- Effort range: 1–3 hours
- Owner role: SDE (backend)
- Dependencies: None
- Risk: Low — additive write path in existing webhook handler

---

**TASK-02**
- Title: Add 409 guard to prevent Pro re-subscription (B3)
- File: `internal/billing/handlers.go`
- Work: In `POST /billing/checkout/subscription`, call `HasActiveSubscription(userID)` before creating a new Razorpay subscription. If active and not cancelled, return `409 Conflict`. The check must cover subscription status values `active` and `authenticated` (Razorpay lifecycle values).
- Size: S
- Effort range: 1–3 hours
- Owner role: SDE (backend)
- Dependencies: None
- Risk: Low — additive guard, existing happy path unchanged

---

### Milestone 1 — Community Post Slugs (Backend prerequisite for Phase 12)

Must deploy and have migration backfill complete before any frontend changes to community post links.

---

**TASK-03**
- Title: Write migration `0019_community_slugs.sql`
- Files: `sypher-api/migrations/0019_community_slugs.sql`
- Work: ADD COLUMN slug TEXT; UPDATE backfill (`'post-' || REPLACE(id::text, '-', '')`); ALTER COLUMN SET NOT NULL; CREATE UNIQUE INDEX. Exactly as specified in the Architect ADR. Idempotent with IF NOT EXISTS guards.
- Size: S
- Effort range: 1–2 hours
- Owner role: SDE (backend)
- Dependencies: None
- Risk: Low — additive, migration runner is idempotent

---

**TASK-04**
- Title: Implement `slug.go` — `slugify()` + `Store.uniqueSlug()` with collision retry loop
- Files: `internal/jobtracker/slug.go` [NEW]
- Work: Pure `slugify()` function per Staff Engineer spec (Step 1–6 with `regexp.MustCompile` as package-level var). `uniqueSlug()` loops up to 5 times using `crypto/rand` 2-byte suffix, NOT `math/rand`. Return error after 5 exhausted retries (caller maps to 500). Match the exact function signature from Staff Engineer ADR §5a.
- Size: S
- Effort range: 2–4 hours
- Owner role: SDE (backend)
- Dependencies: TASK-03 (migration defines the slug column that uniqueSlug queries)
- Risk: Low — pure function + one DB read per insert

---

**TASK-05**
- Title: Write `internal/jobtracker/slug_test.go`
- Files: `internal/jobtracker/slug_test.go` [NEW]
- Work: The exact test matrix from Staff Engineer ADR §10 is required. Must include: normal title, special chars, Unicode drop (Hindi title), truncation to 80 chars, consecutive hyphen collapse, leading/trailing trim. This file is NOT optional.
- Size: S
- Effort range: 1–2 hours
- Owner role: SDE (backend)
- Dependencies: TASK-04
- Risk: None — pure function tests

---

**TASK-06**
- Title: Wire slugs into `store_community.go` and `handlers_community.go`
- Files: `internal/jobtracker/store_community.go` [MOD], `internal/jobtracker/handlers_community.go` [MOD], `internal/jobtracker/types.go` scope (via store column scan)
- Work: `CreatePost` calls `uniqueSlug()` and writes the slug column. Add `GetPostBySlug(ctx, userID, slug)`. Add `p.slug` to `communityPostCols` constant and scan. `CommunityPost.Slug` field added to the struct. In `GetCommunityPost` and `PublicGetCommunityPost`, replace `pathUUID` with dual-path lookup (UUID-first, slug fallback). `ErrNoRows` → 404 in both branches (existing pattern preserved).
- Size: M
- Effort range: 3–5 hours
- Owner role: SDE (backend)
- Dependencies: TASK-04, TASK-03
- Risk: Medium — dual-path change touches existing public-facing handlers; verify UUID URLs still return 200 post-deploy

---

**TASK-07**
- Title: Add `slug: string` to `ApiCommunityPost` in `lib/api-client.ts`
- Files: `job-tracker/lib/api-client.ts` [MOD]
- Work: Add `slug: string` field to `ApiCommunityPost` type. Add `authorSlug?: string` if not already present. Run `pnpm tsc --noEmit` — must pass zero errors.
- Size: S
- Effort range: 30 minutes
- Owner role: SDE (frontend)
- Dependencies: TASK-06 (backend now returns `slug` in API response)
- Risk: None — additive field; TypeScript will flag missing field if used before it is added

---

**TASK-08**
- Title: Wire community post card links to use `post.slug` instead of `post.id`
- Files: `job-tracker/app/community/[section]/page.tsx` [MOD]
- Work: Change the post card `<Link>` href from `post.id` to `post.slug`. This is a one-line change per card renderer. Deploy AFTER TASK-03 migration + TASK-06 backend are live (both UUID and slug URLs resolve via dual-path lookup).
- Size: S
- Effort range: 30 minutes
- Owner role: SDE (frontend)
- Dependencies: TASK-07, TASK-06 (backend serving slugs and resolving slug URLs)
- Risk: Low — dual-path ensures old bookmarked UUID URLs still work

---

### Milestone 2 — Phase 12 Backend (Sort + Metadata Validation)

---

**TASK-09**
- Title: Write migration `0020_community_sort_indexes.sql`
- Files: `sypher-api/migrations/0020_community_sort_indexes.sql` [NEW]
- Work: Two partial indexes: `(surface, vote_count DESC, created_at DESC) WHERE status = 'active'` and `(surface, comment_count DESC, created_at DESC) WHERE status = 'active'`. IF NOT EXISTS guards. Both additive — zero downtime.
- Size: S
- Effort range: 1–2 hours
- Owner role: SDE (backend)
- Dependencies: None (independent of slug migration but both ship together in Milestone 2)
- Risk: None — additive index creation on an existing table

---

**TASK-10**
- Title: Add `?sort=` support to `ListPosts()` in `store_community.go`
- Files: `internal/jobtracker/store_community.go` [MOD]
- Work: Add `Sort` field to `CommunityListOpts`. In `ListPosts()`, use the `allowedSortClauses` allowlist map (Staff Engineer ADR §12) to select the ORDER BY clause. NEVER interpolate the raw `?sort=` value into SQL — map lookup only. Default to `"newest"` for unrecognized or absent values. Cursor pagination remains `created_at`-based per the Architect ADR trade-off (document this in a code comment).
- Size: S
- Effort range: 2–3 hours
- Owner role: SDE (backend)
- Dependencies: TASK-09 (indexes must exist before sort queries run in production)
- Risk: Low — fallback to `"newest"` on unknown sort values is safe

---

**TASK-11**
- Title: Implement `validators_community.go` — per-surface metadata validation
- Files: `internal/jobtracker/validators_community.go` [NEW]
- Work: New file per Staff Engineer ADR §4. `validateCommunityMetadata(surface, raw json.RawMessage) *metaValidationError` with switch dispatch. Per-surface validators for `experiences` (outcome, difficulty), `ask` (tags ≤ 3 from allowlist), `recruiters` (specializations, hiringLevels), `reviews` (targetRole, experienceLevel). `validOutcomes` map must contain `{"Offer", "Reject", "Ghosted", "InProgress", "Withdrew"}` — the user-resolved decision. The `"Rejected"` value must NOT be in the valid set. `referrals` returns nil (no constraints).
- Size: M
- Effort range: 2–4 hours
- Owner role: SDE (backend)
- Dependencies: None (pure functions, no DB)
- Risk: Low — new code path; existing posts in DB are never re-validated

---

**TASK-12**
- Title: Write `internal/jobtracker/validators_community_test.go`
- Files: `internal/jobtracker/validators_community_test.go` [NEW]
- Work: The exact test matrix from Staff Engineer ADR §10 is required. Cover: valid/invalid outcome for experiences, tags count boundary (3 ok, 4 fail), empty metadata ok, referrals pass-through, reviews targetRole valid/invalid. This file is NOT optional.
- Size: S
- Effort range: 1–2 hours
- Owner role: SDE (backend)
- Dependencies: TASK-11
- Risk: None — pure function tests

---

**TASK-13**
- Title: Wire metadata validation into `CreateCommunityPost` and `UpdateCommunityPost`
- Files: `internal/jobtracker/handlers_community.go` [MOD]
- Work: Call `validateCommunityMetadata(surface, in.Metadata)` after body-length check, before `in.Surface = surface` in `CreateCommunityPost`. Use the three-key response shape `{error: "invalid_metadata", field: "...", message: "..."}` as specified in Staff Engineer ADR §4. For `UpdateCommunityPost`: validate only the fields explicitly provided in the PATCH body (not the full stored metadata) to avoid 400-rejecting existing posts with stale enum values on a title-only edit. Add `?sort=` parsing in `parseListOpts`.
- Size: M
- Effort range: 3–4 hours
- Owner role: SDE (backend)
- Dependencies: TASK-11, TASK-10
- Risk: Medium — `UpdateCommunityPost` partial-metadata validation is the subtle case; test explicitly

---

### Milestone 3 — Phase 12 Frontend

TASK-13 (backend validation) and TASK-08 (slug links) must be deployed before this milestone ships to production to avoid users getting 400 errors on the old OUTCOMES values.

---

**TASK-14**
- Title: Realign community enum constants (OUTCOMES, TARGET_ROLES, EXPERIENCE_LEVELS, ROUND_TYPES)
- Files: `job-tracker/app/community/[section]/page.tsx` [MOD]
- Work:
  - `OUTCOMES = ["Offer", "Reject", "Ghosted", "InProgress", "Withdrew"]` (user-resolved decision)
  - Add `OUTCOME_LABELS: Record<string, string> = { Reject: "Rejected", InProgress: "In Progress", Offer: "Offer", Ghosted: "Ghosted", Withdrew: "Withdrew" }` for display
  - `TARGET_ROLES = ["SDE", "PM", "Data Science", "Design", "DevOps", "QA", "Other"]` (7 values)
  - `EXPERIENCE_LEVELS = ["Fresher", "Junior (0-2)", "Mid (2-5)", "Senior (5+)", "Lead (8+)"]` (5 values)
  - `ROUND_TYPES` expanded to 13 values: add Group Discussion, Case Study, Culture Fit
  - All select elements that render OUTCOMES values must use `OUTCOME_LABELS[v]` for display but `v` as the submitted value
  - **This change must deploy at the same time as or after TASK-13 (backend validation)**
- Size: S
- Effort range: 1–2 hours
- Owner role: SDE (frontend)
- Dependencies: TASK-13 must be live before this ships to production
- Risk: Medium — deployment coordination required; enum mismatch between frontend and backend causes 400s

---

**TASK-15**
- Title: Wire filter/sort buttons to URL query state in community page
- Files: `job-tracker/app/community/[section]/page.tsx` [MOD], `job-tracker/lib/community.ts` [MOD]
- Work: Replace decorative `<button className="filter-box">` elements with wired `onClick` handlers. Use `useSearchParams()` + `useRouter()` per Architect ADR §Feature 3. `setFilter(key, value)` pattern writes to URL without page navigation. `listCommunityPosts()` reads `sort`, `tag`, `outcome`, `role`, `level` from `searchParams`. `useEffect` re-fetches on `searchParams` change. Sort tabs on recruiter surface wired to `?sort=votes|newest|most-reviewed|least-reviewed`.
- Size: M
- Effort range: 3–5 hours
- Owner role: SDE (frontend)
- Dependencies: TASK-10 (backend `?sort=` support), TASK-14 (enums must be aligned before filter values are sent)
- Risk: Low — URL state is additive; existing rendering falls back to "newest" if params absent

---

**TASK-16**
- Title: Wire author chip links to `/u/{authorSlug}` in community post list
- Files: `job-tracker/app/community/[section]/page.tsx` [MOD]
- Work: In the post card renderer, wrap `post.authorName` in `<Link href={"/u/" + post.authorSlug}>` when `post.authorSlug` is non-null. Render plain `<span>` when null (anonymous or unfilled profiles). No new API calls needed — `authorSlug` is returned in the existing list response (once TASK-06 + TASK-07 are done).
- Size: S
- Effort range: 1 hour
- Owner role: SDE (frontend)
- Dependencies: TASK-07 (`ApiCommunityPost` must have `authorSlug` field)
- Risk: None

---

**TASK-17**
- Title: Fix community recruiter list to render from API (B5)
- Files: `job-tracker/app/community/[section]/page.tsx` [MOD]
- Work: Remove `seedRecruiters` constant and `filteredRecruiters` computed variable. For `section === "recruiters"`, render from `posts` (real API data). The post-list renderer already handles any surface uniformly. Verify the empty-state component is shown when `posts.length === 0` before removing seed data from the production deployment.
- Size: S
- Effort range: 1–2 hours
- Owner role: SDE (frontend)
- Dependencies: TASK-15 (filter wiring; ensures the rendered list is the correct filtered API result)
- Risk: Medium — removal of seed data may expose empty surfaces if the API is not returning data; confirm real recruiter posts exist in prod before deploying

---

**TASK-18**
- Title: Populate "Link to tracked application" select in experience modal
- Files: `job-tracker/app/community/[section]/page.tsx` [MOD]
- Work: In `ExperienceModal`, add `useEffect` on modal open to `GET /job-tracker/applications?limit=200`. Populate a `<select>` with `{id, company, role}` entries. On selection, auto-fill `draft.company` and `draft.role`. The "None — enter manually" option must remain as the default. No new API endpoint needed.
- Size: S
- Effort range: 1–2 hours
- Owner role: SDE (frontend)
- Dependencies: TASK-14 (modal is being modified in this milestone; do in the same pass)
- Risk: Low

---

**TASK-19**
- Title: Fix profile URL validation fields (B4)
- Files: `job-tracker/app/profile/page.tsx` [MOD]
- Work: Switch `type="text"` to `type="url"` on LinkedIn, GitHub, and Website inputs in the profile About form. This is the fix for the live `<a href="hasdkjaamc">` broken link. Verify with `pnpm tsc --noEmit` after the change.
- Size: S
- Effort range: 30 minutes
- Owner role: SDE (frontend)
- Dependencies: None
- Risk: None

---

### Milestone 4 — Phase 10: AI PDFs

---

**TASK-20**
- Title: Add `go-pdf/fpdf` to `go.mod` and implement `handlers_ai_pdf.go`
- Files: `sypher-api/go.mod` [MOD], `sypher-api/go.sum` [MOD], `internal/jobtracker/handlers_ai_pdf.go` [NEW], `internal/jobtracker/routes.go` [MOD]
- Work:
  - `go get github.com/go-pdf/fpdf@latest` then `go mod tidy`
  - New file `handlers_ai_pdf.go` per Staff Engineer ADR §6b/6c:
    - `CoverLetterPDF` — accepts `{text, company, role}` in body (text already generated by prior AI call; no AI re-call). Set Content-Type + Content-Disposition headers BEFORE `pdf.Output(w)`. No `gateAICredit` call (Staff Engineer ADR §6e).
    - `ResumeTweakPDF` — `pathUUID` for `{id}`, call `h.store.GetResumeTweak`, prefer `UserEdits` if non-empty else `TweakedText`. No credits.
    - `LatestResumeReportPDF` — call `h.store.LatestReport(ctx, uid)`. No credits.
  - `buildCoverLetterPDF`, `buildTweakPDF`, `buildReportPDF` helper functions per PDF layout spec in Architect ADR §Feature 4.
  - `sanitizeFilename` function per Staff Engineer ADR §6d.
  - Register three new routes in `routes.go`.
  - `go build ./...` must produce zero output.
- Size: L
- Effort range: 4–8 hours
- Owner role: SDE (backend)
- Dependencies: TASK-01, TASK-02 (billing bugs fixed first to avoid contaminating billing state during testing)
- Risk: Medium — new library; PDF header ordering is a known footgun (headers must be set before any `pdf.Output` write begins)

---

**TASK-21**
- Title: Add "Download PDF" buttons to cover letter modal, tweak modal, and resume report step 4
- Files: `job-tracker/app/applications/page.tsx` [MOD], `job-tracker/app/resume/page.tsx` [MOD], `job-tracker/lib/api-client.ts` [MOD]
- Work:
  - Add `downloadPDF(endpoint, body, filename)` helper to `lib/api-client.ts` using `api.raw()` + `URL.createObjectURL` pattern per Architect ADR §Feature 4.
  - Cover letter modal: after successful generation, render "Download PDF" button that calls `downloadPDF("/job-tracker/ai/cover-letter/pdf", {text, company, role}, filename)`.
  - Tweak modal (wherever the result panel lives): render "Download PDF" button that calls `downloadPDF("/job-tracker/ai/resume/tweaks/{id}/pdf", {}, filename)`.
  - Resume report step 4 (Results): render "Download report" button that calls `downloadPDF("/job-tracker/ai/resume/report/latest/pdf", {}, "resume-report-{date}.pdf")`.
  - No JSX restructuring per memory rule — only add the download button element; do not restructure existing layout.
- Size: M
- Effort range: 2–4 hours
- Owner role: SDE (frontend)
- Dependencies: TASK-20 (PDF endpoints must exist)
- Risk: Low — additive UI additions; `api.raw()` already exists

---

### Milestone 5 — Phase 14: Design System Polish

---

**TASK-22**
- Title: Add CSS design tokens and stage color tokens to `globals.css`
- Files: `job-tracker/app/globals.css` [MOD], `job-tracker/app/applications/page.tsx` [MOD]
- Work:
  - Append to `:root` block: `--bg-hover`, `--accent-soft`, `--font-mono`, `--radius-sm`, `--radius-2xl`
  - Add all 7 `--stage-{name}` tokens: `interested`, `applied`, `phone`, `technical`, `onsite`, `offer`, `rejected`
  - Add dark-theme mirrors in the existing `[data-theme="dark"]` block following current patterns
  - In `app/applications/page.tsx`: replace hardcoded hex values at lines 756–763 with `background: var(--stage-{stage})`
  - In `components/ui.tsx`: add `font-family: var(--font-mono)` to `MetricCard` numeric values
  - These CSS additions are purely additive — zero regression risk
- Size: S
- Effort range: 2–3 hours
- Owner role: SDE (frontend)
- Dependencies: None
- Risk: None — additive CSS tokens

---

**TASK-23**
- Title: Implement `ModalShell` in `components/ui.tsx` and migrate all 8 modal sites
- Files: `job-tracker/components/ui.tsx` [MOD], `job-tracker/app/applications/page.tsx` [MOD], `job-tracker/app/settings/page.tsx` [MOD], `job-tracker/app/community/[section]/page.tsx` [MOD], `job-tracker/package.json` [MOD]
- Work:
  - `pnpm add @radix-ui/react-focus-scope` — confirm pnpm-lock.yaml updates
  - `pnpm tsc --noEmit` — zero errors before starting modal work
  - Export `ModalShell` from `components/ui.tsx` per Architect ADR §Feature 6 spec (open, onClose, title, titleId, children, width props)
  - `ModalShell` internals: `overflow-hidden` on body, Escape key listener → `onClose()`, `<FocusScope trapped loop>`, `role="dialog" aria-modal="true" aria-labelledby`, `inert` on `<main>` via `document.querySelector("main")`
  - If TypeScript flags `inert` attribute: use `{...({ inert: showModal ? "" : undefined } as any)}` cast per Staff Engineer ADR §8
  - Migrate all 8 modal sites simultaneously in ONE PR (not incrementally):
    1. Add application modal (`app/applications/page.tsx` ~line 805)
    2. Edit application modal (~line 959)
    3. View application sidebar (~line 1092)
    4. Set reminder modal (~line 1164)
    5. Cover letter modal (~line 1380)
    6. Resume tweak modal
    7. Add/edit recruiter modal
    8. Delete account modal (`app/settings/page.tsx`)
  - Each site: replace inline backdrop div with `<ModalShell>`, add `autoFocus` to close button, add `triggerRef` + `requestAnimationFrame(() => triggerRef.current?.focus())` on close
  - `pnpm tsc --noEmit` must pass zero errors after migration
  - Manual keyboard-only test: Tab cycles within modal, Escape closes, focus returns to trigger
- Size: XL
- Effort range: 6–10 hours
- Owner role: SDE (frontend)
- Dependencies: TASK-22 (tokens must exist so ModalShell can reference CSS variables)
- Risk: High — largest single-PR change; partial migration creates CSS inconsistency. If a modal site is missed, the inconsistency is immediately visible to keyboard users. Do not merge until all 8 sites pass the keyboard test.

---

### Milestone 6 — Phase 15: Mobile CSS Surgery

---

**TASK-24**
- Title: Mobile CSS — breakpoint fix, iOS modal padding, hover gating, reduced-motion, touch targets
- Files: `job-tracker/app/globals.css` [MOD]
- Work (all in one PR — these changes interact):
  - Breakpoints: find-and-replace `@media (min-width: 720px)` → `@media (min-width: 48rem)` and `@media (max-width: 720px)` → `@media (max-width: 767px)`. Audit for `max-width: 720px` complements before applying (risk mitigation from PM ADR).
  - Modal overlay: add `padding-top: 8svh` and change `place-items: center` to `place-items: start center` on `.modal-backdrop`. Applies to all 38 overlay uses.
  - Hover gating: wrap all ~113 `:hover` rules in `@media (hover: hover) { ... }`. Use a script or careful find-and-replace — not by hand. Verify no `:hover` rules outside the media query after the pass.
  - Reduced-motion: wrap sidebar collapse transition, modal fade-in, and button hover transitions in `@media (prefers-reduced-motion: no-preference)`.
  - Touch targets: add `::before` pseudo-element with `inset: -10px; content: ""; position: absolute` to `.icon-button` class. Add `position: relative` to `.icon-button`.
  - Manual visual regression at 320px, 375px, 480px, 768px, 1280px before merge.
- Size: L
- Effort range: 4–7 hours
- Owner role: SDE (frontend)
- Dependencies: TASK-23 (ModalShell is already handling `overflow-hidden` on body; verify no conflict with Phase 15 scroll-lock approach)
- Risk: Medium-High — ~9,200-line CSS file; hover-gating is the highest mechanical risk (missing a selector silently loses a hover style on desktop). Table-stacked cards at <480px (the `data-label` card pattern) is additive CSS only.

---

### Parallel Track — Test Coverage

This track runs concurrently with all milestones and is not gated on any feature milestone.

---

**TASK-25**
- Title: Expand `internal/jobtracker` test coverage — billing paths (B2/B3 regression tests)
- Files: `internal/billing/webhook_test.go` [NEW or MOD], `internal/billing/handlers_test.go` [NEW or MOD]
- Work: Add regression tests for the B2 webhook fix (assert `current_period_end` is written on `subscription.charged` event) and B3 guard (assert `POST /billing/checkout/subscription` returns 409 when user is already active). Use `httptest.NewRecorder` + existing handler struct. These must pass before TASK-01 and TASK-02 merge.
- Size: M
- Effort range: 2–4 hours
- Owner role: SDE (backend)
- Dependencies: TASK-01, TASK-02 (write tests alongside the fixes)
- Risk: Low

---

**TASK-26**
- Title: Expand `internal/jobtracker` test coverage — slug and community validator (required)
- Files: `internal/jobtracker/slug_test.go` [NEW], `internal/jobtracker/validators_community_test.go` [NEW]
- Work: The exact test matrices from Staff Engineer ADR §10. Both files are required before their respective source files merge. Not optional. See TASK-05 and TASK-12 (this task is the tracking item for the overall test obligation; TASK-05 and TASK-12 are the individual sub-tasks).
- Size: S
- Effort range: 2–3 hours combined
- Owner role: SDE (backend)
- Dependencies: TASK-04 (slug.go), TASK-11 (validators_community.go)
- Risk: None

---

## Critical Path

```
TASK-01 (B2 fix)
TASK-02 (B3 fix)
  |
TASK-03 (migration 0019 slugs)
  |
TASK-04 (slug.go) ──── TASK-05 (slug tests)
  |
TASK-06 (store + handlers for slugs)
  |
TASK-07 (ApiCommunityPost.slug type)
  |
TASK-08 (frontend slug links)
  |
TASK-09 (migration 0020 sort indexes)
TASK-10 (ListPosts sort param) ────── TASK-11 (validators_community.go) ── TASK-12 (validator tests)
  |                                        |
TASK-13 (wire validation + sort into handler)
  |
[TASK-14 through TASK-19 in parallel — Phase 12 frontend]
  |
TASK-20 (go-pdf/fpdf + PDF handlers)
  |
TASK-21 (frontend PDF download buttons)
  |
TASK-22 (CSS design tokens)
  |
TASK-23 (ModalShell + focus trap — all 8 sites)
  |
TASK-24 (Mobile CSS surgery)
```

**Dominant constraint:** TASK-13 must deploy before TASK-14 ships to production (OUTCOMES enum wire-format alignment). These are the only two tasks with a hard deployment-order dependency that can cause visible user breakage if violated.

**Second constraint:** TASK-06 (slug backend) must deploy before TASK-08 (frontend slug links). The dual-path lookup ensures old UUID bookmarks still work.

---

## Sequencing Rationale

1. **Bugs before features (TASK-01, TASK-02):** B2 and B3 are billing bugs with user-visible broken state. Fixes are small and self-contained. Shipping them first reduces support surface area during the remaining feature work.

2. **Migrations before dependent code (TASK-03, TASK-09):** Index/column additions run at zero downtime. Backend code that depends on the new column (slug) cannot ship before the column exists. Migrations go first.

3. **Backend validation before frontend enum change (TASK-13 before TASK-14):** Shipping the frontend OUTCOMES change before backend validation is live means the frontend sends `"Reject"` to a backend that accepts both `"Rejected"` and `"Reject"` — safe. The reverse (backend enforcing `"Reject"` while frontend still sends `"Rejected"`) causes 400 errors for active users. Always ship backend validation first or simultaneously.

4. **Phase 12 before Phase 10 (community before PDFs):** Community work (slugs + sort + validation) is lower risk (additive). Phase 10 introduces a new Go library and a new streaming response pattern. De-risking the lower-complexity work first creates a clean deployment before the higher-complexity change.

5. **Design tokens before ModalShell (TASK-22 before TASK-23):** CSS token additions are zero-risk. ModalShell migration is high-risk. Staging the additive-only CSS first means a partial rollback is possible after TASK-22 without visual regression if TASK-23 needs to be reverted.

6. **Mobile CSS last (TASK-24):** Phase 15 is the broadest-surface change (9,200-line file, 113 rules modified). Doing it last means all prior features have stable markup and CSS structure to regress against.

---

## Risk Register

| ID | Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|---|
| R1 | OUTCOMES enum deployed in wrong order (frontend before backend) | Medium | High | Never merge TASK-14 before TASK-13 is live. Add a deployment checklist comment to the PR. |
| R2 | ModalShell migration (TASK-23) is partially complete when merged | Medium | High | Require all 8 sites in a single PR. PR reviewer must verify keyboard navigation on each modal before approving. |
| R3 | `pdf.Output(w)` writes after headers are already committed on error | Medium | Medium | Set Content-Type and Content-Disposition before any PDF construction begins. If `buildXxxPDF` errors, return before touching the writer. Pattern is documented in Staff Engineer ADR §6c — follow it exactly. |
| R4 | Slug collision on concurrent same-title insert | Low | Low | 5-retry loop in `uniqueSlug` handles this. Unique constraint is the final arbiter. Return 500 on exhaustion — acceptable for this probability. |
| R5 | Hover-gating pass (TASK-24) misses selectors | Medium | Medium | Use `grep -n ':hover' app/globals.css` before and after the pass to count matches; the post-pass count inside `@media (hover: hover)` must equal the pre-pass count outside it. |
| R6 | `go-pdf/fpdf` import path confusion with archived original | Low | Medium | Use `github.com/go-pdf/fpdf` (maintained fork). Run `go mod verify` after adding. |
| R7 | Seed recruiter data removal (TASK-17) exposes empty community surface | Medium | Medium | Check prod recruiter count via `GET /job-tracker/community/recruiters` before merging. Only remove seed data when real posts > 0. |
| R8 | `inert` attribute TypeScript type gap in ModalShell | High | Low | Known issue; use `{...({ inert: showModal ? "" : undefined } as any)}` cast per Staff Engineer ADR §8. Run `pnpm tsc --noEmit` after. |
| R9 | TASK-23 breaks existing `overflow-hidden` body scroll lock used in Phase 15 | Low | Medium | ModalShell's `overflow-hidden` useEffect replaces the per-modal scroll lock. Verify Phase 15 TASK-24 scroll lock approach does not conflict after both land. |
| R10 | `uniqueSlug` uses `math/rand` instead of `crypto/rand` | Low | Medium | Code review gate: grep the file for `math/rand` before merge. Must be zero. |

---

## Buffer Assumptions

- **25% buffer reserved** on total effort estimate (embedded in the effort ranges above — low end is optimistic, high end includes one round of rework per task).
- **TASK-23 (ModalShell)** has the widest range (6–10 hours) because 8 modal sites with manual keyboard testing is inherently variable. Reserve a full engineering day for this task alone.
- **TASK-24 (Mobile CSS)** has the second-widest range because hover-gating 113 rules is mechanical but repetitive — fatigue causes misses. Reserve a half-day just for the post-change grep verification.
- **Deployment coordination (TASK-13/TASK-14)** has no buffer-hours cost but requires human scheduling attention — this is the single highest-risk coordination point.

---

## Effort Summary

| Milestone | Tasks | T-shirt | Effort Range |
|---|---|---|---|
| M0 — Bug Fixes | TASK-01, TASK-02 | S+S | 0.5–1 day |
| M1 — Community Slugs | TASK-03 through TASK-08 | S+S+S+S+M+S | 1.5–3 days |
| M2 — Phase 12 Backend | TASK-09 through TASK-13 | S+S+M+S+M | 1.5–3 days |
| M3 — Phase 12 Frontend | TASK-14 through TASK-19 | S+M+S+S+S+S | 1.5–3 days |
| M4 — Phase 10 PDFs | TASK-20, TASK-21 | L+M | 1.5–3 days |
| M5 — Phase 14 Design | TASK-22, TASK-23 | S+XL | 2–3.5 days |
| M6 — Phase 15 Mobile | TASK-24 | L | 1–1.5 days |
| Parallel — Tests | TASK-25, TASK-26 | M+S | 1–2 days (parallel) |
| **Total** | **26 tasks** | | **10–19 days gross; 8–14 days net (parallel test track)** |

Sustainable pace at 1–2 engineers: 3–4 weeks to v1.0.

---

## Open Questions

These do not block task creation but must be resolved before the relevant task merges.

1. **TASK-13 / TASK-17 deployment window:** Who owns the prod deployment of the backend validation (TASK-13) and the frontend enum change (TASK-14) to ensure they ship in the correct order? Needs an explicit owner and a deploy checklist.
2. **TASK-17 seed data removal:** Does the prod recruiter surface currently have real API posts? Must confirm before removing `seedRecruiters`. If zero, defer TASK-17 until the recruiter surface has genuine content.
3. **TASK-23 `inert` on `<main>`:** The `ModalShell` component sets `inert` via `document.querySelector("main")`. This may not work if the app wraps page content in a `<div>` rather than a semantic `<main>`. Verify the `ProductFrame` layout element before implementing.
4. **TASK-20 PDF credits decision (resolved by Staff Engineer but confirm with product owner):** `POST /ai/resume/tweaks/{id}/pdf` does NOT debit credits. Confirm this is the final product decision before shipping.
