-- 0002_auth.sql
-- Shared authentication: users + per-user API tokens (e.g. browser extension).
-- Idempotent: safe to re-run.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE SCHEMA IF NOT EXISTS auth;

CREATE TABLE IF NOT EXISTS auth.users (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  google_id    TEXT NOT NULL,
  email        TEXT NOT NULL,
  name         TEXT NOT NULL DEFAULT '',
  picture_url  TEXT,
  timezone     TEXT NOT NULL DEFAULT 'UTC',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS users_google_id_idx
  ON auth.users (google_id);

CREATE INDEX IF NOT EXISTS users_email_lower_idx
  ON auth.users (lower(email));

COMMENT ON TABLE auth.users IS
  'Identity records. One row per Google account that has ever signed in.';

CREATE TABLE IF NOT EXISTS auth.api_tokens (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  token_hash   TEXT NOT NULL,
  prefix       TEXT NOT NULL,
  label        TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_used_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS api_tokens_hash_idx
  ON auth.api_tokens (token_hash);

CREATE INDEX IF NOT EXISTS api_tokens_user_idx
  ON auth.api_tokens (user_id, created_at DESC);

COMMENT ON TABLE auth.api_tokens IS
  'Long-lived per-user tokens for the browser extension. token_hash is sha256(token).';
