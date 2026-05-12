# ADR: Pegasus Gap Analysis — Feature Completeness, Remaining Scope, and v1.0 Definition

**Status:** Accepted  
**Date:** 2026-05-12  
**Author:** Product Manager Agent  
**Feature slug:** pegasus-gap-analysis  
**Source of truth:** `/Users/shubham.dixit/Desktop/Personal/MyContent/SAAS/findings.md` (live crawl of naukriclear.com, 2026-05-08)  
**Handoff context:** `/Users/shubham.dixit/Desktop/Personal/MyContent/SAAS/HANDOFF.md`

---

## Problem Statement

Pegasus has completed 13 of its 16 planned development phases, but the remaining work is undocumented at a requirements level. Engineers and QA are working from a checklist in HANDOFF.md without user stories, acceptance criteria, or a clear definition of "done" for v1.0. This ADR establishes that definition.

**Target user:** Indian job-seekers (primarily SDE, PM, QA, Data) who are actively applying to 5–50 companies simultaneously and need a calm, private system to track their pipeline — plus a community layer for peer advice and recruiter discovery.

---

## Context

### What phases are complete

Phases 1–9, 10A, 11, 13, and 16 are complete (per HANDOFF.md §16). The remaining open phases are:

- Phase 10 (remainder): AI PDFs + Avatar upload
- Phase 12: Community polish
- Phase 14: Design system polish
- Phase 15: Mobile responsiveness
- A set of small unverified items from Phase 7 that may or may not have landed

---

## Decision: Feature Completeness Audit

Every feature from the spec (findings.md) is audited below. Status codes:

- Complete (built and working)
- Partial (built but missing sub-features)
- Missing (not built at all)

### Core Application Tracker

| Feature | Status | Evidence |
|---|---|---|
| Add / Edit / Delete application | Complete | Modal confirmed with all 11 fields including `applyDeadline` and `source` select |
| Stage pipeline (7 stages) | Complete | `INTERESTED`, `APPLIED`, `PHONE_SCREEN`, `TECHNICAL`, `ONSITE`, `OFFER`, `REJECTED` — confirmed in page.tsx |
| Source select (5 values + blank) | Complete | `LINKEDIN`, `NAUKRI`, `REFERRAL`, `COMPANY_SITE`, `OTHER` wired correctly with UPPERCASE enum values |
| List view | Complete | Table with COMPANY / ROLE / STAGE / SOURCE / APPLIED / UPDATED columns |
| Board (kanban) view | Complete | 7 columns with drag-and-drop and optimistic UI |
| CSV export | Complete | Confirmed `GET /job-tracker/applications/export` |
| CSV import (2-stage) | Complete | Template download + preview + column mapper + commit |
| Cursor pagination ("Load more") | Complete | `nextCursor` pattern implemented in `loadMore()` |
| `applyDeadline` field in modal | Complete | Field present at line 1022–1029 of applications/page.tsx, type="date" |
| Source as `<select>` | Complete | Confirmed at lines 998–1013 of applications/page.tsx |
| Reminder (bell icon per row) | Complete | Quick-pick + datetime-local modal, POSTs to reminders endpoint |
| Application detail view (modal) | Complete | Sidebar with Details, AI tools, and Timeline sections |
| Stage history timeline | Complete | `ApplicationTimeline` component wired to `/timeline` endpoint |
| Deep-link via `?view=<id>` | Complete | URL param handled with grace timeout and toast |

### Dashboard

| Feature | Status | Evidence |
|---|---|---|
| Stat tiles (Total / Pipeline / Interviews / Offers) | Complete | Both on dashboard and applications page |
| Pipeline funnel chart | Complete | `PipelineFunnel` component confirmed in tree |
| Weekly activity chart | Complete | `WeeklyActivityChart` — 8 weeks bar chart |
| Top sources panel | Complete | LinkedIn / Other split confirmed |
| CTA cards | Complete | Track applications + Connect recruiters |

### AI Features

