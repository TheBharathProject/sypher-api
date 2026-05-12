# ADR: Pegasus Gap Analysis — Remaining Phases Technical Strategy

**Status:** Accepted  
**Date:** 2026-05-12  
**Slug:** pegasus-gap-analysis  
**Authors:** CTO Agent (claude-sonnet-4-6)

---

## Context

Pegasus (job-tracking SaaS at `sypher.in/pegasus`) has shipped 13 phases. This ADR provides a verified architectural inventory of what has been built, a technical gap analysis for the four remaining phases (10, 12, 14, 15), build-vs-buy decisions for the two non-trivial library choices, a risk assessment, and a recommended sequencing. All findings are based on direct inspection of source files.

---

## What Has Been Built — Verified Architectural Summary

### Backend (`sypher-api`) — Phases 1–9, 10A, 11, 13, 16

**Core stack confirmed:**  
Go 1.25, stdlib `net/http` (Go 1.22+ pattern routing), `pgx/v5`, stateless HS256 JWT, `golang.org/x/time/rate` token-bucket rate limiter, in-process cron (`cron.Runner`). No framework. No ORM.

**Routing** (`internal/jobtracker/routes.go`): 60+ routes registered across me, applications, notes, profile, files, AI, tweaks, recruiters, reminders, community, notifications. Both `PUT` and `PATCH` registered for backward-compat with browser extension. Public routes bypass `requireUser` middleware.

**Database:** 18 migrations applied (0001–0018). Next migration number is `0019_`. Schemas: `auth`, `job_tracker`, `billing`, `waitlist`, `ai_usage`. All tables use `ON DELETE CASCADE` for tenant isolation. Migration runner is idempotent (`public._migrations_applied` PK guard).

**Files** (`store_files.go`, `handlers_files.go`): `job_tracker.files` table with `kind TEXT` column. Currently valid kinds: `resume`, `cover_letter`. Avatar is NOT a recognized kind yet — no DB constraint, no handler, no route. The `ledongthuc/pdf` library (v0.0.0-20250511090121) is imported and used exclusively in `handlers_ai.go:extractPDF()` to read uploaded PDFs for AI text extraction. There is no PDF generation capability in the codebase.

**AI** (`handlers_ai.go`): Deepseek client for report, cover letter, extract, tweak. Credit costs: `CostResumeReport=25`, `CostResumeTweak=20`, `CostCoverLetter=10`. Free token quota: 25k/month. No PDF output endpoints exist.

**Community** (`handlers_community.go`, `store_community.go`): Full CRUD, voting (atomic CTE tx), soft-delete comments, per-surface rate limiting (5 posts/surface/day, 50 comments/day, 100 votes/day). `ListPosts()` sorts by `created_at DESC` only — no `?sort=` parameter support. No per-surface metadata validation beyond title length (1–280 chars) and body length (max 16k chars). Metadata is stored as JSONB and passed through unvalidated. The community index (`idx_community_posts_surface_created`) supports only time-ordered queries; there is no `vote_count DESC` or `comment_count DESC` index.

**Profile:** `picture_url` column exists on `auth.users` (read in `store.go:27`, upserted from Google OAuth in `store.go:44–49`). No avatar upload endpoint. No partial unique index on `files WHERE kind='avatar'`.

**Observability:** Structured JSON logging via `log/slog` only. No distributed tracing, no metrics endpoint, no Prometheus, no Sentry, no request-ID propagation header. The `withLogging` middleware logs method, path, status, duration_ms, and IP — that is the full extent of observability.

**Test coverage:** Tests exist for JWT, Razorpay client/webhook HMAC, AI usage quota, CSV/LinkedIn/Naukri importers, IST scheduling math, URL builder, @mention parsing, and email templates. No tests for: applications CRUD handlers, community handlers, notes handlers, profile handlers, billing store, reminder cron logic, or any DB-integration path. Entire `internal/jobtracker/` package has only `mentions_test.go`.

### Frontend (`job-tracker`) — All shipped phases

**Tech stack confirmed:** Next.js 14.2, React 18, TypeScript 5, zero runtime dependencies beyond Next.js itself (`package.json` has only `next`, `react`, `react-dom`). No Radix UI, no Headless UI, no focus-scope library, no design-token library.

