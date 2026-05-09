# ADR-0005 — Per-application deep-link via list-page modal

**Status:** Accepted
**Date:** 2026-05-09

## Context

The Pegasus job extractor extension (Phase 5) shows a duplicate banner
when the user lands on a job they've already saved: *"Already in your
tracker — open it →"*. Until Phase 6a the link landed on `/applications`
(the list) because no per-row detail route existed. The user explicitly
did **not** want a separate detail page in the navigation — the row
context (stage, notes, timeline, AI tools) already exists in the list
page's viewing modal, and duplicating that JSX as a real detail page
would mean two state machines drifting apart.

The list page (`app/applications/page.tsx`) already has a fully
featured viewing-modal flow: `viewingId` / `openView()` / `closeView()`
+ a JSX block that renders timeline, AI cover letter / resume tweak
dialogs, edit + delete affordances.

App-Router precedent in this codebase: `app/auth/callback/page.tsx`
uses `useSearchParams()` inside a `<Suspense>` wrapper.

## Decisions

### D1 — `app/applications/[id]/page.tsx` is a thin redirector

The dynamic route renders nothing of its own. On mount it calls
`router.replace('/applications?view=<id>')`. The user's URL stays clean
once the modal closes, and the heavy work (modal + timeline) reuses
the existing `/applications` page.

**Rejected alternatives:**

- **Duplicate the modal JSX into a real detail page.** Two state
  machines drift; bug fixes to the viewing UI would have to be made
  twice.
- **Intercepting routes (`@modal/[id]`).** Next.js 14 supports them
  but the codebase doesn't use that pattern anywhere; introducing it
  for one screen is cost without benefit.

### D2 — `/applications` reads `?view=<id>` on mount and opens the modal

A `useEffect` watches both `searchParams.get('view')` and `items.length`.
When both exist and the id matches a loaded row, the existing
`openView()` is called so the timeline fetch + state setting happens
exactly as it does for a row click. If items haven't loaded yet, the
effect re-runs once they arrive.

`useSearchParams()` requires a `<Suspense>` boundary at build time
(static prerender bails out otherwise). The page now wraps its body in
`<Suspense fallback={null}><ApplicationsInner /></Suspense>`, matching
`app/auth/callback/page.tsx`.

### D3 — `closeView()` strips the `?view=` param

`closeView()` calls `router.replace('/applications', { scroll: false })`
when `viewParam` is non-null. Browser back from the open modal also
closes it (the URL changed when it opened, so going back closes; if you
arrived via a deep-link, the modal closes and the list is what you see).

### D4 — Bogus id gets a transient toast, not a 404

If `?view=<id>` is present but no matching row exists after items
finish loading, a soft toast `"Application not found"` appears for
~2.4 seconds. The page itself renders normally — the user can keep
using it. This handles ids that are revoked, deleted, or simply
typo'd.

### D5 — Extension banner deep-links to the row

`sypher-job-extractor/popup/popup.js::applyDuplicateNotice` builds
`appUrl(`/applications/${duplicate.applicationId}`)` when the
check-link response includes an `applicationId`. The redirector route
takes care of the modal handoff. The saved-state link still goes to
the list (we don't have the new application's id post-create call).

## Consequences

**Positive:**

- One JSX tree for the modal — bug fixes / additions propagate everywhere.
- URL is shareable: copy the URL while the modal is open and a
  colleague gets the same view.
- Browser back-button closes the modal, matching native expectations.
- Adding more deep-link sources (notifications, emails, community
  cross-references) is now trivial — they all point at
  `/applications/<id>`.

**Negative:**

- Two URLs resolve to effectively the same UX (`/applications?view=X`
  and `/applications/X`). Search engines don't see the app (private,
  auth-gated) so canonicalisation isn't an issue. The redirector file
  has a top-of-file comment explaining this.
- The `<Suspense>` boundary is technically a render boundary; if the
  page ever needs server-side data fetching mixed with the search
  params, the boundary placement may need to move. Not a concern for
  the current pure-client page.
