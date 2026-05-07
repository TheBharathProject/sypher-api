# Tool Playbook: Wiring a Frontend to sypher-api

> This document captures everything we did to make `job-tracker` a real product
> backed by `sypher-api`. It serves two purposes:
>
> 1. **Historical log** — what was built, in what order, why those decisions.
> 2. **Reusable recipe** — the steps to repeat for the next tool (`reel-hooks`,
>    or any future addition).
>
> Read top-to-bottom the first time. Treat sections 4–7 as reference material
> when onboarding the next tool.

---

## 1. Context: what is sypher-api?

`sypher-api` is a **Go modular monolith** living at `api.sypher.in`. Each tool
gets its own package under `internal/<tool>/` and its own SQL schema. One
binary, multiple isolated tools, shared cross-cutting infrastructure (auth,
storage, AI).

Before this work, the only tool was `waitlist` (one endpoint).

After this work, the architecture is:

```
api.sypher.in (Go monolith)
├── internal/auth/         ← shared: Google OAuth + JWT + RequireUser MW
├── internal/storage/      ← shared: Cloudflare R2 (presigned PUT/GET)
├── internal/ai/           ← shared: Deepseek client + per-user usage metering
├── internal/jobtracker/   ← tool: 60+ endpoints under /job-tracker/*
└── internal/waitlist/     ← original tool (unchanged)
```

The same shape will serve any future tool: drop in `internal/<newtool>/`,
write a migration, register routes.

---

## 2. What we built (job-tracker, summary)

| Layer | Outcome |
|---|---|
| **Auth** | Google OAuth login → backend issues HS256 JWT → frontend stores it and sends `Authorization: Bearer <jwt>` |
| **Database** | 5 migrations: waitlist (existing), `auth.users` + `auth.api_tokens`, `job_tracker.*` (apps, notes, profile, sub-resources, feedback), files + AI tables, slot column for vault |
| **Endpoints** | ~60 routes under `/auth/*` and `/job-tracker/*`. Full CRUD for applications, notes, profile sub-resources, file upload via R2 presigned URLs, AI report + cover-letter via Deepseek, per-user usage metering |
| **Frontend wiring** | Replaced every `localStorage`/mock data source on the affected pages with API calls. Added `lib/api-client.ts`, `lib/auth.ts`, `app/auth/callback/page.tsx`. UI/layouts preserved. |
| **Infrastructure** | Postgres locally via Docker, Google OAuth credentials, Cloudflare R2 bucket + token + CORS, Deepseek API key |

What was **deferred** (not built):
- Community endpoints (reviews, experiences, referrals, ask, recruiters)
- Payments / subscriptions
- Templates library
- Public analytics

---

## 3. Reusable recipe (steps in order, for the next tool)

When you do this again for `reel-hooks` or any new tool, follow this order.
The **bold** steps need product/scope decisions before you start.

1. **Decide auth model.** If the tool needs per-user data → reuse
   `internal/auth/RequireUser`. If endpoint-level only (like `waitlist`) →
   use `requireAPIKey`.
2. **Decide scope.** List the endpoints. Map each to UI screens. Defer
   non-essentials in writing.
3. **Decide infra.** New tool may or may not need: file storage (R2), AI
   (Deepseek), background jobs, webhooks. Each one has its own setup chunk.
4. Write the migration: `migrations/000N_<tool>.sql` — schema +
   user-id-keyed tables.
5. Create the tool package: `internal/<tool>/` with `types.go`, `store.go`,
   one or more `handlers_*.go`, `routes.go`. Follow the `internal/jobtracker`
   layout.
6. Wire it in `internal/server/server.go`.
7. Update `.env.example` with any new env vars.
8. `go vet ./...` and `go build ./...` until clean.
9. Add smoke tests for any new shared package (`internal/<shared>/*_test.go`).
10. Build/wire the frontend: `lib/api-client.ts` already exists — extend it
    with new types. Replace mock data sources with API calls. **Do not
    redesign the UI.**
11. Run the DB migration locally, configure env vars, run end-to-end.
12. Document any new infra setup at the bottom of this file (R2 / Deepseek
    sections below are good templates).

---

## 4. Backend journey (what we did, in order)

### 4.1 Phase 1 — Auth + tool package