**CSS:** Hand-rolled `globals.css` (~9,200 lines). No Tailwind. `720px` breakpoint used in at least 10 distinct `@media (max-width: 720px)` rules — confirmed at lines 3477, 3941, 6670, 6808, 7274, 7296, 7493, 7944. No `@media (hover: hover)` wrappers anywhere — 113 `:hover` rules fire on touch devices. No `@media (prefers-reduced-motion)` wrapper. `place-items: center` used in 38 modal overlays — no `padding-top: 8svh` (iOS Safari address-bar fix absent throughout).

**Modal a11y:** `role="dialog"`, `aria-modal="true"`, and `aria-labelledby` are wired in `app/applications/page.tsx` (lines 805–807, 959–961, 1092–1094, 1164–1166, 1380–1382). No focus trap is implemented anywhere — confirmed by absence of any `useFocusTrap`, `FocusScope`, or `inert` attribute usage. When a modal opens, background content remains keyboard-reachable.

**Stage color tokens:** Partial implementation exists. `globals.css` defines `--stage-interest`, `--stage-applied`, `--stage-screen`, `--stage-offer`, `--stage-offer-ink` (lines 40–44). Missing: `--stage-phone`, `--stage-technical`, `--stage-onsite`, `--stage-rejected`. Kanban column bodies do not use any `var(--stage-*)` background — they use hardcoded colors or none.

**Design tokens:** `--bg-hover` exists in dark-theme block at line 585 but is not tokenized into a CSS variable declaration at the `:root` level. `--font-mono` is not a declared token; inline stacks like `ui-monospace, SFMono-Regular, ...` are repeated 8+ times across the file. `--radius-sm` and `--radius-2xl` do not exist.

---

## Gap Analysis by Phase

### Phase 10: AI PDFs + Avatar Upload

**What is missing — backend:**

1. PDF generation: No PDF output handler exists anywhere. The existing `ledongthuc/pdf` library reads PDFs but cannot write them. Two new routes are needed: `POST /job-tracker/ai/cover-letter/pdf` and `POST /job-tracker/ai/resume/tweaks/{id}/pdf`. These must stream `application/pdf` bytes to the client.

2. Avatar upload: No routes, no handler, no store function. The `job_tracker.files` table schema supports a free-form `kind` column (migration 0004) but has no partial unique index on `kind='avatar'`. The `auth.users.picture_url` column exists and is read, but there is no write path from the application itself (only Google OAuth sets it). Three new routes needed: `POST /job-tracker/profile/avatar/upload-url`, `PATCH /job-tracker/profile/avatar/finalize`, `DELETE /job-tracker/profile/avatar`. On finalize, `auth.users.picture_url` must be set; on delete, it must be cleared. A migration (`0019_`) is required to add a partial unique index: `CREATE UNIQUE INDEX ... ON job_tracker.files (user_id) WHERE kind = 'avatar'`.

**What is missing — frontend:**

3. "Download PDF" buttons in: cover letter modal (`app/applications/page.tsx` around the `cover-letter-title` dialog), tweak resume modal (around `tweak-title` dialog), and resume report results step in `app/resume/page.tsx`.

4. Avatar change/remove UI in `app/profile/page.tsx` (referenced in handoff around lines 353–366 but not verified line-by-line — reading the actual handler is sufficient confirmation that nothing exists on the backend side, which is the blocker).

**Build vs Buy — PDF Generation:**

Three options were considered:

_Option A: `gofpdf` (github.com/jung-kurt/gofpdf or go-pdf/fpdf fork)_  
Pure Go. Zero runtime dependencies. Produces deterministic, streaming PDF. FPDF is the Go port of PHP's FPDF library — mature, well-understood. Produces text-first PDFs. Limitation: no HTML/CSS rendering; formatting must be done programmatically (set font, write line, add box). For a cover letter and a resume tweak (which are Markdown/plaintext), this is acceptable — the layout is simple: header block, body paragraphs. Cost: ~1 engineer-day to implement both endpoints. Recommended.

_Option B: Headless Chromium (via `chromedp`)_  
Renders HTML/CSS to PDF — highest fidelity for complex layouts. Cost: ~400MB Docker image increase; requires Chromium installed in the container; cold start latency 2–3s per PDF; significant OCI VM memory pressure (currently a single-container setup). Not justified for plain-text cover letters and resume tweaks. Overkill for v1.

