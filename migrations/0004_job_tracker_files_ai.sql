-- 0004_job_tracker_files_ai.sql
-- Phase 2: file metadata (R2-backed) + AI reports + AI usage metering.
-- Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS job_tracker.files (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,            -- 'resume' | 'cover_letter'
  label        TEXT,
  file_name    TEXT NOT NULL,
  file_size    BIGINT NOT NULL DEFAULT 0,
  mime_type    TEXT,
  storage_key  TEXT NOT NULL,
  uploaded_at  TIMESTAMPTZ,              -- null until client finalizes
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS files_storage_key_idx
  ON job_tracker.files (storage_key);

CREATE INDEX IF NOT EXISTS files_user_kind_idx
  ON job_tracker.files (user_id, kind, created_at DESC);

CREATE TABLE IF NOT EXISTS job_tracker.ai_reports (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  resume_file_id  UUID REFERENCES job_tracker.files(id) ON DELETE SET NULL,
  report_md       TEXT NOT NULL,
  score           INTEGER,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS ai_reports_user_idx
  ON job_tracker.ai_reports (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS job_tracker.ai_usage (
  id          BIGSERIAL PRIMARY KEY,
  user_id     UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  endpoint    TEXT NOT NULL,             -- 'resume_report' | 'cover_letter'
  tokens_in   INTEGER NOT NULL DEFAULT 0,
  tokens_out  INTEGER NOT NULL DEFAULT 0,
  cost_usd    NUMERIC(10,6),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS ai_usage_user_idx
  ON job_tracker.ai_usage (user_id, created_at DESC);
