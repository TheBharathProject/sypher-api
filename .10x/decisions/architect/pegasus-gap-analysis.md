# ADR: Pegasus Gap Analysis — Architect Decision Record

**Status:** Accepted
**Date:** 2026-05-12
**Slug:** pegasus-gap-analysis
**Authors:** Architect Agent (claude-sonnet-4-6)
**Upstream:** CTO ADR `.10x/decisions/cto/pegasus-gap-analysis.md`; PM ADR `.10x/decisions/product-manager/pegasus-gap-analysis.md`

---

## Context

This is a delta-only design. Phases 1–9, 10A, 11, 13, and 16 are complete. The backend is a single Go `net/http` binary; the frontend is a Next.js 14 App Router static export. There is no distributed system to redesign. Every item below is an additive change to an existing, running production system.

The six feature groups below are ordered by implementation dependency:

1. Community post slugs (prerequisite for Phase 12 frontend link change)
2. Phase 12 backend (sort indexes + per-surface metadata validation)
3. Phase 12 frontend (enum realignment, filter URL state, sort tabs, author chips)
4. Phase 10 — AI PDFs (cover letter, resume tweak, resume report)
5. Phase 10 — Avatar upload (deferred P2, design only)
6. Phase 14 — Design system and modal a11y
7. Phase 15 — Mobile CSS surgery

---

## Architecture Decision

**Chosen approach: Additive monolith extension.**

No service decomposition. No new infrastructure. Every remaining feature is a new function, file, or migration within the existing package boundaries. The complexity is already at the right level for a 1–2 person engineering team.

The two non-trivial decisions that were one-way doors — PDF library and focus trap library — are already resolved by the CTO ADR:
- PDF generation: `github.com/go-pdf/fpdf`
- Focus trap: `@radix-ui/react-focus-scope`

---

## Component Diagram

```
sypher-api/
├── internal/jobtracker/
│   ├── slug.go                   [NEW] slugify() + Store.uniqueSlug()
│   ├── handlers_community.go     [MOD] dual-path lookup; per-surface validation; sort param
│   ├── handlers_ai.go            [MOD] cover letter PDF + resume tweak PDF endpoints
│   ├── handlers_files.go         [MOD] avatar upload/finalize/delete routes
│   ├── store_community.go        [MOD] GetPostBySlug(); ListPosts() sort modes
│   ├── store_files.go            [MOD] avatar store fns; UpdateUserPictureURL()
│   ├── store.go                  [MOD] CreatePost writes slug column
│   ├── types.go                  [MOD] CommunityPost gains Slug field
│   └── routes.go                 [MOD] new avatar routes; new PDF routes
├── migrations/
│   ├── 0019_community_slugs.sql  [NEW] slug column + backfill + unique index
│   └── 0020_community_sort_indexes.sql  [NEW] vote_count + comment_count indexes
└── go.mod                        [MOD] add github.com/go-pdf/fpdf

job-tracker/
├── lib/
│   └── api-client.ts             [MOD] ApiCommunityPost gains slug; avatar types; PDF download fn
├── app/community/[section]/
│   └── page.tsx                  [MOD] enum realignment; filter URL state; author chips; sort tabs
└── app/globals.css               [MOD] tokens; stage colors; breakpoints; hover gating; modal a11y
```

---

## Feature 1: Community Post Slugs

### Decision

New file `internal/jobtracker/slug.go`. `GetCommunityPost` and `PublicGetCommunityPost` currently call `pathUUID(w, r, "id")` which hard-returns 400 for non-UUID path values. These handlers must be refactored to a dual-path pattern before the frontend can link to slug URLs.

### Files touched

| File | Change |
|---|---|
| `migrations/0019_community_slugs.sql` | ADD COLUMN slug TEXT; backfill; NOT NULL; UNIQUE INDEX |
| `internal/jobtracker/slug.go` | New: `slugify(string) string`, `(s *Store) uniqueSlug(ctx, string) (string, error)` |
| `internal/jobtracker/store_community.go` | `CreatePost` writes slug via `uniqueSlug()`; new `GetPostBySlug(ctx, userID, slug)` |
| `internal/jobtracker/handlers_community.go` | `GetCommunityPost` + `PublicGetCommunityPost`: replace `pathUUID` call with dual-path lookup |
| `internal/jobtracker/types.go` (via `store_community.go`) | `CommunityPost.Slug` field via scan column addition |
| `store_community.go` communityPostCols | Add `p.slug` to the column constant and scan |
| `lib/api-client.ts` | `ApiCommunityPost` gains `slug: string` |
| `app/community/[section]/page.tsx` | Post card `<Link>` href: `post.id` → `post.slug` |

### Slug generation logic

```
slugify(title):
  1. lowercase
  2. convert spaces + underscores to hyphens
  3. strip chars outside [a-z0-9-]
  4. collapse consecutive hyphens ("--" → "-")
  5. trim leading/trailing hyphens
  6. truncate at 80 chars, back up to last hyphen boundary
  7. if empty result → return "post"

uniqueSlug(ctx, title):
  base = slugify(title)
  if base == "" { base = "post" }
  exists = SELECT EXISTS(SELECT 1 FROM community_posts WHERE slug = $1)
  if !exists { return base, nil }
  suffix = 4 hex chars from crypto/rand (2 bytes)
  candidate = base + "-" + suffix
  if len(candidate) > 85 { candidate = base[:80] + "-" + suffix }
  return candidate, nil
  // Note: no second existence check on candidate — collision probability
  // at 4 hex chars (65,536 combinations) is negligible at Pegasus scale.
  // If the product scales to millions of identically-titled posts, promote
  // to a retry loop.
```