_Option C: `wkhtmltopdf`_  
Binary dependency, not maintained since 2022, uses deprecated WebKit. Would require installing a system binary in Docker. Slightly better HTML support than gofpdf but far worse than Chromium. Worst of both worlds — binary overhead without Chromium's fidelity.

**Decision: Use `gofpdf` (specifically `github.com/go-pdf/fpdf`, the maintained fork).** This is a two-way door — if richer PDF layouts become a product requirement later, the endpoint contract stays identical and the library is swapped inside the handler. The Docker image stays lean.

---

### Phase 12: Community Polish

**What is missing — backend:**

1. Per-surface metadata validation: `CreateCommunityPost` in `handlers_community.go:130–173` performs only title/body length checks. The `metadata` JSONB field is stored verbatim. The handler must validate per-surface: for `experiences` — `outcome` enum (`Offer`, `Reject`, `Ghosted`, `InProgress`, `Withdrew`), `difficulty` enum (`Easy`, `Medium`, `Hard`), `rounds` non-empty; for `ask` — `tags[]` max 3 elements from a 10-value allow-list; for `recruiters` — `specializations` from 12-value list, `hiringLevels` from 8-value list; for `reviews` — `targetRole` and `experienceLevel` from spec enums.

2. Sort modes: `ListPosts()` in `store_community.go:124` uses `ORDER BY p.created_at DESC` hardcoded. The `?sort=` query parameter is not read by `parseListOpts()`. Adding `votes` sort requires a `vote_count DESC, created_at DESC` order — the existing `idx_community_posts_surface_created` index does not cover this. A new index on `(surface, vote_count DESC, created_at DESC) WHERE status = 'active'` is needed. `most-reviewed` / `least-reviewed` similarly need a `comment_count` index. A migration (`0019_` or `0020_`) is required.

**What is missing — frontend:**

3. Enum realignment: `TARGET_ROLES`, `EXPERIENCE_LEVELS`, `ROUND_TYPES` constants in `app/community/[section]/page.tsx` need value updates to match spec. This is a pure frontend change with no backend dependency.

4. Author chip linkification, tag-to-filter links, URL-synced filter state, sort tabs on the recruiter surface, and "link to tracked application" select in the experience modal. These are all frontend-only for the filter/link items, but the application-link select requires fetching the user's application list (API already exists: `GET /job-tracker/applications`).

**No new API endpoints are needed for Phase 12 beyond the sort parameter on the existing list endpoint.**

---

### Phase 14: Design System Polish

**What is missing:**

1. CSS token gaps: `--bg-hover` exists in dark theme body but not as a `:root` custom property. `--font-mono` does not exist as a token. `--accent-soft`, `--radius-sm`, `--radius-2xl` do not exist. Light-theme mirrors for all new tokens needed.

2. Stage color tokens: 4 of 7 tokens missing. Kanban column tints require adding `--stage-phone`, `--stage-technical`, `--stage-onsite`, `--stage-rejected` and applying them as `background-color: var(--stage-{stage})` on kanban column bodies in `app/applications/page.tsx`.

3. `ModalShell` promotion: The modal pattern is currently inline in each modal site. Promoting a shared `ModalShell` export to `components/ui.tsx` reduces duplication but requires refactoring all 8 modal locations in a single pass to avoid inconsistency during transition.