| Feature | Status | Evidence |
|---|---|---|
| Resume report (4-step wizard) | Complete | Upload > Level > Job Info > Results wizard (Phase 11) |
| Resume report PDF download | Partial | Backend endpoint `POST /ai/resume/tweak/pdf` in bundle; no "Download as PDF" button on results page per HANDOFF.md §17 |
| Resume tweak (version history) | Complete | Create / list / get / patch / delete + side-by-side diff view (Phase 10A) |
| Resume tweak PDF download | Missing | No "Download PDF" button in tweak modal per HANDOFF.md §17 |
| Cover letter generation | Complete | Modal wired to `/ai/cover-letter`, shows CREDIT_COSTS |
| Cover letter PDF download | Missing | No "Download PDF" button; endpoint `POST /ai/cover-letter/pdf` exists in bundle but UI not wired |
| AI usage quota display | Complete | Usage fetched and displayed in settings billing section |
| Credits billing model | Complete | SpendCredits / GrantCredits wired; TopUp link present |

### Profile and Public Page

| Feature | Status | Evidence |
|---|---|---|
| Profile editor (About, Experience, Education, Projects, Skills) | Complete | Full CRUD confirmed in HANDOFF.md |
| Avatar change / remove UI | Partial | UI hooks exist in profile/page.tsx (line 353–366 per HANDOFF.md); backend avatar upload endpoints NOT built |
| Public profile `/u/{slug}` | Complete | Route exists, public endpoints confirmed |
| Slug management | Complete | Live availability check + inline editor confirmed in settings/page.tsx |
| Profile visibility toggle | Complete | Hide checkbox wired to `/profile/visibility` |
| URL validation on LinkedIn/GitHub/Website inputs | Partial | `type="url"` on LinkedIn in community recruiter modal confirmed. Profile About form status unverified — HANDOFF.md §17 flags this as a small unverified item |

### Vault (File Storage)

| Feature | Status | Evidence |
|---|---|---|
| Resume slot upload (5 slots) | Complete | Two-step R2 upload confirmed |
| Cover letter slot upload (5 slots) | Complete | Mirror structure confirmed |
| Slot labels | Complete | Label field in finalize |
| Vault-to-tweak integration | Complete | Resume picker in tweak modal pulls from `/resumes` |

### Notes

| Feature | Status | Evidence |
|---|---|---|
| Notes CRUD | Complete | Left-rail + editor pattern confirmed |
| Categories with rename | Complete | Phase 13 added PATCH rename |
| Markdown rendering | Complete | `lib/markdown.ts` present |

### Notifications

| Feature | Status | Evidence |
|---|---|---|
| In-app bell badge | Complete | Unread count polling via `useUnreadCount()` |
| Notification list with mark-read | Complete | Cursor-paginated list confirmed |
| Stale-app notifications (cron) | Complete | `stale-apps` cron at 03:00 IST |
| Reminder notifications (cron) | Complete | `fire-reminders` cron every 5 minutes |
| Email digest (premium) | Complete | `daily-digest` cron at 09:00 IST; gated behind `isPremium` |
| Email pref toggle in settings | Complete | Toggle wired to `/me/email-prefs`, premium-gated with Upgrade CTA for free users |

### Settings

| Feature | Status | Evidence |
|---|---|---|
| Account section (Name, Email, Timezone) | Complete | Read-only display confirmed |
| Appearance / theme switcher | Complete | System / Light / Dark with OS follow mode |
| Billing section (premium hero + credits card + history) | Complete | Full billing UI confirmed in settings/page.tsx |
| Cancel subscription modal | Complete | Proper modal with period-end date (not window.confirm) |
| Profile section (slug + visibility) | Complete | Confirmed |
| Chrome extension section | Complete | Chrome Web Store link, token generation, extension detection, auto-send |
| GitHub link in settings | Missing | No GitHub link present anywhere in settings/page.tsx — only Chrome Web Store link. The crawl finding mentioned `github.com/Jaan-Mustafa/Naukri-Clear-Extenstion` but this is absent from the current code. The app name has migrated from "Naukri Clear" to "Pegasus" — the old GitHub URL is stale and omitted intentionally or accidentally. |
| Discord link in settings | Missing | No Discord link present in settings/page.tsx. The crawl finding mentioned `discord.gg/YJARrBfp` (old branding). The feedback section exists but contains only a textarea and Send button — no Discord link. |
| Feedback form | Complete | Textarea + Send button, POSTs to `/feedback` which writes to DB + Slack |
| Sign-out in settings | Complete | "Sign out" button present at line 519–526 of settings/page.tsx (calls `/auth/logout` + clears token) |

