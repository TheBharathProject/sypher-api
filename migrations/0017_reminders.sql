-- 0017_reminders.sql
-- Per-application reminders. A reminder fires a notification when
-- triggers_at passes. The cron job sets fired_at to prevent double-fire.
-- Reminder notifications use kind = 'reminder', ref_type = 'application'.

CREATE TABLE IF NOT EXISTS job_tracker.reminders (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id        UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  application_id UUID NOT NULL REFERENCES job_tracker.applications(id) ON DELETE CASCADE,
  triggers_at    TIMESTAMPTZ NOT NULL,
  note           TEXT,
  fired_at       TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS reminders_user_app_idx
  ON job_tracker.reminders (user_id, application_id);

-- Partial index over due-but-unfired rows — the cron job queries this path.
CREATE INDEX IF NOT EXISTS reminders_due_idx
  ON job_tracker.reminders (triggers_at)
  WHERE fired_at IS NULL;