### Dual-path lookup

```go
// GetCommunityPost and PublicGetCommunityPost both use this pattern:
idOrSlug := r.PathValue("id")
if parsed, err := uuid.Parse(idOrSlug); err == nil {
    post, err = h.store.GetPost(ctx, parsed, viewerID)
} else {
    post, err = h.store.GetPostBySlug(ctx, idOrSlug, viewerID)
}
// ErrNoRows → 404 in both branches (existing behaviour preserved)
```

### Migration 0019

```sql
-- idempotent
ALTER TABLE job_tracker.community_posts
  ADD COLUMN IF NOT EXISTS slug TEXT;

UPDATE job_tracker.community_posts
  SET slug = 'post-' || REPLACE(id::text, '-', '')
  WHERE slug IS NULL;

ALTER TABLE job_tracker.community_posts
  ALTER COLUMN slug SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS community_posts_slug_idx
  ON job_tracker.community_posts (slug);
```

Rationale for backfill value `'post-' || REPLACE(id::text, '-', '')`: guarantees uniqueness (UUID hex is globally unique), is never empty, passes the NOT NULL constraint, and causes no 404s on existing bookmarks since 0019 must be deployed before any frontend link change. The old UUID-path URLs still resolve via the UUID-first branch of the dual-path lookup.

### Failure modes

| Mode | Detection | Recovery |
|---|---|---|
| Slug collision at uniqueSlug (two concurrent inserts same title) | Postgres UNIQUE constraint error | The second writer retries with a new random suffix (caller catches unique_violation and calls uniqueSlug again with a different entropy draw — implement in the store, not the handler) |
| Title is entirely special chars ("!!!") | slugify returns "post" → uniqueSlug appends suffix | Handled by design; final slug is "post-{hex}" |
| Title is 280 chars → slug would be 280 chars | truncation at 80 chars inside slugify | Handled |
| Empty title submitted | Handler already validates title != "" before calling CreatePost | Slug never sees empty title |
| Migration run on empty table | UPDATE WHERE slug IS NULL affects 0 rows, idempotent | No issue |

---

## Feature 2: Phase 12 Backend — Sort Modes + Per-surface Metadata Validation

### Decision

Two changes in isolation. The sort index migration is additive and zero-regression. The metadata validation is new code in `CreateCommunityPost` only — existing rows in the DB are never touched.

### Migration 0020 — Community sort indexes

```sql
-- Sort by vote_count: recruiter directory "Most Upvoted" + experiences "Most Voted"
CREATE INDEX IF NOT EXISTS idx_community_posts_surface_votes
  ON job_tracker.community_posts (surface, vote_count DESC, created_at DESC)
  WHERE status = 'active';

-- Sort by comment_count: "Most Reviewed" / "Least Reviewed"
CREATE INDEX IF NOT EXISTS idx_community_posts_surface_comments
  ON job_tracker.community_posts (surface, comment_count DESC, created_at DESC)
  WHERE status = 'active';
```

The existing `idx_community_posts_surface_created` already handles `sort=newest`. Both new indexes are partial (`WHERE status = 'active'`) so soft-deleted rows are excluded without a filter step.

### Sort mode: ORDER BY clauses

`CommunityListOpts` gains a `Sort` field (`string`, values `"newest"` | `"votes"` | `"most-reviewed"` | `"least-reviewed"`). `parseListOpts` reads `r.URL.Query().Get("sort")`.

```
sort=newest          → ORDER BY p.created_at DESC          (existing index)
sort=votes           → ORDER BY p.vote_count DESC, p.created_at DESC
sort=most-reviewed   → ORDER BY p.comment_count DESC, p.created_at DESC
sort=least-reviewed  → ORDER BY p.comment_count ASC, p.created_at DESC
```