### Sidebar

| Feature | Status | Evidence |
|---|---|---|
| Sign-out icon button in sidebar user card | Complete | Confirmed in frames.tsx — `SidebarUser` renders a `<button className="signout-icon sidebar-signout-btn">` with `ArrowUpRightIcon`, aria-label "Sign out", at lines 438–450 |
| Avatar in sidebar (Google photo or initials) | Complete | Conditional: Google photo if `pictureUrl` exists, else initials tile |
| Credits balance in sidebar | Complete | `SidebarBilling` component reads from `useBillingMe()` cache |
| Unread notification dot + badge | Complete | `useUnreadCount()` drives both dot and count badge |

### Community

| Feature | Status | Evidence |
|---|---|---|
| Reviews surface (list + post) | Partial | List wired to real API; modal exists. Backend metadata validation not enforced (Phase 12 item) |
| Experiences surface | Partial | List wired; modal exists with rounds. "Link to tracked application" select is stub (always shows "None — enter manually") — Phase 12 item |
| Referrals surface | Partial | List wired; modal exists |
| Ask surface (list + post + tags) | Partial | List wired; Ask modal with tag chip input exists. Tag filters are decorative buttons (not wired to URL/query) — Phase 12 item |
| Recruiter directory (list + add) | Partial | Seed data rendered client-side (not from API) for recruiter list. Real posts from API shown when available |
| Voting | Complete | Backend endpoint confirmed; frontend `voteCount` display in post list |
| Comments | Partial | Backend wired; frontend post detail page (`/community/posts/[id]`) exists but not audited in this pass |
| Author chip links to public profile | Missing | Post rows show `post.authorName` as plain text, not a link to `/u/{slug}`. Phase 12 item. |
| Sort tabs wired to query | Missing | Filter buttons are `<button type="button" className="filter-box">` with no `onClick` handler — entirely decorative. Phase 12 item. |

### Billing

| Feature | Status | Evidence |
|---|---|---|
| Standard subscription (₹99/mo) | Complete | `/billing/checkout/subscription` |
| Plus subscription (₹299/mo) | Complete | `/billing/checkout/subscription-plus` |
| One-time premium pass | Complete | `/billing/checkout/one-time` |
| Credit packs | Complete | `/billing/checkout/credits` with `packId` |
| Cancel / resume subscription | Complete | Modal-based cancel with period-end date shown |
| Billing history | Complete | Collapsible timeline in settings |
| Webhook idempotency | Complete | `processed_events` PK + HMAC verify |
| Pro gate on AI features | Complete | `SpendCredits` + `ErrInsufficientCredits` → 402 |

### Browser Extension

| Feature | Status | Evidence |
|---|---|---|
| API token issue / revoke | Complete | Full token management UI in settings |
| Extension detection | Complete | `data-pegasus-extension="1"` attribute check |
| Auto-send token to extension | Complete | `postMessage` bridge wired |
| Check-link endpoint | Complete | `GET /job-tracker/applications/check-link?jobLink=` |

### Personal Recruiters

| Feature | Status | Evidence |
|---|---|---|
| Private CRM (add / edit / delete / search) | Complete | Phase 9 — full CRUD |

---

## User Impact Ranking — Partial and Missing Items

