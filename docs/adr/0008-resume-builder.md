# ADR-0008 — Resume Builder Module + LaTeX Sidecar

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-05-14 |
| Decision-maker | Shubham |
| Supersedes | — |
| Superseded-by | — |
| Related | ADR-0007 (Activity Center), ADR-0005 (deep-link via list-page modal) |

## Context

Before this work, Pegasus had two resume surfaces but no way to *author*
a resume in-app:

- **Resume AI** (`/resume`) — uploads or pastes an existing resume,
  returns an AI-scored critique. Critique-only.
- **Vault** (`/resumes`) — stores up to 5 uploaded PDFs per user.
  Storage-only.

Users without a polished resume had no path. The Resume Builder fills
that gap: a form-driven editor that compiles to an ATS-friendly,
LaTeX-rendered PDF and lands the result back in the Vault.

This ADR captures every load-bearing decision. The implementation lives
across both repos; the plan file
`~/.claude/plans/abundant-mixing-whisper.md` carries the file-level
breakdown.

## Forces

- Pegasus is a single-pod Go binary + small frontend. Operational
  simplicity is the dominant constraint.
- Users will paste arbitrary LaTeX (Overleaf templates, Jake Gutierrez,
  etc.) — the compile engine must support full TeX Live, not a curated
  subset.
- We don't have the runway to build a full LaTeX editor (autocomplete,
  syntax highlighting, real-time collaboration). MVP is "form first,
  raw LaTeX as expert escape hatch."
- ATS-friendly = real text-extractable PDFs, single column, standard
  fonts, semantic section headers. LaTeX (via pdfTeX/xeTeX) produces
  these by default.
- Existing Vault + ADR-0005 (single-page deep-link) patterns are
  reusable. Don't introduce new infra unless required.

## Decisions

### D1 — Dedicated `/resume-builder` route with its own sidebar entry

**Decision:** A standalone page with a `ProductFrame` shell, listed in
the sidebar under the "You" group (between Resume AI and Vault).
Editor state lives on the URL (`?draft=<id>`).

**Rejected:** A modal on top of Resume AI / Vault. Resume building is a
long-form, multi-session task — modals lose state on accidental
navigation, can't deep-link, and don't accommodate the split-pane
editor + preview layout.

**Why:** Same instinct as ADR-0005 — single page with `?param=` state
beats stacking new routes for every state, but the *page* itself is
worth its own route entry since it's a distinct product surface.

### D2 — Form editor as the default mode; LaTeX source as the escape hatch

**Decision:** The editor has two modes:

- **Form** — six section forms (Personal, Summary, Experience,
  Education, Skills, Projects) write into a structured `DraftContent`.
  HTML preview re-renders on every keystroke.
- **LaTeX source** — a `<textarea>` exposing the auto-generated `.tex`.
  Hand-edits write to `DraftContent.customTex`. Form fields stay
  populated; switching back to Form prompts to discard the LaTeX
  edits.

**Rejected:**

- **LaTeX-only editor** — pricey UX cost for the 90% of users who
  want forms + ATS template + done.
- **Form-only, no LaTeX escape hatch** — power users routinely need to
  paste a battle-tested template from Overleaf or tweak typography
  beyond what form options allow.

**Why:** The form is the happy path; LaTeX is the safety valve.
Bidirectional sync (LaTeX → Form parsing) is intentionally out of scope
— see D7.

### D3 — Drafts persist in a dedicated `resume_builder_drafts` table

**Decision:** Migration `0022_resume_builder_drafts.sql` creates one
row per draft with a JSONB `content` column carrying the full
`DraftContent` payload. Owned per-user, indexed by `(user_id,
updated_at DESC)` for the list rail.

**Rejected:**

- **Reuse `resume_tweaks` table** — semantically wrong; tweaks are AI
  rewrites of an existing resume, drafts are authored from scratch.
- **Local-only autosave (`localStorage`)** — loses drafts on cache
  clear or device switch.

**Why this:** Clean separation. JSONB content keeps the schema stable
across template revisions; new templates may want richer shapes but
the row layout doesn't change.

