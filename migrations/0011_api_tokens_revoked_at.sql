-- 0011_api_tokens_revoked_at.sql
-- Adds revoke support to the existing auth.api_tokens table.
--
-- Phase 5 (browser extension wiring): the Settings UI needs a list +
-- revoke flow for the long-lived `pg_` tokens that authenticate the
-- Pegasus job extractor extension. Until now we only had insert + lookup
-- (lookup bumps last_used_at). A revoked token must stop resolving in
-- LookupAPIToken even if its hash is still on the row.
--
-- We don't hard-delete revoked rows so audit history (which user-agent
-- last hit it, when) survives a revoke.
--
-- Idempotent: safe to re-run.

ALTER TABLE auth.api_tokens
  ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;

-- Lookup index restricted to active tokens. Auth middleware hits this on
-- every extension request; keeping revoked rows out of the index makes
-- the cardinality match the working-set size.
CREATE INDEX IF NOT EXISTS api_tokens_active_idx
  ON auth.api_tokens (token_hash)
  WHERE revoked_at IS NULL;