| Item | Status | Impact | Rationale |
|---|---|---|---|
| Cover letter PDF download | Missing | High | Cover letter generation without download is a dead end — users can't use the output |
| Resume tweak PDF download | Missing | High | Same as above — a polished tweak version with no export mechanism loses trust |
| Resume report PDF download | Partial | High | "Download report" is the natural conclusion of the 4-step wizard; missing it breaks the flow |
| Community filter buttons wired to query | Missing | High | Filter buttons that do nothing actively mislead users; community is already low-content, bad filtering amplifies emptiness |
| Avatar upload (backend + frontend) | Partial | Medium | Public profiles without avatar are faceless; it reduces the perceived quality of `/u/{slug}` pages |
| Author chip links to public profile | Missing | Medium | Community posts lose their social graph dimension — users can't discover contributors |
| "Link to tracked application" select populated | Missing | Medium | The field is a stub that defaults to "None" with no options; the UX implies a feature that doesn't work |
| Sort tabs wired to query | Missing | Medium | Invisible at low content volume; visible once community grows |
| Backend metadata validation per surface | Missing | Medium | Without server-side enum enforcement, invalid outcomes and round types can be submitted and displayed, degrading content quality |
| URL validation on profile About form | Partial | Medium | The crawl found a live `<a href="hasdkjaamc">` on production — a broken link visible to every viewer of that public profile |
| GitHub / Discord links in settings | Missing | Low | These were present in the old Naukri Clear branding; their absence in Pegasus settings is likely intentional rebranding. Low impact unless community building depends on Discord |
| Mobile breakpoint drift (720px vs 768px) | Missing | Low | 48px gap causes layout breaks on exactly 720–767px viewport width — a narrow band, but addressable |
| Modal ARIA / focus trap | Partial | Low | Accessibility gap. Low immediate user impact but a pre-launch hygiene requirement |

---

## Scope of Remaining Work — By Phase

---

### Phase 10 (Remainder): AI PDFs + Avatar Upload

**Problem:** Users who generate AI content (cover letters, tweaked resumes, resume reports) have no way to export that content as a file. Avatar upload requires both a backend endpoint (presigned R2 URL) and frontend wiring that is partially built.

**Target users:** Job-seekers who generate AI content and need to attach it to job applications; users who want a recognizable identity on their public profile.

#### User Stories

**Cover letter PDF**

- As a job-seeker, given I have generated a cover letter for a specific application, when I click "Download PDF" in the cover letter modal, then the browser downloads a formatted PDF of the letter so I can attach it to the application directly.

**Resume tweak PDF**

- As a job-seeker, given I am viewing a tweaked version of my resume in the tweak modal, when I click "Download PDF," then the browser downloads a PDF of the tweaked text so I can submit it without reformatting.

**Resume report PDF**

- As a job-seeker, given I am on step 4 (Results) of the Resume AI wizard, when I click "Download report," then the browser downloads a PDF summary of the AI analysis so I can review it offline or share it with a mentor.

**Avatar upload**

- As a job-seeker, given I am on my profile page, when I click "Change picture" and select a file, then my avatar is uploaded to R2, my sidebar and public profile immediately reflect the new photo, and I can remove it to revert to initials.

#### Acceptance Criteria

- `POST /job-tracker/profile/avatar/upload-url` returns a presigned PUT URL + file ID
- `PATCH /job-tracker/profile/avatar/finalize` sets `auth.users.picture_url` to the R2 public URL
- `DELETE /job-tracker/profile/avatar` clears `picture_url` and deletes the R2 object
- Partial unique index on `job_tracker.files WHERE kind='avatar'` prevents duplicate avatar rows
- "Change picture" + "Remove" buttons in `app/profile/page.tsx` around line 353 are wired to the above endpoints
- `POST /job-tracker/ai/cover-letter/pdf` returns `Content-Type: application/pdf`; frontend triggers a browser download
- "Download PDF" button rendered in the cover letter modal after successful generation
- `POST /job-tracker/ai/resume/tweaks/{id}/pdf` returns `Content-Type: application/pdf`; frontend triggers a browser download
- "Download PDF" button rendered in the tweak result panel
- A "Download report" button on Resume AI wizard step 4 calls the PDF endpoint and triggers download
- PDF filenames follow the pattern `{company}-{role}-cover-letter.pdf`, `{title}-tweak.pdf`, `resume-report.pdf`

#### Out of Scope

- PDF template customisation (fonts, colours, logos)
- Multiple avatar sizes / thumbnails (serve the original R2 URL at all sizes)
- Avatar cropping in-browser
- AI-generated avatar

---

### Phase 12: Community Polish

**Problem:** Community surfaces have functional forms but three gaps degrade the experience significantly: filter/sort controls are decorative, author attribution doesn't link to profiles, and the "link to tracked application" feature in experiences is a stub. Backend validation is also missing, allowing malformed metadata through.

**Target users:** Community contributors who want their posts discovered by others; job-seekers who use the experience and referral feeds to research companies.

#### User Stories

**Sort and filter**