### D4 — LaTeX compilation runs in a sidecar container, not bundled into `sypher-api`

**Decision:** A separate container, **`sypher-tex`**, runs a tiny
Flask wrapper around `texlive/texlive:latest`. The Go backend POSTs
the `.tex` source to it over the internal Docker network
(`sypher-net`) and receives PDF bytes back. URL configured via
`LATEX_SERVICE_URL` env var.

**Three approaches considered before settling on this:**

1. **Tectonic baked into `sypher-api`** — what we shipped first.
   ~50 MB binary, fast startup, but ships a minimal CTAN bundle. Real-
   world resumes (anything that uses `fontawesome5`, `marvosym`, or
   `\input{glyphtounicode}`) crash with SIGABRT and zero useful
   stderr. **Killed: reliability.**
2. **Full TeX Live in the `sypher-api` image** — works but bloats
   every CI/CD push by ~1 GB. Conflates the Go app's update cadence
   with the LaTeX engine's. Re-deploys the Go binary every time we
   want a TeX Live bump. **Killed: image hygiene.**
3. **External LaTeX-as-a-service** (yotech latex.ytotech.com) — works
   but adds vendor risk, per-call network latency, and pricing tiers.
   **Killed: external dependency.**

The sidecar is the right middle: full TeX Live (~2 GB container)
pulled directly to whichever host runs `sypher-api`, never goes
through the CI pipeline. Update cadence is independent. Operationally
identical to how `sypher-postgres` already runs alongside `sypher-api`
on the OCI VM.

**Architecture:**

```
┌─────────────────────────────────────┐
│  Host (Mac dev / OCI VM)            │
│                                     │
│  Docker network: sypher-net         │
│   ┌────────────┐   ┌─────────────┐  │
│   │ sypher-api │──▶│ sypher-tex  │  │
│   │ (Go)       │   │ (texlive +  │  │
│   └─────┬──────┘   │  flask)     │  │
│         │          └─────────────┘  │
│   ┌─────▼──────────┐                │
│   │ sypher-postgres│                │
│   └────────────────┘                │
└─────────────────────────────────────┘
```

**HTTP contract** (`POST /builds/sync`):

```json
{
  "compiler": "pdflatex",
  "resources": [{ "main": true, "content": "<.tex>" }]
}
```

→ `200 application/pdf` with PDF bytes, or
→ `400 application/json` with `{ "logs": "...", "error": "..." }`.

The contract is intentionally identical to YtoTech's `latex-on-http`
so it can be swapped if we ever want a different backing engine.

**Why we wrote the wrapper ourselves:** YtoTech's published Docker
image isn't on Docker Hub and their `docker-compose.yml` requires
Redis + Postgres + Adminer just to host a Flask server. Our wrapper
is ~50 lines of Python — one container, no dependencies, multi-arch
because `texlive/texlive` is multi-arch.

### D5 — `pdflatex` as the default compiler

**Decision:** The Go backend POSTs `compiler: "pdflatex"` by default.
The sidecar accepts `pdflatex`, `xelatex`, and `lualatex`.

**Rejected:** Default to `xelatex`. It's the modern Unicode-aware
choice, but most real-world resume templates (Jake Gutierrez,
Overleaf's templates, anything that uses `\input{glyphtounicode}` or
`\pdfgentounicode=1`) are written for pdfTeX and fail under xelatex.
We can expose a compiler picker in the UI later if users need it for
specific docs.

**Why:** Compatibility with the resumes users actually paste.
pdflatex handles `fontawesome5`, `glyphtounicode`, and the entire
pdfTeX primitive surface. xelatex only matters for fontspec / system
fonts — niche for ATS-friendly resumes.

### D6 — Multi-page preview via slice-and-translate, not pixel-perfect

**Decision:** The HTML preview renders the same content tree N times
inside N page-shaped cards. Each card has explicit top/bottom margin
bands (60 px each at base scale) and a clipped content area
(PAGE_CONTENT_HEIGHT = 936 px). Page i's slice uses
`transform: translateY(-i * PAGE_CONTENT_HEIGHT)` to uncover its
own band.

