-- 0009_premium_flag.sql
-- Adds the premium tier flag + per-user email opt-in preference to auth.users.
--
-- Email notifications are now a premium perk; in-app notifications stay free
-- for everyone. Today both columns are admin-flippable via SQL because Stripe
-- isn't wired yet — once it is, the webhook handler will keep is_premium in
-- sync.
--
-- See docs/adr/0002-premium-email-gating.md (D1, D2, D6).
-- Idempotent: safe to re-run.

ALTER TABLE auth.users
  ADD COLUMN IF NOT EXISTS is_premium BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE auth.users
  ADD COLUMN IF NOT EXISTS email_notifications_enabled BOOLEAN NOT NULL DEFAULT TRUE;