- As a community reader, given I am on `/community/experiences`, when I click "Newest," then the list re-orders server-side and the URL updates to `?sort=newest` so I can deep-link it.
- As a community reader, given I am on `/community/ask`, when I click a tag chip in the tag row, then the list filters to posts with that tag.

**Author attribution**

- As a community reader, given I am looking at a post in the experience feed, when I see the author name below the post, then clicking it navigates to that author's public profile at `/u/{authorSlug}` so I can see their background.

**Linked applications in experiences**

- As a job-seeker, given I tracked the interview at Google → SDE-2 in my applications list, when I open "Share Interview Experience," then the "Link to tracked application" select shows my applications so I can auto-fill company and role.

**Round types alignment**

- As a job-seeker sharing an experience, given I had a Group Discussion and Culture Fit round, when I click "Add Round" in the experience modal, then the round type select includes: Phone Screen, Recruiter Call, Technical, Coding, System Design, Behavioral, Onsite, Hiring Manager, HR, Group Discussion, Case Study, Technical (General), Culture Fit, Take Home, Other.

**Target role / experience level alignment (Reviews)**

- As a job-seeker posting for review, given I work in QA, when I select my target role, then "QA" is a distinct option (not collapsed into "Other"), and the experience levels are: Fresher, Junior (0-2 YOE), Mid (2-5 YOE), Senior (5+ YOE), Lead (8+ YOE).

#### Acceptance Criteria

- Filter buttons in `CommunitySectionPage` have `onClick` handlers that update URL query params (`?sort=`, `?tag=`, `?outcome=`, `?role=`, `?level=`)
- `listCommunityPosts()` in `lib/community.ts` passes sort/filter params to the API
- `GET /job-tracker/community/{surface}` accepts and applies `?sort=newest|votes|most-reviewed|least-reviewed`
- Backend adds a sort index migration (migration `0019_community_sort_indexes.sql`)
- Post author names in the list view are wrapped in `<Link href={"/u/" + post.authorSlug}>` when `post.authorSlug` is non-null
- `ExperienceModal` fetches `GET /job-tracker/applications` on open and populates the "link to tracked application" select; selecting one auto-fills `company` and `role` fields
- `ROUND_TYPES` constant in community/[section]/page.tsx is updated to the 15-value spec list
- `TARGET_ROLES` is updated to: SDE, PM, Data Science, Design, DevOps, QA, Other (7 values matching findings.md §4.10)
- `EXPERIENCE_LEVELS` is updated to: Fresher, Junior (0-2), Mid (2-5), Senior (5+), Lead (8+) (5 values)
- Backend `handlers_community.go:CreatePost` validates: experiences `outcome` against `{Offer, Reject, Ghosted, InProgress, Withdrew}`, ask `tags[]` length ≤ 3, reviewers `targetRole` and `experienceLevel` against the spec enums
- Invalid enum values in community post creation return `400` with `{error: "invalid_metadata", field: "outcome"}`

#### Out of Scope

- Threaded (nested) comments — comments remain flat
- Editing a community post after submission
- Moderation / flagging UI (flag endpoint exists but no admin panel)
- Private community posts
- V2 community features (polls, embeds, reactions beyond upvote)

---

### Phase 14: Design System Polish

**Problem:** The CSS design system has token gaps (missing `--bg-hover`, `--font-mono`, stage color tokens) that cause inconsistency between the spec's intended visual and the rendered output. Modal ARIA semantics are missing, creating an accessibility failure. `ModalShell` is duplicated across pages instead of living in `components/ui.tsx`.

**Target users:** All authenticated users. This phase has no new user-facing functionality — it resolves inconsistency and accessibility gaps that exist today.

#### User Stories

**Kanban stage colours**

- As a job-seeker, given I am on the board view, when I look at the "Rejected" column, then the column background uses a red-tinted token (`--stage-rejected`) so I can identify stage health at a glance without reading the label.

**Modal accessibility**

- As a screen reader user, given a modal is open, then focus is trapped inside the modal, the background is marked `inert`, and the modal is announced as a dialog with the heading as its label, so I can navigate it without leaving the context.

**Monospaced metrics**

- As a dashboard user, given I look at the stat tiles, then numeric values render in a monospace font so column widths stay stable as numbers change.

#### Acceptance Criteria

