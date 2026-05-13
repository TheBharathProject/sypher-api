# ADR-0007 — Activity / History center on `/notifications`

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-05-13 |
| Decision-maker | Shubham |
| Supersedes | — |
| Superseded-by | — |
| Related | ADR-0001 (notifications), ADR-0005 (deep-link via list-page modal) |

## Context

Pegasus stores plenty of per-user history server-side — every resume
report ever generated (`job_tracker.ai_reports`), every reminder
(`reminders`), every stage transition (`application_stage_history`),
every AI call (`ai_usage`), every credit movement
(`billing.credit_transactions`). The frontend exposes almost none of
it as *history*. Today:

- Resume wizard only renders the **latest** report; old scores are
  unreachable.
- Reminders can only be **created** (from a row icon on
  `/applications`); listed reminders, fired reminders, edits, and
  cross-application views don't exist in the UI.
- Stage timelines exist per-application, hidden inside the row drawer.
- AI usage shows only a monthly aggregate; credit transactions are
  surfaced via a settings collapsible but truncated.

A user with three months of activity has no path to "see what I did" —
the data exists, the UI doesn't. The user's pitch: a single tabbed
panel on the existing `/notifications` route, lazy-loaded per tab,
with reminders editable inline so the panel is both archive and
management surface.

This ADR captures the boundaries and trade-offs. The implementation
plan lives at `~/.claude/plans/abundant-mixing-whisper.md`.

## Forces

- **Single-pod Go binary, no Redis / queue / event bus** (ADR-001
  context). New abstractions must pay for themselves within this PR.
- **`/notifications` is already mature**: cursor pagination, mark-read
  flows, a 60-second sidebar count poll, dedup index for cron rows. We
  can lean on it.
- **The user named two domains by example** (resume reports,
  reminders) and gestured at "etc." Scope creep is the dominant risk.
- **Reminder modal already exists** as inline JSX in
  `app/applications/page.tsx` (lines ~1400–1490). Inline modals tend
  to grow features; the second they're consumed from two pages, drift
  begins.
- **Polling thrash budget is finite**: sidebar already polls
  `/notifications/count` every 60s. Auto-refreshing per-tab loops
  multiply that for no validated user signal.
- **Data layer is healthy**: every domain we want to surface is
  already append-only or has `(created_at, id)` indexes, so no schema
  work is required for v1. This is a windfall — most history-UI
  efforts get blocked behind a backfill.

## Decisions

Each decision below carries the strongest counterargument I could
muster against it. The justifications stand only because those
counterarguments are addressed, not waved away.

---

### D1 — One tabbed shell on `/notifications`; route unchanged

**Decision:** Convert `/notifications` into a tabbed shell. Visible
page header reads **"Activity"**; tabs are
`Notifications | Resume reports | Reminders`. Tab state is reflected
in `?tab=…` so a copied URL preserves context.

**Strongest counterargument:** *The URL is now a lie.* `/notifications`
points to a page called "Activity". Every developer who joins later
will hit this dissonance. Search engines aren't an issue (private,
auth-gated) but cognitive friction for maintainers is real, and we're
codifying a misnomer in tens of inbound links (email templates,
extension banners, deep-links from ADR-0005).

**Why I accept it anyway:**

- Renaming the route requires server-side redirects (`/notifications →
  /activity`), updates to every email template (`MAIL_FROM_*` plus the
  HTML/text bodies in `internal/mailer/templates.go`), updates to the
  extension's `popup.js`, and a release-window where old bookmarks
  still resolve. That's three repos and a feature flag, not a
  refactor.
- The user explicitly said *"in the notification section only with
  different labels"* — they're indifferent to the URL.
- The misnomer cost is paid by maintainers (low-frequency reader).
  The redirect cost is paid by every user with a bookmark (one-time
  user-facing churn). The latter is worse per-event.

