-- 0001_waitlist.sql
-- Idempotent: safe to re-run.

CREATE SCHEMA IF NOT EXISTS waitlist;

CREATE TABLE IF NOT EXISTS waitlist.signups (
  id          BIGSERIAL PRIMARY KEY,
  email       TEXT NOT NULL,
  source      TEXT,
  user_agent  TEXT,
  referrer    TEXT,
  ip_hash     TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS signups_email_lower_idx
  ON waitlist.signups (lower(email));

CREATE INDEX IF NOT EXISTS signups_created_at_idx
  ON waitlist.signups (created_at DESC);

COMMENT ON TABLE waitlist.signups IS
  'Email signups from sypher.in waitlist form. Source field tracks the page/section that captured them.';