#### Step 1: New migrations

`migrations/0002_auth.sql`:
- `auth.users` (id UUID, google_id UNIQUE, email, name, picture_url, timezone)
- `auth.api_tokens` (token_hash, prefix, label, last_used_at) — for browser
  extension use case

`migrations/0003_job_tracker.sql`:
- `job_tracker.applications` — full schema with `stage` enum-as-text
- `job_tracker.notes` + `job_tracker.note_categories`
- `job_tracker.profiles` (1:1 with auth.users) + `profile_experiences`,
  `_educations`, `_projects`, `_skills`
- `job_tracker.feedback`

Every table has `user_id UUID REFERENCES auth.users(id) ON DELETE CASCADE`
plus a composite index `(user_id, ...)` for tenant isolation.

All statements idempotent (`IF NOT EXISTS`). The migration runner is
`cmd/migrate` and it just walks `migrations/*.sql` in lex order.

#### Step 2: New shared `internal/auth/` package

Six files following the convention used by `internal/waitlist`:

- `types.go` — `User`, `GoogleProfile`, `APIToken`
- `jwt.go` — `IssueJWT(secret, iss, aud, uid, email, ttl)` and
  `VerifyJWT(...)`. HS256. Uses `github.com/golang-jwt/jwt/v5`.
- `google.go` — `BuildGoogleAuthURL`, `RandomState`, `ExchangeCode`. Two
  HTTP round-trips (token + userinfo). Stdlib only.
- `store.go` — `UpsertUser`, `GetUserByID`, `UpdateUserName`,
  `UpdateUserTimezone`, `IssueAPIToken`, `LookupAPIToken`. Token hash =
  SHA-256(plain).
- `middleware.go` — `RequireUser(secret, iss, aud)` returns
  `func(http.Handler) http.Handler`. Reads `Authorization: Bearer ...`,
  verifies JWT, puts `userID` + `email` into request context. Companion
  `MustUserID(ctx)` helper used by tool handlers.
- `handler.go` — `GoogleStart`, `GoogleCallback`, `Logout`. Callback
  exchanges code, upserts user, issues JWT, redirects to
  `FRONTEND_LOGIN_REDIRECT_URL?token=<jwt>`. Uses a short-lived state
  cookie to prevent CSRF on the OAuth flow.

#### Step 3: New tool package `internal/jobtracker/`

Multiple files because there are ~30 endpoints:

```
internal/jobtracker/
├── types.go             ← request/response shapes
├── store.go             ← all SQL except files (apps, notes, profile, etc.)
├── store_files.go       ← file metadata + AI report storage (added Phase 2)
├── handler.go           ← Handler struct + readJSON / pathUUID / writeDBError helpers
├── handlers_me.go       ← /me, /me/name, /me/timezone, /me/api-token
├── handlers_dashboard.go← /analytics/dashboard
├── handlers_applications.go ← apps CRUD + CSV import/export/preview/template
├── handlers_notes.go    ← notes + categories
├── handlers_profile.go  ← profile + sub-resources + slug + visibility
├── handlers_public.go   ← /public/profile/{slug} (no auth)
├── handlers_feedback.go ← POST /feedback
├── handlers_files.go    ← Phase 2: resumes + cover-letters
├── handlers_ai.go       ← Phase 2: extract + report + cover-letter + usage
└── routes.go            ← RegisterRoutes(mux, h, requireUser)
```

Patterns established:
- All authed routes use `auth.MustUserID(r.Context())` to scope DB queries
- Handlers return JSON via `httpx.WriteJSON` / `httpx.WriteError`
- DB errors mapped via `writeDBError` (404 on `pgx.ErrNoRows`, 500 otherwise)
- Path params via `r.PathValue("id")` (Go 1.22 std mux), validated with
  `uuid.Parse`
- CSV export streams via `encoding/csv`; import accepts both `multipart/form-data`
  (file field) and raw body

#### Step 4: Wire it in `internal/server/server.go`

Inside `routes()`:

```go
authStore := auth.NewStore(s.pool)
authHandler := auth.NewHandler(s.cfg, authStore, s.logger)
mux.HandleFunc("GET /auth/google", authHandler.GoogleStart)
mux.HandleFunc("GET /auth/google/callback", authHandler.GoogleCallback)
mux.HandleFunc("POST /auth/logout", authHandler.Logout)

requireUser := auth.RequireUser(s.cfg.JWTSecret, s.cfg.JWTIssuer, s.cfg.JWTAudience)
jtStore := jobtracker.NewStore(s.pool)
jtHandler := jobtracker.NewHandler(s.cfg, jtStore, authStore, s.logger)
jobtracker.RegisterRoutes(mux, jtHandler, requireUser)
```

Plus extended `internal/config/config.go` with auth + (later) R2 + Deepseek
env vars, and added `Authorization` to the CORS allow-list in
`internal/server/middleware.go`.

#### Step 5: `go.mod` additions

Phase 1 dependencies:
- `github.com/golang-jwt/jwt/v5`
- `github.com/google/uuid`

### 4.2 Phase 2 — Files (R2) + AI (Deepseek)

#### Step 1: New migration

`migrations/0004_job_tracker_files_ai.sql`:
- `job_tracker.files` (kind, label, file_name, file_size, mime_type,
  storage_key, uploaded_at)
- `job_tracker.ai_reports` (resume_file_id FK, report_md, score)
- `job_tracker.ai_usage` (endpoint, tokens_in, tokens_out, cost_usd) for
  per-user metering

#### Step 2: New shared package `internal/storage/` (R2)

`r2.go` — wraps `aws-sdk-go-v2/service/s3` configured for R2:
- `New(ctx, Settings)` — fail-soft constructor; returns error if creds
  missing (so Phase-1-only deploys boot fine)
- `PresignPut(ctx, key, contentType, ttl)` — browser-direct upload URL
- `PresignGet(ctx, key, ttl)` — download URL (currently unused but ready)
- `FetchAll(ctx, key)` — server-side download (used by AI extract)
- `Delete(ctx, key)` — cleanup on row delete

Key convention: `job-tracker/{user_id}/{kind}/{uuid}-{filename}`.

#### Step 3: New shared package `internal/ai/` (Deepseek)

Two files:

- `deepseek.go` — minimal HTTP client to `/chat/completions` (Deepseek is
  OpenAI-compatible, no SDK needed). Two methods: `ResumeReport` and
  `CoverLetter`. Each returns `(text, tokensIn, tokensOut, err)`.
- `usage.go` — `UsageStore` with `MonthlyTotal`, `EnforceLimit`,
  `Record`. `ErrUsageExceeded` sentinel returned to hit the 429 path.

#### Step 4: Phase 2 handlers

Added to `internal/jobtracker/`:

- `handlers_files.go` — resume + cover-letter upload-URL / finalize / list /
  delete. Includes the slot replace-in-place logic.
- `handlers_ai.go` — extract (PDF parsing via `github.com/ledongthuc/pdf`),
  resume report, cover letter, usage. Each AI handler calls
  `aiUsage.EnforceLimit` first.

#### Step 5: Lazy init in server.go

```go
if r2, err := storage.New(ctx, storage.Settings{...}); err == nil {
    jtHandler.WithStorage(r2)
} else {
    s.logger.Info("r2 storage skipped", "reason", err.Error())
}
if aiClient, err := ai.New(ai.Settings{...}); err == nil {
    jtHandler.WithAI(aiClient, ai.NewUsageStore(s.pool, s.cfg.AIUsageMonthlyTokenLimit))
} else {
    s.logger.Info("deepseek skipped", "reason", err.Error())
}
```

**Why lazy:** Phase 1 endpoints work without R2 or Deepseek. The AI/file
endpoints respond `503 ai_unavailable` / `503 storage_unavailable` until
configured. The binary boots regardless.

#### Step 6: `go.mod` Phase 2 additions

- `github.com/aws/aws-sdk-go-v2/{config,credentials,service/s3}`
- `github.com/ledongthuc/pdf`

#### Step 7: Slot column (added late to match original UI)

`migrations/0005_files_slots.sql` — adds `slot INTEGER` to
`job_tracker.files` with a partial unique index `(user_id, kind, slot)
WHERE slot IS NOT NULL`. Backend `CreateFile` accepts an optional slot;
when set, the existing slot occupant is deleted (DB row + R2 object) before
the new file is recorded — atomic replace-in-slot.

---

