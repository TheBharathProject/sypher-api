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

ALTER TABLE job_tracker.ai_reports
  ADD COLUMN report_json JSONB,
  ADD COLUMN draft_id    UUID
    REFERENCES job_tracker.resume_builder_drafts(id) ON DELETE SET NULL,
  ALTER COLUMN report_md DROP NOT NULL;
