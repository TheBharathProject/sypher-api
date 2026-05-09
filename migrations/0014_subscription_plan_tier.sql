-- 0014_subscription_plan_tier.sql
-- Adds plan_tier to billing.subscriptions so we can differentiate
-- pricing tiers within the same kind ('recurring').
--
-- Today there's exactly one recurring tier (₹99/mo, premium-only).
-- Phase 6c introduces 'plus' (₹299/mo, premium + 200 credits granted
-- on every successful charge). Each tier has its own Razorpay plan_id
-- in env (RAZORPAY_PLAN_ID, RAZORPAY_PLAN_ID_PLUS), but at the DB
-- level we just need the discriminator so the webhook can decide
-- whether to grant the bundle credits when subscription.charged fires.
--
-- Existing rows default to 'standard' so the email-gate behaviour
-- doesn't change for users already on the ₹99 plan.
--
-- Idempotent: safe to re-run.

ALTER TABLE billing.subscriptions
  ADD COLUMN IF NOT EXISTS plan_tier TEXT NOT NULL DEFAULT 'standard'
  CHECK (plan_tier IN ('standard', 'plus'));
