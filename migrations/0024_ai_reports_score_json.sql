-- 0024_ai_reports_score_json.sql
--
-- Resume Score upgrade: the Resume AI report flow now produces structured
-- JSON (sectional scores + Fix/Improve/Good findings) instead of a single
-- Markdown blob with one score. Add the JSON column + an optional draft_id
-- so reports generated from a Resume Builder draft can be linked back.
--
-- Old rows keep their report_md and continue to render via the legacy
-- Markdown path. New rows populate report_json and leave report_md NULL.
-- The list/get endpoints dispatch on whichever column is populated.

-- Idempotent: the column may already exist from a hand-run during early
-- iteration. IF NOT EXISTS means re-running this file is a no-op.
ALTER TABLE job_tracker.ai_reports
  ADD COLUMN IF NOT EXISTS report_json JSONB,
  ADD COLUMN IF NOT EXISTS draft_id    UUID;

-- The FK has to be added separately because IF NOT EXISTS doesn't work on
-- ADD CONSTRAINT. Wrap in a check so re-runs don't fail.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'ai_reports_draft_id_fkey'
      AND conrelid = 'job_tracker.ai_reports'::regclass
  ) THEN
    ALTER TABLE job_tracker.ai_reports
      ADD CONSTRAINT ai_reports_draft_id_fkey
        FOREIGN KEY (draft_id)
        REFERENCES job_tracker.resume_builder_drafts(id)
        ON DELETE SET NULL;
  END IF;
END $$;

-- DROP NOT NULL is naturally idempotent — no-op when already nullable.
ALTER TABLE job_tracker.ai_reports
  ALTER COLUMN report_md DROP NOT NULL;
