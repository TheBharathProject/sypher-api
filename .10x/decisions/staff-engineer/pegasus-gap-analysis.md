# ADR: Pegasus Gap Analysis — Staff Engineer Cross-Cutting Concerns

**Status:** Accepted  
**Date:** 2026-05-12  
**Slug:** pegasus-gap-analysis  
**Authors:** Staff Engineer Agent (claude-sonnet-4-6)  
**Depends on:** CTO ADR, PM ADR (same slug)

---

## Purpose

This document is the implementation guide for the SDE who builds the remaining phases. It
documents the exact patterns every new file must follow, derived from reading the live source.
It also flags correctness risks in specs and resolves design questions left open by the upstream
decisions. Do not diverge from the patterns here without an explicit reason documented in a PR
comment.

---

## 1. Backend Handler Patterns — Read These Before Writing a Line

The reference handler file is `internal/jobtracker/handlers_reminders.go`. The reference store
file is `internal/jobtracker/store_reminders.go`. Every new handler must follow these patterns
without exception.

### 1a. Handler method signature

All handlers are methods on `*Handler`. No standalone functions, no package-level state.

```go
func (h *Handler) MyNewHandler(w http.ResponseWriter, r *http.Request) {
    uid, _ := auth.UserID(r.Context())   // or auth.MustUserID when auth is required
    // ...
}
```

Use `auth.UserID` (returns zero + false if missing) for handlers that can work without a user
(public routes). Use `auth.MustUserID` for handlers inside the `requireUser` middleware group —
it panics if the user is absent, which the auth middleware prevents from ever happening in
production. Check `handlers_reminders.go` — it uses `auth.UserID` because reminders are only
reachable after `requireUser`, but `handlers_community.go` uses `auth.MustUserID` for the same
reason. Either is correct inside the authenticated group; be consistent within a single file.

### 1b. Three helper functions — use them everywhere

Defined in `handler.go`. These are not optional:

```go
// readJSON(w, r, &in) — reads body, writes 400 on failure, returns false
if !readJSON(w, r, &in) {
    return
}

// pathUUID(w, r, "id") — parses r.PathValue("id"), writes 400 on failure
id, ok := pathUUID(w, r, "id")
if !ok {
    return
}

// writeDBError(w, err) — maps pgx.ErrNoRows → 404, everything else → 500
// NEVER pass raw err.Error() to the client directly
if err != nil {
    writeDBError(w, err)
    return
}
```

The anti-pattern to avoid:

```go
// WRONG — leaks table/column names, never do this
httpx.WriteError(w, 500, "db_error", err.Error())
```

### 1c. Response format

- Success responses: `httpx.WriteJSON(w, http.StatusOK, payload)`
- Created resources: `httpx.WriteJSON(w, http.StatusCreated, payload)`
- Deletes: `w.WriteHeader(http.StatusNoContent)`
- Error: `httpx.WriteError(w, statusCode, "snake_case_code", "human message")`

The error code must be a lowercase snake_case identifier (e.g. `bad_input`, `not_found`,
`invalid_metadata`). The human message is safe to show to users — never include Go error text.

### 1d. Adding a new optional handler method

If the handler needs a dependency that isn't already on `*Handler` (e.g. a new store for a new
feature that lives outside `internal/jobtracker`), follow the `WithXxx` option-injector pattern
in `handler.go`:

```go
func (h *Handler) WithFoo(f *foo.Client) *Handler {
    h.foo = f
    return h
}
```

Check for nil at the top of any handler that uses it (defensive degradation pattern, same as
`requireAI` in `handlers_ai.go`):

```go
func (h *Handler) requireFoo(w http.ResponseWriter) bool {
    if h.foo == nil {
        httpx.WriteError(w, http.StatusServiceUnavailable, "foo_unavailable",
            "foo is not configured on this deployment")
        return false
    }
    return true
}
```

---

## 2. Backend Store Patterns — Reference: `store_reminders.go`

### 2a. Query constants

All SQL goes in `const` strings at the top of the function body. Never inline SQL in function
call arguments. Example from `store_reminders.go`:

```go
func (s *Store) CreateReminder(ctx context.Context, ...) (*Reminder, error) {
    const q = `
        INSERT INTO job_tracker.reminders (user_id, application_id, triggers_at, note)
        VALUES ($1, $2, $3, $4)
        RETURNING id, application_id, triggers_at, note, fired_at, created_at, updated_at
    `
    row := s.pool.QueryRow(ctx, q, ...)
    return scanReminder(row)
}
```

### 2b. Tenant isolation on every query

