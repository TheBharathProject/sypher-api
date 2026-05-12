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

## Current Handoff — To Architect

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
