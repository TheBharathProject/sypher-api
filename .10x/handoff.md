# Handoff

## Project context

Pegasus is a job-tracking SaaS (`sypher.in/pegasus`) built inside the Sypher multi-tool factory. Backend is a Go API (`sypher-api`) served from an OCI VM via Docker. Frontend is a Next.js 14 app (`job-tracker`) deployed as a static export behind the Sypher shell's reverse proxy.

## Key paths

- Backend: `~/Desktop/Personal/MyContent/SAAS/sypher-api`
- Frontend: `~/Desktop/Personal/MyContent/SAAS/job-tracker`
- Handoff doc: `~/Desktop/Personal/MyContent/SAAS/HANDOFF.md`
- Spec reference: `~/Desktop/Personal/MyContent/SAAS/findings.md`

## Architecture summary

- Auth: stateless HS256 JWT, 7-day TTL, `pg_` API tokens (sha256-hashed at rest)
- DB: Postgres via pgx/v5, 18 migrations, two schemas (`auth`, `job_tracker`)
- Files: Cloudflare R2, two-step presigned PUT
- AI: Deepseek (resume report, cover letter, extract, tweak) — credits system
- Billing: Razorpay (subscriptions + one-time + credit packs)
- Cron: in-process, IST-anchored, single-pod assumption
- Rate limiting: token-bucket per user ID via `golang.org/x/time/rate`
- Frontend transport: native fetch via `lib/api-client.ts` (no React Query)

## Current Handoff — To: QA + Security (post SDE)

### What was built (TASK-01, TASK-02, TASK-25) — commit f0ca0f9

**Behavioral changes: none.** Both TASK-01 (currentPeriodEnd write) and TASK-02 (409 guard) were already implemented in production code. This session added regression tests only plus two infrastructure changes to enable testing:

1. `internal/auth/middleware.go` — new exported `WithUserID(ctx, id) context.Context` helper. Only used in tests. Does NOT change auth middleware behavior.
2. `internal/billing/store.go` — `Store.pool` field type changed from `*pgxpool.Pool` to a new `dbPool` interface. `*pgxpool.Pool` satisfies the interface automatically. `NewStore` signature unchanged. Runtime behavior identical.
3. `internal/billing/handlers_test.go` — 9 new tests, all in-process (no DB, no network).

**What QA should test:**
- `POST /billing/checkout/subscription` with an active subscription returns 409 `{"error":"already_subscribed"}`
- `POST /billing/checkout/subscription-plus` same
- A user whose subscription has `cancel_at_period_end=true` can reach the checkout form (no 409)
- After a `subscription.charged` or `subscription.activated` webhook, `GET /billing/me` returns a non-null `currentPeriodEnd`

**What Security should review:**
- `auth.WithUserID` is purely a test helper — it bypasses JWT verification. Confirm it is not reachable from any production HTTP path (it sets the same context key that RequireUser sets, so a request must still pass through RequireUser in the actual server; there is no HTTP route that calls WithUserID).
- `dbPool` interface exposure: internal only, no external API surface change.

---

## Current Handoff — To: SDE

**Senior Engineer decision document:** `.10x/decisions/senior-engineer/pegasus-gap-analysis.md`

### Pre-implementation discoveries (read before starting)

Two tasks are already done in the current codebase — skip them and write confirmation tests only:

- **TASK-02 (409 guard for re-subscription):** `requireNoActivePremium` already exists in
  `internal/billing/handlers.go` (lines 55–80) and is already wired to all three checkout handlers.
  Your job: write the regression test in `billing/handlers_test.go`, document as "verified".

- **TASK-19 (profile URL `type="url"`):** All three URL inputs in `app/profile/page.tsx`
  (lines 668, 677, 686) already use `type="url"`. Your job: run `pnpm tsc --noEmit`, confirm
  the browser validation works in-page, document as "verified".

### Critical file locations (verified from source)

