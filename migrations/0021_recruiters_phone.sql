-- 0021_recruiters_phone.sql
-- Add optional phone number to the private recruiter contacts table.

ALTER TABLE job_tracker.recruiters
  ADD COLUMN IF NOT EXISTS phone TEXT;
