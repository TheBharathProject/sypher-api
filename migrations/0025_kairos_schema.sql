-- 0025_kairos_schema.sql
-- Kairos = options research + backtesting tool. Second product on the Sypher
-- platform after Pegasus. See ADR-0009 through ADR-0014 for the design.
--
-- This migration creates:
--   - kairos schema
--   - kairos.option_chains: partitioned-by-week table of options snapshots
--   - kairos.strategies:    user-saved strategy DSL (JSONB legs)
--   - kairos.backtests:     async backtest jobs + results
--   - kairos.provider_tokens: encrypted broker access tokens (1 row/provider)
--
-- Nothing else. Stock OHLCV, fundamentals, announcements are not stored —
-- they flow through the in-memory marketdata cache (ADR-0014).

CREATE SCHEMA IF NOT EXISTS kairos;

-- ============================================================================
-- option_chains — partitioned by snapshot_date (week-grain children).
-- See ADR-0009 D1 for partition-key rationale.
-- ============================================================================

-- Partition key is snapshot_time directly (TIMESTAMPTZ). We originally
-- tried a STORED generated column `snapshot_date GENERATED AS
-- (snapshot_time::date)`, but Postgres rejects the cast as not-immutable
-- (timestamptz → date depends on session TimeZone). Partitioning on the
-- timestamp itself works just as well — week boundaries are expressed as
-- explicit UTC timestamps, queries that filter by snapshot_time or a
-- date range still get partition pruning, and the application doesn't
-- have to set a separate snapshot_date field on every insert.
CREATE TABLE IF NOT EXISTS kairos.option_chains (
  id             BIGSERIAL,
  provider       TEXT        NOT NULL,            -- 'kite' | 'dhan' | 'upstox' | 'angel' | 'null'
  underlying     TEXT        NOT NULL,            -- 'NIFTY' | 'BANKNIFTY' | 'SENSEX'
  expiry_date    DATE        NOT NULL,
  strike         INT         NOT NULL,
  option_type    CHAR(2)     NOT NULL,            -- 'CE' | 'PE'
  snapshot_time  TIMESTAMPTZ NOT NULL,
  spot           NUMERIC(12,2),
  ltp            NUMERIC(10,2),
  bid            NUMERIC(10,2),
  ask            NUMERIC(10,2),
  oi             BIGINT,
  oi_change      BIGINT,
  volume         BIGINT,
  PRIMARY KEY (id, snapshot_time)
) PARTITION BY RANGE (snapshot_time);

-- Lookup index — declared on parent so Postgres propagates to each new
-- weekly child partition. Column order targets the hot query shape:
-- "give me rows for underlying X, expiry Y, in time range Z, at strike S
-- on side T". ADR-0009 D2.
CREATE INDEX IF NOT EXISTS option_chains_lookup_idx
  ON kairos.option_chains
  (underlying, expiry_date, snapshot_time, strike, option_type);

-- The first three weekly partitions, so the table is immediately usable
-- after deploy. The kairos-create-partitions cron job (ADR-0009 D4) then
-- maintains the rolling window.
--
-- Boundaries are explicit UTC timestamps. Week starts on Monday 00:00 UTC.

DO $$
DECLARE
  wk_start TIMESTAMPTZ := DATE_TRUNC('week', CURRENT_DATE AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
  i        INT;
  child    TEXT;
  start_t  TIMESTAMPTZ;
  end_t    TIMESTAMPTZ;
  iso_year INT;
  iso_week INT;
BEGIN
  FOR i IN 0..2 LOOP
    start_t := wk_start + (i || ' weeks')::INTERVAL;
    end_t   := start_t + INTERVAL '7 days';
    iso_year := EXTRACT(ISOYEAR FROM start_t);
    iso_week := EXTRACT(WEEK FROM start_t);
    child := FORMAT('kairos.option_chains_%s_w%s', iso_year, LPAD(iso_week::text, 2, '0'));
    EXECUTE FORMAT(
      'CREATE TABLE IF NOT EXISTS %s PARTITION OF kairos.option_chains FOR VALUES FROM (%L) TO (%L)',
      child, start_t, end_t
    );
  END LOOP;
END $$;

-- ============================================================================
-- strategies — user-saved strategies. legs is JSONB (ADR-0012 D1).
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.strategies (
  id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     UUID         NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  name        TEXT         NOT NULL,
  underlying  TEXT         NOT NULL,             -- 'NIFTY' | 'BANKNIFTY' | 'SENSEX'
  legs        JSONB        NOT NULL,
  entry_time  TEXT         NOT NULL DEFAULT '09:20',
  exit_time   TEXT         NOT NULL DEFAULT '15:15',
  stop_loss   NUMERIC(5,2) NOT NULL DEFAULT 0,   -- % of premium, 0 = disabled
  target      NUMERIC(5,2) NOT NULL DEFAULT 0,   -- % of premium, 0 = disabled
  created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS strategies_user_idx
  ON kairos.strategies (user_id, created_at DESC);

-- ============================================================================
-- backtests — async job rows. status moves pending → running → done|failed.
-- See ADR-0010 D4 for the lifecycle.
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.backtests (
  id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID         NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  -- strategy_id is SET NULL on delete: a saved strategy can be deleted, but
  -- its backtest results are still valid history. The frozen legs JSONB
  -- below is what makes the result reproducible.
  strategy_id  UUID         REFERENCES kairos.strategies(id) ON DELETE SET NULL,
  name         TEXT         NOT NULL,
  underlying   TEXT         NOT NULL,
  legs         JSONB        NOT NULL,            -- copy of strategy.legs at run time
  from_date    DATE         NOT NULL,
  to_date      DATE         NOT NULL,
  entry_time   TEXT         NOT NULL,
  exit_time    TEXT         NOT NULL,
  stop_loss    NUMERIC(5,2),
  target       NUMERIC(5,2),
  status       TEXT         NOT NULL DEFAULT 'pending', -- pending|running|done|failed
  result       JSONB,                            -- { totalPnl, winRate, sharpe, maxDrawdown, trades }
  error        TEXT,
  started_at   TIMESTAMPTZ,
  finished_at  TIMESTAMPTZ,
  created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS backtests_user_idx
  ON kairos.backtests (user_id, created_at DESC);

-- Pending-pickup index: the startup sweep (ADR-0010 D5) selects the oldest
-- pending rows. Partial index keeps it tiny — only un-done rows live here.
CREATE INDEX IF NOT EXISTS backtests_pending_idx
  ON kairos.backtests (created_at)
  WHERE status IN ('pending', 'running');

-- ============================================================================
-- provider_tokens — encrypted broker access tokens. One row per provider.
-- See ADR-0011 D5.
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.provider_tokens (
  provider      TEXT         PRIMARY KEY,        -- 'kite' | 'dhan' | 'upstox' | 'angel'
  access_token  TEXT         NOT NULL,           -- encrypted via internal/security/aead
  refresh_token TEXT,                            -- encrypted; nullable (some providers don't have one)
  expires_at    TIMESTAMPTZ,                     -- nullable; nil for long-lived tokens
  metadata      JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- provider-specific extras (user_id, feed_token, etc.)
  updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