| What | Path | Key detail |
|---|---|---|
| Billing webhook handler | `internal/billing/webhook.go` | `dispatch()` at line 180; `subscription.activated` at line 185 |
| Billing store | `internal/billing/store.go` | `ActivateRecurring` line 112; `BumpRecurringPeriodEnd` line 133 |
| Community store | `internal/jobtracker/store_community.go` | `communityPostCols` line 80; `scanCommunityPost` line 87; `CommunityPost` struct line 21 |
| Community handler | `internal/jobtracker/handlers_community.go` | `CreateCommunityPost` line 130; `GetCommunityPost` line 176 |
| AI handler | `internal/jobtracker/handlers_ai.go` | `gateAICredit` line 49; `GenerateCoverLetter` line 291 |
| Routes | `internal/jobtracker/routes.go` | Add 3 PDF routes after line 126 |
| Handler struct | `internal/jobtracker/handler.go` | `Handler` struct line 25; `h.logger` is `*slog.Logger` |
| Validate helpers | `internal/jobtracker/validate.go` | `ValidateURL`, `parseEmail` — new validators go here |
| Community page | `app/community/[section]/page.tsx` | 1354 lines; `seedRecruiters` line 101; `OUTCOMES` line 126; local `ModalShell` line 596 |
| API client types | `lib/api-client.ts` | `ApiCommunityPost` at line 330; add `slug: string` after `authorSlug` |
| Community lib | `lib/community.ts` | `listCommunityPosts` line 25; needs `sort` param |
| CSS tokens | `app/globals.css` | `:root` block lines 18–52; 10387 lines total; 106 `:hover` rules; 13 `720px` occurrences |
| UI components | `components/ui.tsx` | 45 lines; add `ModalShell` export here; add `"use client"` directive |
| ProductFrame main | `components/frames.tsx` | `<main>` at line 347; `document.querySelector("main")` works for `inert` |
| Applications page | `app/applications/page.tsx` | 1494 lines; cover letter modal line 1089; tweak modal line 1161; kanban stage-dot lines 752–763 |
| Resume page | `app/resume/page.tsx` | 406 lines; Results step at line 184 |

### Function signatures — new code you must write

```go
// slug.go
func slugify(title string) string
func (s *Store) uniqueSlug(ctx context.Context, title string) (string, error)

// store_community.go
func (s *Store) GetPostBySlug(ctx context.Context, slug string, viewerID *uuid.UUID) (*CommunityPost, error)

// validators_community.go
func validateCommunityMetadata(surface string, raw json.RawMessage) *metaValidationError
type metaValidationError struct { field string; msg string }

// handlers_ai_pdf.go
func (h *Handler) CoverLetterPDF(w http.ResponseWriter, r *http.Request)
func (h *Handler) ResumeTweakPDF(w http.ResponseWriter, r *http.Request)
func (h *Handler) LatestResumeReportPDF(w http.ResponseWriter, r *http.Request)
func sanitizeFilename(s string) string
func buildCoverLetterPDF(text, company, role string) *fpdf.Fpdf
func buildTweakPDF(title, content string) *fpdf.Fpdf
func buildReportPDF(score int, content string) *fpdf.Fpdf
```

```typescript
// lib/api-client.ts
export async function downloadPDF(endpoint: string, body: Record<string, unknown>, filename: string): Promise<void>
```

### Non-obvious gotchas checklist

- [ ] `communityPostCols` SELECT order must match `scanCommunityPost` Scan order exactly — adding `p.slug` in the wrong position corrupts silently
- [ ] `crypto/rand`, NOT `math/rand` in `slug.go` — grep before merge
- [ ] `Content-Type` + `Content-Disposition` headers MUST be set BEFORE `pdf.Output(w)` — once Output writes, headers are committed
- [ ] PDF endpoints: NO `gateAICredit` call — credit was paid at text-generation time
- [ ] TASK-13/TASK-14 deployment order: backend validation first or simultaneous — never frontend first
- [ ] `validOutcomes` in `validators_community.go`: `"Reject"`, NOT `"Rejected"` — five exact values
- [ ] Local `ModalShell` in `app/community/[section]/page.tsx` (line 596): DELETE after TASK-23 migration
- [ ] `globals.css` line 1324 `width: min(100%, 720px)` is NOT a breakpoint — do not change in TASK-24
- [ ] `globals.css` line 6472 `max-width: 720px` is an element width — do not change in TASK-24
- [ ] TASK-17 (remove seed recruiters): confirm prod recruiter count > 0 before deploying
- [ ] `pf.slug` (author profile slug) and `p.slug` (post slug) are DIFFERENT in the JOIN query
- [ ] `billing.Store` has no interface — mock via tiny wrapper struct for TASK-25 handler tests

## Previous Handoff — To: Senior Engineer + SDE

**EM decision document:** `.10x/decisions/engineering-manager/pegasus-gap-analysis.md`

### OUTCOMES enum decision (resolved today)