**Mitigation:** A top-of-file comment in `app/notifications/page.tsx`
documenting the misnomer + this ADR reference. A `// TODO(activity-rename)`
breadcrumb in `internal/jobtracker/routes.go` next to the notifications
route group. When we hit the "more than 5 tabs" mark, revisit the
rename (D2's revisit trigger covers this).

---

### D2 — v1 ships exactly three tabs; defer the rest by name

**Decision:** Tabs in v1 — **Notifications**, **Resume reports**,
**Reminders**. Deferred and explicitly named so they don't reappear
as scope creep:

- AI usage history (needs new `GET /ai/usage/history`)
- Credit transactions ledger (needs new `GET /billing/transactions`)
- Cover-letter generation history (currently ephemeral — needs
  persistence first)
- Application stage timelines as a cross-app feed (today is per-app)
- Generic chronological "all activity" feed (would need new
  `activity_log` table + handler instrumentation)
- Recruiters CRM audit log (no audit table — state-only today)

**Strongest counterargument:** *Sample size of one.* The user named two
domains, but the validation that "resume + reminders" is what
*Pegasus's users at large* want is anecdotal. We may ship 3 tabs and
discover the first feedback is "where's my AI usage?". Then we
delivered shallow value behind a Big Feature Header.

**Why I accept it anyway:**

- The three tabs are not the *only* signal — they also match the two
  domains where (a) history is *visible* to the user (a score they
  remember; a reminder they set) and (b) there's a clear *action* to
  take (re-read a past report; cancel/edit a reminder). AI usage and
  credits are passive metrics — useful but not "I came here to see
  this" data.
- Each deferred tab has a known v2 cost (one endpoint + one pane).
  Wrong-bet recovery is cheap.
- Maximalist v1 (5 tabs) doubles backend review surface for endpoints
  whose UI patterns we haven't validated. Better to validate the
  shell with 3 then templatize.

**Mitigation:** Ship the tabs telemetry-enabled (D11). If after two
weeks the most-clicked deferred-tab signal exceeds 30% of "Resume
reports" clicks, v2 prioritises it explicitly.

---

### D3 — Lazy-load per tab; no auto-refresh; manual refresh button

**Decision:** First activation of a tab fires one fetch. Subsequent
re-visits within the session use cached state. No background polling.
Each tab has a small `↻` refresh affordance in its toolbar.

**Strongest counterargument:** *Lazy loading makes every first click
slow.* The user said "don't hit DB for all at once," but they didn't
say "make every tab switch wait for an HTTP round-trip." Prefetching
inactive tabs in the background after initial paint of the active tab
gets us both: no fan-out at page mount, no perceptible wait on click.

**Why I accept lazy-only anyway:**

- Background prefetch hides cost from the user but doesn't reduce
  total cost — three queries still fire per visit, just staggered.
  For users who never click those tabs, that's still waste.
- The first-click latency on the new endpoints is bounded by P50 < 80ms
  (small user-scoped tables, indexed by `(user_id, created_at)`). The
  perceived "slow" is sub-second; we're not paying it for nothing.
- Background prefetch makes per-tab error states harder — was that
  500 from the prefetch or the click? The simpler model loads on
  click and surfaces errors in context.

**Mitigation:** Tab panes render a skeleton on first load (not a
spinner) so the user sees structure immediately. If telemetry (D11)
shows P95 first-click > 800ms for any tab, revisit with prefetch.

---

### D4 — One new endpoint; reuse existing ones for the other two tabs

**Decision:** Add `GET /job-tracker/ai/resume/reports` with cursor
pagination. Returns `AIReportSummary[]` — id, title, score,
resumeFilename, appliedRole, createdAt — explicitly **no markdown
body**. Drill-in for the full report reuses the existing per-id
endpoint if one exists (audit first); otherwise add `GET
/ai/resume/reports/{id}` in the same PR.

Reminders: reuse `GET /reminders`. Verify the SELECT joins
`job_tracker.applications` so the row payload carries company/role
(if not, expand it — treat as a tweak, not a new endpoint).

Notifications: no change.

**Strongest counterargument 1:** *Summary vs full split forces an extra
round-trip per drill-in.* If the user opens 5 reports, that's 5 extra
HTTP calls vs. one fat list response.

**Why split anyway:** Markdown bodies are 3–10KB; a 50-row list with
inline bodies is 150–500KB and 90% of the bytes are unused (user only
drills into ~1 per session based on internal expectation). The split
also enables HTTP caching on the per-id endpoint (markdown is
immutable post-generation — `Cache-Control: max-age=31536000,
immutable` is honest). Inlined bodies make caching the list response
near-useless because any new report invalidates everything.

**Strongest counterargument 2:** *Why not a single aggregator endpoint
`/activity` that returns mixed-domain rows?* It's how Slack /
GitHub-style activity feeds work.

**Why per-domain anyway:** Polymorphic responses force the FE to
discriminate on `kind` (we already do this for notifications), which
is fine when *all* rows are first-class events. Here the rows aren't —
a resume report is a heavy artifact, a reminder is a future action, a
notification is a past event. Their pagination keys differ; their
filter axes differ (D8). One endpoint per domain keeps the contract
simple and each query optimisable.

**Strongest counterargument 3:** *Adding the per-id endpoint now is
preemptive.* If the existing `LatestReport` already covers it (via a
hidden id param), we don't need a new route.

**Mitigation:** Audit `internal/jobtracker/handlers_ai.go` and
`store_*.go` for an existing single-report fetcher before writing one.
If absent, add the minimum surface needed: `GET
/ai/resume/reports/{id}` returning the full `AIReport` shape used by
the existing latest-report response — no new type required.

---

### D5 — Cursor pagination matching `ListNotificationsOpts`

**Decision:** New list endpoint uses `?cursor=<base64-of-ts>&limit=<n>`,
returns `{ items, nextCursor }`. `limit` default 50, max 200. Cursor
encodes the last row's `created_at` (UTC, microsecond precision).

**Strongest counterargument:** *Cursor is overkill at our scale.*
Today's largest user probably has <100 reports. Offset/limit would
work and be one fewer thing to think about.

**Why cursor anyway:** `ListNotifications` is already cursor-paginated;
the FE has the pattern. Mixing pagination styles costs more in FE
mental overhead than the cursor implementation costs server-side
(~15 lines, copy-paste from the existing one). Offset/limit also has
the well-known "skipped row under concurrent insert" hazard — even at
small scale, the cron job that writes notifications mid-list-render
can cause the FE to see a duplicate or miss one. Free correctness.

**Mitigation:** `limit` capped at 200 prevents accidental
`?limit=10000` DOS. Cursor parsing is strict (no graceful fallback to
offset) — fail fast on malformed input.

---

### D6 — Extract the reminder modal to a shared component

**Decision:** Move modal JSX from `app/applications/page.tsx` into
`components/reminder-modal.tsx`. Component signature:

```ts
<ReminderModal
  mode="create" applicationId={id}     onClose onSaved />
<ReminderModal
  mode="edit"   reminderId={id}        onClose onSaved />
```

`mode` is the discriminator; the two prop sets are mutually exclusive
via a TS discriminated union (not optional fields). Applications page
imports and renders with `mode="create"` (unchanged UX). New Reminders
tab imports with `mode="edit"` for inline edits.

**Strongest counterargument:** *Premature abstraction.* The applications
page is the only consumer today. Extracting before the second consumer
is built means we'll get the abstraction wrong (e.g. coupling we don't
notice until the second use site exposes it).

