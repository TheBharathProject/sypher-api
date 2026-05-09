-- 0012_api_tokens_scopes.sql
-- Restricts what a long-lived browser-extension token can do.
--
-- Until now an api_token authenticated against every /job-tracker/*
-- endpoint behind RequireUser — a leaked extension token effectively
-- had full account access (delete account, edit posts, change slug, …).
-- Phase 5 ships a single scope `extension:capture` that's narrowly
-- limited to the three routes the extension actually uses:
--
--   GET  /job-tracker/me
--   GET  /job-tracker/applications/check-link
--   POST /job-tracker/applications
--
-- The middleware reads `scopes` after resolving the token; if the
-- token's scope set is non-empty AND none of those scopes whitelist the
-- current route, the request is rejected with 403.
--
-- An empty scopes array is a "full-access" legacy token — kept for
-- backward compat in case any old tokens exist; new tokens are issued
-- with `['extension:capture']` by default.
--
-- Idempotent: safe to re-run.

ALTER TABLE auth.api_tokens
  ADD COLUMN IF NOT EXISTS scopes TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[];
