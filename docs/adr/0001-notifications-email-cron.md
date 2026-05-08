# ADR-001 — Notifications, email, and scheduled jobs

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-05-09 |
| Decision-maker | Shubham |
| Supersedes | — |
| Superseded-by | — |

## Context

Pegasus today has zero way to tell a user that something happened. The applications table has a `stale: true` boolean that nothing surfaces; `apply_deadline` ticks past unnoticed; community comments (Phase 3) will need a "reply on your post" path the moment they're built. Without a notification system in place, every later feature has to invent its own ad-hoc nudge.

Phase 2 builds that spine — three things in one cut:

1. **In-app notifications** — one table, one feed, a bell with an unread dot.
2. **Email digests** — daily 09:00 IST roll-up of stale apps + approaching deadlines, sent via Resend.
3. **Scheduled jobs** — an in-process cron loop that the rest of the API can register tickers against.

Phase 3 (community) will bolt onto Phase 2 by emitting a single `community_reply` notification row when a comment lands; no new infra needed there.

## Forces

- Pegasus is a **single-pod Go binary** behind Caddy on the OCI VM. No Redis, no message queue, no managed cron, no email scaffolding (`vm-deploy-pattern.md`, `self-hosted-postgres.md`).
- Existing scaffolding:
  - `cmd/api/main.go` — `signal.NotifyContext(ctx, SIGINT, SIGTERM)` is already plumbed and passed into `server.Start(ctx)`.
  - `internal/server/server.go` — http server runs as a goroutine; `select{}` waits on `<-ctx.Done()`.
  - Handler pattern: `NewHandler(cfg, store, authStore, logger)` with optional builders `.WithStorage(r2)`, `.WithAI(client, usage)`.
  - Config: `required("FOO")` for must-have, `envWithDefault("FOO", default)` for optional.
- Pegasus is free; users haven't paid for promised SLAs. Email delivery is best-effort.
- Future-self hates outbox tables; we don't introduce one until something actually requires it.

## Decisions

### D1 — One notifications table, `kind` discriminator

**Decision:** Single `job_tracker.notifications` table with `kind TEXT` (`'app_stale'` / `'app_deadline'` / `'community_reply'` / `'digest'`), polymorphic `ref_type` + `ref_id`, optional `link_path` for the in-app deep link.

**Rejected:** One table per source (`stale_alerts`, `deadline_alerts`, `comment_replies`). Cleaner per-domain queries but the bell badge would need a `UNION` across N tables, and every new notification kind would mean a new migration.

**Why this:** The set of "things to nudge a user about" stays small (target: <10 kinds across the lifetime of the app). One table with a partial index per common access path is plenty for Postgres at this scale. Frontend reads `kind` and chooses an icon + verb client-side.

### D2 — Resend as email provider, optional config

**Decision:** `internal/mailer/` package with one `Mailer` interface and a `ResendMailer` implementation. Env vars `RESEND_API_KEY`, `MAIL_FROM_ADDRESS`, `MAIL_FROM_NAME` — all **optional**. If `RESEND_API_KEY` is unset, the package returns a `slogMailer` that logs would-be sends and drops them; the rest of Phase 2 still works (in-app notifications still ship).