Fix the **frontend** to send `"Reject"` and `"InProgress"` to match the existing backend constants. Do NOT rename backend enum values. Do NOT write a DB migration for existing rows. In `app/community/[section]/page.tsx`, update the `OUTCOMES` constant to `["Offer", "Reject", "Ghosted", "InProgress", "Withdrew"]` and add an `OUTCOME_LABELS` display map. This is captured in TASK-14.

### Start here — ordered task list

Work in milestone order. Do not start M2 until M0 and M1 backend tasks are deployed.

| Order | Task | What to do | Critical constraint |
|---|---|---|---|
| 1 | TASK-01 | `internal/billing/webhook.go` — write `current_period_end` on `subscription.activated` + `subscription.charged` | None |
| 2 | TASK-02 | `internal/billing/handlers.go` — add 409 guard in subscription checkout | None |
| 3 | TASK-03 | Write `migrations/0019_community_slugs.sql` | None |
| 4 | TASK-04 | New `internal/jobtracker/slug.go` — use `crypto/rand`, 5-retry loop | After TASK-03 |
| 5 | TASK-05 | `slug_test.go` — required, all edge cases from Staff Engineer ADR §10 | After TASK-04 |
| 6 | TASK-06 | `store_community.go` + `handlers_community.go` — `GetPostBySlug`, dual-path lookup, `CommunityPost.Slug` scan | After TASK-04 |
| 7 | TASK-07 | `lib/api-client.ts` — add `slug: string` to `ApiCommunityPost` | After TASK-06 deployed |
| 8 | TASK-08 | `app/community/[section]/page.tsx` — post card href → `post.slug` | After TASK-07 |
| 9 | TASK-09 | Write `migrations/0020_community_sort_indexes.sql` | None |
| 10 | TASK-10 | `store_community.go` — `CommunityListOpts.Sort`, allowlist ORDER BY dispatch | After TASK-09 |
| 11 | TASK-11 | New `internal/jobtracker/validators_community.go` — per-surface switch dispatch | None |
| 12 | TASK-12 | `validators_community_test.go` — required | After TASK-11 |
| 13 | TASK-13 | `handlers_community.go` — call validator in Create/Update; wire `?sort=` from `parseListOpts` | After TASK-11, TASK-10 |
| 14 | TASK-14 | `app/community/[section]/page.tsx` — update OUTCOMES + other enums + OUTCOME_LABELS map | **Deploy at same time as or after TASK-13** |
| 15 | TASK-15 | `app/community/[section]/page.tsx` + `lib/community.ts` — URL filter state, sort tabs | After TASK-14 |
| 16 | TASK-16 | Author chip → `<Link href={"/u/" + post.authorSlug}>` | After TASK-07 |
| 17 | TASK-17 | Remove `seedRecruiters`, render from `posts` | After TASK-15; only if prod has real recruiter posts |
| 18 | TASK-18 | Experience modal — populate app-link select via `GET /applications?limit=200` | After TASK-14 |
| 19 | TASK-19 | `app/profile/page.tsx` — `type="url"` on LinkedIn/GitHub/Website inputs | None |
| 20 | TASK-20 | `go get github.com/go-pdf/fpdf`; new `handlers_ai_pdf.go`; 3 PDF routes in `routes.go` | After M0 deployed |
| 21 | TASK-21 | Frontend "Download PDF" + "Download report" buttons | After TASK-20 |
| 22 | TASK-22 | `app/globals.css` — CSS tokens + 7 stage tokens + dark mirrors; kanban `var(--stage-*)` | None |
| 23 | TASK-23 | `components/ui.tsx` ModalShell; `pnpm add @radix-ui/react-focus-scope`; all 8 modal sites in ONE PR | After TASK-22 |
| 24 | TASK-24 | `app/globals.css` — breakpoints (48rem), 8svh modal, hover gating, reduced-motion, touch targets | After TASK-23 |

### Key patterns — read before writing code

- **Handler template:** `internal/jobtracker/handlers_reminders.go`
- **Store template:** `internal/jobtracker/store_reminders.go`
- **All new `Api*` types:** bottom of `lib/api-client.ts`
- **PDF streaming:** set `Content-Type` + `Content-Disposition` headers BEFORE `pdf.Output(w)`. No credit gate on PDF endpoints.
- **Slug collision:** use `crypto/rand`, NOT `math/rand`. Loop 5 times.
- **Sort SQL injection:** allowlist map lookup only; never interpolate `?sort=` raw value into SQL.
- **Modal a11y:** all 8 sites in one PR; `<FocusScope trapped loop>` + `inert` on `<main>`.
- **No UI restructuring:** memory rule — when wiring backends to existing UI, only swap data sources; do not restructure JSX.
- **No new billing credit constants:** PDF endpoints are free (credit was paid at text-generation time).

