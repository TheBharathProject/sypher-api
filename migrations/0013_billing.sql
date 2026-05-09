-- 0013_billing.sql
-- Phase 6b: Razorpay-backed billing.
--
-- Three charge types coexist via a single webhook endpoint:
--   1. Recurring premium  — Razorpay subscription (sub_xxx).
--   2. One-time premium   — Razorpay order (order_xxx) → 30-day pass.
--   3. Credits top-up     — Razorpay order → increment auth.users
--                           .credits_balance + ledger row.
--
-- Schema design (ADR-0006 D1):
--   - billing.subscriptions    — premium-tier state (kinds 1 + 2).
--   - billing.credit_transactions — append-only ledger of credit moves.
--   - billing.processed_events — webhook idempotency.
--   - auth.users.credits_balance — denormalised ledger sum, updated
--                                  inside the same txn as each ledger
--                                  row so reads stay O(1).
--
-- Idempotent: safe to re-run.

CREATE SCHEMA IF NOT EXISTS billing;

CREATE TABLE IF NOT EXISTS billing.subscriptions (
  id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id                  UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  -- 'recurring' = Razorpay subscription with eMandate / autopay.
  -- 'one_time'  = single order; current_period_end = paid_at + 30d;
  --                 cron flips status='expired' when ttl elapses.
  kind                     TEXT NOT NULL CHECK (kind IN ('recurring','one_time')),
  -- 'pending'   = checkout started, awaiting Razorpay webhook.
  -- 'active'    = paid + currently within the period.
  -- 'halted'    = recurring renewal failed; user lost premium.
  -- 'cancelled' = user requested cancel (recurring only); usually
  --                 stays 'active' until current_period_end then flips
  --                 'cancelled' on subscription.cancelled webhook.
  -- 'expired'   = one_time pass aged out; or final state for cancelled
  --                 recurring after the period ended.
  status                   TEXT NOT NULL CHECK (status IN ('pending','active','halted','cancelled','expired')),
  -- Mutually exclusive: exactly one is non-NULL.
  razorpay_subscription_id TEXT UNIQUE,
  razorpay_order_id        TEXT UNIQUE,
  -- Most-recent successful payment_id (set once on first success).
  razorpay_payment_id      TEXT,
  amount_paise             INT NOT NULL,
  currency                 TEXT NOT NULL DEFAULT 'INR',
  -- NULL until first payment lands. For recurring this advances on
  -- every subscription.charged webhook; for one_time it's set once.
  current_period_end       TIMESTAMPTZ,
  -- Recurring only: user requested cancel; we honour by NOT renewing
  -- (Razorpay handles the rest). Display shows "expires on YYYY-MM-DD".
  cancel_at_period_end     BOOLEAN NOT NULL DEFAULT FALSE,
  created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Lookup by user, only active rows — tiny index, hot path for /billing/me
-- and the email-gate refresh helper.
CREATE INDEX IF NOT EXISTS subs_user_active_idx
  ON billing.subscriptions (user_id)
  WHERE status = 'active';

-- Cron job for expiring one-time passes scans this index daily.
CREATE INDEX IF NOT EXISTS subs_period_end_idx
  ON billing.subscriptions (current_period_end)
  WHERE status = 'active' AND kind = 'one_time';

CREATE TABLE IF NOT EXISTS billing.credit_transactions (
  id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id                  UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  -- Positive on top-up (Razorpay payment.captured), negative on
  -- consumption (AI handlers calling Store.SpendCredits). NEVER zero.
  delta                    INT NOT NULL CHECK (delta <> 0),
  -- Free-text label. Conventional values used today:
  --   'razorpay_purchase' — credit pack bought via checkout.
  --   'admin_grant'       — manual SQL adjustment by support.
  --   'refund'            — corresponding negative for a refunded purchase.
  --   'ai_*'              — consumption (e.g. 'ai_cover_letter'); reserved
  --                         for Phase 7 AI-handler wiring.
  reason                   TEXT NOT NULL,
  -- Cached running total at the moment this txn was applied. Lets the
  -- ledger view show "balance after this row" without a recompute.
  balance_after            INT NOT NULL,
  -- Set on purchase rows (Razorpay correlation); NULL on consumption.
  razorpay_payment_id      TEXT,
  razorpay_order_id        TEXT,
  -- The thing that triggered consumption. ref_type is loose ('application',
  -- 'resume', 'cover_letter') so adding a new AI feature doesn't need a
  -- migration.
  ref_type                 TEXT,
  ref_id                   UUID,
  created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS credit_tx_user_idx
  ON billing.credit_transactions (user_id, created_at DESC);

-- Webhook idempotency: every Razorpay event has a stable event_id; we
-- INSERT first, swallow 23505 (duplicate key) as "already processed".
CREATE TABLE IF NOT EXISTS billing.processed_events (
  event_id    TEXT PRIMARY KEY,
  received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Cached credit balance on the user row. The ledger is authoritative;
-- this column is updated atomically inside the same txn as each ledger
-- insert. Reads (e.g. AI handlers checking pre-call) stay O(1).
ALTER TABLE auth.users
  ADD COLUMN IF NOT EXISTS credits_balance INT NOT NULL DEFAULT 0;