**Rejected:** SES (requires AWS account + sandbox removal), Postmark ($15/mo floor), no email at all (defers learning until a Phase 3 user asks why they didn't get a reply email).

**Why this:** Resend's HTTP API is dead simple — `POST /emails` with bearer auth — so we don't take the SDK as a dependency, just `net/http`. Free tier (3K/mo, 100/day) covers Phase 2 traffic comfortably. Domain setup is one-time DKIM + SPF work, separate from the code.

### D3 — In-process cron via `time.Ticker` + child context

**Decision:** New `internal/cron/cron.go` with a `Runner` type that takes a `context.Context` and a list of jobs (`{Name, NextFire, Run}`). Schedule is computed manually (`time.Until(next09IST)`), no external library. Runner spawns from `server.Start(ctx)` before its `select{}` block; child ctx cancels on SIGTERM and the runner's `Wait()` lets graceful shutdown wait for in-flight jobs.

**Rejected:** External cron hitting protected HTTP (more infra, harder to test); `robfig/cron/v3` (third-party dependency for two jobs that don't need cron syntax).

**Why this:** Single-pod, so two pods racing the same minute boundary isn't a concern. Adding the runner is ~80 lines of Go. When/if we go multi-pod, the migration path is a `cron_locks` table + a "run if I won the lock" wrapper — additive, no refactor.

### D4 — Synchronous notification creation; async fire-and-forget email

**Decision:** When a handler creates a community comment, the same request creates the notification row in the same DB transaction. The `Mailer.Send(...)` call is dispatched in a `go func() { ... }()` goroutine with its own 10-second context timeout, so a slow Resend never blocks the user-facing response.

**Rejected:** Outbox table + drainer (more code, write amplification, latency between action and bell badge appearing); fully synchronous email (user-facing requests held hostage by Resend rate limits).

**Why this:** The notification row is the source of truth — even if the email dispatch goroutine dies on a panic or Resend 5xx's, the user still sees the unread dot on next page load. This makes Resend a soft dependency.

### D5 — Idempotency via partial unique index for cron-generated notifications

**Decision:** Add a unique partial index `(user_id, kind, ref_id, ((created_at AT TIME ZONE 'UTC')::date))` for `kind IN ('app_stale','app_deadline','digest')`. If the cron job double-fires the same minute (e.g. a deploy crossing 09:00:00), the second insert hits a unique violation and we swallow it.

**Rejected:** Check-then-insert in a transaction (race-prone), nondeterministic dedup ids.

**Why this:** Postgres' partial unique index is the cheapest possible idempotency primitive — no app-level locking, single round-trip insert, behaviour is correct under concurrent access. The `::date` cast scopes "the same day" without tying to the exact timestamp. Partial scope means manual / community-reply rows don't get accidentally deduped.

### D6 — In-app notifications are the load-bearing path; email is decoration

**Decision:** Every event creates a notification row first, then maybe sends an email. Users with no email config still get the bell. Users with email get the bell + an inbox copy.

**Why this:** Decouples the product surface from a 3rd-party SaaS. The day Resend has an outage, Pegasus doesn't degrade visibly.

### D7 — No outbox table; revisit when proven necessary

**Decision:** Resend failures are logged and dropped. We don't retry, we don't queue.

**Trigger to revisit:** if logs show >1% of email sends failing in any 24h period, ship an outbox in a future phase. Until then, the noise of an outbox table beats the win.

### D8 — Triggers are explicit method calls, not an event bus

**Decision:** A handler that needs to nudge calls `h.notifier.Push(ctx, NotificationInput{...})` directly. No event bus, no listener registration, no observer pattern.

**Why this:** Two trigger points exist today (cron jobs) and one will exist after Phase 3 (community reply). Three call sites isn't enough surface to justify event-bus indirection. Re-evaluate once we have ~10 trigger points.

### D9 — `Notifier` and `Mailer` interfaces, dev-mode no-op stubs

**Decision:**

```go
type Notifier interface {
    Push(ctx context.Context, in NotificationInput) error
    PushIdempotent(ctx context.Context, in NotificationInput, dayKey time.Time) (created bool, err error)
}

type Mailer interface {
    Send(ctx context.Context, to, subject, htmlBody, textBody string) error
}
```

In dev (or when `RESEND_API_KEY` is empty), `Mailer` is a `slogMailer` that prints what it would send. `Notifier` always writes to the DB.

**Why this:** Tests don't talk to Resend. Local dev doesn't accidentally email someone. Both interfaces are tiny — no SDK lock-in.

### D10 — Migration `0008_notifications.sql`

**Decision:** One migration adds the `notifications` table and the partial unique index. Idempotent via `CREATE TABLE IF NOT EXISTS` and `CREATE UNIQUE INDEX IF NOT EXISTS`. Notification `kind` is loose (`TEXT`), not an enum — adding a new kind is a code-only change, no migration.

## Consequences

### Positive

- One notification table → one bell-badge query, one feed query, easy `UNION`-free aggregation later (e.g. "show all activity in the last 7 days").
- In-process cron → no new infra to manage, no shared secret to rotate, no GitHub Actions to wire.
- Synchronous notification + async email → bell is always correct; email is best-effort.
- Mailer optional → can ship Phase 2 to prod and decide later whether to set up DKIM. The DB migration runs cleanly without `RESEND_API_KEY`.

### Negative

- Going multi-pod later requires a `cron_locks` table or external cron migration. Tracked under "When to revisit" below.
- Resend outages cause silently-dropped emails. Mitigated by D6 (in-app notification always works) and D7 (revisit if loss rate >1%).
- The `kind` discriminator means frontend has to switch on string values for icon + verb. Manageable today; if it grows past ~10 kinds we can carve out a per-kind component map.
- No notification preferences (mute, frequency, quiet hours). v1 is "send all kinds; user can mark-read or delete account". Preferences land in v2 if users ask.

## When to revisit

- **Multi-pod:** add `cron_locks` table + lock acquisition wrapper. Don't refactor cron itself.
- **Email preferences UI:** add `notification_prefs` table; cron + handler check it before pushing.
- **Outbox pattern:** trigger is sustained Resend failure rate (>1% over 24h).
- **`kind` count creep:** once we cross ~10 distinct notification kinds, consider per-kind icon/verb config tables instead of frontend switches.

## Notes for future ADRs in this repo

This is the first ADR. The pattern moving forward:

- File name: `NNNN-short-kebab-title.md`, padded to 4 digits.
- Status values: `Proposed`, `Accepted`, `Deprecated`, `Superseded`.
- Each ADR is one decision tree. If a follow-up adds a new constraint that changes a prior decision, write a new ADR that explicitly supersedes the old one — don't edit the old one in place.
- Keep ADRs short. The point is to capture *why*, not document the implementation. The implementation lives in code; comments in code reference back to the ADR by number.