Every query that touches user data must filter by `user_id = $N`. Every table has
`ON DELETE CASCADE` so deleting a user cascades correctly, but it does not substitute for the
query-level filter. Example: `WHERE id = $1 AND user_id = $2`.

### 2c. Scanner pattern

For structs that are fetched in both single-row (QueryRow) and multi-row (Query loop) contexts,
extract a scanner helper following the `scanReminder` / `scanReminderCols` split in
`store_reminders.go`:

```go
// Accepts any value that implements Scan(…any) error — works for both
// pgx.Row and the scan closure from pgx.Rows.
func scanFoo(row interface{ Scan(...any) error }) (*Foo, error) {
    return scanFooCols(row.Scan)
}

func scanFooCols(scan func(...any) error) (*Foo, error) {
    // ...
}
```

### 2d. Empty-slice guarantee

When a list query returns zero rows, return `[]T{}` not `nil`. A nil slice marshals as `null`
in JSON, which breaks TypeScript's `items.map(...)` calls. See `store_reminders.go:61–62`:

```go
if out == nil {
    out = []Reminder{}
}
```

### 2e. Validate times in the store, not just the handler

`store_reminders.go:CreateReminder` validates `in.TriggersAt` via `time.Parse(time.RFC3339, ...)`.
This is intentional: the store is the last line of defense before the DB. Keep this pattern.

---

## 3. Validator Pattern — `validate.go`

All standalone validators live in `internal/jobtracker/validate.go`. The file currently has
`ValidateURL` and `parseEmail`. New validators go in the same file. Functions should be
package-level (not methods on Store or Handler) because they operate on values, not DB state.

Signature convention: return `error` where nil means valid, non-nil is a safe user-facing
message (no internal details). Example:

```go
// New validator for Phase 12 — add to validate.go
func validateCommunityMetadata(surface string, meta map[string]any) error {
    // ...
}
```

---

## 4. Community Metadata Validator — Design Decision

### The question

Three approaches were considered for per-surface validation in `CreateCommunityPost`:

(a) A `map[string]func(metadata map[string]any) error` lookup inside the handler
(b) A separate `validators_community.go` file with one func per surface
(c) An inline switch statement in the handler

### Recommendation: (b), with exact file placement

Create `internal/jobtracker/validators_community.go`. This is the right call for three reasons:

1. The handler (`handlers_community.go`) is already 528 lines. Adding a map or switch block
   with 5 surfaces × multiple enum checks will push it past 650 lines and obscure the request
   flow.
2. `validate.go` is for field-level validators (URL, email). Per-surface metadata validation is
   structural — it deserves its own file the same way `store_community.go` isolates community
   DB logic.
3. A separate file means each surface validator is independently testable without mocking HTTP.

### Exact pattern for `validators_community.go`

```go
package jobtracker

import (
    "encoding/json"
    "fmt"
)

// validateCommunityMetadata hard-rejects new writes with invalid enum
// values. It returns an error with a field name suitable for the
// {error: "invalid_metadata", field: "..."} response shape.
// Existing DB rows are NOT affected — validation applies only to
// CreatePost and UpdatePost.
func validateCommunityMetadata(surface string, raw json.RawMessage) *metaValidationError {
    if len(raw) == 0 {
        return nil
    }
    var meta map[string]any
    if err := json.Unmarshal(raw, &meta); err != nil {
        return &metaValidationError{field: "metadata", msg: "metadata must be a JSON object"}
    }
    switch surface {
    case "experiences":
        return validateExperiencesMeta(meta)
    case "ask":
        return validateAskMeta(meta)
    case "recruiters":
        return validateRecruitersMeta(meta)
    case "reviews":
        return validateReviewsMeta(meta)
    }
    return nil // "referrals" has no constrained enums
}

type metaValidationError struct {
    field string
    msg   string
}

func (e *metaValidationError) Error() string { return e.msg }

// --- per-surface validators ---

var validOutcomes = map[string]bool{
    "Offer": true, "Reject": true, "Ghosted": true,
    "InProgress": true, "Withdrew": true,
}
var validDifficulties = map[string]bool{
    "Easy": true, "Medium": true, "Hard": true,
}

func validateExperiencesMeta(meta map[string]any) *metaValidationError {
    if outcome, ok := meta["outcome"].(string); ok && outcome != "" {
        if !validOutcomes[outcome] {
            return &metaValidationError{
                field: "outcome",
                msg:   fmt.Sprintf("outcome must be one of: Offer, Reject, Ghosted, InProgress, Withdrew; got %q", outcome),
            }
        }
    }
    if diff, ok := meta["difficulty"].(string); ok && diff != "" {
        if !validDifficulties[diff] {
            return &metaValidationError{
                field: "difficulty",
                msg:   fmt.Sprintf("difficulty must be one of: Easy, Medium, Hard; got %q", diff),
            }
        }
    }
    return nil
}

// ... validateAskMeta, validateRecruitersMeta, validateReviewsMeta follow
// the same pattern: extract field, check against a package-level map,
// return *metaValidationError with field name + safe message.
```