- `app/globals.css` adds tokens: `--bg-hover`, `--accent-soft`, `--font-mono`, `--radius-sm`, `--radius-2xl`, and light-theme mirrors for each
- `app/globals.css` adds 7 stage tokens: `--stage-interested`, `--stage-applied`, `--stage-phone`, `--stage-technical`, `--stage-onsite`, `--stage-offer`, `--stage-rejected` with light-theme mirrors
- Kanban column bodies in `app/applications/page.tsx` apply `background: var(--stage-{stage})` instead of the hardcoded hex values at lines 756–763
- `MetricCard` numeric values in `components/ui.tsx` apply `font-family: var(--font-mono)`
- `ModalShell` is exported from `components/ui.tsx` and the inline version in `app/profile/page.tsx:913` is replaced with the shared import
- All 8 modal sites (Add application, Edit application, View application, Set reminder, Cover letter, Resume tweak, Add recruiter, Delete account) have:
  - `role="dialog"` and `aria-modal="true"` on the backdrop div (already done on several — verify all 8)
  - `aria-labelledby="modal-title"` on the backdrop matching the modal's `<h2 id="modal-title">`
  - A focus trap (either `radix-ui/react-focus-scope` or a `useFocusTrap` hook)
  - `inert` attribute set on `<main>` when modal is open
  - Focus returns to the trigger button on close

#### Out of Scope

- Animated transitions (no animation library to be added)
- Redesigning the page layouts
- Adding Tailwind (explicitly forbidden — CSS only per HANDOFF.md §4)
- Dark/light mode parity audit beyond the token additions above

---

### Phase 15: Mobile Responsiveness

**Problem:** The app targets `@media (min-width: 720px)` instead of the spec's `768px` — a 48px gap that causes layout breaks on mid-range phones. Modal positioning breaks on iOS Safari due to the dynamic address bar. Hover states stick on touch devices. Touch targets are below the 36px minimum.

**Target users:** Job-seekers who use the app on mobile, particularly on `/applications` (quick status checks), `/notifications`, and `/community` surfaces.

#### User Stories

**Mobile form layout**

- As a mobile user, given I open the "Add application" modal on my phone, when the modal renders, then all fields are in a single column and no field is clipped or requires horizontal scrolling.

**iOS modal positioning**

- As an iPhone user, given I open a modal, when Safari's address bar is visible, then the modal is not half-obscured — it positions relative to the visible viewport height (using `8svh` padding-top instead of a fixed pixel offset).

**Touch targets**

- As a mobile user, given I tap the bell icon on an application row, then my tap registers reliably because the touch target is at least 36×36px — the icon may be 14px but it has invisible padding bringing the tap area up to spec.

#### Acceptance Criteria

- `@media (min-width: 720px)` in `app/globals.css` is replaced globally with `@media (min-width: 48rem)` (768px)
- Modal backdrops use `padding-top: 8svh` (viewport-relative, respects iOS Safari dynamic bar) instead of a fixed `pt-[8vh]`
- All `:hover` style rules in `app/globals.css` are wrapped in `@media (hover: hover)` to prevent stuck states on touch
- Form grids collapse to 1 column below 480px (`grid-template-columns: 1fr`) and use 2 columns above
- Sidebar transitions are wrapped in `@media (prefers-reduced-motion: no-preference)`
- Each modal site is verified to set `overflow: hidden` on `<body>` (or equivalent scroll lock) when open
- Icon buttons that are 24px or smaller receive a pseudo-element or invisible padding bringing the touch area to 36px minimum
- Kanban board (`section.board-grid`) scrolls horizontally on mobile with a visible scroll indicator
- The applications table at `<480px` renders as stacked `data-label` cards (the `data-label` attributes are already in the markup — CSS `display: flex` arrangement needed)

#### Out of Scope

- Native app / PWA manifest
- Offline support
- Push notifications (separate from in-app)
- Dedicated mobile layouts (the target is a single responsive layout, not a separate mobile view)

---

### Known Small Items (Phase 7 Verification)

These items were intended to land in Phase 7 but HANDOFF.md §17 flags them as unverified.

#### Audit findings from reading the actual files:

