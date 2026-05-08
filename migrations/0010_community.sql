-- 0010_community.sql
-- Community surfaces (Reviews / Experiences / Referrals / Ask / Recruiters).
--
-- Single posts table with a `surface` discriminator + JSONB metadata so
-- adding a new surface is a code-only change. See ADR-003 (D1, D2).
-- The notification dedup index for `community_vote` rides alongside the
-- Phase 2 dedup index — same partial-unique pattern.
--
-- Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS job_tracker.community_posts (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  -- One of: 'reviews' | 'experiences' | 'referrals' | 'ask' | 'recruiters'.
  -- CHECK constraint mirrors the frontend's section param so a stray POST
  -- can't smuggle in an invalid surface.
  surface       TEXT NOT NULL CHECK (surface IN ('reviews','experiences','referrals','ask','recruiters')),
  title         TEXT NOT NULL,
  body          TEXT,
  -- Per-surface fields live here. Schema is enforced by the handler, not
  -- the DB — adding a new field on one surface stays a one-line change.
  metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
  is_public     BOOLEAN NOT NULL DEFAULT TRUE,
  vote_count    INT NOT NULL DEFAULT 0,
  comment_count INT NOT NULL DEFAULT 0,
  -- 'active' = visible. 'removed' = soft-deleted by author. 'flagged' =
  -- user-reported, needs review (admin queue UI is v2).
  status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','removed','flagged')),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Public list query: WHERE surface = ? AND status = 'active' ORDER BY created_at DESC
-- The partial index (status='active') keeps soft-deleted rows out of the
-- index entirely — list queries don't even see them.
CREATE INDEX IF NOT EXISTS idx_community_posts_surface_created
  ON job_tracker.community_posts (surface, created_at DESC)
  WHERE status = 'active';

-- "My posts" query: by author, newest first.
CREATE INDEX IF NOT EXISTS idx_community_posts_user_created
  ON job_tracker.community_posts (user_id, created_at DESC);

-- =============================================================================
-- Votes — one row per (post, user). PRIMARY KEY makes upserts trivial.
-- =============================================================================
CREATE TABLE IF NOT EXISTS job_tracker.community_votes (
  post_id    UUID NOT NULL REFERENCES job_tracker.community_posts(id) ON DELETE CASCADE,
  user_id    UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  -- +1 = upvote, -1 = downvote. 0 isn't stored — clearing a vote
  -- removes the row.
  value      SMALLINT NOT NULL CHECK (value IN (-1, 1)),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (post_id, user_id)
);

-- =============================================================================
-- Comments — flat or nested via parent_id self-reference. Soft-deleted rows
-- stay in place so the thread structure survives.
-- =============================================================================
CREATE TABLE IF NOT EXISTS job_tracker.community_comments (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  post_id    UUID NOT NULL REFERENCES job_tracker.community_posts(id) ON DELETE CASCADE,
  user_id    UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  parent_id  UUID REFERENCES job_tracker.community_comments(id) ON DELETE CASCADE,
  body       TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','removed','flagged')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Comments-on-a-post query (the most common): by post, oldest first so
-- the UI can render a chronological thread without resorting in app code.
CREATE INDEX IF NOT EXISTS idx_community_comments_post_created
  ON job_tracker.community_comments (post_id, created_at);

-- Participant dedup query (D4): find every distinct user_id who has
-- commented on a post, excluding the new commenter. Same index above
-- already covers this since it leads with post_id.

-- =============================================================================
-- Notification dedup for community_vote (ADR-003 D5).
--
-- The Phase 2 dedup index covered ('app_stale','app_deadline','digest').
-- Extending it to add 'community_vote' would require dropping + recreating
-- the partial-WHERE clause; instead we add a sibling index.
-- =============================================================================
CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_community_vote_dedup
  ON job_tracker.notifications (
    user_id,
    kind,
    ref_id,
    ((created_at AT TIME ZONE 'UTC')::date)
  )
  WHERE kind = 'community_vote';

-- Same shape, for community_mention. Stops a single post body from
-- producing two mentions of the same user via accidental double-mention
-- (e.g. "@bob, @bob will love this") within a day.
CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_community_mention_dedup
  ON job_tracker.notifications (
    user_id,
    kind,
    ref_id,
    ((created_at AT TIME ZONE 'UTC')::date)
  )
  WHERE kind = 'community_mention';