### Call site in `handlers_community.go:CreateCommunityPost`

Insert after the body-length check and before `in.Surface = surface`:

```go
if verr := validateCommunityMetadata(surface, in.Metadata); verr != nil {
    httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{
        "error":   "invalid_metadata",
        "field":   verr.field,
        "message": verr.msg,
    })
    return
}
```

Note: the response uses `map[string]string` with three keys to match the PM's acceptance
criteria (`{error: "invalid_metadata", field: "outcome"}`). This diverges from the standard
two-key `httpx.WriteError` shape, so write it explicitly as shown.

The same call goes into `UpdateCommunityPost` immediately after the title check.

---

## 5. Slug Generation — Correctness Issues

### 5a. Collision retry strategy — one retry is not enough

The spec proposes: try base slug → if taken, append 4-hex suffix, return. There is no second
retry. The problem:

- 4 hex characters = 2 random bytes = 65,536 possible suffixes.
- At 65,536 posts with the same title (unlikely), every suffix is taken.
- More practically: two concurrent requests with the same title both see the base slug as
  available, both attempt to insert it, one gets a unique-constraint violation.

**Fix:** `uniqueSlug` must loop with retry, not bail after one attempt. Use a `crypto/rand`
suffix and catch the unique constraint violation from the INSERT:

```go
// slug.go — corrected uniqueSlug
import (
    "crypto/rand"
    "encoding/hex"
    "fmt"
    "context"
)

func (s *Store) uniqueSlug(ctx context.Context, title string) (string, error) {
    base := slugify(title)
    if base == "" {
        base = "post"
    }
    // First try: base slug with no suffix
    candidate := base
    for attempt := 0; attempt < 5; attempt++ {
        var exists bool
        err := s.pool.QueryRow(ctx,
            `SELECT EXISTS(SELECT 1 FROM job_tracker.community_posts WHERE slug = $1)`,
            candidate,
        ).Scan(&exists)
        if err != nil {
            return "", fmt.Errorf("slug availability check: %w", err)
        }
        if !exists {
            return candidate, nil
        }
        // Collision — regenerate suffix for next loop
        b := make([]byte, 2)
        if _, err := rand.Read(b); err != nil {
            return "", err
        }
        // Truncate base so base+"-"+4hex <= 85 chars total
        trimmed := base
        if len(base) > 80 {
            trimmed = base[:80]
        }
        candidate = trimmed + "-" + hex.EncodeToString(b)
    }
    return "", fmt.Errorf("slug: exhausted retries for base %q", base)
}
```

Five retries is paranoid given the collision probability (~1/65,536 per attempt), but it handles
the race condition where two concurrent inserts with the same title are in flight. The
`community_posts_slug_idx` unique index is the final arbiter — if the insert fails with a
unique violation, the caller should retry at the HTTP level (this case is so rare that returning
a 500 and having the client retry is acceptable).

Note: use `crypto/rand`, not `math/rand`. The spec uses `rand.Read` without specifying which
package — always use `crypto/rand` for anything that produces URLs.

### 5b. Backfill slug — use the spec's formula unchanged

The spec's backfill: `'post-' || REPLACE(id::text, '-', '')` produces a 37-character string
(`post-` + 32 hex chars). This is correct. Do not use `id::text` directly because that produces
a UUID with hyphens (`xxxxxxxx-xxxx-...`), which is valid in a slug but ugly and inconsistent
with the spec's intent to produce a safe path-segment. The `REPLACE` call removes hyphens so
the backfill slug is all-alphanumeric after the `post-` prefix. Keep the spec's formula.

One nuance: the migration must run `UPDATE ... WHERE slug IS NULL` before the `NOT NULL`
constraint, and before the unique index. The spec already has this order correct — do not reorder.

### 5c. Unicode titles — the spec's `slugify` is wrong for non-ASCII

The spec defines `slugify` as: lowercase, spaces/underscores to hyphens, strip chars not
matching `[a-z0-9-]`. This will convert a Hindi or Tamil job title (e.g. "गूगल SDE-2 इंटरव्यू")
entirely to empty string, which then falls through to the `"post"` default, producing a
non-descriptive slug like `post-a3f2`.

This is acceptable for v1.0 given the primary user is Indian job-seekers writing job titles in
English. However, the failure mode is silent (the post is created with a UUID-style fallback
slug rather than erroring), so document it.

