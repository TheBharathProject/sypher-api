# ADR-002 — Premium tier flag + email gating policy

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-05-09 |
| Decision-maker | Shubham |
| Supersedes | — |
| Superseded-by | — |
| Related | [ADR-001 — Notifications, email, cron](./0001-notifications-email-cron.md) |

## Context

Pegasus has been free for everyone since launch — that's reflected in the published Terms and the marketing copy. The user has decided that **email** notifications become a premium perk while **in-app** notifications stay free for everyone.

This ADR captures the policy and the wiring. Two product surfaces are affected:

1. **The existing Phase 2 daily-digest email** (cron-driven, fires at 09:00 IST). Today it sends to every user with at least one stale or deadline-approaching application. After this change, only premium users get the email — the in-app notification still lands for everyone.
2. **Future Phase 3 community emails** (community-reply, community-mention, etc.) — same gate, same call site.

Stripe billing is **not yet wired**. Premium status is admin-flippable via SQL until paying users exist.

## Forces

- Resend account costs nothing for our volume — the "premium" boundary is value-perceived, not cost-driven. Free users still get the bell badge and `/notifications` page; the paid line is "Pegasus emails me when I should look".
- We don't yet have Stripe wired. Hand-wiring premium via SQL is acceptable because there are zero paying users; switching to Stripe later means wiring a webhook to flip the same column we already check.
- ADR-001's Notifier interface is already email-agnostic — the email decision happens at the cron-job (and future community-handler) call site, not inside Notifier. This is exactly where the premium gate belongs (ADR-001 D8 explicit-method-calls policy).
- Existing free users must not lose anything they had. They keep the bell, the `/notifications` page, every notification kind. The only thing the policy removes from them is the email leg.

## Decisions

### D1 — `is_premium BOOLEAN` on `auth.users`, default `false`

Single column on the existing user row. No `subscriptions` table yet. Migration `0009_premium_flag.sql`:

```sql
ALTER TABLE auth.users
  ADD COLUMN IF NOT EXISTS is_premium BOOLEAN NOT NULL DEFAULT FALSE;
```

When Stripe lands later, the `subscriptions` table joins back via `user_id`; `is_premium` becomes the cached resolution (webhook handler keeps it in sync).

**Rejected:** separate `subscriptions` table now (over-engineered for a one-bit flag); `tier ENUM` (loose TEXT bigger blast radius).

### D2 — `email_notifications_enabled BOOLEAN` on `auth.users`, default `true`

Per-user opt-out preference. Default-on so premium users get emails they're paying for without having to flip a switch first. Free users have the column too — its value is ignored by the gate, which checks `is_premium` first.

```sql
ALTER TABLE auth.users
  ADD COLUMN IF NOT EXISTS email_notifications_enabled BOOLEAN NOT NULL DEFAULT TRUE;
```

### D3 — Email gate at the call site, not in Notifier

Pattern stays exactly as Phase 2: `Notifier.Push` writes the row; the caller decides whether to follow up with `mailer.Send`. New helper `Store.UserCanReceiveEmail(ctx, uid) (bool, error)` returns `is_premium AND email_notifications_enabled`.

Every email call site wraps `m.Send` with this check:

```go
if canEmail, _ := store.UserCanReceiveEmail(ctx, uid); canEmail {
    if err := m.Send(ctx, msg); err != nil { /* log */ }
}
```

**Why this:** No special-case in Notifier means future kinds (digest, community-reply, future) all use the same gate, and the gate is co-located with each email send — easy to audit ("show me everywhere we send email" is `grep mailer.Send` and every one of those should have the gate above it).

### D4 — Settings UI: "Upgrade" CTA for free users, toggle for premium users

Free users see an "Email notifications · Premium feature → Upgrade" row in `/settings`. Upgrade link points to `/upgrade` which renders a "Premium is rolling out — drop your email" stub (no real billing yet).

Premium users see a checkbox "Email me about activity" with a small caption explaining what kinds (digest + community replies). Toggle persists via `PATCH /me/email-prefs`.

A free user POSTing `/me/email-prefs {enabled: true}` gets `402 Payment Required` with body `{error: "premium_required"}`. The frontend never offers them the toggle, but the API enforces it independently.

### D5 — Retroactive: existing daily-digest now premium-only

The Phase 2 cron job's email leg gets wrapped with the new gate. In-app notification stays for everyone — only the email send is gated. A `// see ADR-002 D5` comment in `cron/jobs/applications.go` explains why the wrap is there.

This is the entire retroactive change for existing users: no schema migration drops their data, no notifications disappear from their feed, no behaviour changes except "you stop getting emails until you upgrade".

### D6 — Stripe deferred; premium is admin-flippable for now

Until Stripe ships, premium is granted via:

```sql
UPDATE auth.users SET is_premium = true WHERE id = '<uid>';
```

This trust model is fine today (single dev, no users at scale; database access is already admin-only). When a future phase ships Stripe, the webhook handler will flip the same column. No code that depends on `is_premium` needs to change.

## Consequences

### Positive

- One column means the gate is dead simple; no joins to evaluate "can this user get email".
- Default-true email preference means a future Stripe upgrade flow gives instant value with no extra UX step.
- Existing Phase 2 cron job needs a 3-line change. Existing tests still pass.
- Free users' experience doesn't degrade — they keep the bell badge and the `/notifications` page; only the inbox copy goes away.

### Negative

- Two booleans on `auth.users` is technically denormalized — when Stripe wires, `is_premium` becomes derived state cached on the user row. Mitigation: webhook handler keeps it in sync; a future `Store.RefreshIsPremium(uid)` repair function is straightforward.
- "Premium" with no Stripe is a paper boundary. Anyone with database access can grant themselves premium. This matches the trust model today and will stop mattering once Stripe is the source of truth.
- The Terms doc at `sypher-shell/app/terms/page.tsx` says emails are best-effort for everyone. It needs a small update to mention the premium tier when this ADR ships. Tracked as a Phase 3a follow-up.

## When to revisit

- **Stripe wires:** flip `is_premium` from "set by SQL" to "set by webhook". Add `subscriptions` table; `is_premium` becomes a cached projection of `subscriptions.status IN ('active', 'trialing')`.
- **Multiple email kinds emerge:** if users want fine-grained control (mute community replies but keep digest), expand `email_notifications_enabled` from a single boolean to a per-kind preferences blob (JSONB on the same row, or a `notification_prefs` table).
- **Free users complain about emails being premium-only:** revisit the policy. The all-free-everything alternative is a single `if mailer != nil { m.Send(...) }` change at the same call site.