### Deployment coordination (highest risk)

TASK-13 (backend validation) must go to production before or simultaneously with TASK-14 (frontend enum update). If TASK-14 ships first, it sends `"Reject"` to a backend that still accepts both values — safe. If TASK-13 ships first without TASK-14, the backend rejects `"Rejected"` (old frontend value) — breaking for active users. Coordinate explicitly. Add a comment in the TASK-14 PR checklist.

---

## Previous Handoff — To Staff Engineer + Engineering Manager

**Architect decision document:** `.10x/decisions/architect/pegasus-gap-analysis.md`

### Component list (new files + modifications)

**Backend (`sypher-api/internal/jobtracker/`):**
- `slug.go` [NEW] — `slugify()` pure function + `Store.uniqueSlug()` with collision retry
- `handlers_community.go` [MOD] — dual-path UUID/slug lookup; `validateCommunityMetadata()` dispatch table; `?sort=` param
- `handlers_ai.go` [MOD] — `GenerateCoverLetterPDF`, `ResumeTweakPDF`, `LatestResumeReportPDF`
- `handlers_files.go` [MOD] — `RequestAvatarUploadURL`, `FinalizeAvatar`, `DeleteAvatar` (deferred P2)
- `store_community.go` [MOD] — `GetPostBySlug()`; `CommunityListOpts.Sort`; `ListPosts()` ORDER BY dispatch
- `store_files.go` [MOD] — avatar store functions; link to `auth.Store.UpdateUserPictureURL()`
- `types.go` [MOD via store_community.go] — `CommunityPost.Slug` field added to scan + JSON
- `routes.go` [MOD] — PDF routes; avatar routes (deferred)

**Migrations:**
- `migrations/0019_community_slugs.sql` [NEW] — slug column, backfill, unique index
- `migrations/0020_community_sort_indexes.sql` [NEW] — vote_count + comment_count partial indexes
- `migrations/0021_avatar_partial_unique.sql` [NEW, deferred] — `WHERE kind='avatar'` partial unique

**Frontend (`job-tracker/`):**
- `lib/api-client.ts` [MOD] — `ApiCommunityPost.slug: string`; avatar types; PDF download helper
- `app/community/[section]/page.tsx` [MOD] — enum realignment; URL filter state; author chip links; recruiter from API
- `components/ui.tsx` [MOD] — `ModalShell` new export; `MetricCard` monospace
- `app/applications/page.tsx` [MOD] — kanban stage token colors; 5 modal sites → ModalShell
- `app/settings/page.tsx` [MOD] — delete account modal → ModalShell
- `app/resume/page.tsx` [MOD] — "Download report" button on step 4
- `app/globals.css` [MOD] — CSS tokens; stage colors; breakpoints; hover gating; modal a11y
- `package.json` [MOD] — add `@radix-ui/react-focus-scope`
- `go.mod` + `go.sum` [MOD] — add `github.com/go-pdf/fpdf`

### Integration points requiring coordination

1. **Enum realignment + backend validation must ship together.** Phase 12 frontend OUTCOMES change (`"Rejected"` → `"Reject"`) must coincide with or precede the backend validation landing. Split deployment risks 400 errors for users on the old frontend.

2. **Slug migration must precede frontend link change.** Deploy backend `0019` + `slug.go` first; then deploy frontend `post.id` → `post.slug` link change. Both UUID-style and slug-style URLs work after deploy order is respected.

3. **Avatar finalize calls `auth.Store.UpdateUserPictureURL`** — new method needed on `auth.Store`, not `jobtracker.Store`. The `jobtracker.Handler` already holds `authStore` for API tokens; no new dependency wiring required.

4. **ModalShell refactor is one atomic PR** — all 8 sites must change simultaneously. Partial migration creates CSS inconsistency. See `.10x/decisions/architect/pegasus-gap-analysis.md` for the full 8-site list.

### Open questions for EM/Staff to resolve before implementation

