# ADR-003 — Community schema + 4 notification triggers

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-05-09 |
| Decision-maker | Shubham |
| Related | [ADR-001 — Notifications, email, cron](./0001-notifications-email-cron.md), [ADR-002 — Premium gating](./0002-premium-email-gating.md) |

## Context

Pegasus has 5 community surfaces — Reviews, Experiences, Referrals, Ask, Recruiters — that have been pixel-complete on the frontend with mock submits since the early polish work. Each surface presents a per-surface form (round counts for Experiences, target role for Reviews, recruiter chips for Recruiters, etc.) but the underlying lifecycle (post / vote / comment / soft-delete / moderate) is identical across the five.

Phase 3b ships the schema + CRUD; Phase 3c wires the frontend to it; Phase 3d adds notification triggers. This ADR covers all three because they share the same structural decisions.

## Forces

- 5 surfaces with very different per-surface metadata, but identical post / vote / comment lifecycle.
- Community is the FIRST synchronous trigger source for notifications — every event happens inside a request, not a cron tick. The Phase 2 idempotency model (partial unique index on cron kinds only) doesn't directly apply; we extend the same pattern with sibling indexes for `community_vote` and `community_mention`.
- 4 trigger kinds are in scope: reply-on-your-post, reply-on-thread-you-participated-in, vote-on-your-post, mention.
- Notification spam is a real risk. Vote-on-your-post in particular: a popular post shouldn't blow out the bell. The day-bucketed dedup pattern from ADR-001 D5 handles this naturally.
- Public read for unauthed browse — same pattern as `/public/profile/{slug}`.

## Decisions

### D1 — One `community_posts` table with `surface` discriminator + JSONB metadata

Five surfaces share post / vote / comment plumbing. Per-surface fields (round counts, target role, recruiter title, tag list) live in a `metadata JSONB` column. Database doesn't enforce metadata shape — the handler validates per-surface input on the way in.

**Rejected:** five tables. UNION on every "all activity" query, five sets of indexes, five duplicated handlers. ~10x more code for zero practical benefit at this scale.

### D2 — Three tables: `community_posts` + `community_votes` + `community_comments`

Standard shape. Comments support replies via `parent_id` self-reference (nested threads, max-depth not enforced at DB — the UI clamps display).

`community_votes` uses `PRIMARY KEY (post_id, user_id)` so vote upserts are a single `INSERT ... ON CONFLICT DO UPDATE`.

### D3 — Notification trigger placement: synchronous, in the same request

- Comment created → push `community_reply` to post author + each prior commenter (DISTINCT user_id, excluding the new commenter).
- Vote received → push `community_vote` to post author (deduped per day per post via partial unique index on `(user_id, ref_id, day)` for `kind='community_vote'`).
- Mention parsed → push `community_mention` to each mentioned user.

All happen INSIDE the same handler request. If push fails, the comment/vote/post still succeeds — push errors are logged, not bubbled (graceful degradation per ADR-001 D6 spirit).

### D4 — "Reply on thread you participated in" via single DISTINCT query

When a comment lands, `Store.NotifiableCommentParticipants(ctx, postID, excludeUserID)` returns DISTINCT `user_id` from `community_comments` where `post_id = ?` AND `user_id != excludeUserID` AND `status = 'active'`. Then add the post author. Push `community_reply` to each.

This naturally delivers the "reply on your post" case as a subset (post author is in the list because they commented OR is added explicitly).

### D5 — Vote dedup via partial unique index, mirrors ADR-001 D5

Same pattern as Phase 2's cron dedup. Sibling unique partial indexes on `(user_id, kind, ref_id, ((created_at AT TIME ZONE 'UTC')::date))` for `kind='community_vote'` and `kind='community_mention'`.

The dedup index for `community_vote` is keyed by `(post_owner_user_id, 'community_vote', post_id, day)` — so a popular post still only nudges its author once a day, regardless of how many people upvoted it. **Author** of the post is the recipient (they're the user_id on the notification row), and the post's id is the ref_id, which is what makes the dedup work.

### D6 — Mention parser

Mentions are `@<slug>` where `<slug>` matches the public profile slug (`[a-z0-9-]{3,40}` per existing `CheckSlug`). Parser is a regex `@([a-z0-9][a-z0-9-]{2,39})\b` on post body + comment body at write time. Each unique match → `Store.UserIDBySlug(slug)` → if found AND not the author, push `community_mention`.

**Rejected:** mention markdown like `@[Display Name](id:uuid)`. Every slug-aware client (the website, future extension) would need to format it; raw `@slug` is enough for v1.

### D7 — Public read endpoints — `/job-tracker/public/community/{surface}`

Same pattern as `/public/profile/{slug}` — bypass auth middleware, filter to `is_public=true AND status='active'`. Returns the same shape as the authed endpoint minus the user's own vote state.

### D8 — Soft-delete only; no hard delete on user-action

Comments and posts that the user "deletes" set `status='removed'` and the row stays. Preserves thread structure (orphaned comments lose context if their parent vanishes). Hard delete only via admin tooling once that exists.

### D9 — Rate limiting at the handler level

Per-user posting limits, configurable via env (defaults conservative for v1):
- 5 posts per surface per day
- 50 comments per day  
- 100 votes per day

Enforced via `Store.RecentPostCount` / `RecentCommentCount` / `RecentVoteCount`. Hitting the limit returns `429 Too Many Requests` with a `Retry-After` header.

## Consequences

### Positive

- Phase 3 ships all 5 surfaces from day one because the schema is one table. Each surface only needs a per-surface validation function on input + a frontend draft shape (already exists from the modal work).
- "Reply on thread you participated in" comes free from the existing DISTINCT-on-comments query — no engagement-tracking column needed.
- Vote dedup via partial unique index means popular posts don't blow out the bell.
- Mention parser is ~30 lines of Go and reuses the existing slug regex.

### Negative

- JSONB metadata means no DB-level validation per surface. Tradeoff worth it; handlers do the validation.
- Soft-delete leaves orphan rows that admin queries need to filter. We've handled this pattern before (`applications.stale`).
- Rate limits will catch some legitimate bursts (e.g. a user pasting their interview notes across many companies). Numbers are env-tunable; adjust based on log signal.

## When to revisit

- **Per-tag tracking on Ask** ("subscribe to `salary` tag, get notified on new questions"): would need a `community_subscriptions` table and a fan-out trigger. Defer until users ask.
- **Activity feed** ("show me everything happening, newest first across all surfaces"): the unified table makes this a single query. Add when there's product demand.
- **Vote-on-your-comment** notification (currently only post-level): trivial extension if comments grow important enough to warrant their own scoring.
