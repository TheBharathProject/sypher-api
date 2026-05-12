# Project Status

**Project:** sypher-api / Pegasus (job-tracking SaaS)
**Phase:** Planning Complete — ready for implementation
**Date:** 2026-05-12
**Active slug:** `pegasus-gap-analysis`

## What exists (discovered)

- **Backend:** Go 1.25, net/http stdlib, pgx/v5, stateless JWT (HS256), in-process cron
- **Frontend:** Next.js 14.2, App Router, TypeScript, hand-rolled CSS (no Tailwind), basePath `/pegasus`
- **DB:** Postgres 14+, 18 migrations applied (0001–0018)
- **Infra:** Docker + GHCR + OCI VM, GitHub Actions CI, Caddy reverse proxy

## Completed phases

| Phase | Feature | Status |
|---|---|---|
| 1 | Core job tracker (applications, notes, profile, OAuth, JWT) | Done |
| 2 | Files (R2), AI (Deepseek), notifications, email digest | Done |
| 3 | Community (reviews, experiences, referrals, ask, recruiters) | Done |
| 4 | Browser extension API tokens, public profile | Done |
| 5 | Razorpay billing (subscriptions, one-time, credits, webhook) | Done |
| 6 | Backend hygiene (JWT fragment, enum validation, opaque errors, URL validation, Slack) | Done |
| 7 | Frontend small wins (url inputs, source select, aria labels) | Done (partial) |
| 8 | Reminders (DB, CRUD, cron, bell icon + modal) | Done |
| 9 | Personal Recruiters (CRM, CRUD, page, sidebar) | Done |
| 10A | Resume Tweak version history | Done |
| 11 | Resume wizard steps 2–3 (Level + Job Info) | Done |
| 13 | Notes category rename + rate limiting | Done |
| 16 | Cursor pagination on applications | Done |

## Remaining phases — Full task list

### Milestone 0 — Bug Fixes (unblock first)

| Task | Title | Size | Owner | Status |
|---|---|---|---|---|
| TASK-01 | Fix `currentPeriodEnd` null for active subscriptions (B2) | S | SDE backend | Done — already implemented; regression tests added (f0ca0f9) |
| TASK-02 | Add 409 guard to prevent Pro re-subscription (B3) | S | SDE backend | Done — already implemented; regression tests added (f0ca0f9) |

### Milestone 1 — Community Post Slugs

| Task | Title | Size | Owner | Status |
|---|---|---|---|---|
| TASK-03 | Migration `0019_community_slugs.sql` | S | SDE backend | Done (abed2a3) |
| TASK-04 | `slug.go` — `slugify()` + `Store.uniqueSlug()` with collision retry | S | SDE backend | Done (abed2a3) |
| TASK-05 | `slug_test.go` (required) | S | SDE backend | Done (abed2a3) — 13 tests passing |
| TASK-06 | Wire slugs into store + handlers (dual-path lookup) | M | SDE backend | Done (abed2a3) |
| TASK-07 | Add `slug: string` to `ApiCommunityPost` type | S | SDE frontend | Done (7c5e01f) |
| TASK-08 | Community post card links use `post.slug` | S | SDE frontend | Done (7c5e01f) |

### Milestone 2 — Phase 12 Backend

| Task | Title | Size | Owner | Status |
|---|---|---|---|---|
| TASK-09 | Migration `0020_community_sort_indexes.sql` | S | SDE backend | Pending |
| TASK-10 | `ListPosts()` sort param with allowlist SQL guard | S | SDE backend | Pending |
| TASK-11 | `validators_community.go` — per-surface metadata validation | M | SDE backend | Pending |
| TASK-12 | `validators_community_test.go` (required) | S | SDE backend | Pending |
| TASK-13 | Wire validation + sort into `handlers_community.go` | M | SDE backend | Pending |

### Milestone 3 — Phase 12 Frontend