The cursor pagination currently uses `created_at` as the cursor token. For non-time-ordered sorts, cursor pagination by `created_at` is incorrect (a row inserted after the cutoff with higher votes won't appear). Accepted trade-off: cursor pagination remains `created_at`-based for v1.0. Sort tabs in the frontend reload from page 1 on sort change (standard UI behaviour). This is correct because users sorting by votes are looking at the top of the corpus, not paginating deep. Document this in code comments.

The `writeListResponse` function currently uses `posts[len(posts)-1].CreatedAt` as the next cursor regardless of sort mode. This is acceptable for v1.0 because:
- `votes` and `most-reviewed` sorts are not paginated in the typical UX (user browses the top N)
- A correct keyset cursor for vote-sorted results would require composite (vote_count, created_at, id) encoding — disproportionate complexity at current content volume

### Per-surface metadata validation dispatch table

Add a `validateCommunityMetadata(surface string, metadata json.RawMessage) error` function to `handlers_community.go`, called from `CreateCommunityPost` after the title/body checks. Hard-reject with `400` on any violation; existing data is never re-validated.

```
surface="experiences":
  Parse metadata → struct{Outcome, Difficulty string; Rounds []any}
  Outcome must ∈ {"Offer","Reject","Ghosted","InProgress","Withdrew"}
  Difficulty must ∈ {"Easy","Medium","Hard"}
  Rounds must be non-nil and len >= 1
  Error shape: {error: "invalid_metadata", field: "outcome", message: "..."}

surface="ask":
  Parse metadata → struct{Tags []string}
  len(Tags) must be <= 3
  each tag must ∈ the 10-value allow-list (see constants below)

surface="recruiters":
  Parse metadata → struct{Specializations, HiringLevels []string}
  each Specializations[i] must ∈ 12-value list
  each HiringLevels[i] must ∈ 8-value list

surface="reviews":
  Parse metadata → struct{TargetRole, ExperienceLevel string}
  TargetRole must ∈ {"SDE","PM","Data Science","Design","DevOps","QA","Other"}
  ExperienceLevel must ∈ {"Fresher","Junior (0-2)","Mid (2-5)","Senior (5+)","Lead (8+)"}

surface="referrals":
  No metadata validation (referral surface has no spec-defined enum constraints)
```

Implement as a `map[string]func(json.RawMessage) error` dispatch table keyed by surface name. Each validator function is a pure function with no side effects — testable in isolation. Define enum allow-lists as `var` package-level maps (same pattern as `validStages` and `validSources` in `types.go`).

### Files touched

| File | Change |
|---|---|
| `migrations/0020_community_sort_indexes.sql` | New: two partial indexes on vote_count + comment_count |
| `internal/jobtracker/store_community.go` | `CommunityListOpts.Sort` field; `ListPosts()` ORDER BY dispatch |
| `internal/jobtracker/handlers_community.go` | `parseListOpts` reads `?sort=`; `validateCommunityMetadata()` called in `CreateCommunityPost` |

### Migration sequence confirmed

- `0019_community_slugs.sql` — ships with Phase 12 (prerequisite)
- `0020_community_sort_indexes.sql` — ships with Phase 12 backend

Both are additive index/column operations. They can be applied in sequence in a single `migrate` run. There is no ordering dependency between them (0020 does not reference the slug column). The numbers follow the next-available convention (`0018_` is the last existing migration).

---

## Feature 3: Phase 12 Frontend

### Files touched

| File | Change |
|---|---|
| `app/community/[section]/page.tsx` | Enum updates; filter URL state; sort tabs; author chip links; recruiter list from API; application link select |
| `lib/community.ts` | `listCommunityPosts()` passes `sort` + filter params |

### Enum realignment (constants in `page.tsx`)

```typescript
// Before (10 values)
const ROUND_TYPES = ["Phone Screen","Recruiter Call","Technical","Coding",
  "System Design","Behavioral","Onsite","Hiring Manager","HR","Other"];

// After (13 values — add 3 new types per spec §5.10)
const ROUND_TYPES = ["Phone Screen","Recruiter Call","Technical","Coding",
  "System Design","Behavioral","Onsite","Hiring Manager","HR",
  "Group Discussion","Case Study","Culture Fit","Other"];

// Before (10 values)
const TARGET_ROLES = ["Frontend Engineer","Backend Engineer","Full-Stack Engineer",
  "Mobile Engineer","Data Engineer","ML Engineer","DevOps / Platform",
  "Product Manager","Designer","Other"];

// After (7 values per spec §4.10)
const TARGET_ROLES = ["SDE","PM","Data Science","Design","DevOps","QA","Other"];

// Before (6 values, wrong naming)
const EXPERIENCE_LEVELS = ["Intern","Entry Level","Mid Level","Senior","Staff+","Manager"];

// After (5 values per spec)
const EXPERIENCE_LEVELS = ["Fresher","Junior (0-2)","Mid (2-5)","Senior (5+)","Lead (8+)"];

// Before (mismatched with backend)
const OUTCOMES = ["Offer","Rejected","In Progress","Withdrew","Ghosted"];

// After (match backend enum exactly)
const OUTCOMES = ["Offer","Reject","Ghosted","InProgress","Withdrew"];
```

Note: `OUTCOMES` label displayed in the UI should be a display-name map if the raw enum values ("Reject", "InProgress") are undesirable visually. Add a `const OUTCOME_LABELS: Record<string, string>` alongside `OUTCOMES` if needed. The backend stores the raw enum value in JSONB metadata.

### Filter URL state pattern

Replace decorative `<button className="filter-box">` elements with wired handlers. Use `useSearchParams()` (Next.js App Router) + `useRouter()` for URL updates without page navigation.

```typescript
// In CommunitySectionPage:
const searchParams = useSearchParams();
const router = useRouter();

const currentSort = searchParams.get("sort") ?? "newest";

function setFilter(key: string, value: string) {
  const params = new URLSearchParams(searchParams.toString());
  params.set(key, value);
  router.replace(`?${params.toString()}`, { scroll: false });
}
```

The `listCommunityPosts()` call in the `useEffect` reads from `searchParams` so the list re-fetches on URL change. This is the standard Next.js pattern for URL-synced list pages.

### Author chips

```tsx
// In post card renderer — wrap authorName in Link when authorSlug is present:
{post.authorSlug
  ? <Link href={withBase(`/u/${post.authorSlug}`)} className="author-chip">
      {post.authorName}
    </Link>
  : <span className="author-chip">{post.authorName}</span>
}
```

### Recruiter list from API (B5 fix)

Remove `filteredRecruiters` computed from `seedRecruiters`. For `section === "recruiters"`, render from `posts` (the real API response). Remove the `seedRecruiters` constant entirely.

### "Link to tracked application" select

In the experience modal, add a `useEffect` on modal open to `GET /job-tracker/applications?limit=200` and populate a `<select>` with `{id, company, role}` entries. On selection, auto-fill `draft.company` and `draft.role`. No new API endpoint needed.

---

## Feature 4: Phase 10 — AI PDFs

### Decision

Two new POST endpoints: `POST /job-tracker/ai/cover-letter/pdf` and `POST /job-tracker/ai/resume/tweaks/{id}/pdf`. Both accept the same request bodies as their text counterparts, generate the AI output, render it to PDF via `go-pdf/fpdf`, and stream `application/pdf` to the response. Credits are debited at the same cost as the text endpoints (CoverLetter=10, ResumeTweak=20).

PDF generation does NOT re-call Deepseek if existing content is available. The resume tweak endpoint reads the stored `tweaked_text` (or `user_edits` if set) from the DB. The cover letter endpoint must re-generate (cover letters are not persisted to DB — they are ephemeral).

### go.mod addition

```
github.com/go-pdf/fpdf v2.x.x
```

### PDF layout spec

```
Cover letter PDF:
  Page: A4, portrait, margins: 25mm all sides
  Header: Company name (bold, 14pt) | Role (regular, 12pt) | date
  Body: cover letter text, 11pt, line spacing 1.4
  Footer: "Generated by Pegasus · sypher.in/pegasus"

Resume tweak PDF:
  Page: A4, portrait, margins: 20mm all sides
  Header: Tweak title (bold, 14pt) | Created date
  Body: tweaked resume text (or user_edits if non-empty), 10pt, line spacing 1.3
  Footer: "Generated by Pegasus · sypher.in/pegasus"

Resume report PDF:
  Page: A4, portrait, margins: 25mm all sides
  Header: "Resume Analysis Report" (bold, 16pt) | date
  Score line: "Score: {n}/100" (bold, 12pt)
  Body: report markdown rendered as plain text (strip markdown syntax), 11pt
  Footer: "Generated by Pegasus · sypher.in/pegasus"
```

For all PDFs: use `fpdf.UnicodeTranslatorFromDescriptor("")` for UTF-8 handling. Font: Helvetica (built-in, no font file needed). Long words that exceed line width are broken at word boundaries by fpdf's `MultiCell`.

### New handler functions in `handlers_ai.go`

```go
// POST /job-tracker/ai/cover-letter/pdf
// Body: same as GenerateCoverLetter (jobDescription, resumeText/resumeFileId)
// Returns: application/pdf binary stream
// Credits: CostCoverLetter=10 (same as text endpoint)
func (h *Handler) GenerateCoverLetterPDF(w http.ResponseWriter, r *http.Request)

// POST /job-tracker/ai/resume/tweaks/{id}/pdf
// Reads tweaked_text (or user_edits if set) from DB — no AI re-call
// Returns: application/pdf binary stream
// Credits: CostResumeTweak=20 (charged same as creation — creation already debited,
//          so PDF export is a second debit; this is acceptable at v1.0.
//          Alternative: free PDF export. Decision: charge, keeps credits model simple.
//          If users complain, make PDF free by removing the gateAICredit call here.)
func (h *Handler) ResumeTweakPDF(w http.ResponseWriter, r *http.Request)
```

Open question: should `ResumeTweakPDF` debit credits again? The user already paid for the tweak creation. Making PDF export free (no gateAICredit call) is the simpler and user-friendlier option. This is flagged for Staff Engineer decision; default recommendation is **no credit debit on PDF export** since it is a retrieval of already-generated content.

### Response header pattern

```go
w.Header().Set("Content-Type", "application/pdf")
w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, safeName))
// No Content-Length set — streaming write; fpdf.Output writes directly to w
```

### PDF filename convention

```
cover letter:  {company}-{role}-cover-letter.pdf   (slugified, max 60 chars total)
resume tweak:  {title}-tweak.pdf                   (slugified)
resume report: resume-report-{YYYYMMDD}.pdf
```

### New routes in `routes.go`

```go
g("POST", "/job-tracker/ai/cover-letter/pdf", h.GenerateCoverLetterPDF)
g("POST", "/job-tracker/ai/resume/tweaks/{id}/pdf", h.ResumeTweakPDF)
g("GET",  "/job-tracker/ai/resume/report/latest/pdf", h.LatestResumeReportPDF)
```

Rate limiting: inherit the existing `/job-tracker/ai/*` rate limit (5/min, burst 10).

### Frontend PDF download pattern

```typescript
// In cover letter modal and tweak modal:
async function downloadPDF(endpoint: string, body: object, filename: string) {
  const res = await api.raw(endpoint, { method: "POST", body });
  const blob = await (res as Response).blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}
```

The `api.raw()` method already exists in `lib/api-client.ts` and returns the raw `Response` object, so `rawResponse: true` is already handled.

### Files touched

| File | Change |
|---|---|
| `go.mod` + `go.sum` | Add `github.com/go-pdf/fpdf` |
| `internal/jobtracker/handlers_ai.go` | `GenerateCoverLetterPDF`, `ResumeTweakPDF`, `LatestResumeReportPDF`; helper `renderPDF(title, body string) ([]byte, error)` |
| `internal/jobtracker/routes.go` | Three new PDF routes |
| `app/applications/page.tsx` | "Download PDF" button in cover letter modal |
| `app/resume/page.tsx` | "Download report" button on step 4 (Results) |
| `app/applications/[id]/page.tsx` or tweak modal location | "Download PDF" button in tweak result panel |

### Failure modes

| Mode | Detection | Recovery |
|---|---|---|
| fpdf render error | Returns error from `pdf.Output()` | 500 with `internal_error` (do not expose fpdf error text) |
| Very long cover letter (>20 pages) | fpdf handles pagination automatically | No issue |
| Non-UTF8 characters in AI output | `UnicodeTranslator` strips unsupported chars | Acceptable degradation |
| Cover letter re-generation fails (AI down) | Deepseek returns error | Same 502 as text endpoint |
| Tweak not found or not owned | `GetResumeTweak` returns pgx.ErrNoRows | 404 (existing pattern) |

---

## Feature 5: Phase 10 — Avatar Upload (P2 — Deferred, Design Only)

**Status: Deferred post-v1.0. Design is included for when work begins.**

### Migration (0021 when scheduled)

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_files_avatar_per_user
  ON job_tracker.files (user_id)
  WHERE kind = 'avatar';
```

This partial unique index enforces one active avatar row per user without a DB constraint on the `kind` column (which would break existing resume/cover_letter rows).

### Routes (to add in routes.go when implementing)

```go
g("POST",   "/job-tracker/profile/avatar/upload-url", h.RequestAvatarUploadURL)
g("PATCH",  "/job-tracker/profile/avatar/finalize",   h.FinalizeAvatar)
g("DELETE", "/job-tracker/profile/avatar",            h.DeleteAvatar)
```

### Handler logic outline

`RequestAvatarUploadURL`:
- Validate `fileName`, `mimeType` (must be `image/jpeg`, `image/png`, `image/webp`), `fileSize` (max 5 MiB)
- Key pattern: `job-tracker/{userID}/avatar/{uuid}-{safeName}`
- Call `store.CreateFile(ctx, userID, "avatar", ...)` — the partial unique index will enforce one-per-user at the DB level; if violated, `pgx` returns a unique_violation error which the handler maps to `409 Conflict`

`FinalizeAvatar`:
- Call `store.FinalizeFile(ctx, userID, fileID, size)`
- Compose the public R2 URL from `cfg.R2PublicURL + "/" + storageKey`
- Call `store.UpdateUserPictureURL(ctx, userID, publicURL)` (new store function on `auth.Store`)

`DeleteAvatar`:
- Find the file row `WHERE user_id = $1 AND kind = 'avatar'` (no ID needed — one per user)
- Delete R2 object
- Delete DB row
- Call `store.UpdateUserPictureURL(ctx, userID, "")` to clear `picture_url`

`store.UpdateUserPictureURL`:
```go
func (s *Store) UpdateUserPictureURL(ctx context.Context, userID uuid.UUID, url string) error {
    _, err := s.pool.Exec(ctx,
        `UPDATE auth.users SET picture_url = NULLIF($1, '') WHERE id = $2`,
        url, userID,
    )
    return err
}
```

This must live on `auth.Store` (not `jobtracker.Store`) since `auth.users` is in the `auth` schema and `auth.Store` already owns that table. The `jobtracker.Handler` already holds `authStore *auth.Store` for API token operations, so no new wiring is needed.

### Frontend (when implementing)

- "Change picture" and "Remove" buttons near line 353 of `app/profile/page.tsx`
- Same two-step R2 upload UX pattern as resume/cover-letter vault
- On finalize success, update `me.pictureUrl` in local state → sidebar avatar updates immediately

---

## Feature 6: Phase 14 — Design System + Modal A11y

### CSS tokens to add to `:root` in `app/globals.css`

```css
/* Existing `:root` block — append these: */
--bg-hover: rgba(0, 0, 0, 0.04);
--accent-soft: rgba(var(--accent-rgb), 0.12);
--font-mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
--radius-sm: 4px;
--radius-2xl: 20px;

/* Stage color tokens (missing 4 of 7; add all 7 for completeness) */
--stage-interested: #e8f4fd;
--stage-applied:    #fef9e7;
--stage-phone:      #eaf7ed;
--stage-technical:  #fdf2e9;
--stage-onsite:     #f4ecf7;
--stage-offer:      #d5f5e3;
--stage-rejected:   #fdecea;
```

Dark-theme mirrors (`[data-theme="dark"] :root` block or the existing `.dark` / `[data-theme="dark"]` rule in globals.css — match the existing pattern):
```css
--bg-hover: rgba(255, 255, 255, 0.06);
--stage-interested: #1a2a3a;
--stage-applied:    #3a2e10;
--stage-phone:      #162318;
--stage-technical:  #3a2010;
--stage-onsite:     #2e1a3a;
--stage-offer:      #0d2e1a;
--stage-rejected:   #2e0d0d;
```

### ModalShell component

Extract to `components/ui.tsx` as a new export. Required props:

```typescript
export type ModalShellProps = {
  open: boolean;                    // controls visibility
  onClose: () => void;              // called on backdrop click + Escape key
  title: string;                    // the dialog's accessible name
  titleId?: string;                 // defaults to "modal-title"
  children: ReactNode;
  width?: "sm" | "md" | "lg";      // maps to CSS width tokens
};

export function ModalShell({ open, onClose, title, titleId = "modal-title", children, width = "md" }: ModalShellProps) {
  // 1. useEffect: add/remove overflow-hidden on document.body
  // 2. useEffect: listen for Escape keydown → onClose()
  // 3. FocusScope from @radix-ui/react-focus-scope wraps content
  // 4. backdrop div with role="dialog" aria-modal="true" aria-labelledby={titleId}
  // 5. inert attribute on <main> via a ref or document.querySelector — see note below
}
```

The `inert` attribute on `<main>` cannot be set declaratively from a component that is rendered inside `<main>` itself (the modal portal is inside the layout). Pattern: the `ModalShell` `useEffect` finds `document.querySelector("main")` and sets/removes `inert` when `open` changes. This is a direct DOM mutation — acceptable because there is no SSR for authenticated pages (all pages are client-side guarded with `isAuthed()` checks).

### Focus trap integration

```typescript
import { FocusScope } from "@radix-ui/react-focus-scope";

// Inside ModalShell render:
<FocusScope trapped={open} restoreFocus asChild>
  <div role="dialog" aria-modal="true" aria-labelledby={titleId} ...>
    <h2 id={titleId}>{title}</h2>
    {children}
  </div>
</FocusScope>
```

`restoreFocus` returns focus to the trigger element on close. `trapped` gates the scope — only active when open.

### 8 modal sites to update

All 8 must be updated in a single PR to avoid a partial-refactor state:

1. Add application modal — `app/applications/page.tsx` (~line 805)
2. Edit application modal — `app/applications/page.tsx` (~line 959)
3. View application modal (sidebar) — `app/applications/page.tsx` (~line 1092)
4. Set reminder modal — `app/applications/page.tsx` (~line 1164)
5. Cover letter modal — `app/applications/page.tsx` (~line 1380)
6. Resume tweak modal — `app/applications/page.tsx` or `app/applications/[id]/page.tsx`
7. Add recruiter modal — `app/community/[section]/page.tsx` (recruiter surface) or `app/recruiters/page.tsx`
8. Delete account modal — `app/settings/page.tsx`

Each site: replace inline backdrop/dialog div with `<ModalShell>` import, move `<h2>` inside with `id="modal-title"`, verify `aria-labelledby` is on the ModalShell wrapper.

### Files touched (Phase 14)

| File | Change |
|---|---|
| `app/globals.css` | CSS token additions; stage color tokens; dark-theme mirrors |
| `app/applications/page.tsx` | Kanban column backgrounds use `var(--stage-{stage})`; 5 modal sites → ModalShell |
| `components/ui.tsx` | New `ModalShell` export; `MetricCard` numeric value gets `font-family: var(--font-mono)` |
| `package.json` | Add `@radix-ui/react-focus-scope` |
| `app/settings/page.tsx` | Delete account modal → ModalShell |
| `app/community/[section]/page.tsx` or `app/recruiters/page.tsx` | Recruiter modal → ModalShell |

---

## Feature 7: Phase 15 — Mobile CSS Surgery

### Scope

This is a CSS-only phase. No backend changes. No new components.

### Breakpoint change

Find-and-replace in `app/globals.css`:
```
@media (min-width: 720px)  →  @media (min-width: 48rem)
@media (max-width: 720px)  →  @media (max-width: 767px)
```

Do not use `768px` literals — use `rem` so the breakpoint scales with the user's browser font size preference.

### iOS modal positioning

For every modal backdrop overlay currently using `place-items: center`:
```css
/* Before */
.modal-backdrop { display: grid; place-items: center; }

/* After */
.modal-backdrop {
  display: grid;
  place-items: start center;
  padding-top: 8svh;    /* svh = small viewport height, respects iOS dynamic bar */
}
```

`svh` has full browser support at iOS 16+. For iOS 15 (minor share), the modal may be slightly off — acceptable.

### Hover gating pattern

Every `:hover` rule becomes:
```css
@media (hover: hover) {
  .element:hover { ... }
}
```

This is a mechanical transformation of ~113 rules. It should be done with a script or careful find-and-replace, not by hand. The transformation is safe because on pointer devices `(hover: hover)` is always true; on touch devices it is false, so the stuck-hover bug is resolved.

### Reduced-motion

```css
@media (prefers-reduced-motion: no-preference) {
  /* all transition and animation declarations */
}
```

Specifically: sidebar collapse transition, modal fade-in, button hover transitions.

### Touch targets

For 24px icon buttons (bell icon, vote arrows, close buttons):
```css
.icon-button::before {
  content: "";
  position: absolute;
  inset: -10px;   /* extends tap area to 44px without changing visual size */
}
.icon-button { position: relative; }
```

### Files touched (Phase 15)

| File | Change |
|---|---|
| `app/globals.css` | Breakpoint replace (×10+ rules); modal `8svh` padding; `@media (hover: hover)` wrappers (×113 rules); `@media (prefers-reduced-motion)`; touch target pseudo-elements |

---

## Data Model Additions

| Migration | Table | Column/Index Added | Purpose |
|---|---|---|---|
| 0019 | `job_tracker.community_posts` | `slug TEXT NOT NULL UNIQUE` | Human-readable post URLs |
| 0020 | `job_tracker.community_posts` | Partial index on `(surface, vote_count DESC, created_at DESC)` | `?sort=votes` query |
| 0020 | `job_tracker.community_posts` | Partial index on `(surface, comment_count DESC, created_at DESC)` | `?sort=most-reviewed` query |
| 0021 (deferred) | `job_tracker.files` | Partial unique index on `(user_id) WHERE kind='avatar'` | One avatar per user |

---

## API Contracts (New Endpoints)

```
POST /job-tracker/ai/cover-letter/pdf
  Request:  {jobDescription: string, resumeText?: string, resumeFileId?: string}
  Response: Content-Type: application/pdf; Content-Disposition: attachment; filename="..."
  Auth: required
  Credits: CostCoverLetter=10 (after free quota)
  Rate limit: /job-tracker/ai/* bucket (5/min)

POST /job-tracker/ai/resume/tweaks/{id}/pdf
  Request:  {} (empty body; content read from DB row)
  Response: Content-Type: application/pdf
  Auth: required (owner only — store.GetResumeTweak enforces this)
  Credits: TBD (see open question below)
  Rate limit: /job-tracker/ai/* bucket

GET /job-tracker/ai/resume/report/latest/pdf
  Request:  no body
  Response: Content-Type: application/pdf
  Auth: required
  Credits: none (report was already paid for on generation)
  Rate limit: /job-tracker/ai/* bucket
```

Modified endpoints (sort parameter):
```
GET /job-tracker/community/{surface}
  New query param: ?sort=newest|votes|most-reviewed|least-reviewed (default: newest)
```

---

## Integration Points Between Features

- **Slugs → Phase 12 frontend**: Slug feature must deploy and the migration must backfill before the frontend switches post card links from `post.id` to `post.slug`. Sequence: deploy backend 0019 migration, deploy frontend change.
- **Phase 12 enum validation → Phase 12 frontend enum realignment**: The backend now hard-rejects old enum values ("Rejected" → must be "Reject"). The frontend enum constants must be updated before or simultaneous with the backend validation landing, otherwise existing users submitting via the old frontend will receive 400 errors. These two changes must ship in the same deployment.
- **PDF endpoints → Credits**: PDF endpoints reuse `gateAICredit()` from `handlers_ai.go` unchanged. No new billing logic.
- **Avatar finalize → auth.Store**: The `UpdateUserPictureURL` method must be added to `auth.Store` (not `jobtracker.Store`). The `jobtracker.Handler` already holds `authStore *auth.Store` — no new dependency wiring needed.
- **ModalShell → all 8 modal sites**: Must be a single atomic PR. A half-migrated state creates CSS inconsistency.

---

## Scalability Path

Current bottleneck is `ListPosts()` with its single created_at index. The two new indexes in 0020 address the immediate vote/comment sort query plans. At the current content volume, no further optimization is needed.

If community content volume grows to >100k posts per surface:
- The `LIMIT 50` cap on list queries is already in place
- Cursor pagination prevents deep-offset scans
- The partial `WHERE status = 'active'` on all community indexes keeps soft-deleted rows out of hot paths

PDF generation is synchronous (in-request). At Pegasus scale (100–1000 DAU) this is fine — `go-pdf/fpdf` renders in <100ms for typical text. If PDF generation becomes a bottleneck, it can be extracted to a background job with a polling endpoint (same pattern as the existing cron infrastructure) without changing the API contract.

---

## Failure Modes Summary

| Feature | Failure | Detection | Recovery |
|---|---|---|---|
| Slug — DB unique violation on concurrent insert | Postgres `23505` unique_violation | `pgx` error code check in `CreatePost` | Retry `uniqueSlug` once with fresh entropy |
| Slug — dual-path lookup on malformed non-UUID non-slug | `GetPostBySlug` returns `pgx.ErrNoRows` | Standard 404 path | User sees 404 (correct behaviour) |
| Sort — query with unknown `?sort=` value | `parseListOpts` ignores unknown values, falls back to "newest" | No error; default sort applied | Graceful |
| Metadata validation — new surface added without validator | `validateCommunityMetadata` has no entry for surface | No validation (pass-through) | Acceptable — new surfaces default to unvalidated until validator added |
| PDF — fpdf render error | `pdf.Output()` returns non-nil error | 500 response | Retry on client (UI shows error toast) |
| PDF — tweak row deleted between request and PDF render | `GetResumeTweak` returns `pgx.ErrNoRows` | 404 response | Client shows error toast |
| Avatar — partial unique index violation (second avatar upload attempt) | Postgres unique_violation on `CreateFile` | 409 Conflict | UI prompts "delete existing avatar first" |
| ModalShell — `inert` on `<main>` fails (old browser) | No JS error; attribute is a no-op | ARIA fallback still works | Acceptable; focus trap still active |
| Mobile breakpoint — `svh` not supported (iOS 15) | Browser ignores unknown unit; modal may be partially obscured | Visual regression | Accepted for iOS 15 (diminishing share) |

---

## Migration Path (How We Get From Here to There)

All changes are additive. The deployment sequence per feature:

**Slugs:**
1. Deploy migration `0019` (adds column, backfills, adds index) — zero downtime; existing queries unaffected
2. Deploy backend binary (new `slug.go`, updated `store_community.go`, updated handlers) — new posts get slugs; old posts return UUID-derived slugs
3. Deploy frontend (link change) — both UUID and slug URLs work (backward compat)

**Phase 12 backend:**
1. Deploy migration `0020` (new sort indexes) — zero downtime
2. Deploy backend binary (sort param support, metadata validation) — existing clients unaffected since they don't send `?sort=`

**Phase 12 frontend + backend validation (must be simultaneous):**
- The enum realignment (OUTCOMES: "Rejected" → "Reject") must deploy at the same time as or after the backend validation. Ship as a single coordinated deploy.

**Phase 10 PDFs:**
1. `go mod tidy` adds `go-pdf/fpdf` to `go.mod`/`go.sum`
2. Docker build picks it up automatically (pure Go, no system deps)
3. Deploy backend — new routes only, no migration needed

**Phase 14:**
1. CSS token additions (additive, zero risk) — can ship any time
2. ModalShell + focus trap (`npm install @radix-ui/react-focus-scope`) — single PR touching all 8 modal sites

**Phase 15:**
- Single PR for all CSS surgery; manual visual regression at 320px, 375px, 768px before merge

---

## Alternatives Considered

### Sort: keyset cursor for vote-sorted results

A correct pagination cursor for `ORDER BY vote_count DESC` would encode `(vote_count, created_at, id)` as the cursor token. This prevents the "same post appearing twice" issue when a post's vote count changes between pages.

Rejected for v1.0: the "most voted" tab is a discovery surface, not a browsing surface. Users do not typically paginate to page 5 of a vote-sorted feed. The current LIMIT 50 cap and created_at-based cursor are sufficient. Revisit if pagination depth beyond 2–3 pages becomes a real use pattern.

### PDF: headless Chromium (chromedp)

Rejected per CTO ADR — 400MB Docker image bloat, 2–3s cold-start latency, not justified for plaintext PDFs on a single-container OCI VM.

### PDF: client-side generation (jsPDF in browser)

Rejected: requires sending the full cover letter text to the browser and generating PDF there. Removes the server as the source of truth for content. Increases bundle size. Does not work for the resume report (server-rendered Markdown).

### Avatar: dedicated `avatars` table

Rejected: the existing `job_tracker.files` table already supports a `kind` discriminator column. A partial unique index on `(user_id) WHERE kind='avatar'` achieves the same constraint without a new table. The simpler solution wins.

### ModalShell: per-page modal variants

Rejected: the 8 modal sites already have inconsistent ARIA attributes. A shared `ModalShell` component makes it impossible to ship a modal site without the correct accessibility attributes — correctness by construction.

---

## Open Questions

These do not block implementation but should be resolved before the relevant PR merges:

1. **PDF export credits (Phase 10 resume tweak PDF)**: Should `POST /job-tracker/ai/resume/tweaks/{id}/pdf` debit credits? The tweak content already exists in the DB (no new AI call). Recommendation: no debit (free export of already-generated content). Confirm with product owner before implementation.

2. **OUTCOMES display labels (Phase 12)**: The backend enum values `"Reject"` and `"InProgress"` are machine-readable. If shown directly in the UI, they look awkward. Should `page.tsx` maintain a `OUTCOME_DISPLAY_LABELS` map, or is renaming the enum values in the backend (`"Rejected"`, `"In Progress"`) preferable? The PM ADR implies hard-coding `"Reject"` / `"InProgress"` on both sides; confirm before touching the backend validation enum definition.

3. **`uniqueSlug` second-attempt collision retry**: The current design generates one candidate. If the candidate collides (probability ~1 in 65,536), the insert fails with unique_violation. Should `CreatePost` retry once with a new entropy draw? Recommendation: yes — add a single retry in `CreatePost` that calls `uniqueSlug` again on unique_violation. This avoids returning a 500 to the user for an extremely rare but technically possible event.

---

## Success Criteria

- `POST /job-tracker/community/reviews` returns `slug: "my-google-l5-experience"` in the response body
- `GET /job-tracker/community/posts/my-google-l5-experience` and `GET /job-tracker/community/posts/{original-uuid}` both return 200 with the same post
- `GET /job-tracker/community/reviews?sort=votes` returns posts in `vote_count DESC` order (verified by checking the first 3 rows against the DB)
- `POST /job-tracker/community/reviews` with `metadata.targetRole = "InvalidRole"` returns `400 {error: "invalid_metadata", field: "targetRole"}`
- `POST /job-tracker/ai/cover-letter/pdf` returns `Content-Type: application/pdf` and the browser downloads a readable PDF
- `POST /job-tracker/ai/resume/tweaks/{id}/pdf` returns a PDF containing the tweak's `tweaked_text` (or `user_edits` if set)
- All 8 modal sites: Tab key cycles only within the open modal; Escape closes it; focus returns to the trigger element
- `app/globals.css` breakpoint is `48rem`, not `720px`
- iOS Safari: modal does not jump when address bar appears (`8svh` top padding confirmed in responsive dev tools)
