-- 0006_field_alignment.sql
-- Aligns the job_tracker schema with the live naukriclear.com API contract.
-- Idempotent. ADD COLUMN uses IF NOT EXISTS; renames are guarded by an
-- information_schema check so re-runs are a no-op.
--
-- Old columns intentionally retained where they exist (period on
-- experiences/educations) so we don't lose data — they're just not
-- returned in any response.

-- ============================================================================
-- Notes: body -> content
-- ============================================================================
DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='notes' AND column_name='body'
  ) THEN
    ALTER TABLE job_tracker.notes RENAME COLUMN body TO content;
  END IF;
END $$;

-- ============================================================================
-- Applications: description -> job_description, add stage_changed_at
-- ============================================================================
DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='applications' AND column_name='description'
  ) THEN
    ALTER TABLE job_tracker.applications RENAME COLUMN description TO job_description;
  END IF;
END $$;

ALTER TABLE job_tracker.applications
  ADD COLUMN IF NOT EXISTS stage_changed_at TIMESTAMPTZ;

-- Backfill stage_changed_at from updated_at where unset
UPDATE job_tracker.applications
   SET stage_changed_at = updated_at
 WHERE stage_changed_at IS NULL;

-- ============================================================================
-- Profile root: social URLs
-- ============================================================================
ALTER TABLE job_tracker.profiles
  ADD COLUMN IF NOT EXISTS linkedin_url TEXT,
  ADD COLUMN IF NOT EXISTS github_url   TEXT,
  ADD COLUMN IF NOT EXISTS website_url  TEXT;

-- ============================================================================
-- Profile experiences: structured dates + location, renames
-- ============================================================================
ALTER TABLE job_tracker.profile_experiences
  ADD COLUMN IF NOT EXISTS location   TEXT,
  ADD COLUMN IF NOT EXISTS start_date DATE,
  ADD COLUMN IF NOT EXISTS end_date   DATE,
  ADD COLUMN IF NOT EXISTS current    BOOLEAN NOT NULL DEFAULT FALSE;

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='profile_experiences' AND column_name='summary'
  ) THEN
    ALTER TABLE job_tracker.profile_experiences RENAME COLUMN summary TO description;
  END IF;
END $$;

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='profile_experiences' AND column_name='ordinal'
  ) THEN
    ALTER TABLE job_tracker.profile_experiences RENAME COLUMN ordinal TO sort_order;
  END IF;
END $$;

-- ============================================================================
-- Profile educations: dates + extras, renames
-- ============================================================================
ALTER TABLE job_tracker.profile_educations
  ADD COLUMN IF NOT EXISTS field       TEXT,
  ADD COLUMN IF NOT EXISTS start_date  DATE,
  ADD COLUMN IF NOT EXISTS end_date    DATE,
  ADD COLUMN IF NOT EXISTS gpa         TEXT,
  ADD COLUMN IF NOT EXISTS description TEXT;

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='profile_educations' AND column_name='ordinal'
  ) THEN
    ALTER TABLE job_tracker.profile_educations RENAME COLUMN ordinal TO sort_order;
  END IF;
END $$;

-- ============================================================================
-- Profile projects: tech_stack + link, renames
-- ============================================================================
ALTER TABLE job_tracker.profile_projects
  ADD COLUMN IF NOT EXISTS tech_stack TEXT,
  ADD COLUMN IF NOT EXISTS link       TEXT;

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='profile_projects' AND column_name='summary'
  ) THEN
    ALTER TABLE job_tracker.profile_projects RENAME COLUMN summary TO description;
  END IF;
END $$;

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema='job_tracker' AND table_name='profile_projects' AND column_name='ordinal'
  ) THEN
    ALTER TABLE job_tracker.profile_projects RENAME COLUMN ordinal TO sort_order;
  END IF;
END $$;

-- ============================================================================
-- Profile skills: add sort_order
-- ============================================================================
ALTER TABLE job_tracker.profile_skills
  ADD COLUMN IF NOT EXISTS sort_order INTEGER NOT NULL DEFAULT 0;
