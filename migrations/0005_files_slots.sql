-- 0005_files_slots.sql
-- Adds slot positioning to job_tracker.files so the vault UI can present
-- 5 fixed slots per kind (resume / cover_letter) the way the original
-- frontend modelled them.
-- Idempotent: safe to re-run.

ALTER TABLE job_tracker.files
  ADD COLUMN IF NOT EXISTS slot INTEGER;

-- Enforce uniqueness per (user, kind, slot) when a slot is set.
CREATE UNIQUE INDEX IF NOT EXISTS files_user_kind_slot_idx
  ON job_tracker.files (user_id, kind, slot)
  WHERE slot IS NOT NULL;