## 5. Frontend journey (`job-tracker`)

### 5.1 New foundation files

- `lib/api-client.ts` — single fetch wrapper. Reads
  `NEXT_PUBLIC_API_BASE_URL`, attaches `Authorization: Bearer ${token}`,
  normalises errors via `ApiError`. Exports typed result interfaces
  (`ApiUser`, `ApiApplication`, `ApiNote`, `ApiProfile`, `ApiPublicProfile`,
  etc.) so each page is type-safe.
- `lib/auth.ts` — `getToken()`, `setToken()`, `clearToken()`, `isAuthed()`,
  `loginUrl()`. Storage is `localStorage` keyed `sypher_jwt`.
- `app/auth/callback/page.tsx` — receives `?token=...` from the OAuth
  redirect, stores it, bounces to `/dashboard`. Wrapped in `<Suspense>`
  because Next 14 requires that for `useSearchParams`.
- `.env.local` and `.env.production` — `NEXT_PUBLIC_API_BASE_URL` only.

### 5.2 Pages wired (list)

| Page | What changed | Notes |
|---|---|---|
| `app/login/page.tsx` | Continue-with-Google button now points at `${api}/auth/google` | Layout untouched |
| `app/dashboard/page.tsx` | Fetches `/me` + `/analytics/dashboard` | Replaced `dashboardSummary` import |
| `app/applications/page.tsx` | Full CRUD + CSV import/export via API | `id` type changed `number` → `string` (UUID). Layout untouched |
| `app/notes/page.tsx` | API CRUD + categories. Markdown toolbar + undo/redo + view modes preserved. Debounced 500ms autosave | Reconstructed editor (full original wasn't recoverable from disk; rebuilt from icon imports + first 150 lines) |
| `app/profile/page.tsx` | Profile root + 4 sub-resources via API | Layout exactly as original |
| `app/settings/page.tsx` | `/me`, `/profile`, `/me/api-token`, `/feedback`, `/profile/visibility`, `/profile/slug`, `/ai/usage` for the Pro plan line | Added Sign Out button (only addition) |
| `app/u/[slug]/page.tsx` | Public profile via API, no auth | Converted from SSG to client component; no other changes |
| `app/resumes/page.tsx` | 5-slot UI with R2 presigned upload flow per slot | Original layout preserved with slot column persistence |
| `app/resume/page.tsx` | 4-step stepper preserved; AI report generation on submit; results render inline | Layout preserved |

### 5.3 The R2 upload flow (browser-direct, three-step)

```
1. POST /job-tracker/resumes/upload-url   → { uploadUrl, file: {id, ...} }
2. PUT  <uploadUrl>                       → 200 (browser → R2 directly)
3. PATCH /job-tracker/resumes/{id}/finalize → marks uploaded_at = NOW()
```

The presigned URL is generated by sypher-api but the bytes never stream
through it. R2 charges no egress, no per-byte fees on uploads.

### 5.4 What was *not* changed (and why)

UI/layouts on all pages were preserved per a hard rule. Originally I
shortened the notes page (dropped toolbar) and rearranged Vault / Resume /
Settings / Profile. After pushback, all originals were restored. The rule
**"don't change UI when wiring backends"** is now saved as a memory and
applies to all future tools.

---

## 6. Infrastructure setup (one-time per environment)

These are the steps to set up a fresh environment (your laptop, a teammate's,
a new staging instance). Detailed walkthroughs live in conversation history;
the bullets below are the index.

### 6.1 Postgres (local)

```bash
docker run -d --name sypher-pg \
  -e POSTGRES_USER=sypher \
  -e POSTGRES_PASSWORD=local-dev-pass \
  -e POSTGRES_DB=sypher \
  -p 5432:5432 \
  postgres:17-alpine

# Apply all migrations
cd sypher-api
set -a; source .env; set +a
MIGRATIONS_DIR=./migrations go run ./cmd/migrate
```

Migration runner is idempotent — safe to re-run after adding new files.

### 6.2 Google OAuth

1. Cloud Console → New project → OAuth consent screen → External →
   add yourself as a test user.
2. Credentials → Create OAuth client ID → Web application.
3. Authorised redirect URI: **exactly**
   `http://localhost:8000/auth/google/callback` (no trailing slash, http).
4. Copy Client ID + Secret into `.env`:
   - `GOOGLE_OAUTH_CLIENT_ID`
   - `GOOGLE_OAUTH_CLIENT_SECRET`
   - `GOOGLE_OAUTH_REDIRECT_URL=http://localhost:8000/auth/google/callback`
   - `JWT_SECRET=$(openssl rand -hex 32)`
   - `FRONTEND_LOGIN_REDIRECT_URL=http://localhost:3000/auth/callback`

### 6.3 Cloudflare R2 (optional — Phase 2 only)

1. Dashboard → R2 → Enable R2 → Create bucket `sypher-job-tracker`.
2. Manage R2 API Tokens → Create token, Object Read & Write, scoped to the
   bucket. Copy access key + secret.
3. Account ID is in the right rail of any Cloudflare page.
4. Bucket Settings → CORS Policy:
   ```json
   [{
     "AllowedOrigins": ["http://localhost:3000"],
     "AllowedMethods": ["GET", "PUT", "POST", "DELETE", "HEAD"],
     "AllowedHeaders": ["*"],
     "ExposeHeaders": ["ETag", "Content-Length"],
     "MaxAgeSeconds": 3600
   }]
   ```
   Without this the browser PUT will fail with a CORS error.
5. `.env`: `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`,
   `R2_BUCKET=sypher-job-tracker`.

### 6.4 Deepseek (optional — Phase 2 only)

1. platform.deepseek.com → sign up.
2. Top up $2 (very cheap; lasts a long time).
3. API Keys → Create new → copy `sk-...`.
4. `.env`: `DEEPSEEK_API_KEY=sk-...`. Defaults for `DEEPSEEK_BASE_URL` and
   `DEEPSEEK_MODEL` are correct.

### 6.5 Run

Backend (terminal 1):
```bash
cd sypher-api
set -a; source .env; set +a
go run ./cmd/api
```

Healthy startup logs include:
```
db pool ready
r2 storage configured            ← only if R2 envs set
deepseek configured              ← only if DEEPSEEK_API_KEY set
http server starting addr=:8000
```

Frontend (terminal 2):
```bash
cd job-tracker
npm install   # first time
npm run dev
```

Open http://localhost:3000 → Login → Continue with Google → land on dashboard.

---

## 7. Verification checklist

Run these after any new tool install / fresh environment:

```bash
# 1. Backend health
curl http://localhost:8000/health
# {"ok":true}

# 2. Migrations applied
psql "$DATABASE_URL" -c "\dn"           # schemas: auth, job_tracker, waitlist
psql "$DATABASE_URL" -c "\dt job_tracker.*"  # tables incl. files (Phase 2)
psql "$DATABASE_URL" -c "\d job_tracker.files" | grep slot   # slot column

# 3. Auth round-trip — sign in via UI, check token exists
# Browser devtools → Application → Local Storage → localhost:3000 → sypher_jwt

# 4. Each domain works
#   - Apps: create one in /applications, see it in psql
#   - Notes: create one, type, reload — body persists
#   - Profile: add a skill, set a slug
#   - Public profile: toggle Hide off in settings, open /u/<slug> in incognito

# 5. Phase 2 (if configured)
#   - Vault: upload to slot 1 (Network tab: upload-url → PUT to R2 → finalize)
#   - AI: paste resume text on /resume → Continue → score + report renders
#   - Settings: "Pro plan ... AI tokens this month" shows real numbers

# 6. Tenant isolation
#   - Sign in as user A, create data
#   - Sign out, sign in as user B, confirm A's data is invisible

# 7. Tests
cd sypher-api
go vet ./...
go build ./...
go test -race ./...
```

---

## 8. Common gotchas (lessons we learned the hard way)

| Symptom | Root cause | Fix |
|---|---|---|
| `useSearchParams() should be wrapped in a suspense boundary` on `next build` | Next 14 requires Suspense around any `useSearchParams` reader | Wrap the page body in `<Suspense fallback={...}>` |
| `column "slot" does not exist (SQLSTATE 42703)` after deploying | Migration `0005` not applied | Re-run migrate; runner is idempotent |
| Browser PUT to R2 fails with CORS error | Missing or misconfigured CORS rule on bucket | Apply the policy in 6.3.4. Wait 30–60 sec for edge propagation |
| `S3 PutObject 403 SignatureDoesNotMatch` | Account ID, access key, or secret typo | Re-copy from Cloudflare; confirm `R2_ACCOUNT_ID` matches the endpoint subdomain |
| AI report returns `503 ai_unavailable` | `DEEPSEEK_API_KEY` unset | Set in `.env`, restart server |
| AI report returns `429 usage_exceeded` | Hit the per-user monthly cap | Raise `AI_USAGE_MONTHLY_TOKEN_LIMIT` in `.env` |
| `unauthorized` on every API call | JWT not stored or missing on request | Check `localStorage[sypher_jwt]` in devtools; clear & re-login |
| Multiple slots showing the same file | Frontend optimistic state out of sync with DB | Rely on the `refresh()` after each write; don't mutate local state without re-fetch |
| Migrate runner says `applied=4` but not `5` | Stale `MIGRATIONS_DIR` or running from wrong directory | Run from `sypher-api/` with `MIGRATIONS_DIR=./migrations` |
| Old `go run ./cmd/api` process serving stale code after a column rename | After migration `0006` renamed `body→content`, `description→job_description`, etc., a long-running build still references old names | **Restart the API after every migration that renames or drops columns.** Idempotent renames are safe to apply repeatedly. |
| Renaming a column in an idempotent migration | `ALTER COLUMN RENAME` is not idempotent on its own | Wrap in `DO $$ IF EXISTS (info_schema lookup) THEN ... END $$;` — see `migrations/0006_field_alignment.sql` for the pattern |

---

## 9. Known inconsistencies (deliberate, documented)

### 9.1 `/resume` page promises "we never store the file" but actually does

The `/resume` AI page uploads the resume to R2 + creates a row in
`job_tracker.files` with `slot=NULL`. The drop-zone copy says otherwise.

**Decision:** keep as-is for now (user's call). To resolve later, either:
- Backend deletes the file after extraction (matches UI promise), or
- Frontend extracts text client-side before sending (true "never store"), or
- UI gets a copy update (would require sign-off per the no-UI-changes rule).

### 9.2 AI-source resumes don't appear in the vault

Files uploaded via `/resume` have `slot=NULL`; the vault UI only shows the
5 numbered slots. So you can't see/manage AI-source uploads through the UI
right now. They occupy R2 and a DB row but are invisible.

If this becomes a problem, fix per 9.1.

---

## 9.5. Round 2 — Live parity alignment (2026-05-07)

Background: an audit of the live `naukriclear.com` API surface (see
`docs/LIVE_PARITY_AUDIT.md`) surfaced field-level and shape-level deltas
between live and our build. This round closed the deltas the user
selected — backend-only, no frontend changes.

### Migration

`migrations/0006_field_alignment.sql` — idempotent. Renames are guarded
with `DO $$ ... EXISTS (info_schema) ... END $$;` so re-runs are safe.

| Change | Why |
|---|---|
| `notes.body` → `notes.content` | match live |
| `applications.description` → `applications.job_description` | match live, future-clearer |
| add `applications.stage_changed_at TIMESTAMPTZ` | enables stale derivation; updated only when `stage` actually changes |
| `profiles`: add `linkedin_url`, `github_url`, `website_url` | richer public profile |
| `profile_experiences`: add `location`, `start_date`, `end_date`, `current`; rename `summary` → `description`, `ordinal` → `sort_order` | structured dates instead of free-text `period` |
| `profile_educations`: add `field`, `start_date`, `end_date`, `gpa`, `description`; rename `ordinal` → `sort_order` | same |
| `profile_projects`: add `tech_stack`, `link`; rename `summary` → `description`, `ordinal` → `sort_order` | richer project shape |
| `profile_skills`: add `sort_order` | sortable |

`period` columns on experiences/educations are **kept** in DB (data
preservation) but no longer returned in any response.

### New + changed endpoints

| Method | Path | Change |
|---|---|---|
| `GET` | `/job-tracker/notes/{id}` | **new** — full content (list endpoint now returns `excerpt`) |
| `GET` | `/job-tracker/profile/slug` | **changed** — now returns `{slug, isPublic, profileUrl}` (was `{available}`) |
| `GET` | `/job-tracker/profile/slug/check?slug=X` | **new** — `{slug, available}` |
| `GET` | `/job-tracker/public/profile/{slug}` | **enriched body** — adds `pictureUrl`, `linkedinUrl`, `githubUrl`, `websiteUrl`; sub-resources use new shape |
| `GET` | `/job-tracker/public/analytics/{slug}` | **new, unauthenticated** — `{summary, funnel, weeklyActivity}` |
| `GET` | `/job-tracker/notes` | **slimmer body** — returns `excerpt` (200 chars) instead of full content |
| `GET` | `/job-tracker/resumes` and `/cover-letters` | **wrapped** in `{resumes/coverLetters, count, limit:5}` |
| `PATCH` | `/job-tracker/profile/visibility` | accepts both `{public}` (legacy) and `{isPublic}` (canonical) |

### Derived helpers added to `Store`

- `Funnel(ctx, userID)` — per-stage counts in canonical order
- `WeeklyActivity(ctx, userID, weeks)` — Monday-anchored UTC ISO weeks
- `PublicAnalyticsBySlug(ctx, slug)` — composes Dashboard + Funnel + WeeklyActivity, gated on `is_public=true`
- `resolvePublicSlug(ctx, slug)` — slug → user_id, requires public

### What we deliberately deferred this round

CSV column alignment, AI metric/PDF changes, notifications, payments,
community, templates, and all frontend rewiring. See
`LIVE_PARITY_AUDIT.md` §6 for the full inventory.

### Operational note

After this round's migration applies, the running `go run ./cmd/api`
process must be restarted — the binary in memory still references old
column names. Idempotent migration means safe to re-run if needed.

---

## 9.7. Auto-slug provisioning at first sign-in (2026-05-08)

When a new user completes Google OAuth, `auth.Handler.GoogleCallback`
now runs registered `ProfileProvisioner`s after `UpsertUser`. The
job-tracker store is registered as one in `server.go`. Effect:

1. `ensureProfile` — creates an empty `job_tracker.profiles` row.
2. `ensureSlug` — picks a slug, preferring the user's name
   ("Shubham Dixit" → `shubham-dixit`), falling back to the email
   local-part. Resolves uniqueness with `-2`, `-3`, … suffixes.
3. `is_public` is **untouched** — stays `FALSE` per the schema default,
   so the public route returns 404 until the user opts in via
   `PATCH /profile/visibility`.

The slug never auto-rewrites once set: if a user changes their Google
display name later, the slug stays where they (or the bootstrap)
originally placed it. They can override anytime via `PATCH /profile/slug`.

Lazy fallback: `GetProfile` still calls `ensureSlug`, so existing users
who never had a slug get one on their next `/profile` fetch even without
re-signing-in.

### Files touched
- `internal/jobtracker/store.go` — new `slugifyAlphaNum`, `slugFromName`,
  `ProvisionProfile`. `ensureSlug` now reads name+email together and
  prefers name.
- `internal/auth/handler.go` — new `ProfileProvisioner` interface +
  `WithProvisioner` builder. Callback iterates registered provisioners.
- `internal/server/server.go` — wires `jtStore` as the auth provisioner.

### Cross-package interface

`auth.ProfileProvisioner` is the only contract — keeps auth from
importing jobtracker. Any future tool with bootstrap needs implements
the same interface and registers via `authHandler.WithProvisioner(...)`.

---

## 9.6. Round 3 — Frontend wiring after live parity (2026-05-07)

Backend Round 2 (§9.5) renamed columns and reshaped sub-resources without touching the frontend. This round caught the frontend up. **Zero UI changes** — strictly data-layer.

### New frontend file

- `job-tracker/lib/period.ts` — `formatPeriod({startDate, endDate, current})` and `parsePeriod(s)`. Bridges the existing free-text "2024 – Present" prompt UX with the structured date fields the backend now stores. Year-resolution; tolerant of empty / unparseable input.

### `lib/api-client.ts` shape changes

- `ApiApplication`: `description` → `jobDescription`; added `stageChangedAt?`
- `ApiNote`: `body` → `content` (full record, used for detail/create/update)
- `ApiNoteListItem` (new): excerpt-only shape returned by `GET /notes`
- `ApiExperience` / `ApiEducation` / `ApiProject`: drop `period`/`summary`/`ordinal`; add structured dates + `description` + `sortOrder` + (project) `techStack`/`link`
- `ApiSkill`: add `sortOrder`
- `ApiProfile` / `ApiPublicProfile`: add `linkedinUrl`/`githubUrl`/`websiteUrl`. Public profile gains `pictureUrl`. Public skills `string[]` → `ApiSkill[]`.
- `ApiFile` + `ApiResumesResponse` + `ApiCoverLettersResponse`: lifted out of inline page declarations; envelope unwraps `{resumes/coverLetters, count, limit:5}`

### Per-page rewires

| Page | Change |
|---|---|
| `app/applications/page.tsx` | rename `description` → `jobDescription` (draft, openEdit, view modal, AI dialogs, form input). 7 sites. |
| `app/notes/page.tsx` | list type `ApiNote[]` → `ApiNoteListItem[]`; on activation, fetch `GET /notes/{id}` to populate full content; PUT bodies use `content:` instead of `body:`; conflict-resolver reads `server.content`. Internal `Draft.body` kept as in-memory name; translation happens at the API boundary. |
| `app/profile/page.tsx` | imports `formatPeriod`/`parsePeriod`. Display: every `{exp.period}` / `{ed.period}` → `{formatPeriod(exp)}`. Inputs: prompt result fed through `parsePeriod` to derive `startDate`/`endDate`/`current`. Project `summary` → `description` on the wire. `sortOrder` added to skills/experiences/educations/projects. |
| `app/u/[slug]/page.tsx` | imports `formatPeriod`. Skills now objects (`skill.id`/`skill.name`). `experience.summary` → `experience.description`. `experience.period` → `formatPeriod(experience)`. Same for educations. `project.summary` → `project.description`. |
| `app/resumes/page.tsx` | inline `ApiFile` removed; uses shared types. Envelope unwrap: `r.resumes` and `c.coverLetters`. |
| `app/settings/page.tsx` | visibility PATCH body `{public}` → `{isPublic}`. (Backend accepts both for one round, but the frontend now sends the canonical key.) |
| `app/dashboard/page.tsx` | no change — user's client-side funnel/weekly/topSources logic only reads fields that didn't rename. |
| `app/resume/page.tsx` | no change — AI report endpoint shape unchanged this round. |

### Verification result

`npx tsc --noEmit` clean. `npx next build` clean — all 25 routes compile.

### Operational note

After this round, restart `npm run dev` to pick up the type changes. The
backend (`go run ./cmd/api`) was already restarted in §9.5. Both services
must be on their post-Round-2/Round-3 binaries together.

---

## 10. The next tool: condensed checklist

When the next tool comes (`reel-hooks`, etc.), this is the outline:

```
[ ] Decide: per-user (RequireUser) or shared (X-API-Key)?
[ ] Decide: needs file storage? AI? Other infra?
[ ] migrations/000N_<tool>.sql — schema + indexes
[ ] internal/<tool>/types.go — request/response shapes
[ ] internal/<tool>/store.go — SQL
[ ] internal/<tool>/handlers_*.go — split by domain
[ ] internal/<tool>/routes.go — RegisterRoutes(mux, h, requireUser)
[ ] Wire in internal/server/server.go
[ ] Update .env.example
[ ] go vet ./... && go build ./... && go test -race ./...
[ ] Frontend: extend lib/api-client.ts with new types
[ ] Frontend: wire each page to API; do NOT redesign UI
[ ] Update this playbook's section 9 with any new known inconsistencies
[ ] Document any new infra setup (e.g. new R2 bucket, new vendor) above
```

---

## 11. Cross-references

- **`docs/GUIDE.md`** — canonical contributor guide for the monolith
  (architecture, conventions, deploy lifecycle). Read alongside this
  playbook.
- **`docs/LIVE_PARITY_AUDIT.md`** — the live-vs-ours audit + decision inventory that drove Round 2.
- **`README.md`** — service overview, endpoint table, local dev quick-start.
- **`.env.example`** — every env var with a comment explaining it.
- **`migrations/`** — the schema's source of truth, lex-ordered.

---

*Last updated: 2026-05-08 — auto-slug provisioning on first sign-in.*
