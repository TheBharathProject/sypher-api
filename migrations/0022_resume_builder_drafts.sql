-- 0022_resume_builder_drafts.sql
-- Resume Builder module — drafts authored from scratch (not AI rewrites of
-- an uploaded resume; see docs/adr/0007 for the surface decision). Each
-- row holds the structured form payload as JSONB; the LaTeX template
-- consumes that payload at render time. Compiled PDFs land in
-- job_tracker.files (kind='resume') via the existing Vault flow — they
-- don't live in this table.
--
-- Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS job_tracker.resume_builder_drafts (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  title       TEXT NOT NULL,
  template_id TEXT NOT NULL,           -- 'classic-v1' in MVP; opens room for v2 multi-template
  content     JSONB NOT NULL,          -- form payload — see DraftContent in Go types
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- List queries are always user-scoped and order by recency.
CREATE INDEX IF NOT EXISTS resume_builder_drafts_user_idx
  ON job_tracker.resume_builder_drafts (user_id, updated_at DESC);
