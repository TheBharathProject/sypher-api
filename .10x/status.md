# Project Status

**Project:** sypher-api / Pegasus (job-tracking SaaS)  
**Phase:** CTO ADR complete — ready for PM + Architect  
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

## Remaining phases

| Phase | Feature | Priority | Recommended order |
|---|---|---|---|
| community-slugs | Human-readable slugs on community post URLs | P1 | 0th — ships with Phase 12 |
| 12 backend | Community sort indexes + per-surface metadata validation | Medium | 1st |
| 12 frontend | Enum realignment, filter URL state, sort tabs | Medium | 2nd |
| 10 | AI PDFs (go-pdf/fpdf) + Avatar upload (files table kind='avatar') | High | 3rd |
| 14 CSS | Design tokens, kanban tints | Medium | 4th |
| 14 a11y | ModalShell, focus trap (@radix-ui/react-focus-scope), inert | Medium | 5th |
| 15 | Mobile breakpoints, iOS Safari modal, hover gating, touch targets | Low | 6th |

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

## Open questions (need answers before Phase 2 / Architect dispatch)

| Q# | Question | Options |
|---|---|---|
| Q1 | GitHub + Discord links in Settings — old Naukri Clear branding, never ported | (a) link to Pegasus-branded repos/Discord (b) leave out of v1.0 |
| Q2 | Community `OUTCOMES` enum drift — old posts may have "Rejected"/"In Progress" vs correct "Reject"/"InProgress" | (a) coerce on read (b) hard-reject new writes only (c) DB migration to normalize first |
| Q3 | Avatar upload — block v1.0 launch or ship as initials-fallback and add post-launch? | (a) block v1.0 (b) post-launch |

## CI fix (pending push)

`go.mod` was updated by `go mod tidy` — `golang.org/x/time` promoted from `// indirect` to direct.
Push with: `git add go.mod go.sum && git commit -m "fix: promote golang.org/x/time to direct dependency" && git push`
