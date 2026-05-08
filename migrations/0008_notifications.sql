-- 0008_notifications.sql
-- In-app notifications + email-trigger source rows. Single table with a
-- `kind` discriminator so the bell badge stays a single SELECT and
-- adding new kinds is a code-only change.
--
-- See docs/adr/0001-notifications-email-cron.md (D1, D5, D10).
-- Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS job_tracker.notifications (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  -- 'app_stale' | 'app_deadline' | 'community_reply' | 'digest' | future kinds.
  -- Loose TEXT (not enum) by deliberate choice — see ADR-001 D10.
  kind        TEXT NOT NULL,
  -- ref_type/ref_id is a polymorphic pointer to whatever this notification
  -- is *about* — the application, the community post, etc. Optional because
  -- some kinds (e.g. 'digest') aggregate over many objects.
  ref_type    TEXT,
  ref_id      UUID,
  title       TEXT NOT NULL,
  body        TEXT,
  -- Frontend deep-link target. Inside Pegasus this is basePath-relative —
  -- e.g. '/applications/<uuid>'. The frontend prepends '/pegasus' as needed.
  link_path   TEXT,
  -- NULL = unread. Stamping this with NOW() is what "mark read" does.
  read_at     TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Bell badge query: WHERE user_id = ? AND read_at IS NULL ORDER BY created_at DESC.
-- NULLS FIRST keeps unread rows at the front of the index (matches the query).
CREATE INDEX IF NOT EXISTS idx_notifications_user_unread
  ON job_tracker.notifications (user_id, read_at NULLS FIRST, created_at DESC);

-- Idempotency for cron-generated kinds: same user, same kind, same ref,
-- same UTC calendar day → at most one row. Cron jobs INSERT then ignore the
-- ensuing 23505 (unique violation) so a deploy crossing 09:00 boundary
-- doesn't double-send. Partial scope leaves community-reply / manual rows
-- alone — those WANT to be created independently per event.
--
-- See docs/adr/0001-notifications-email-cron.md (D5).
CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_dedup_daily
  ON job_tracker.notifications (
    user_id,
    kind,
    ref_id,
    ((created_at AT TIME ZONE 'UTC')::date)
  )
  WHERE kind IN ('app_stale', 'app_deadline', 'digest');