**Why extract anyway:**

- The second consumer is in *this* PR. The "wait for second use" rule
  is meant to prevent abstraction-on-spec; here the second use is
  concrete and lands in the same commit.
- A `mode` discriminator with two prop branches handles both call
  sites without growing the component to a generic form-builder. The
  abstraction shape is bounded by the actual use cases, not imagined.
- Reminders fields are stable (triggers_at, note) — the most likely
  evolution is *adding* fields (channel, snooze interval), which is
  exactly when one-place-to-update pays off.

**The applications-page workflow is bit-for-bit preserved.** The user
explicitly asked about this: extracting the modal does not remove the
ability to create a reminder from the row. Same JSX, different
filepath.

**Mitigation:** Acceptance test (in the verification section of the
plan): "create reminder from `/applications` row → still works." If
the extracted component grows past ~150 lines or sprouts a third
mode in v2, revisit (probably split create/edit into two components
sharing a hook).

---

### D7 — Per-tab toolbars; no global filter bar

**Decision:** Each pane owns its own toolbar. Notifications keeps its
existing `All | Unread` chip pair. Reminders gets `Pending | Fired |
All`. Resume reports gets sort options (date | score) only — no
filters in v1. Toolbars are visually aligned (same chip style, same
row position) but functionally independent.

**Strongest counterargument:** *Inconsistent toolbars look like a bug.*
Three rows of chips that mean different things on each tab will
confuse users who came expecting the same control to do the same thing
everywhere.