| Item | Finding |
|---|---|
| `applyDeadline` date input in Add/Edit Application modal | **Present** — confirmed at lines 1022–1029 of `app/applications/page.tsx`. Field is `type="date"`, label "Last date to apply", wired to `draft.applyDeadline`. No action needed. |
| Source as `<select>` in applications modal | **Present** — confirmed at lines 998–1013. All 5 enum values + blank "—" option, correct UPPERCASE values. No action needed. |
| Sign-out icon button in sidebar user card (`components/frames.tsx`) | **Present** — confirmed in `SidebarUser` at lines 438–450 of frames.tsx. `<button className="signout-icon sidebar-signout-btn">` with `aria-label="Sign out"`. No action needed. |
| GitHub link in Settings | **Absent** — settings/page.tsx has no GitHub link. Only Chrome Web Store link exists in the Browser extension section. Rebranding from Naukri Clear to Pegasus makes the old GitHub URL (`github.com/Jaan-Mustafa/Naukri-Clear-Extenstion`) stale. No action unless a Pegasus-branded GitHub is set up. |
| Discord link in Settings | **Absent** — settings/page.tsx has no Discord link. The feedback section is a plain textarea + Send. Decision: intentional omission pending community scale. |
| `type="url"` on profile About inputs | **Unverified** — profile/page.tsx was not read in this pass. HANDOFF.md says the bug was "a live `<a href='hasdkjaamc'>`" on the production profile page. This is still an open bug requiring verification. |

---

## Bugs and UX Gaps (Confirmed from Code Reading)

### B1 — `api/payment/status` over-fetch (Critical, backend)
The payment status endpoint is called 10 times per dashboard render because multiple components use independent hooks without de-duplication. The frontend's `lib/billing.ts` exports `fetchBillingMe()` with a 30-second cache — this is used in settings/page.tsx but likely not in all sidebar components. Verify `SidebarBilling` uses `useBillingMe()` (it does per frames.tsx line 374). Investigate whether the old `api/payment/status` calls from the crawled production build pre-date the `lib/billing.ts` cache fix.

**Fix status:** Partially addressed — `useBillingMe()` hook exists with 30s cache. Production build may lag behind current code.

### B2 — `currentPeriodEnd` null for active subscriptions (High, backend)
`GET /billing/me` returns `currentPeriodEnd: null` for active Pro accounts. The Razorpay webhook handler likely doesn't write `current_end` from the `payload.subscription.entity.current_end` event field. The Settings billing UI already handles the null case (the stamp is conditionally rendered at line 640), so there is no crash — but the renewal date is never shown.

**Fix:** In `internal/billing/webhook.go`, write `current_period_end` from the Razorpay event payload on `subscription.activated` and `subscription.charged` events.

### B3 — Pro users can re-create subscriptions (High, backend)
`POST /billing/checkout/subscription` succeeds even when the user is already active. This creates orphaned `created`-status subscriptions in Razorpay.

**Fix:** Add a guard in the checkout handler — if `HasActiveSubscription(userID)` returns true and subscription is not cancelled, return `409 Conflict`.

### B4 — Profile URL fields don't validate format (Medium, frontend)
The live production profile has `<a href="hasdkjaamc">` due to a text input accepting any string. The fix (switch `type="text"` to `type="url"` on LinkedIn, GitHub, and Website inputs in `app/profile/page.tsx`) must be verified.

### B5 — Community recruiter list renders seed data, not API data (Medium, frontend)
`CommunitySectionPage` for the `recruiters` surface renders `seedRecruiters` (a hardcoded array of 10 entries) for the display while the real API returns real entries (77 per crawl). The `posts` state holds real posts from `listCommunityPosts()`, but the `filteredRecruiters` variable used in the render logic is computed from `seedRecruiters`, not `posts`. This is a data source split that causes community recruiters to show stale seed data.

**Fix:** In `CommunitySectionPage`, for `section === "recruiters"`, render from `filteredPosts(posts, search)` rather than `filteredRecruiters`. The post-list renderer already handles any surface uniformly. Remove `seedRecruiters` constant.

### B6 — Community round types misaligned with spec (Medium, frontend)
Current `ROUND_TYPES` in `app/community/[section]/page.tsx` has 10 values. Spec (findings.md §5.10) requires 13+ types including Group Discussion, Case Study, Technical (General), and Culture Fit. This is a Phase 12 item but flagged here as an active bug since the modal is live.