**Recommended strategy for slugify:**

```go
func slugify(title string) string {
    // Step 1: Unicode-normalize to NFC (handles composed vs decomposed forms)
    // golang.org/x/text/unicode/norm is already available via x/text (indirect dep)
    // For now: use strings.ToLower on ASCII range only
    s := strings.ToLower(title)
    // Step 2: replace spaces, underscores, non-breaking spaces with hyphens
    s = strings.NewReplacer(" ", "-", "_", "-", " ", "-").Replace(s)
    // Step 3: remove any byte that isn't [a-z0-9-]
    var b strings.Builder
    for _, r := range s {
        if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
            b.WriteRune(r)
        }
        // Non-ASCII runes (Hindi, Tamil, etc.) are silently dropped.
        // The uniqueSlug caller will fall back to "post-{hex}" if result is empty.
    }
    // Step 4: collapse consecutive hyphens
    result := regexp.MustCompile(`-{2,}`).ReplaceAllString(b.String(), "-")
    // Step 5: trim leading/trailing hyphens
    result = strings.Trim(result, "-")
    // Step 6: truncate at 80 chars, back up to last hyphen boundary
    if len(result) > 80 {
        result = result[:80]
        if i := strings.LastIndexByte(result, '-'); i > 40 {
            result = result[:i]
        }
    }
    return result
}
```

The `regexp.MustCompile` call should be a package-level `var` (not inside the function) to
avoid recompiling on every call:

```go
var consecutiveHyphens = regexp.MustCompile(`-{2,}`)
```

If the product later needs real Hindi/Tamil slug generation, add `golang.org/x/text/transform`
and `golang.org/x/text/unicode/norm` — both are already indirect dependencies via the existing
`golang.org/x/text` transitive import in `go.mod`. That is a future improvement, not a v1.0
requirement.

---

## 6. PDF Endpoints — Design and Reuse Analysis

### 6a. Can PDF handlers call existing store/AI methods directly?

Yes. The cover letter PDF endpoint does not re-run AI generation. Looking at `handlers_ai.go`,
`GenerateCoverLetter` returns the text via `httpx.WriteJSON` and does not persist the cover
letter text. The PDF endpoint must therefore either:

(a) Accept the cover-letter text directly in the POST body (client already has it from the
    first call), or
(b) Re-generate the cover letter (burns credits again — unacceptable).

**Option (a) is correct.** The client calls `POST /ai/cover-letter` to get the text, then calls
`POST /ai/cover-letter/pdf` with the text in the body to get the PDF. No AI call happens in the
PDF endpoint; it only does layout and streaming. No new store methods are needed.

For `POST /ai/resume/tweaks/{id}/pdf`: the tweak text is persisted in `job_tracker.resume_tweaks`.
The PDF handler calls `h.store.GetResumeTweak(ctx, uid, id)` (already exists in
`store_resume_tweaks.go`) to fetch `tweakedText` (and `userEdits` if the user has edited it,
preferring `userEdits` when non-empty). No new store methods needed.

For resume report PDF: the report is persisted. Use `h.store.LatestReport(ctx, uid)` (already
exists in the store). No new store methods needed.

### 6b. Separate handler file vs adding to existing

**Use a separate file: `internal/jobtracker/handlers_ai_pdf.go`.**

`handlers_ai.go` is already 376 lines. Adding three PDF handlers inline would bring it to ~550+
lines and mix two concerns: "run AI and return text" vs "fetch stored text and render PDF". The
PDF handlers have a distinct import requirement (`fpdf` package) that should not pollute the
AI handler imports. One file per concern.

### 6c. Streaming pattern

`go-pdf/fpdf`'s `Output` method writes to an `io.Writer`. `http.ResponseWriter` implements
`io.Writer`. The pattern:

```go
// handlers_ai_pdf.go

import (
    "net/http"
    "fmt"

    "github.com/go-pdf/fpdf"
    "github.com/TheBharathProject/sypher-api/internal/auth"
    "github.com/TheBharathProject/sypher-api/internal/httpx"
)

type coverLetterPDFInput struct {
    Text    string `json:"text"`    // the cover letter text from the prior AI call
    Company string `json:"company"` // for the filename
    Role    string `json:"role"`    // for the filename
}

// POST /job-tracker/ai/cover-letter/pdf
func (h *Handler) CoverLetterPDF(w http.ResponseWriter, r *http.Request) {
    uid := auth.MustUserID(r.Context())
    _ = uid // used for auth only; no store call needed

    var in coverLetterPDFInput
    if !readJSON(w, r, &in) {
        return
    }
    if in.Text == "" {
        httpx.WriteError(w, http.StatusBadRequest, "bad_input", "text is required")
        return
    }

    pdf := buildCoverLetterPDF(in.Text, in.Company, in.Role)

    filename := sanitizeFilename(fmt.Sprintf("%s-%s-cover-letter.pdf", in.Company, in.Role))
    w.Header().Set("Content-Type", "application/pdf")
    w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
    // fpdf.Output writes directly to the ResponseWriter.
    // On error after headers are sent, there is nothing useful we can do
    // except log it — the response is already committed.
    if err := pdf.Output(w); err != nil {
        h.logger.Error("pdf output", "err", err)
    }
}

func buildCoverLetterPDF(text, company, role string) *fpdf.Fpdf {
    f := fpdf.New("P", "mm", "A4", "")
    f.AddPage()
    f.SetFont("Helvetica", "B", 14)
    if company != "" && role != "" {
        f.Cell(0, 10, fmt.Sprintf("%s — %s", company, role))
        f.Ln(8)
    }
    f.SetFont("Helvetica", "", 11)
    f.SetLeftMargin(20)
    f.SetRightMargin(20)
    // MultiCell wraps long lines automatically.
    f.MultiCell(0, 6, text, "", "L", false)
    return f
}
```

**Important:** Set `Content-Type` and `Content-Disposition` headers BEFORE calling
`pdf.Output(w)`. Once `Output` starts writing, Go's `http.ResponseWriter` auto-sends the 200
status header and you cannot set headers afterward. If `buildCoverLetterPDF` returns an error
(malformed input), handle it before writing any headers.

For `TweakPDF` (`GET /ai/resume/tweaks/{id}/pdf` or `POST`): fetch the row via
`h.store.GetResumeTweak`, prefer `row.UserEdits` if non-empty, else `row.TweakedText`. Build
the PDF with title as the header. Filename: `{title}-tweak.pdf` (sanitized).

### 6d. Filename sanitization

The PM spec requires filenames like `{company}-{role}-cover-letter.pdf`. User-provided strings
can contain slashes, quotes, and non-ASCII characters. Add a small sanitizer to `handlers_ai_pdf.go`:

```go
var unsafeFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9._\-]`)

func sanitizeFilename(s string) string {
    s = unsafeFilenameChars.ReplaceAllString(s, "-")
    s = strings.Trim(s, "-")
    if len(s) > 200 {
        s = s[:200]
    }
    if s == "" {
        s = "download"
    }
    return s
}
```

### 6e. Credit gating

**No credit gate on PDF endpoints.** The credit was already spent when the user generated the
text via the AI endpoint. The PDF is a formatting operation, not an AI call. Do not call
`gateAICredit` in PDF handlers.

### 6f. Add to go.mod

```bash
go get github.com/go-pdf/fpdf@latest
```

Verify the import path is `github.com/go-pdf/fpdf` (the maintained fork), not
`github.com/jung-kurt/gofpdf` (the archived original). After `go get`, run `go mod tidy`.

---

## 7. Frontend Patterns — Read Before Touching any Frontend File

### 7a. All `Api*` types go in `lib/api-client.ts`

Every new TypeScript type that represents an API response shape must be added to
`lib/api-client.ts`. Do not define types inline in page files or component files. The file
currently ends at line ~400 with `ApiApplicationPage`. Add new types at the bottom.

For Phase 12 (slugs): add `slug: string` to `ApiCommunityPost`:

```typescript
export type ApiCommunityPost = {
  id: string;
  userId: string;
  authorName: string;
  authorSlug?: string;
  surface: "reviews" | "experiences" | "referrals" | "ask" | "recruiters";
  title: string;
  body?: string;
  metadata: Record<string, unknown>;
  slug: string;          // add this field
  isPublic: boolean;
  voteCount: number;
  commentCount: number;
  status: "active" | "removed" | "flagged";
  myVote: -1 | 0 | 1;
  createdAt: string;
  updatedAt: string;
};
```

For Phase 10 (avatar): `ApiFile` currently has `kind: "resume" | "cover_letter"`. Widen it:

```typescript
kind: "resume" | "cover_letter" | "avatar";
```

### 7b. New page template — follow `app/recruiters/page.tsx`

The recruiter page is the most recently written page and sets the template. Key elements:

1. Top of file: `"use client"` directive
2. Imports: `useEffect`, `useState` only (no third-party hooks)
3. Auth guard at the top of `useEffect`: `if (!isAuthed()) { goTo("/login"); return; }`
4. State: `items`, `loading`, `error`, `showModal`, `editingId`, `draft`, `busy`, `formError`
5. `refresh()` is an async function called in `useEffect` and after mutations
6. `filtered` is a `useMemo` or derived variable, not state
7. Modals: local boolean `showModal`, not a separate component file
8. API calls: `api.get`, `api.post`, `api.patch`, `api.delete` directly — no wrapper layer

### 7c. CSS token naming convention from `app/globals.css:1-52`

Existing tokens follow this naming convention:

```
--bg            base background
--bg-soft       slightly elevated background
--bg-elevated   card/panel background
--bg-deep       deepest background (sidebar headers)
--bg-input      input field background
--text          primary text
--text-soft     secondary/muted text
--text-faint    very muted (disabled, placeholder)
--border        default border
--border-strong stronger border
--accent        primary accent color
--accent-ink    text on accent backgrounds
--surface       alias for --bg
--surface-2     alias for --bg-elevated
--chip          tag/chip background
--radius-xl     20px
--radius-lg     12px
--radius-md     8px
--shadow        standard box shadow
--stage-{name}  per-stage color (interest, screen, applied, offer, offer-ink)
```

New tokens for Phase 14 must follow the same `--{category}-{qualifier}` pattern:

```css
/* Add to :root block in app/globals.css */
--bg-hover: #2a2a2a;           /* hover state for interactive elements */
--accent-soft: #3a3a3a;        /* softer accent for backgrounds */
--font-mono: ui-monospace, SFMono-Regular, "SF Mono", Consolas, monospace;
--radius-sm: 4px;
--radius-2xl: 28px;