| Task | Title | Size | Owner | Status |
|---|---|---|---|---|
| TASK-14 | Realign enum constants (OUTCOMES, TARGET_ROLES, EXPERIENCE_LEVELS, ROUND_TYPES) — deploy after TASK-13 | S | SDE frontend | Pending |
| TASK-15 | Wire filter/sort buttons to URL query state | M | SDE frontend | Pending |
| TASK-16 | Author chip links to `/u/{authorSlug}` | S | SDE frontend | Pending |
| TASK-17 | Fix community recruiter list from API, remove seed data (B5) | S | SDE frontend | Pending |
| TASK-18 | Populate "Link to tracked application" select in experience modal | S | SDE frontend | Pending |
| TASK-19 | Fix profile URL input fields `type="url"` (B4) | S | SDE frontend | Done — already `type="url"` on all three fields (lines 668, 677, 686); `pnpm tsc --noEmit` passes zero errors |

### Milestone 4 — Phase 10: AI PDFs

| Task | Title | Size | Owner | Status |
|---|---|---|---|---|
| TASK-20 | `go-pdf/fpdf` + `handlers_ai_pdf.go` — 3 PDF endpoints | L | SDE backend | Pending |
| TASK-21 | Frontend "Download PDF" / "Download report" buttons | M | SDE frontend | Pending |

### Milestone 5 — Phase 14: Design System

| Task | Title | Size | Owner | Status |
|---|---|---|---|---|
| TASK-22 | CSS design tokens + stage color tokens + MetricCard mono | S | SDE frontend | Pending |
| TASK-23 | ModalShell + FocusScope — all 8 modal sites in one PR | XL | SDE frontend | Pending |

### Milestone 6 — Phase 15: Mobile CSS

| Task | Title | Size | Owner | Status |
|---|---|---|---|---|
| TASK-24 | Breakpoint fix + iOS modal padding + hover gating + reduced-motion + touch targets | L | SDE frontend | Pending |

### Parallel Track — Test Coverage

| Task | Title | Size | Owner | Status |
|---|---|---|---|---|
| TASK-25 | Billing regression tests (B2/B3) | M | SDE backend | Done — 13 tests passing, -race green (f0ca0f9) |
| TASK-26 | Slug + community validator tests (required, see TASK-05/TASK-12) | S | SDE backend | Partial — TASK-05 (slug tests) done (abed2a3); TASK-12 (validator tests) pending |

## Slug feature spec

`.10x/specs/2026-05-12-community-post-slugs.md` — full spec including:
- Migration `0019_community_slugs.sql` (backfill + unique index)
- `slugify()` + `uniqueSlug()` logic in new `internal/jobtracker/slug.go`
- Dual-path lookup: UUID or slug both work (backward compat)
- Frontend: 1 line change in community list + `slug: string` in `ApiCommunityPost`
- Effort: ~half a day

## CTO ADR

- Written: `.10x/decisions/cto/pegasus-gap-analysis.md`
- Index updated: `.10x/decisions/cto/_index.md`
- Key decisions: gofpdf (go-pdf/fpdf fork) for PDF gen; @radix-ui/react-focus-scope for focus trap
- Highest risk item: Phase 14 ModalShell refactor (8 modal sites, do in one PR)
- Critical tech debt: internal/jobtracker has only 1 test file (mentions_test.go); no handler/store tests

## Open questions — RESOLVED

| Q# | Question | Decision |
|---|---|---|
| Q1 | Naukri Clear GitHub/Discord links — never ported | Resolved: nothing pointing to Naukri stays in Pegasus. Removed "Naukri" from UI copy in `settings/page.tsx` and `applications/page.tsx`. `NAUKRI` source enum kept (functional job board value with backend importer). No Naukri Clear competitor links to add. |
| Q2 | Community `OUTCOMES` enum drift | Resolved: hard-reject new writes only (400 bad_input on POST/PATCH). Existing DB rows untouched. |
| Q3 | Avatar upload blocks v1.0? | Resolved: P2, ships post-launch. Initials-fallback acceptable at v1.0. |

## CI fix (pending push)

`go.mod` was updated by `go mod tidy` — `golang.org/x/time` promoted from `// indirect` to direct.
Push with: `git add go.mod go.sum && git commit -m "fix: promote golang.org/x/time to direct dependency" && git push`
