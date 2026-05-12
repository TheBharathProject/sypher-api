-- 0019_community_slugs.sql
-- Adds a slug column to community_posts for human-readable URLs.
-- Existing rows are backfilled with 'post-' + UUID hex (36 chars minus 4 hyphens = 32 hex chars).
-- The unique index is the final arbiter of collision prevention at write time.
--
-- Idempotent: safe to re-run (IF NOT EXISTS / WHERE slug IS NULL guards).

BEGIN;

ALTER TABLE job_tracker.community_posts
  ADD COLUMN IF NOT EXISTS slug TEXT;

-- Backfill existing rows. New posts get their slug from the Go slugify() function.
-- 'post-' || 32-char hex = 37 chars, well under the 80-char slug limit.
UPDATE job_tracker.community_posts
  SET slug = 'post-' || REPLACE(id::text, '-', '')
  WHERE slug IS NULL;

-- Now that every row has a value, enforce NOT NULL.
ALTER TABLE job_tracker.community_posts
  ALTER COLUMN slug SET NOT NULL;

-- Unique index makes duplicate slug inserts fail fast (the uniqueSlug retry loop
-- in Go handles the application-level collision avoidance).
CREATE UNIQUE INDEX IF NOT EXISTS community_posts_slug_idx
  ON job_tracker.community_posts (slug);

COMMIT;