/* Stage color tokens — extend the existing 3-token set to all 7 stages */
--stage-interested: #efe7d2;   /* existing --stage-interest renamed */
--stage-applied: #1f2e27;      /* existing */
--stage-phone: #232648;        /* same as --stage-screen, new name */
--stage-technical: #1a1f3a;    /* new */
--stage-onsite: #2a1a3a;       /* new */
--stage-offer: #d9f7e0;        /* existing */
--stage-rejected: #3a1a1a;     /* new */
```

Add light-theme mirrors in the `[data-theme="light"]` block that starts at line 57. Match
the existing pattern: stage tints keep the same values in both themes (the CTO ADR notes
"Stage tints don't flip; they're accent colors that read on both backgrounds").

### 7d. Inline edit pattern from `app/notes/page.tsx`

The notes page (lines 850–890) demonstrates the inline category rename pattern. Use it for any
future inline edit:

```
state: [renamingId, setRenamingId] + [renamingName, setRenamingName]
trigger: onDoubleClick on the display element → setRenamingId(id); setRenamingName(name)
render: conditional — if (renamingId === item.id) show <input> else show <span>
submit: <form onSubmit={saveRename}> wrapping the input
cancel: onKeyDown Escape → setRenamingId(null)
save: api.patch + clear state + refresh
```

Do not replicate this pattern for any new feature in Phases 10/12/14/15 — it is already
correctly implemented in notes. This is here as reference for any future work.

### 7e. Modal pattern — all 8 sites must be consistent

Eight modal sites currently exist:
1. `app/applications/page.tsx` — Add application, Edit application, View application, Set
   reminder, Cover letter (5 modals in one file)
2. `app/recruiters/page.tsx` — Add/edit recruiter
3. `app/settings/page.tsx` — Delete account, Cancel subscription
4. `app/community/[section]/page.tsx` — ExperienceModal, ReviewModal, etc.

When Phase 14 lands, every modal must have:
- `role="dialog"` and `aria-modal="true"` on the backdrop div
- `aria-labelledby="modal-title"` pointing to the modal's `<h2 id="modal-title">`
- `<FocusScope trapped loop>` wrapping the modal content
- `inert` set on `<main>` when modal is open
- A close button that restores focus to the trigger (see §8 below)

The applications page already has `role="dialog"` and `aria-modal="true"` on several modals
(CTO ADR confirms lines 805–807, 959–961, etc.). Check before adding duplicates.

---

## 8. `@radix-ui/react-focus-scope` Integration

### Install

```bash
# from the job-tracker/ root
pnpm add @radix-ui/react-focus-scope
```

This is the first runtime dependency added to `package.json` beyond `next`, `react`, `react-dom`.
It will add one entry to `package.json` and transitive entries to `pnpm-lock.yaml`. Run
`pnpm tsc --noEmit` after install to confirm no type errors.

### Usage pattern in each modal

```tsx
import { FocusScope } from "@radix-ui/react-focus-scope";

// Inside the modal render:
<div
  role="dialog"
  aria-modal="true"
  aria-labelledby="modal-title"
  className="modal-backdrop"
