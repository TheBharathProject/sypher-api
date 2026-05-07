# Live Parity Audit — naukriclear.com vs. job-tracker on sypher-api

> **Purpose:** capture the gap between the live reference product
> (naukriclear.com / api.naukriclear.com) and our build, so we can pick
> what to fix, what to leave, and what to skip.
>
> **Method:** authenticated read-only `curl` sweep against
> `api.naukriclear.com/api/*` using a session cookie owned by Shubham.
> About 50 GET requests, no mutations. Responses captured under
> `/tmp/nc-audit/*` during the audit; this document is the digest.
>
> **Date:** 2026-05-07
>
> **Action format:** every numbered item in §6 has a single-line label so
> you can reply with "do 1, 2, 5; skip 3; defer 4, 6, 7."

---

## 1. Headline differences

| Area | Live | Ours |
|---|---|---|
| ID type | numeric BIGSERIAL | UUID |
| Auth | Session cookie (Spring) | JWT (HS256) |
| Pagination | `{items, nextCursor, hasMore, total}` envelope | bare arrays |
| List endpoints | return excerpt/lightweight | return full record |
| Profile sub-resources | only POST/PUT/DELETE; reads via nested `/api/profile` | full GET/POST/PUT/DELETE |
| AI report | rendered to PDF, stored in R2, returned as presigned URL | stored as Markdown in DB |
| AI usage cap | daily count of AI uses (50/day) | monthly token total |
| CSV columns | Title-cased + BOM | lowercase camelCase |

ID-type and pagination shape differences are deep contracts. The rest are
cosmetic and easy to align.

---

## 2. Endpoint-by-endpoint comparison

### 2.1 Auth / Me

| Path | Live | Ours | Status |
|---|---|---|---|
| `GET /api/me` | `{id:311, email, name, pictureUrl, timezone}` | same shape (UUID id) | ✓ parity (modulo id type) |
| `PATCH /api/me/name` | ✓ | ✓ | ✓ |
| `PATCH /api/me/timezone` | ✓ | ✓ | ✓ |
| `POST /api/me/api-token` | ✓ (GET → 405) | ✓ | ✓ |

### 2.2 Dashboard analytics

`GET /api/analytics/dashboard` live response:

```json
{
  "summary": { "total", "inPipeline", "interviews", "offers",
               "addedThisWeek", "responseRate", "conversionRate" },
  "funnel":  [ { "stage", "count" }, ... 7 stages ],
  "weeklyActivity": [ { "weekStart", "count" }, ... 8 weeks ],
  "topSources": [ ... ]   // not seen — likely empty since 5 apps
}
```

Ours returns only `summary` (flat, not under a `summary` key).

### 2.3 Applications

| Field | Live | Ours |
|---|---|---|
| `id` | number | UUID |
| `company`, `role`, `stage`, `notes`, `appliedAt`, `applyDeadline`, `location`, `salaryRange`, `source`, `jobLink`, `stale` | ✓ | ✓ |
| `jobDescription` | ✓ | named `description` |
| `stageChangedAt` | ✓ separate column | ✗ missing |
| `createdAt`, `updatedAt` | ✓ | ✓ |

CSV export: `"Company","Role","Stage","Applied Date","Location","Salary Range","Source","Job Link","Notes","Created At","Updated At"` with UTF-8 BOM. Our export uses `company,role,source,location,salaryRange,stage,appliedAt,applyDeadline,jobLink,description,notes,stale`.

### 2.4 Notes + Categories

**`GET /api/notes`** (list)

```json
[ { "id", "categoryId", "title", "excerpt", "pinned", "updatedAt" } ]
```

**`GET /api/notes/{id}`** (detail)

```json
{ "id", "categoryId", "title", "content", "pinned", "createdAt", "updatedAt" }
```

Field name is `content` (not `body`). List returns excerpt, detail returns full content. Ours returns full body in list.

**`GET /api/notes/categories`**

```json
[ { "id", "name", "color", "position", "noteCount" } ]
```

We're missing `position` (sort order) and `noteCount` (denormalized).

### 2.5 Profile root + sub-resources

**`GET /api/profile`** live shape:

