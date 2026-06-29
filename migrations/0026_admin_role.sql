-- 0026_admin_role.sql
-- Adds the platform admin flag. Bootstrap: flip via manual SQL
-- (UPDATE auth.users SET is_admin = TRUE WHERE email = '...'),
-- same operating procedure as is_premium pre-billing (ADR-0002).
-- Idempotent: safe to re-run.

ALTER TABLE auth.users
  ADD COLUMN IF NOT EXISTS is_admin BOOLEAN NOT NULL DEFAULT FALSE;