>
  <FocusScope trapped loop>
    <div className="modal-panel">
      <h2 id="modal-title">Add Application</h2>
      {/* modal content */}
      <button
        type="button"
        autoFocus           {/* focus lands here on open */}
        aria-label="Close"
        onClick={closeModal}
        ref={closeButtonRef}
      >
        ✕
      </button>
    </div>
  </FocusScope>
</div>
```

`trapped` prevents Tab from leaving the modal. `loop` makes Tab wrap from the last focusable
element back to the first. `autoFocus` on the close button ensures keyboard users land somewhere
useful immediately on open (the close button, not the first form field, to avoid accidental
submission).

### Focus restoration on close

```tsx
const triggerRef = useRef<HTMLButtonElement>(null);

const openModal = () => {
  setShowModal(true);
  // triggerRef.current is the button the user clicked to open the modal.
  // We'll return focus to it on close.
};

const closeModal = () => {
  setShowModal(false);
  // Use a microtask-delayed focus restore to avoid fighting with React's
  // state update batching and the FocusScope unmount.
  requestAnimationFrame(() => {
    triggerRef.current?.focus();
  });
};

// The trigger button:
<button ref={triggerRef} onClick={openModal}>Add Application</button>
```

### Highest-risk modal sites

Ranked by how many users encounter them per session and how much keyboard breakage would hurt:

1. **Add/Edit application modal** (`app/applications/page.tsx`) — touched on every new
   application entry. Highest-frequency modal in the app.
2. **Cover letter modal** (`app/applications/page.tsx`) — AI output modal, user reads output;
   keyboard-trap failure would strand screen reader users inside it.
3. **Cancel subscription modal** (`app/settings/page.tsx`) — destructive action, must be
   dismissable cleanly.
4. All remaining modals at equal lower priority.

### `inert` attribute on `<main>`

```tsx
// In the page component, find the <main> element (or the wrapping div
// inside ProductFrame) and toggle inert:

<main inert={showModal ? true : undefined}>
  {/* page content */}
</main>
```

`inert={undefined}` removes the attribute from the DOM entirely (React handles this correctly).
`inert={true}` or `inert=""` both set the attribute. TypeScript may flag `inert` as not in the
JSX types — add `inert?: boolean | ""` to the HTMLAttributes extension if needed, or cast with
`{...({ inert: showModal ? "" : undefined } as any)}`.

---

## 9. Tech Debt — What NOT to Touch

### Do not change

- `internal/billing/costs.go` — PDF endpoints must NOT define new credit constants. PDF is
  free (see §6e above). Do not add `CostCoverLetterPDF` or any such constant.
- `internal/jobtracker/types.go` enum validators (`isValidStage`, `isValidSource`) — these are
  correct. Community metadata enum validation is separate (see §4).
- `app/globals.css` breakpoint values — the `720px` to `768px` fix is a Phase 15 item. Do not
  touch breakpoints in Phases 12/14 work. A partial fix that changes some but not all instances
  creates a worse inconsistency than the current uniform 720px.
- `lib/billing.ts:CREDIT_COSTS` — mirrors `costs.go`. Any change to cost constants in Go must
  be manually synced to this file. There is no code generation. The two files must stay in sync.
- `internal/jobtracker/store_community.go:ListPosts` hardcoded `ORDER BY p.created_at DESC` —
  the sort extension is a Phase 12 item. Do not touch this query for slug work.

---

## 10. Unit Tests — Minimum Required for This Cycle

The `internal/jobtracker` package has exactly one test file (`mentions_test.go`). The CTO ADR
flags this as the highest-risk tech debt gap. Two new functions added in this cycle MUST have
unit tests before merge. These are pure functions (no DB, no HTTP) so they are trivial to test.

### `internal/jobtracker/slug_test.go`

```go
package jobtracker

import "testing"

func TestSlugify(t *testing.T) {
    cases := []struct{ in, want string }{
        {"My Google L5 Interview Went!", "my-google-l5-interview-went"},
        {"How to crack SDE-2 at Amazon", "how-to-crack-sde-2-at-amazon"},
        {"!!!", ""},                        // all special chars → empty
        {"  leading spaces  ", "leading-spaces"},
        {"a--b---c", "a-b-c"},             // consecutive hyphens collapsed
        {strings.Repeat("x", 100), strings.Repeat("x", 80)}, // truncate
        // Unicode: silently dropped, no panic
        {"गूगल interview experience", "interview-experience"},
    }
    for _, c := range cases {
        if got := slugify(c.in); got != c.want {
            t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
        }
    }
}
```

### `internal/jobtracker/validators_community_test.go`

```go
package jobtracker

import (
    "encoding/json"
    "testing"
)