1. Should `POST /job-tracker/ai/resume/tweaks/{id}/pdf` debit credits? Content already exists in DB; no new AI call. Recommendation: no debit (free export).
2. Should `OUTCOMES` backend enum values stay as `"Reject"` / `"InProgress"` or be renamed to `"Rejected"` / `"In Progress"` for UX? Affects both backend validation constants and frontend display labels simultaneously.
3. Should `uniqueSlug` retry once on unique_violation inside `CreatePost`, or surface the error to the user? Recommendation: retry once with fresh entropy.

---

## Previous Handoff — To Architect (completed)

**PM decision document:** `.10x/decisions/product-manager/pegasus-gap-analysis.md`

### Requirements summary

The PM gap analysis is complete. Priorities for the next engineering cycle:

**P0 — Must have for v1.0 (unblock these first)**
- Cover letter PDF download (`POST /job-tracker/ai/cover-letter/pdf` + frontend "Download PDF" button)
- Resume tweak PDF download (`POST /job-tracker/ai/resume/tweaks/{id}/pdf` + frontend "Download PDF" button)
- Resume report PDF download ("Download report" on wizard step 4)
- Community filter/sort wired to real API (replace decorative filter buttons with URL query state)
- Community recruiter list rendered from API posts, not seed data
- Profile URL validation (`type="url"` on LinkedIn/GitHub/Website in `app/profile/page.tsx`)
- `currentPeriodEnd` null fix in Razorpay webhook handler (`subscription.activated` + `subscription.charged` events must write `current_end`)
- Pro re-subscription guard — `POST /billing/checkout/subscription` must return `409 Conflict` when user already has an active subscription

