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
| 12 backend | Community sort indexes + per-surface metadata validation | Medium | 1st |
| 12 frontend | Enum realignment, filter URL state, sort tabs | Medium | 2nd |
| 10 | AI PDFs (go-pdf/fpdf) + Avatar upload (files table kind='avatar') | High | 3rd |
| 14 CSS | Design tokens, kanban tints | Medium | 4th |
| 14 a11y | ModalShell, focus trap (@radix-ui/react-focus-scope), inert | Medium | 5th |
| 15 | Mobile breakpoints, iOS Safari modal, hover gating, touch targets | Low | 6th |

## CTO ADR

- Written: `.10x/decisions/cto/pegasus-gap-analysis.md`
- Index updated: `.10x/decisions/cto/_index.md`
- Key decisions: gofpdf (go-pdf/fpdf fork) for PDF gen; @radix-ui/react-focus-scope for focus trap
- Highest risk item: Phase 14 ModalShell refactor (8 modal sites, do in one PR)
- Critical tech debt: internal/jobtracker has only 1 test file (mentions_test.go); no handler/store tests

## Open questions

None — sequencing and build/buy decisions are resolved in the ADR.