```json
{
  "id", "headline", "about", "location",
  "linkedinUrl", "githubUrl", "websiteUrl",
  "slug", "isPublic",
  "experiences": [ ... ], "educations": [ ... ],
  "projects": [ ... ],   "skills":     [ ... ]
}
```

We're missing **`linkedinUrl`, `githubUrl`, `websiteUrl`**.

**Per sub-resource:**

| Sub-resource | Live fields | Ours | Missing |
|---|---|---|---|
| Experience | `id, company, title, location, startDate, endDate, current, description, sortOrder` | `id, company, title, period, summary, ordinal` | location, startDate, endDate, current, description (named summary) |
| Education | `id, school, degree, field, startDate, endDate, gpa, description, sortOrder` | `id, school, degree, period, ordinal` | field, startDate, endDate, gpa, description |
| Project | `id, name, description, techStack, link, sortOrder` | `id, name, summary, ordinal` | techStack, link, description (named summary) |
| Skill | `id, name, category, sortOrder` | `id, name, category` | sortOrder |

Live uses **proper dates + `current` flag**; we use a free-text `period`
field. Naming convention live: `sortOrder`. Ours: `ordinal`.

**Slug endpoints** live:

| Path | Live | Ours |
|---|---|---|
| `GET /api/profile/slug` | `{slug, isPublic, profileUrl}` (current state) | `{available: bool}` (treats it as a check) |
| `GET /api/profile/slug/check?slug=X` | `{slug, available}` | doesn't exist; we accept `?slug=` on the same path |
| `PATCH /api/profile/slug` | `{slug}` body | ✓ same |
| `PATCH /api/profile/visibility` | `{isPublic}` body | ✓ accepts `{public}` (key name diff) |

### 2.6 Resumes / Cover Letters

Live wraps the list:

```json
{ "resumes": [ {...} ], "count": 1, "limit": 5 }
```

Per-file fields live: `{id, fileName, fileSize, label, uploadedAt}`. Ours
adds `kind, slot, mimeType, createdAt` etc.

