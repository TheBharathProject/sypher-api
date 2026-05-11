-- 0015_resume_tweaks.sql
-- Versioned AI resume tweaks. Every call to POST /ai/resume/tweak
-- writes one row here so users can come back later and see their
-- past iterations + continue refining from any version.
--
-- parent_id forms a revision chain: NULL = root tweak, non-NULL
-- points at the version this one was derived from. ON DELETE SET NULL
-- so deleting a parent doesn't cascade-wipe its descendants — the
-- chain just gets a NULL link in the middle.
--
-- application_id is optional: tweaks may be standalone (just resume
-- + JD, no tracked application) or tied to a specific row in
-- job_tracker.applications for easy lookup.
--
-- Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS job_tracker.resume_tweaks (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  parent_id       UUID REFERENCES job_tracker.resume_tweaks(id) ON DELETE SET NULL,
  application_id  UUID REFERENCES job_tracker.applications(id) ON DELETE SET NULL,
  source_file_id  UUID REFERENCES job_tracker.files(id) ON DELETE SET NULL,
  title           TEXT NOT NULL,                  -- e.g. "Stripe SDE — round 1"
  source_text     TEXT NOT NULL,                  -- the resume input at tweak time
  prompt          TEXT NOT NULL,                  -- the JD / cover letter the user provided
  tweaked_text    TEXT NOT NULL,                  -- AI output (markdown)
  user_edits      TEXT,                           -- optional post-AI manual edits
  tokens_in       INTEGER NOT NULL DEFAULT 0,
  tokens_out      INTEGER NOT NULL DEFAULT 0,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- "Show me my recent tweaks" query.
CREATE INDEX IF NOT EXISTS resume_tweaks_user_created_idx
  ON job_tracker.resume_tweaks (user_id, created_at DESC);

-- "Show me tweaks for this application."
CREATE INDEX IF NOT EXISTS resume_tweaks_user_app_idx
  ON job_tracker.resume_tweaks (user_id, application_id, created_at DESC)
  WHERE application_id IS NOT NULL;

-- "What was this version derived from / who derives from this version" —
-- partial because most root tweaks have no parent.
CREATE INDEX IF NOT EXISTS resume_tweaks_parent_idx
  ON job_tracker.resume_tweaks (parent_id)
  WHERE parent_id IS NOT NULL;
