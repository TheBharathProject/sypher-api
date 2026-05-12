-- 0020_community_sort_indexes.sql
-- Partial indexes to support ?sort=votes and ?sort=most-reviewed on the
-- community list endpoint. Both indexes filter on status='active' so
-- soft-deleted rows are excluded without a separate WHERE clause.
--
-- Idempotent: IF NOT EXISTS guards make this safe to re-run.

BEGIN;

CREATE INDEX IF NOT EXISTS community_posts_vote_count_idx
  ON job_tracker.community_posts (surface, vote_count DESC, created_at DESC)
  WHERE status = 'active';

CREATE INDEX IF NOT EXISTS community_posts_comment_count_idx
  ON job_tracker.community_posts (surface, comment_count DESC, created_at DESC)
  WHERE status = 'active';

COMMIT;