**P1 — Should have for v1.0**
- Avatar upload: `POST /job-tracker/profile/avatar/upload-url`, `PATCH /job-tracker/profile/avatar/finalize`, `DELETE /job-tracker/profile/avatar` + frontend wiring in `app/profile/page.tsx`
- Author chip links to public profile (`<Link href={"/u/" + post.authorSlug}>` in community post list)
- "Link to tracked application" select populated in experience modal (fetch user's apps on modal open)
- Round types / target roles / outcomes realigned with spec enums (see ADR for exact values)
- Backend community metadata validation (enum enforcement per surface in `handlers_community.go`)
- Mobile breakpoint fix: `@media (min-width: 720px)` → `@media (min-width: 48rem)` everywhere in `app/globals.css`
- iOS modal positioning: `padding-top: 8svh` (dynamic viewport unit)

**P2 — Nice to have**
- Modal focus trap + `inert` on `<main>` for all 8 modal sites
- Stage color tokens in kanban (replace hardcoded hex in `app/applications/page.tsx` lines 756–763)
- Monospace font on MetricCard numeric values
- ModalShell promoted to `components/ui.tsx` export

### Acceptance criteria reference

Full Given/When/Then acceptance criteria for each P0/P1 item are in:
`.10x/decisions/product-manager/pegasus-gap-analysis.md`

### Bugs requiring fixes before v1.0

| Bug ID | Description | File |
|---|---|---|
| B2 | `currentPeriodEnd` null for active subscriptions | `internal/billing/webhook.go` |
| B3 | Pro users can re-create subscriptions (409 guard missing) | `internal/billing/handlers.go` |
| B4 | Profile URL fields don't validate format | `app/profile/page.tsx` |
| B5 | Community recruiter list renders seed data instead of API data | `app/community/[section]/page.tsx` |
| B6 | `ROUND_TYPES` has 10 values; spec requires 15 | `app/community/[section]/page.tsx` |
| B7 | `TARGET_ROLES` has 10 values; spec requires 7 | `app/community/[section]/page.tsx` |
| B8 | `OUTCOMES` uses "Rejected"/"In Progress"; backend expects "Reject"/"InProgress" | `app/community/[section]/page.tsx` |

## What was just generated

`.10x/decisions/product-manager/pegasus-gap-analysis.md` — full PM ADR covering:
- Feature completeness audit (Complete / Partial / Missing) for every spec feature
- User impact ranking for all gaps
- User stories + acceptance criteria for Phases 10, 12, 14, and 15
- Confirmed bug catalogue from reading live frontend files
- v1.0 definition of done
- P0/P1/P2/P3 prioritisation ladder

---

## CTO Layer (added 2026-05-12)

**To:** PM and Architect  
**From:** CTO Agent

### Business context

The CTO gap analysis confirms the PM's P0/P1 prioritisation. Two build-vs-buy decisions are resolved:

1. **PDF generation:** Use `github.com/go-pdf/fpdf` (maintained Go fork of gofpdf). Pure Go, zero Docker image increase, sufficient for text-first cover letter and resume tweak output. Headless Chromium is rejected — 400MB image overhead, 2–3s cold-start latency, not justified for plaintext PDFs on a single-container OCI deployment. Decision is reversible; the endpoint contract is stable even if the library is swapped later.

2. **Focus trap:** Use `@radix-ui/react-focus-scope`. The frontend currently has zero runtime dependencies beyond Next.js. This would be the first. Accepted because a correct, maintained focus trap is non-trivial to write (~50 lines but with real browser edge-case maintenance burden). `@radix-ui/react-focus-scope` is a standalone 2kB package — not the full Radix ecosystem.

### Constraints

- Next migration number: `0019_` (Phase 12 community sort indexes). Phase 10 avatar partial unique index will be `0020_`.
- `internal/jobtracker` has only one test file (`mentions_test.go`). All 60+ handler/store functions are untested. New code in billing, auth, or AI credit paths requires unit tests before merge.
- Single-pod cron assumption is still in place. `stale-apps` and `daily-digest` are not multi-instance safe. Do not run a second container until this is resolved.
- No Tailwind, no OpenAPI spec, no new infra.

### Recommended sequencing

1. Phase 12 backend (sort migration + per-surface validation) — additive, zero regression risk
2. Phase 12 frontend (enum realignment, URL-synced filters, sort tabs)
3. Phase 10 (go-pdf/fpdf PDF endpoints + avatar routes + migration)
4. Phase 14 CSS tokens + kanban tints — additive
5. Phase 14 ModalShell + focus trap — highest-risk item, do in one PR across all 8 modal sites
6. Phase 15 mobile — broad CSS surgery, visual regression check required

### References

- CTO ADR: `.10x/decisions/cto/pegasus-gap-analysis.md`
- CTO index: `.10x/decisions/cto/_index.md`

---

## Staff Engineer Layer (added 2026-05-12)

**To:** Senior Engineer + SDE  
**From:** Staff Engineer Agent

### Pattern guide

Every new handler must follow the `handlers_reminders.go` template exactly:
- Use `readJSON`, `pathUUID`, `writeDBError` helpers from `handler.go`
- Use `httpx.WriteJSON` / `httpx.WriteError` for all responses — never write `err.Error()` to client
- New store functions follow `store_reminders.go`: `const q = ...` inside each function, tenant filter on every query, `if out == nil { out = []T{} }` before returning a list
- New standalone validators go in `validate.go` (field-level) or the new `validators_community.go` (structural/per-surface)

### Key patterns resolved in this cycle

| Decision | Outcome | Reference |
|---|---|---|
| Community metadata validator placement | New file `validators_community.go`, switch dispatch, `*metaValidationError` return type | ADR §4 |
| PDF handler file | New file `handlers_ai_pdf.go` — separate from AI text handlers | ADR §6b |
| PDF streaming | Set headers first, then `pdf.Output(w)` directly — no buffering | ADR §6c |
| PDF credit gating | No credit gate on PDF endpoints — credit was spent at text-generation time | ADR §6e |
| Slug collision retry | Loop 5 times with `crypto/rand` suffix, not a single retry | ADR §5a |
| Unicode slug handling | Non-ASCII silently dropped → falls back to `post-{hex}` slug | ADR §5c |
| Focus trap | `<FocusScope trapped loop>` from `@radix-ui/react-focus-scope`, apply to all 8 modal sites in one PR | ADR §8 |
| Sort parameter injection | Use allowlist map — never interpolate `?sort=` raw value into SQL | ADR §12 |

### Example file references

- Handler template: `internal/jobtracker/handlers_reminders.go`
- Store template: `internal/jobtracker/store_reminders.go`
- Validator location: `internal/jobtracker/validate.go`
- Cost constants: `internal/billing/costs.go` (do NOT add PDF cost constants here)
- Frontend page template: `app/recruiters/page.tsx`
- All frontend types: `lib/api-client.ts` (add new `Api*` types at the bottom)
- CSS tokens: `app/globals.css` lines 18–52 (`:root` block)

### Test files to create

- `internal/jobtracker/slug_test.go` — required, covers `slugify()` edge cases including Unicode drop
- `internal/jobtracker/validators_community_test.go` — required, covers all 4 surfaces × valid/invalid enum values

### Staff Engineer ADR

`.10x/decisions/staff-engineer/pegasus-gap-analysis.md`
