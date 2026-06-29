-- 0003_job_tracker.sql
-- Job-tracker tool: applications, notes, profile, feedback.
-- Every product table is keyed by user_id for tenant isolation.
-- Idempotent: safe to re-run.

CREATE SCHEMA IF NOT EXISTS job_tracker;

-- ============================================================================
-- Applications
-- ============================================================================

CREATE TABLE IF NOT EXISTS job_tracker.applications (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  company         TEXT NOT NULL,
  role            TEXT NOT NULL,
  source          TEXT,
  location        TEXT,
  salary_range    TEXT,
  stage           TEXT NOT NULL DEFAULT 'INTERESTED',
  applied_at      DATE,
  apply_deadline  DATE,
  job_link        TEXT,
  description     TEXT,
  notes           TEXT,
  stale           BOOLEAN NOT NULL DEFAULT FALSE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS applications_user_created_idx
  ON job_tracker.applications (user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS applications_user_stage_idx
  ON job_tracker.applications (user_id, stage);

-- ============================================================================
-- Notes + categories
-- ============================================================================

CREATE TABLE IF NOT EXISTS job_tracker.note_categories (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  color       TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS note_categories_user_idx
  ON job_tracker.note_categories (user_id, name);

CREATE TABLE IF NOT EXISTS job_tracker.notes (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  category_id  UUID REFERENCES job_tracker.note_categories(id) ON DELETE SET NULL,
  title        TEXT NOT NULL DEFAULT '',
  body         TEXT NOT NULL DEFAULT '',
  pinned       BOOLEAN NOT NULL DEFAULT FALSE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS notes_user_updated_idx
  ON job_tracker.notes (user_id, updated_at DESC);

CREATE INDEX IF NOT EXISTS notes_user_category_idx
  ON job_tracker.notes (user_id, category_id);

-- ============================================================================
-- Profile + sub-resources
-- ============================================================================

CREATE TABLE IF NOT EXISTS job_tracker.profiles (
  user_id     UUID PRIMARY KEY REFERENCES auth.users(id) ON DELETE CASCADE,
  slug        TEXT,
  is_public   BOOLEAN NOT NULL DEFAULT FALSE,
  headline    TEXT,
  about       TEXT,
  location    TEXT,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS profiles_slug_idx
  ON job_tracker.profiles (lower(slug))
  WHERE slug IS NOT NULL;

CREATE TABLE IF NOT EXISTS job_tracker.profile_experiences (
  id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id   UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  company   TEXT NOT NULL,
  title     TEXT NOT NULL,
  period    TEXT,
  summary   TEXT,
  ordinal   INTEGER NOT NULL DEFAULT 0
);

-- Guarded: 0006_field_alignment.sql renames "ordinal" to "sort_order" on this
-- table. The rename carries the existing index over to the new column, so on
-- re-runs against an up-to-date schema the index already exists in its
-- superseded (user_id, sort_order) form — but Postgres resolves index columns
-- BEFORE the IF NOT EXISTS name check, so an unguarded statement errors with
-- 'column "ordinal" does not exist'. On a fresh DB the column exists at
-- 0003-time and the index is created exactly as before.
DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='profile_experiences' AND column_name='ordinal'
  ) THEN
    CREATE INDEX IF NOT EXISTS profile_experiences_user_idx
      ON job_tracker.profile_experiences (user_id, ordinal);
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS job_tracker.profile_educations (
  id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id   UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  school    TEXT NOT NULL,
  degree    TEXT,
  period    TEXT,
  ordinal   INTEGER NOT NULL DEFAULT 0
);

-- Guarded: see profile_experiences_user_idx above (0006 renames ordinal).
DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='profile_educations' AND column_name='ordinal'
  ) THEN
    CREATE INDEX IF NOT EXISTS profile_educations_user_idx
      ON job_tracker.profile_educations (user_id, ordinal);
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS job_tracker.profile_projects (
  id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id   UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  name      TEXT NOT NULL,
  summary   TEXT,
  ordinal   INTEGER NOT NULL DEFAULT 0
);

-- Guarded: see profile_experiences_user_idx above (0006 renames ordinal).
DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='profile_projects' AND column_name='ordinal'
  ) THEN
    CREATE INDEX IF NOT EXISTS profile_projects_user_idx
      ON job_tracker.profile_projects (user_id, ordinal);
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS job_tracker.profile_skills (
  id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id   UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  name      TEXT NOT NULL,
  category  TEXT
);

CREATE INDEX IF NOT EXISTS profile_skills_user_idx
  ON job_tracker.profile_skills (user_id, name);

-- ============================================================================
-- Feedback
-- ============================================================================

CREATE TABLE IF NOT EXISTS job_tracker.feedback (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     UUID REFERENCES auth.users(id) ON DELETE SET NULL,
  message     TEXT NOT NULL,
  context     JSONB,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS feedback_created_idx
  ON job_tracker.feedback (created_at DESC);
