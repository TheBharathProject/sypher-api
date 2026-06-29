-- 0027_kairos_v2.sql
-- Kairos v2: watchlists, alerts, paper trading, NSE announcements, screener
-- fundamentals, and ingest observability. See the kairos overhaul design
-- spec §2 (docs/superpowers/specs/2026-06-12-kairos-overhaul-design.md).
--
-- This migration creates:
--   - kairos.watchlists:          user symbol lists
--   - kairos.watchlist_items:     one row per (list, symbol)
--   - kairos.alerts:              price / pct-change alert rules
--   - kairos.paper_accounts:      one virtual-cash account per user
--   - kairos.paper_orders:        paper order book (EQ + option contracts)
--   - kairos.paper_positions:     net paper positions (signed qty)
--   - kairos.announcements:       NSE corporate announcements feed
--   - kairos.equity_fundamentals: screener ratio table (CSV-upserted)
--   - kairos.ingest_runs:         audit log for crons + manual ingests
--
-- Idempotent: safe to re-run.

CREATE SCHEMA IF NOT EXISTS kairos;

-- ============================================================================
-- watchlists / watchlist_items — user symbol lists. A default list is
-- auto-created on first GET (spec §3.4). Quotes are composed client-side
-- via the marketdata batch endpoint, so no price columns here.
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.watchlists (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     UUID        NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  name        TEXT        NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS watchlists_user_idx
  ON kairos.watchlists (user_id);

CREATE TABLE IF NOT EXISTS kairos.watchlist_items (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  watchlist_id  UUID        NOT NULL REFERENCES kairos.watchlists(id) ON DELETE CASCADE,
  symbol        TEXT        NOT NULL,
  exchange      TEXT        NOT NULL DEFAULT 'NSE',
  added_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (watchlist_id, symbol)
);

-- ============================================================================
-- alerts — evaluated by a 60s market-hours cron (spec §3.4). Trigger flips
-- status to 'triggered' and stamps triggered_at; the row stays in the feed.
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.alerts (
  id                 UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id            UUID          NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  symbol             TEXT,
  rule               TEXT          CHECK (rule IN ('price_above','price_below','pct_change_above','pct_change_below')),
  threshold          NUMERIC(12,2),
  status             TEXT          NOT NULL DEFAULT 'active' CHECK (status IN ('active','triggered','paused')),
  triggered_at       TIMESTAMPTZ,
  last_evaluated_at  TIMESTAMPTZ,
  created_at         TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

-- Evaluator pickup: the cron batches quotes by symbol over active alerts
-- only. Partial index keeps it tiny — triggered/paused rows live elsewhere.
CREATE INDEX IF NOT EXISTS alerts_active_idx
  ON kairos.alerts (symbol)
  WHERE status = 'active';

CREATE INDEX IF NOT EXISTS alerts_user_idx
  ON kairos.alerts (user_id, created_at DESC);

-- ============================================================================
-- paper_accounts — one virtual cash account per user, auto-provisioned with
-- ₹10,00,000 on first touch (spec §3.5). reset_at stamps the last reset.
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.paper_accounts (
  user_id           UUID          PRIMARY KEY REFERENCES auth.users(id) ON DELETE CASCADE,
  starting_capital  NUMERIC(14,2) NOT NULL DEFAULT 1000000.00,
  cash              NUMERIC(14,2) NOT NULL,
  created_at        TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
  reset_at          TIMESTAMPTZ
);

-- ============================================================================
-- paper_orders — paper order book. MARKET orders fill immediately; LIMIT
-- orders sit OPEN until the 60s sweep cron fills them (spec §3.5).
-- Option-contract columns (underlying/expiry/strike/opt_type) are NULL for
-- kind='EQ'.
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.paper_orders (
  id             UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id        UUID          NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  symbol         TEXT          NOT NULL,
  kind           TEXT          CHECK (kind IN ('EQ','OPT')),
  underlying     TEXT,
  expiry         DATE,
  strike         INT,
  opt_type       CHAR(2),                         -- 'CE' | 'PE'
  side           TEXT          CHECK (side IN ('BUY','SELL')),
  qty            INT           CHECK (qty > 0),
  order_type     TEXT          CHECK (order_type IN ('MARKET','LIMIT')),
  limit_price    NUMERIC(12,2),                   -- NULL for MARKET orders
  status         TEXT          CHECK (status IN ('OPEN','FILLED','CANCELLED','REJECTED')),
  reject_reason  TEXT,
  fill_price     NUMERIC(12,2),
  fees           NUMERIC(12,2),                   -- shared Indian cost model (costs.go)
  placed_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
  filled_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS paper_orders_user_idx
  ON kairos.paper_orders (user_id, placed_at DESC);

-- Sweep pickup: the limit-fill cron scans only OPEN orders. Partial index
-- keeps it tiny — filled/cancelled/rejected rows live elsewhere.
CREATE INDEX IF NOT EXISTS paper_orders_open_idx
  ON kairos.paper_orders (placed_at)
  WHERE status = 'OPEN';

-- ============================================================================
-- paper_positions — net position per (user, symbol). qty is signed
-- (negative = short); avg-price method with realized P&L (spec §3.5).
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.paper_positions (
  id            UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       UUID          NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  symbol        TEXT,
  kind          TEXT          CHECK (kind IN ('EQ','OPT')),
  underlying    TEXT,
  expiry        DATE,
  strike        INT,
  opt_type      CHAR(2),                          -- 'CE' | 'PE'
  qty           INT,                              -- signed; negative = short
  avg_price     NUMERIC(12,2),
  realized_pnl  NUMERIC(14,2) NOT NULL DEFAULT 0,
  updated_at    TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
  UNIQUE (user_id, symbol)
);

-- ============================================================================
-- announcements — NSE corporate announcements, ingested by a 10-min cron on
-- market days, deduped by nse_id (spec §3.4). raw keeps the source payload.
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.announcements (
  id              BIGSERIAL   PRIMARY KEY,
  nse_id          TEXT        UNIQUE,
  symbol          TEXT,
  company         TEXT,
  category        TEXT,
  headline        TEXT,
  attachment_url  TEXT,
  announced_at    TIMESTAMPTZ,
  raw             JSONB,
  ingested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS announcements_time_idx
  ON kairos.announcements (announced_at DESC);

CREATE INDEX IF NOT EXISTS announcements_symbol_idx
  ON kairos.announcements (symbol, announced_at DESC);

-- ============================================================================
-- equity_fundamentals — screener ratio table, upserted from admin CSV
-- uploads (spec §3.7). Live prices come from marketdata, not this table.
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.equity_fundamentals (
  symbol        TEXT        PRIMARY KEY,
  name          TEXT,
  sector        TEXT,
  mkt_cap_cr    NUMERIC,                          -- market cap, ₹ crore
  pe            NUMERIC,
  roce          NUMERIC,
  roe           NUMERIC,
  de            NUMERIC,                          -- debt-to-equity
  np_y1         NUMERIC,                          -- net profit, latest year
  np_y2         NUMERIC,                          -- net profit, prior year
  opm           NUMERIC,                          -- operating profit margin %
  cagr3_profit  NUMERIC,
  cagr3_sales   NUMERIC,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================================
-- ingest_runs — one row per cron run / manual snapshot / export, for the
-- admin ingest-runs log (spec §3.8). ok stays NULL while a run is in flight.
-- ============================================================================

CREATE TABLE IF NOT EXISTS kairos.ingest_runs (
  id            BIGSERIAL   PRIMARY KEY,
  job           TEXT        NOT NULL,
  started_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  finished_at   TIMESTAMPTZ,
  ok            BOOLEAN,
  rows_written  INT,
  detail        TEXT
);

CREATE INDEX IF NOT EXISTS ingest_runs_job_idx
  ON kairos.ingest_runs (job, started_at DESC);