**Rejected:**

- **Pixel-perfect HTML rendering of LaTeX** — would require shipping
  Latin Modern web fonts + dozens of glyph fixes + custom layout.
  Months of work for marginal value.
- **Server-side compile-on-every-keystroke** — round-trip latency
  kills the typing experience even with debounce.
- **WASM LaTeX in browser** (SwiftLaTeX et al.) — 10-20 MB bundle,
  slow first paint.

**Accepted trade-off:** small preview-to-PDF fidelity gap. Mitigated
by the **HTML preview / Compiled PDF** toggle in Form mode — one
click swaps the right pane for an iframe of the actual Tectonic-
compiled PDF so users can verify before exporting.

### D7 — Form → LaTeX is auto-synced; LaTeX → Form is *not*

**Decision:** When the editor is in Form mode and a stale `customTex`
exists from a previous LaTeX-mode session, any form edit clears
`customTex`. Switching back to LaTeX mode re-generates the `.tex`
from the current form state.

The reverse direction (LaTeX → Form) is **explicitly not supported**.
A banner in Form mode warns the user when there's a hand-edited
LaTeX fork; switching to LaTeX preserves it; the **Form** button
prompts before discarding.

**Rejected:** Parse the LaTeX back into `DraftContent` fields. Doing
this correctly requires a LaTeX parser that understands `geometry`,
`enumitem`, every macro the user might invent, and the full TeX
language. Months of work; even Overleaf doesn't try.

**Why this contract:** It's the industry standard for "structured
editor with raw escape hatch" tools. v2 could approach it via AI
(prompt DeepSeek to JSON-ify a `.tex` file) but the result is
error-prone and not in scope.

### D8 — Style options as form controls + CSS variables + LaTeX template params

**Decision:** Four user-tweakable visual options stored on
`DraftContent.style`:

- `accentColor` — hex string ("#1a1a1a" default), drives section rule
  + link color
- `sectionDivider` — `solid` | `dashed` | `none`
- `fontFamily` — `serif` | `sans`
- `headerAlignment` — `center` | `left`

The HTML preview consumes these via CSS custom properties
(`--rb-accent`, `--rb-divider-style`, etc.) set inline by
`PageInnerContent`. The LaTeX `classic-v1` template branches on the
same fields via `text/template` conditionals. Both surfaces honour
the same source of truth.

**Why this set:** These four options cover the most common "make it
look slightly different" asks without ballooning into a Canva-style
design tool. New options are cheap to add (one field, one CSS
variable, one template branch).

### D9 — Save-to-Vault reuses the existing 5-slot file system

**Decision:** "Save to Vault slot N" compiles the PDF, uploads it to
R2 via `R2.Put` (added in this work), and creates a
`job_tracker.files` row with `kind='resume'` and the chosen slot.
Replace-in-slot semantics are unchanged from the existing Vault flow.

**Why:** The Vault is already the user's "my resumes" surface. A
generated PDF is just another resume from the storage layer's
perspective — natural loop into Resume AI (which can critique your
own builder output) and into the application-tracking flows.

### D10 — Compile is on-demand, not auto-debounce-on-edit

**Decision:** Both the **Compile preview** button (in Form mode and
LaTeX mode) and the **Export → PDF** modal trigger compiles only on
explicit click. There is no auto-compile-after-debounce loop.

**Why:** Tectonic-quality compilers are fast (<5s), but auto-compile
on every edit pulse hammers the sidecar for no user-visible win —
the HTML preview already gives instant feedback in Form mode. If a
user wants the *real* PDF, they're in LaTeX mode or about to export
— a click is fine.

## Consequences

### Positive

- Users can build a resume from scratch in Pegasus, then critique it
  via Resume AI, then track applications against it — a real
  end-to-end loop instead of three disconnected tools.
- The sidecar pattern keeps `sypher-api` lean (~150 MB image, no
  LaTeX). CI/CD pipeline is unchanged; LaTeX updates are an
  out-of-band concern.