func TestValidateCommunityMetadata(t *testing.T) {
    cases := []struct {
        name    string
        surface string
        meta    string
        wantErr bool
        field   string // expected error field when wantErr=true
    }{
        {"experiences valid outcome", "experiences",
            `{"outcome":"Offer"}`, false, ""},
        {"experiences invalid outcome", "experiences",
            `{"outcome":"Rejected"}`, true, "outcome"},
        {"experiences valid difficulty", "experiences",
            `{"difficulty":"Hard"}`, false, ""},
        {"experiences invalid difficulty", "experiences",
            `{"difficulty":"Extreme"}`, true, "difficulty"},
        {"ask tags count=3 ok", "ask",
            `{"tags":["a","b","c"]}`, false, ""},
        {"ask tags count=4 fail", "ask",
            `{"tags":["a","b","c","d"]}`, true, "tags"},
        {"referrals no validation", "referrals",
            `{"anything":"goes"}`, false, ""},
        {"empty metadata ok", "experiences",
            `{}`, false, ""},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            err := validateCommunityMetadata(c.surface, json.RawMessage(c.meta))
            if (err != nil) != c.wantErr {
                t.Fatalf("wantErr=%v got err=%v", c.wantErr, err)
            }
            if err != nil && err.field != c.field {
                t.Errorf("want field=%q got %q", c.field, err.field)
            }
        })
    }
}
```

These two test files are not optional. They are the minimum new coverage this cycle must produce.

---

## 11. Adoption Path for New Patterns

### `validators_community.go` (new file this cycle)

This is a new pattern (per-surface metadata validators in a standalone file). If community
surfaces are added in the future, each new surface's validator goes in the same file. The
pattern is: add a `validate{Surface}Meta` function + register it in the switch in
`validateCommunityMetadata`. Do not scatter validators across handler files.

### `handlers_ai_pdf.go` (new file this cycle)

Future PDF endpoints (resume report PDF, avatar export, etc.) go in this file. The pattern is:
`build{Content}PDF` returns `*fpdf.Fpdf`, the handler sets headers and calls `pdf.Output(w)`.
No streaming buffer, no temp files.

### `@radix-ui/react-focus-scope` (new dependency this cycle)

Apply `<FocusScope trapped loop>` to exactly the 8 modal sites listed in §7e. Do not apply it
to non-modal overlays (dropdowns, tooltips). Do not install additional Radix packages alongside
this one — the zero-dependency principle is still in force; this is a one-time exception.

---

## 12. Open Questions

- **`UpdateCommunityPost` enum validation scope:** Should updating a post re-validate metadata
  even for fields the user did not change? The safest answer is yes (full validation on every
  write). But if the DB already has a row with `"outcome": "Rejected"` (old enum value), then
  a user editing only the title would get a 400 because the stored metadata fails the new
  validator. Recommendation: validate only fields explicitly provided in the PATCH body, not the
  full stored metadata. This requires the handler to unmarshal and diff the incoming metadata
  against the stored row — slightly more complex but correct. Flag this in the PR for
  confirmation before merging.

- **Sort query for `most-reviewed` / `least-reviewed`:** The store's `ListPosts` uses a dynamic
  query string (`q += " ORDER BY ..."`). The `?sort=` parameter needs to be sanitized before
  being interpolated into the SQL to prevent SQL injection. Use an allowlist map approach:
  ```go
  var allowedSortClauses = map[string]string{
      "newest":        "p.created_at DESC",
      "votes":         "p.vote_count DESC, p.created_at DESC",
      "most-reviewed": "p.comment_count DESC, p.created_at DESC",
      "least-reviewed": "p.comment_count ASC, p.created_at DESC",
  }
  ```
  Look up the sort clause from this map; if absent, default to `newest`. Never interpolate the
  raw query param into SQL.

- **`inert` TypeScript type gap:** `inert` is a valid HTML attribute but may not be in React's
  JSX types in the project's TypeScript version. Confirm with `pnpm tsc --noEmit` after adding
  it; if it errors, the cast pattern in §8 is the fix.

---

## Success Criteria (Verifiable)

- `go test ./internal/jobtracker/...` passes with `slug_test.go` and
  `validators_community_test.go` present and green.
- `pnpm tsc --noEmit` emits zero errors after adding `slug: string` to `ApiCommunityPost`.
- `POST /ai/cover-letter/pdf` returns `Content-Type: application/pdf` with a non-empty body and
  does not charge credits.
- `POST /community/experiences` with `{"outcome":"Rejected"}` returns `400 invalid_metadata`.
- `POST /community/experiences` with `{"outcome":"Reject"}` succeeds.
- Keyboard tab on any modal cycles only within the modal and does not reach background content.
- `go build ./...` produces no output (zero compilation errors).
