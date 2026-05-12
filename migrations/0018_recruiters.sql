-- 0018_recruiters.sql
-- Personal recruiter CRM. Distinct from /community/recruiters (the public
-- directory). This table is per-user, private, and never publicly exposed.

CREATE TABLE IF NOT EXISTS job_tracker.recruiters (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  email        TEXT NOT NULL,
  company      TEXT,
  linkedin_url TEXT,
  notes        TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS recruiters_user_idx ON job_tracker.recruiters (user_id);