- pdflatex + full TeX Live means **anything that compiles on
  Overleaf compiles here**. Tested with the Jake Gutierrez template
  (fontawesome5, glyphtounicode, tabularx, fancyhdr).
- Form ↔ LaTeX contract is honest: forms are the source of truth
  until you fork to LaTeX, then LaTeX is. No magic, no surprises.
- Multi-page preview shows page boundaries so users see overflow
  before exporting.
- The HTML/PDF preview toggle (D6) gives users fast feedback by
  default + pixel-true verification on demand.

### Negative

- **One extra container to manage** per host (`sypher-tex`). On the
  OCI VM that's one more `docker run -d` to remember, one more
  service to monitor. Mitigated by the lifecycle being identical to
  `sypher-postgres` (already there).
- **First-time sidecar build is ~5 minutes** (texlive base layer
  is 2.25 GB). Pulled to each host once; cached forever after.
- **Preview ≠ PDF** (D6). Users who haven't clicked **Compiled PDF**
  may be surprised by typography differences at export time. The
  toggle is one click; not a hidden cliff.
- **LaTeX → Form is one-way** (D7). Power users who deeply
  customise LaTeX can't go back to the form without losing edits.
  Acceptable: they're power users, they know what they signed up
  for.
- The Resume Builder is a substantial new surface — 8+ React
  components, 8 backend handlers, 1 sidecar service. Some chance
  of "shiny new feature" syndrome where users build a draft once
  and never come back. Telemetry would help.

### Neutral

- No new infra dependencies (no Redis, no queue, no extra DB).
- No env-var sprawl: `LATEX_SERVICE_URL` is the only new one.
- ATS-friendliness is preserved end-to-end: pdflatex produces
  text-extractable PDFs by default; the `classic-v1` template
  avoids tables-for-layout and uses semantic `\section` headers.

## When to revisit

- **Compiler picker in the UI.** Trigger: a user asks "how do I make
  this xelatex" or needs `fontspec`. Add a dropdown to the LaTeX
  mode toolbar (`pdflatex` | `xelatex` | `lualatex`).
- **Multiple templates.** Trigger: enough users hit "I want a modern
  / two-column / minimalist look." Add new `.tex.tmpl` + matching
  preview CSS, dropdown selector. Today there's only `classic-v1`.
- **AI bullet rewrites.** Trigger: noticed users edit bullets often
  in real time. Add a "✨ improve" affordance per `\resumeItem`,
  routed through `ResumeTweak`-style DeepSeek prompts. Costs ~5
  credits via existing `gateAICredit`.
- **CodeMirror for the LaTeX textarea.** Trigger: users complain
  about the plain `<textarea>` for hand-edits. ~120 KB bundle add
  for syntax highlighting + bracket matching.
- **LaTeX → Form via AI.** Trigger: many users want to import a
  LaTeX template AND keep using forms. DeepSeek prompt that parses
  arbitrary `.tex` → `DraftContent` JSON. Error-prone but viable;
  separate ADR.
- **Multi-pod operation.** If we ever go horizontal, the sidecar
  becomes a shared service (one `sypher-tex` per cluster, not per
  pod). Trivial swap.

## Operational notes

The sidecar setup is documented in **`docs/sypher-tex.md`**. Local
dev (Mac), production (OCI), debugging, and bumping TeX Live
versions all live there. This ADR is the *why*; that doc is the
*how*.

## Notes for future readers

- The sidecar contract mirrors YtoTech's `latex-on-http` API
  (`POST /builds/sync` with `{compiler, resources}`). This was
  deliberate so a future migration to a managed service is a URL
  swap, not a protocol rewrite.
- The `customTex` field on `DraftContent` is the seam through which
  every "raw LaTeX" feature flows — including possible future
  features like "import an Overleaf project zip" or "tailor this
  resume to this job description via AI." Keep the field's contract
  stable: if present and non-empty, it overrides template render
  verbatim.
- ADR-0007 D1 ("misnomer route name carries debt for cosmetic
  reasons") applies here too: `/resume-builder` is unambiguous, but
  if we ever rename the sidebar item to something product-y
  ("Resume Author"?), keep the route stable.