4. Modal accessibility — focus trap: No focus trap implemented. 8 modal sites need it. Focus trap is non-trivial to implement correctly (must handle dynamically rendered content, portals, the browser's native focus algorithm edge cases with `tabindex=-1`, and restoration on close).

**Build vs Buy — Focus Trap:**

_Option A: Roll our own `useFocusTrap` hook_  
A basic focus trap is ~50 lines of TypeScript: query all focusable elements, listen for Tab/Shift+Tab keydown, cycle between first and last. Edge cases: elements removed while modal is open, dynamically added elements, nested focusables inside shadow DOM (not applicable here), iframe focusables (not applicable). For this specific codebase — static modal content, no portals, no iframes — a custom hook is viable and keeps the zero-dependency `package.json` intact. Cost: ~0.5 engineer-days including testing.

_Option B: `@radix-ui/react-focus-scope`_  
Battle-tested, handles every edge case, 2kB gzip. Adds a runtime dependency. The `package.json` currently has zero UI-library dependencies — this would be the first. However, since Radix's focus-scope is a standalone package (not the full Radix ecosystem), the dependency surface is minimal. Cost: npm install + 10 lines of code per modal site.

**Decision: Use `@radix-ui/react-focus-scope`.** The correct-by-default behaviour and ongoing edge-case maintenance from the Radix team outweigh the philosophical purity of zero dependencies. The `inert` attribute on `<main>` (which also blocks screen-reader traversal into background content) should be set alongside. This is also a two-way door — the package can be removed and replaced with a custom hook without changing the modal API surface.

Additionally: `aria-labelledby` is already wired in the applications page modals. Verify other modal sites (`app/profile/page.tsx`, `app/community/`, `app/notes/`) before calling Phase 14 complete.

---

### Phase 15: Mobile Responsiveness

**What is missing:**

1. Breakpoint consistency: The main layout breakpoint appears at `720px` in at least 10 rules. The spec target is `768px` (standard `48rem` for tablet portrait). This is a 48px gap that affects layout on devices 720–767px wide. Replace `max-width: 720px` with `max-width: 767px` (or use `48rem`) throughout.

2. iOS Safari modal: All 38 uses of `place-items: center` in modal overlays center the dialog relative to the viewport — which on iOS Safari is the full-height viewport including the hidden address bar. When the address bar slides in, dialogs jump. The fix is `place-items: start center; padding-top: 8svh` on the overlay grid. `svh` (small viewport height) accounts for the minimum visible viewport.

3. Hover gating: 113 `:hover` rules fire on touch devices where hover state gets stuck after tap. All `:hover` rules should be wrapped in `@media (hover: hover)`. This is a global find-and-replace pass.

4. Reduced-motion: No `@media (prefers-reduced-motion)` wrapper exists. All CSS transitions (sidebar collapse, modal fade, button hover) should be inside a `no-preference` media query.

5. Touch targets: 24px icon buttons exist throughout. Minimum accessible touch target is 44px (WCAG 2.5.5). Adding invisible padding via `::before` pseudo-element is the standard fix without changing visual size.

**This phase has no backend changes.**

---

## Cross-Cutting Gaps

**Test coverage (critical gap):** The entire `internal/jobtracker` package — which handles 60+ routes covering every product feature — has one test file (`mentions_test.go`) testing a pure-string function. There are no handler tests, no store integration tests, no HTTP-level tests. Adding coverage here is the highest-risk gap because changes to any handler can silently break existing users with no safety net. Recommended approach: use `httptest.NewRecorder` + the existing handler struct for unit tests, and a `testcontainers-go` Postgres container for integration tests. Target the highest-risk paths first: billing webhook HMAC, credit deduction, cursor pagination edge cases.

**Observability gap (medium):** The API emits structured slog lines but has no request-ID header, no distributed trace, no metrics endpoint. On the single-container OCI deployment this is tolerable — `docker logs` is the primary debugging tool. If traffic grows to where correlating a user-reported error to a specific request becomes hard, add `X-Request-ID` propagation (a UUID injected in `withLogging`, echoed in the response header, and included in every slog call). This is 20 lines of code and a two-way door.

**Missing public API contracts:** No OpenAPI spec exists. All type definitions live in `internal/jobtracker/types.go` (Go) and `lib/api-client.ts` (TypeScript). Drift between these two is a latent risk — the browser extension is also a consumer. Consider generating a types file from the Go structs or maintaining a schema file.

**Single-pod cron assumption:** All cron jobs assume one running instance. `fire-reminders` uses `FOR UPDATE SKIP LOCKED` which is correct for multi-instance, but `stale-apps` and `daily-digest` do not. If a second container is ever started (e.g. blue-green deploy), both will fire the daily jobs. This is acceptable at current scale but should be documented as an explicit constraint, not discovered the hard way.

---

## Technical Risk Assessment by Phase

| Phase | Risk | Rationale |
|---|---|---|
| 10 (AI PDFs + Avatar) | Medium | New library (`go-pdf/fpdf`) in critical path. Avatar requires a new migration + auth.users write path. Risk is additive (new routes), not modifying existing ones. Highest backend complexity of the remaining phases. |
| 12 (Community Polish) | Low-Medium | Sort index addition (0019_) is additive. Per-surface metadata validation is a new code path in `CreateCommunityPost` — incorrect validation would 400-reject valid existing clients. Test the validation logic in isolation. Frontend enum changes are purely cosmetic. |
| 14 (Design System) | Medium | `ModalShell` promotion is the highest-risk item: refactoring 8 modal sites simultaneously. A partial refactor (some modals on new shell, some old) will create CSS inconsistencies that are hard to debug. Do it in one PR or not at all. CSS token additions are additive and zero-risk. |
| 15 (Mobile) | Low-Medium | Global `:hover` → `@media (hover: hover)` wrap is a large find-and-replace across ~9,200 lines of CSS. Risk of missing a selector or introducing a specificity conflict. The `720px → 768px` breakpoint change could visually regress any layout tested at exactly 720–767px width. Snapshot test the main layouts before and after. |

The phase with highest risk of breaking existing functionality is **Phase 14** (ModalShell refactor) followed by **Phase 15** (global CSS surgery). Phases 10 and 12 are additive.

---

## Decision — Recommended Sequencing

**Rationale:** Sequence by (1) user impact, (2) risk isolation, (3) dependency order.

1. **Phase 12 backend first (sort indexes + validation migration):** The migration is additive and the validation is new code in a new path. No existing functionality is touched. Unblocks frontend enum work immediately.

2. **Phase 12 frontend (enum realignment, filter URL state, sort tabs):** Pure frontend, no backend blocker after migration. Low risk. Delivers visible community quality improvement.

3. **Phase 10 (AI PDFs + Avatar):** Backend-heavy. Add `go-pdf/fpdf` to `go.mod`, implement PDF endpoints, write the avatar routes + migration. Frontend "Download PDF" buttons are thin wrappers. Do avatar frontend in the same PR as backend to avoid a broken half-state in production.

4. **Phase 14 CSS tokens + kanban tints:** Additive CSS changes, zero risk. Deliver this before touching modal a11y.

5. **Phase 14 ModalShell + focus trap + `inert`:** Highest-risk item. Install `@radix-ui/react-focus-scope`. Refactor all 8 modal sites in one PR. Verify with keyboard navigation manually.

6. **Phase 15 (Mobile):** Last because it is purely cosmetic/a11y and carries the most "broad but shallow" change surface. Run a visual regression check (even manual, at 320px / 375px / 768px) before shipping.

**Outside this sequencing:** Bolster `internal/jobtracker` test coverage is a parallel track that should begin immediately and is not gated on any phase.

---

## Trade-offs Accepted

- Staying with `gofpdf`/`go-pdf/fpdf` means PDF output will be text-first with limited design fidelity. If the product ever needs branded, pixel-perfect PDFs, this needs to be revisited (Chromium route).
- Adding `@radix-ui/react-focus-scope` breaks the zero-runtime-dependency frontend constraint. Accepted because the alternative (custom focus trap) carries ongoing maintenance risk for a subtle browser behaviour.
- No OpenAPI spec will be generated now. Acceptable while the browser extension is the only third-party consumer and the extension author is the same as the API author.
- Test coverage gap in `internal/jobtracker` remains a technical debt item with no scheduled resolution sprint. Accepted as a known risk at current user scale.

---

## Success Criteria

- Phase 10: `POST /job-tracker/ai/cover-letter/pdf` returns a valid `application/pdf` response. Avatar upload/delete round-trips correctly. `picture_url` is updated in `auth.users` on finalize and cleared on delete.
- Phase 12: `?sort=votes` returns posts ordered by `vote_count DESC`. Per-surface validation rejects invalid `outcome`/`difficulty`/`tags` values with `400 bad_input`. Frontend enum selects show the spec-aligned values.
- Phase 14: All 8 modal sites pass a keyboard-only navigation test (Tab cycles within modal, Escape closes, focus returns to trigger element). All 7 stage color tokens exist and are applied to kanban column backgrounds.
- Phase 15: No layout regressions at 320px, 375px, 768px viewport widths. iOS Safari modal does not jump when address bar appears. `:hover` states do not stick on tap on a touch device.

---

## Review Point

Revisit this ADR after Phase 10 ships (expected ~2 engineer-weeks from sequencing start). Specifically review:

1. Whether `go-pdf/fpdf` output quality is acceptable to users or if a Chromium-based approach is needed.
2. Whether the observability gap (no request-ID, no traces) is causing production debugging pain.
3. Whether the browser extension needs a versioned API contract (OpenAPI) due to divergence.