### B7 — Community TARGET_ROLES misaligned with spec (Medium, frontend)
Current `TARGET_ROLES` has 10 values (Frontend Engineer, Backend Engineer, Full-Stack Engineer, Mobile Engineer, Data Engineer, ML Engineer, DevOps / Platform, Product Manager, Designer, Other). Spec requires 7 simplified values (SDE, PM, Data Science, Design, DevOps, QA, Other) matching the filter options in the reviews surface.

### B8 — `OUTCOMES` uses "Rejected" but spec uses "Reject" (Low, frontend)
`OUTCOMES = ["Offer", "Rejected", "In Progress", "Withdrew", "Ghosted"]`. The spec's backend validation (per HANDOFF.md §17) expects `{Offer, Reject, Ghosted, InProgress, Withdrew}`. "Rejected" vs "Reject" and "In Progress" vs "InProgress" will fail server-side validation once backend enforcement lands.

---

## Success Criteria for "Done" — Pegasus v1.0

Pegasus v1.0 is complete when ALL of the following are true:

### Core loop

- A user can sign in with Google, add an application with all fields (including `applyDeadline` and `source`), drag it through the pipeline on the kanban board, set a reminder, and receive an in-app notification when the reminder fires — all without guidance.

### AI loop

- A user can upload a resume, run the 4-step wizard, read the report, and download the report as a PDF. They can also open an application's detail view, generate a cover letter, and download it as a PDF.

### Profile loop

- A user can set up their public profile with avatar, experiences, education, projects, and skills; share the `/u/{slug}` link; and have a visitor see a complete, navigable profile.

### Community loop

- A user can post an experience, referral, or question and see it appear in the live feed. Author names link to public profiles. Sort and filter controls change the list.

### Billing loop

- A free user can upgrade to Standard or Plus, see their credit balance update, use AI features that consume credits, and cancel without reaching a broken state.

### Quality gates

- No modal crashes a screen reader (ARIA requirements met for all 8 modal sites)
- The app renders correctly at 320px, 375px, 480px, 768px, and 1280px viewport widths
- `currentPeriodEnd` is never null for an active subscription
- Pro users cannot create a second subscription without cancelling the first
- Profile URL fields (`linkedinUrl`, `githubUrl`, `websiteUrl`) reject non-URL strings client-side
- Community recruiter list shows real API data, not seed data

---

## Prioritization

| Priority | Items |
|---|---|
| P0 — Must have for v1.0 | Cover letter PDF download; Resume tweak PDF download; Resume report PDF download; Community filter/sort wired to API; Community recruiter list from real API data; Profile URL validation; `currentPeriodEnd` webhook fix; Pro re-subscription guard (409) |
| P1 — Should have for v1.0 | Avatar upload (backend + frontend); Author chip links to profiles; "Link to tracked application" in experience modal; Round types / target roles / outcomes aligned with spec; Backend community metadata validation; Mobile breakpoint fix (720→768); iOS modal positioning fix |
| P2 — Nice to have | Modal focus trap + inert background; Monospace MetricCard values; Stage color tokens in kanban; ModalShell promoted to shared component; Touch target padding; Reduced-motion wrapping |
| P3 — Won't do now | GitHub/Discord links in settings (stale branding; revisit when Discord community is active); Threaded comments; PDF template customisation; Native app / PWA |

---

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `gofpdf` PDF generation requires layout design choices (fonts, margins, header) that are subjective | Medium | Medium | Pick one minimal layout (clean monospace font, company/role header, body text) — don't let perfect block good |
| Avatar R2 costs grow if users upload large avatars without resize | Low | Low | Enforce 5MB server-side limit; document that client-side resize is a future improvement |
| Community seed data removal reveals empty surfaces | Medium | High | Remove seed data only after confirming real API returns data; use existing empty state component |
| Breakpoint change (720→768) could shift layouts on real devices if CSS uses overlapping queries | Low | Medium | Audit `globals.css` for any `max-width: 720px` complements before applying the fix |
| Backend community enum validation will 400 on existing posts if they contain old enum values | Low | High | Add validation as additive (warn + log invalid values) before enforcing as hard errors; run a DB audit first |