**Why per-tab anyway:** The filter axes are genuinely different — a
chip pair that maps "Unread" to "Pending" to "By score" *is* the
confusion. Visual consistency (same shape) plus semantic clarity (each
chip labeled for what it does in that pane) is the honest answer. The
alternative — forcing the same axis everywhere — would mean dropping
useful filters (e.g. no pending/fired split) to fit the existing
notifications shape.

**Mitigation:** Visual style guideline documented in the plan: chip
height/padding matches the existing `.filter-box` rules. Each chip
group has a small label prefix ("Status:", "Sort:") to make the axis
explicit when it's not obvious from chip text alone.

---

### D8 — Notifications read-state stays independent of history tabs

**Decision:** Opening Resume reports or Reminders does **not** mark any
notification as read. The bell badge reflects only what the user has
explicitly acknowledged via the Notifications tab.

**Strongest counterargument:** *Users will feel the bell is "stuck."*
If a `reminder_fired` notification arrives and the user opens the
Reminders tab to act on it, leaving the badge counting that
notification as unread feels broken.

**Why independence anyway:**

- The notification and the underlying object are different events.
  Seeing a reminder in the Reminders tab isn't necessarily
  acknowledging the *alert* — the user may be editing future
  reminders, having forgotten the fired one.
- A "smart" mark-read coupling would need per-kind rules (resume
  reports have no notification at all today; reminders do; ai-usage
  doesn't). The rule table grows with every new tab.

**Where this is genuinely weak:** for `reminder_fired` notifications,
the coupling is intuitive ("I saw it, clear it"). The single-rule
solution (mark-read all reminder-kind notifications when the
Reminders tab opens) is small. I'm rejecting it not because it's
wrong, but because it's the first crack in a "smart" coupling rule
table that grows with every tab. Keep the rule for v2 when we have
data on whether users actually complain.

**Mitigation:** Each pane shows the relevant notifications inline at
the top (if any). The user can mark-read from there without leaving
the tab. This gets us 80% of the "intuitive" feel without coupling
state machines.

---

### D9 — No schema changes; no migration

**Decision:** Everything v1 needs is already in the DB. No new tables,
no new columns, no migration. `ai_reports`, `reminders`,
`notifications`, and `applications` cover the read paths.

**Strongest counterargument:** *We may discover during implementation
that `ai_reports` lacks a column we need for the summary shape*
(e.g. resume filename is via FK only, target role might be embedded
in the prompt rather than its own column).

**Why I'm comfortable anyway:** The Task #1 audit (see plan file)
runs *before* the endpoint is coded; if a column is missing we
either (a) derive it (JOIN with the resume file) or (b) add a
migration *then* and update this ADR with a D9.5 amendment. The
no-migration claim is conditional on the audit confirming columns
exist; if it doesn't, this ADR is amended, not silently violated.

---

### D10 — Auth and rate limiting on the new endpoint

**Decision:** `GET /ai/resume/reports` is gated by the existing
`requireUser` middleware (same as every other `/job-tracker/*` route).
**No additional rate limit** — unlike the recruiter phone endpoint
(ADR-adjacent, in `handlers_recruiters.go`), the response carries no
sensitive identifying data; it carries the user's own historical
artifacts. The existing per-user limiters (5/min for AI, 1/min for
imports) don't apply because this endpoint is neither AI- nor
import-related.

**Strongest counterargument:** *Any user-data endpoint can be abused
under credential compromise.* If an attacker has a valid JWT, they
can scrape the entire report list. Rate-limiting at 60/min would
slow that to a crawl.

**Why no rate limit anyway:**

- Credential compromise is the wrong threat model to defend against
  at this endpoint; the entire `/job-tracker/*` surface is equally
  vulnerable. Rate-limiting one endpoint is theatre unless we limit
  all of them.
- Real abuse vectors here (a logged-in user hitting refresh) are
  bounded by the manual refresh button + tab-cache (D3); we won't
  see > 1 req/sec from real UI.

**Mitigation:** If telemetry shows abnormal traffic to this path,
add a per-user limiter (the `withUserRateLimit` middleware in
`internal/server/ratelimit.go` is the existing tool — set to 60/min
which is harmless to real users).

---

### D11 — Telemetry on tab activations and drill-ins

**Decision:** Log a structured event per tab activation
(`tab_activated`, properties: tab name, first-time-this-session bool)
and per drill-in (`resume_report_opened`, `reminder_edit_opened`).
Log via the existing `slog` logger at INFO level. No dashboards in v1;
just enough breadcrumbs to inform the v2 deferred-tab prioritisation
(D2 mitigation).

**Strongest counterargument:** *Premature instrumentation.* Pegasus
doesn't have an analytics pipeline today; structured slog lines won't
get aggregated unless someone runs `jq` on production logs.

**Why log anyway:** The cost is one line per event, and the alternative
(adding instrumentation later) is *also* cheap but loses the
two-week observation window that decides D2's revisit triggers. Free
optionality.

**Mitigation:** Don't add new metric backends, dashboards, or events
beyond what slog already does. If we want real analytics later, that's
a separate ADR.

---

### D12 — Empty states are first-class, with action CTAs

**Decision:** Each pane has an explicit empty state. Resume reports
empty → "No resume reports yet. Run your first review →" links to
`/resume`. Reminders empty → "No reminders set. Reminders show up here
when you set them on an application." Notifications empty → existing
copy unchanged.

**Strongest counterargument:** *Just say "nothing here yet."* Empty
states with CTAs feel marketing-y; users who hit them likely already
know what the feature does.

**Why CTAs anyway:** This page is the *destination* for a new product
surface. The first user to land here may have come from a sidebar
click without knowing what each tab does. Linking to the source
feature (`/resume`, `/applications`) provides discoverable forward
paths. CTAs disappear once the user has any data — they're never
chrome for repeat visitors.

---

### D13 — Mobile: tables collapse to cards using the recruiter-table pattern

**Decision:** Both new tables (Resume reports, Reminders) follow the
same responsive pattern used in the recruiter table:
`.recruiter-table` at wide widths, individual `div.recruiter-card`
rows under ~640px width. No new CSS primitives.

**Strongest counterargument:** *That pattern was designed for
recruiters' 5-column rows; resume reports has 6 columns, reminders
has 4 — the breakpoint where collapse-to-card looks good may differ.*

**Why reuse anyway:** Three custom mobile patterns is a maintenance
burden; one shared pattern with two consumers is acceptable. If a
specific pane needs a different breakpoint, override the CSS variable
locally — don't fork the pattern.

**Mitigation:** Spot-check both panes at 375px, 640px, 1024px widths
during verification. Document any per-pane overrides at the top of the
relevant CSS rule block.

---

### D14 — Deploy is a normal release; no `deploy.sh` or migration changes

**Decision:** Ship via the standard image push + `deploy.sh` flow.
Step 5 of `deploy.sh` (migrate) is a no-op for this PR. Frontend
deploys via existing Vercel pipeline. No env var additions.

**Why call this out:** The recruiter-phone work raised the same
question. Future readers should see the explicit "no infra change"
verdict alongside the feature ADR.

---

## Risk register

| # | Risk | Likelihood | Severity | Mitigation |
|---|---|---|---|---|
| R1 | The audit (Task #1) reveals `ai_reports` lacks a needed column. | Low | Medium | Either derive from FK or amend this ADR with a migration step. Plan task #1 runs *before* endpoint code. |
| R2 | Users complain about the misnomer (`/notifications` showing "Activity"). | Medium | Low | Documented (D1 mitigation). Rename only when deferred-tab pressure forces a v2 reorganisation. |
| R3 | Lazy-loaded tab first-click feels slow on cold cache. | Medium | Low | Skeleton renders (D3 mitigation). Revisit with prefetch if P95 > 800ms. |
| R4 | Reminder modal extraction breaks the applications-page workflow. | Low | High | Acceptance test in plan verification; smoke before merge. Easy rollback (single file). |
| R5 | Resume report markdown sizes balloon, list response inflates anyway because of metadata creep. | Low | Medium | Summary shape is *explicit* (D4). Adding fields requires this ADR be amended. |
| R6 | Read-state independence (D8) generates user complaints about the "stuck" bell. | Medium | Low | Per-pane inline notification surface (D8 mitigation). Telemetry shows whether users are dismissing or ignoring. |
| R7 | New endpoint becomes a slow query under load (no `(user_id, created_at)` index on `ai_reports`). | Low | Medium | Verify index exists in Task #1 audit. If missing, add migration. |
| R8 | Tab state in `?tab=` collides with someone navigating via browser back-button mid-edit (reminder modal open + tab changes). | Low | Low | Tab change closes any open modal in the pane. Document as a UX rule in the shell component. |
| R9 | Telemetry slog lines bloat production logs. | Low | Low | Two event types, INFO level; ignorable noise. Drop to DEBUG if it becomes a problem. |
| R10 | The "extract modal" diff conflicts with concurrent work in `app/applications/page.tsx`. | Medium | Low | Rebase before merge; the extraction is a self-contained block. |

---

## Consequences

### Positive

- **`/notifications` becomes a single destination** for "what's
  happening / what did I do" — one sidebar entry, additive
  navigation.
- **Resume report history is finally visible** — users can compare
  scores across iterations without re-running.
- **Reminders gain a global view + edit surface** — no more digging
  through individual application rows.
- **Reminder modal extraction pays the first form-drift dividend**
  the moment v2 adds a channel or snooze field.
- **Template for future tabs**: v2 (AI usage, credits, cross-app
  timeline) drops in as a new endpoint + a new pane + a tab nav
  entry. Shell stays unchanged.
- **No data layer work**: no migrations, no backfills, no risk of
  multi-pod schema-drift hazards.

### Negative

- **Route misnomer** (D1 weakness) — debt we knowingly carry.
- **Sample-of-one scope** (D2 weakness) — three tabs may miss what
  users actually want; mitigated by telemetry, not eliminated.
- **First-click lag per tab** (D3 weakness) — skeletons mask it but
  don't remove it.
- **Per-pane duplication** of pagination state, refresh logic, and
  empty-state CTAs. Refactor opportunity in v2 if we end up with 5+
  panes.
- **Read-state independence** (D8 weakness) may feel inconsistent.
  Inline notification surface helps; doesn't fully resolve.
- **Modal extraction** is one more component to keep in mind when
  reading the reminders feature end-to-end (one indirection added).
  Trade-off priced into D6.

### Neutral observations

- No new infra, no new env vars, no new SDKs.
- The new endpoint is a near-clone of `ListNotifications` —
  maintainer onboarding is "read that file, this file is the same
  shape."
- Existing `/notifications/count` poll keeps working as the sole
  badge-driver; no coordination needed across tabs.

---

## When to revisit

- **More tabs (AI usage, credits, cover letters).** Trigger: D11
  telemetry shows > 30% of users clicking the deferred-tab slot OR
  three support requests for "where do I see X?". Add per-domain
  endpoint + pane. No shell change required.
- **Generic activity feed across all domains.** Trigger: a third "I
  want X chronologically mixed with Y" request. Build the
  `activity_log` table then, with N domains of evidence to inform
  shape.
- **Real-time updates** (SSE / short-poll). Trigger: multi-device
  users complain about stale reminder state. Limit poll to active
  tab only.
- **Read-state convergence (reverse D8).** Trigger: telemetry shows
  unread badge stays non-zero for users who clearly visited the
  relevant tab. Add explicit per-pane "mark notifications seen"
  buttons first; full coupling only if those don't address it.
- **Route rename `/activity`.** Trigger: hitting > 5 tabs OR a
  product surface that depends on the URL semantically (e.g. a
  marketing page referencing it). Plan: server-side redirect from
  `/notifications`, email template sweep, extension popup link
  update. Multi-repo, separate ADR.
- **Prefetch on inactive tabs (reverse D3).** Trigger: P95 first-click
  latency > 800ms for any pane. Stagger prefetch behind the active
  tab's paint.
- **Rate limit the new endpoint.** Trigger: abnormal traffic in
  request logs. Reuse `withUserRateLimit` (`internal/server/ratelimit.go`).

---

## Notes for future readers

- The plan file at `~/.claude/plans/abundant-mixing-whisper.md` carries
  the file-level work breakdown and verification steps. This ADR is
  the **why**; the plan is the **how**.
- ADR-001 D1 ("one notifications table, polymorphic ref") is what
  makes D8 cheap to maintain — the notifications surface stays the one
  place we track unread state, independent of history domains.
- ADR-005 (deep-link via list-page modal) is precedent for the
  pattern used here: don't add a new route when the existing page can
  carry the new state via a query param. Same instinct, applied at
  the tab level.
