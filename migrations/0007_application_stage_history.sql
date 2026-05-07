-- 0007_application_stage_history.sql
-- Records every stage transition on a job application so the UI can render
-- a true timeline ("Moved from INTERESTED to TECHNICAL · 2 days ago").
-- Inserted by the application handlers on create and on stage-change update.
-- Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS job_tracker.application_stage_history (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  application_id  UUID NOT NULL REFERENCES job_tracker.applications(id) ON DELETE CASCADE,
  from_stage      TEXT,                              -- NULL on the initial create row
  to_stage        TEXT NOT NULL,
  changed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS app_stage_history_app_idx
  ON job_tracker.application_stage_history (application_id, changed_at);

CREATE INDEX IF NOT EXISTS app_stage_history_user_idx
  ON job_tracker.application_stage_history (user_id, changed_at DESC);

COMMENT ON TABLE job_tracker.application_stage_history IS
  'Append-only log of stage transitions per application. The timeline UI reads this in chronological order.';