Live enforces **5-per-kind cap** at the backend (we don't).

### 2.7 AI

**`GET /api/ai/usage`** live:

```json
{ "used": 0, "isPro": true, "limit": 50 }
```

Ours: `{ used, limit, periodStart, periodEnd }`. Different metric — live is **daily action count**, ours is **monthly token total**.

**`GET /api/ai/resume/report/latest`** live:

```json
{ "downloadUrl": "https://...r2.cloudflarestorage.com/resume-reports/311/latest.pdf?X-Amz-..." }
```

Live renders to PDF + stores in R2 + returns presigned URL. Ours stores
markdown in DB and returns `{id, reportMd, score, createdAt}`.

### 2.8 Payments

Live:

```json
GET /api/payment/status → { "isPro": true, "plan": "pro",
                             "currentPeriodEnd": null, "isCancelled": false }
```

Plus (not probed, mutating): `POST /api/payment/create-subscription`,
`/verify`, `/cancel-subscription`.

We have nothing.

### 2.9 Community

Live has a full content network. All endpoints use
`{items, nextCursor, hasMore, total}` paging.

| Path | Live shape | Ours |
|---|---|---|
| `GET /api/community/recruiters` | rich list (slug, voteCount, reviewCount, avgRating, contributionCount, trustLevel, voted) | ✗ |
| `GET /api/community/recruiters/admin/reported` | 403 admin-gated | ✗ |
| `GET /api/community/experiences` | list with embedded `author` (userId, name, slug, headline, pictureUrl) | ✗ |
| `GET /api/community/experiences/{id}` | full detail with `rounds` array (roundType, questions, tips, sortOrder) + tips, salary | ✗ |
| `GET /api/community/reviews` | empty | ✗ |
| `GET /api/community/referrals` | empty | ✗ |
| `GET /api/community/ask` | empty | ✗ |
| `GET /api/community/ask/tags` | 12 seed tags `{name, usageCount}` | ✗ |

### 2.10 Notifications (we missed this entirely)

```json
GET /api/notifications → { "notifications": [], "unreadCount": 0 }
```

Was not in the original plan. Modest scope.

### 2.11 Templates

`GET /api/templates` returns `[]` on live too (looks WIP). Skip.

---

## 3. Public profile — full flow

### 3.1 How it works on live

User journey in **Settings → Profile**:

1. Pick a slug. Backend validates with
   `GET /api/profile/slug/check?slug=...`. On submit, `PATCH /api/profile/slug`.
2. Fill in profile data (root + 4 sub-resources).
3. Toggle "Public": `PATCH /api/profile/visibility`.
4. While `isPublic=false`, **public route returns 404** (confirmed for
   `shubham-dixit`).
5. While `isPublic=true`, **two endpoints unlock**:
   - `GET /api/public/profile/{slug}` → rich profile payload
   - `GET /api/public/analytics/{slug}` → dashboard summary + funnel + weekly

### 3.2 Public profile payload

```jsonc
{
  "name": "...",            // from auth.users
  "pictureUrl": "...",      // from auth.users
  "headline", "about", "location",
  "linkedinUrl", "githubUrl", "websiteUrl",   // socials we don't have
  "slug",
  "experiences": [
    { "id", "company", "title", "location",
      "startDate", "endDate", "current",
      "description", "sortOrder" }
  ],
  "educations":  [ { "id", "school", "degree", "field",
                     "startDate", "endDate", "gpa",
                     "description", "sortOrder" } ],
  "projects":    [ { "id", "name", "description", "techStack",
                     "link", "sortOrder" } ],
  "skills":      [ { "id", "name", "category", "sortOrder" } ]
}
```

### 3.3 Public analytics payload

```jsonc
{
  "summary": { ... 7 fields ... },
  "funnel":  [ { "stage", "count" }, ... ],
  "weeklyActivity": [ { "weekStart", "count" }, ... ]
}
```

This is the **differentiating feature** of the public profile. Visitors
see "Md Rizabul has interviewed at 7 places, 1 offer, response rate 69%"
— turning a static resume page into evidence of an active job-seeker.

### 3.4 What we'd need to reach parity on public profile

**Backend (migration `0006_profile_richer.sql`):**

1. `profiles`: add `linkedin_url`, `github_url`, `website_url`
2. `profile_experiences`: drop `period`, add `location`, `start_date DATE`,
   `end_date DATE`, `current BOOLEAN`. Rename `summary` → `description`.
   Rename `ordinal` → `sort_order`.
3. `profile_educations`: drop `period`, add `field`, `start_date`,
   `end_date`, `gpa`, `description`. Rename ordinal.
4. `profile_projects`: add `tech_stack`, `link`. Rename `summary` →
   `description`. Rename ordinal.
5. `profile_skills`: rename ordinal → `sort_order`.

**Backend new endpoint:**

- `GET /job-tracker/public/analytics/{slug}` — gated on `is_public=true`,
  returns `{summary, funnel, weeklyActivity}` for that user's apps.

**Backend route split:**

- `GET /job-tracker/profile/slug` → `{slug, isPublic, profileUrl}`
- `GET /job-tracker/profile/slug/check?slug=X` → `{slug, available}`

**Frontend (`job-tracker/`):**

- Settings → Profile section: add `linkedinUrl/githubUrl/websiteUrl` inputs.
  *This is a UI addition — needs sign-off.*
- Profile page: replace free-text "period" with date pickers + "I currently
  work here" checkbox. *UI addition — needs sign-off.*
- `/u/[slug]`: render the analytics block alongside the profile. *UI
  addition — needs sign-off.*

The UI side is the only place where the no-UI-changes rule clashes with
parity. List those changes, present options, let user decide.

---

## 4. CSV export/import deltas

| Aspect | Live | Ours |
|---|---|---|
| Encoding | UTF-8 with BOM | UTF-8 no BOM |
| Header style | Title Case ("Company", "Role", "Stage", "Applied Date", ...) | lowercase camelCase |
| Columns (in order) | Company, Role, Stage, Applied Date, Location, Salary Range, Source, Job Link, Notes, Created At, Updated At | company, role, source, location, salaryRange, stage, appliedAt, applyDeadline, jobLink, description, notes, stale |
| Stage values | "Interested", "Applied", etc. (Title Case) | enum values "INTERESTED", "APPLIED", ... |

If you import a live export into our backend, it will parse 0 rows (column
names don't match). Likewise live can't read ours.

---

## 5. Notes & data shape deltas (denormalised view)

Categorised list of every concrete delta found, regardless of feature
area. Use this as a checklist when deciding tweaks.

### 5.1 Schema-level

- **Apps**: rename `description` → `job_description`. Add `stage_changed_at`.
- **Notes**: rename `body` → `content`.
- **Note categories**: add `position INTEGER NOT NULL DEFAULT 0`. Compute or denormalise `noteCount`.
- **Profile**: add `linkedin_url`, `github_url`, `website_url`.
- **Profile experiences**: replace `period TEXT` with `start_date DATE`, `end_date DATE`, `current BOOLEAN`. Add `location TEXT`. Rename `summary` → `description`. Rename `ordinal` → `sort_order`.
- **Profile educations**: replace `period TEXT` with `start_date`, `end_date`. Add `field`, `gpa`, `description`. Rename ordinal.
- **Profile projects**: add `tech_stack`, `link`. Rename summary → description, ordinal → sort_order.
- **Profile skills**: rename ordinal → sort_order.
- **Files**: enforce 5-per-kind limit at insert (live does).

### 5.2 Wire-shape level

- Wrap resumes/cover-letters list in `{resumes: [...], count, limit:5}`.
- Slug endpoint split: `/profile/slug` (current) vs `/profile/slug/check`.
- Visibility body key: live `{isPublic}`, ours `{public}`. Pick one.
- Notes list returns `excerpt` (truncated body), detail returns full `content`.
- Dashboard returns `{summary, funnel, weeklyActivity, topSources}` not flat summary.
- AI usage shape: live `{used, isPro, limit:50}` (daily uses), ours `{used, limit, periodStart, periodEnd}` (monthly tokens). Pick one metric.
- AI report latest: live returns `{downloadUrl}` to PDF; ours returns `{reportMd, score}`. Pick one or expose both.
- Pagination envelope `{items, nextCursor, hasMore, total}` for any list expected to grow (community). Currently we use bare arrays.

### 5.3 New surfaces (not in any plan)

- `GET /api/notifications` + `unreadCount`. Mark-as-read endpoint likely.
- `GET /api/public/analytics/{slug}`.
- `isPro` flag on user / payment status.

---

## 6. Tweak inventory (decision-ready)

Each tweak is numbered. Reply with the IDs you want done / skipped / deferred.

### Tier 1 — quick contract fixes (no UI change, ≤2h total)

| # | Tweak | Touches |
|---|---|---|
| **T1.1** | Rename `notes.body` column → `notes.content`. Update store + handler + frontend type. | Migration + 3 files |
| **T1.2** | Notes list returns `excerpt` (first ~200 chars), keep detail returning full content. Add `GET /job-tracker/notes/{id}`. | store_files unaffected; jobtracker store + handlers |
| **T1.3** | Add `position INTEGER` to `note_categories`; include `noteCount` in list response. | Migration + store |
| **T1.4** | Split slug endpoint: `GET /profile/slug` → current state with `profileUrl`; `GET /profile/slug/check?slug=X` → availability. | routes + handlers_profile |
| **T1.5** | Visibility body key: change ours from `{public}` → `{isPublic}` to match live. | handlers_profile + frontend |
| **T1.6** | Resumes/Cover-letters list envelope: `{resumes: [], count, limit:5}` + enforce 5-per-kind cap with 409. | handlers_files + store_files |
| **T1.7** | CSV export: switch to Title Case columns + UTF-8 BOM matching live exactly. | handlers_applications |
| **T1.8** | CSV import: accept Title Case columns (or both — auto-detect). | handlers_applications |

### Tier 2 — schema additions (≤half day, no UI change required, but sets up Tier 3)

| # | Tweak | Touches |
|---|---|---|
| **T2.1** | Apps: rename `description` → `job_description`, add `stage_changed_at TIMESTAMPTZ` updated on stage change, derive `stale` from it. | Migration + store + handler |
| **T2.2** | Profile root: add `linkedin_url`, `github_url`, `website_url` columns + accept in PUT body. (UI doesn't surface yet — backend only.) | Migration + store + types |
| **T2.3** | Profile experiences: replace `period` with `start_date`, `end_date`, `current` + add `location`. Rename `summary` → `description`, `ordinal` → `sort_order`. | Migration + store + handler |
| **T2.4** | Profile educations: replace `period` with `start_date`, `end_date` + add `field`, `gpa`, `description`. Rename ordinal. | Migration + store + handler |
| **T2.5** | Profile projects: add `tech_stack`, `link`. Rename summary, ordinal. | Migration + store + handler |
| **T2.6** | Profile skills: rename `ordinal` → `sort_order`. | Migration + store |
| **T2.7** | Dashboard: add `funnel` and `weeklyActivity` to the response. Frontend `dashboard/page.tsx` can drop the client-side aggregation. | handlers_dashboard + store |

### Tier 3 — feature additions (mix of backend + UI sign-off needed)

| # | Tweak | Backend | UI |
|---|---|---|---|
| **T3.1** | `GET /job-tracker/public/analytics/{slug}` — analytics for public profile. | New handler + store query | Render the block on `/u/[slug]` (UI addition) |
| **T3.2** | Notifications: table + `GET /job-tracker/notifications` + `POST /job-tracker/notifications/{id}/read`. | Migration + new package | UI addition (badge + dropdown) |
| **T3.3** | AI report PDF: render markdown → PDF, upload to R2, return `{downloadUrl}` alongside markdown. | Library choice + R2 write | None (existing UI keeps working) |
| **T3.4** | Daily AI cap (50 uses/day) like live. Replace token-monthly metric or run both. | usage.go change + config | None |
| **T3.5** | Settings: surface social URL inputs (`linkedinUrl`, etc.) under Profile. | None (T2.2 covers backend) | UI addition (3 inputs in Profile section) |
| **T3.6** | Profile page: date pickers + "Currently here" checkbox for experiences/educations. | None (T2.3/T2.4 cover backend) | UI replacement (free-text → dates) |

### Tier 4 — explicitly deferred (no urgency)

| # | Feature | Notes |
|---|---|---|
| **T4.1** | Community: recruiters | Largest community surface |
| **T4.2** | Community: experiences (with `rounds` sub-table) | Rich nested model |
| **T4.3** | Community: reviews | Empty on live too |
| **T4.4** | Community: referrals | Empty on live too |
| **T4.5** | Community: ask + tags | Empty on live; tags seeded |
| **T4.6** | Community: comments + votes + reports | Cross-cutting interaction layer |
| **T4.7** | Payments / subscriptions | `isPro` flag + Stripe |
| **T4.8** | Templates | Empty on live too |

---

## 7. Inconsistencies in our current build (already known)

Carried forward from `TOOL_PLAYBOOK.md` §9 for completeness:

- **`/resume` page promises "we never store the file" but it does.** Files
  upload to R2 + create a row with `slot=NULL`.
- **AI-source resumes don't appear in the vault.** They have `slot=NULL`;
  the vault only shows the 5 numbered slots.

Decisions on these are still open.

---

## 8. Suggested decision pass

Reply with three lists:

```
DO:    T1.1, T1.4, T1.7, T2.7, ...
SKIP:  T1.5, T3.4, ...     (won't do, accept the divergence)
DEFER: T3.2, T4.*, ...     (later milestone)
```

Anything not mentioned, I'll treat as "ask before touching."

---

## 9. Cross-references

- **`docs/GUIDE.md`** — architecture + conventions
- **`docs/TOOL_PLAYBOOK.md`** — what we built + how to repeat for the next tool
- **`docs/feature-inventory.md`** (in `job-tracker/`) — original inventory done from the live site at the start of the project
- **`/tmp/nc-audit/*.json|*.csv`** — raw responses captured during this audit. Will be cleared on next reboot.

---

*Last updated: 2026-05-07 — initial parity audit against live naukriclear.com.*
