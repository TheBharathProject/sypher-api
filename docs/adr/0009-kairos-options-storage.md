# ADR-0009 — Kairos options chain storage (partitioned Postgres)

**Status:** Accepted
**Date:** 2026-05-17

## Context

Kairos is the second product on the Sypher platform: an options research and
backtesting tool. Its core artefact is the **option chain snapshot** — at
every minute during market hours, for every underlying we track
(NIFTY, BANKNIFTY, SENSEX), for every active expiry, for every strike
on either side (CE / PE), we record one row of market data.

Volume math, sized for what backtesting needs to be useful:

- 3 underlyings × ~80 in-band strikes × 2 sides (CE/PE) × ~5 active
  expiries = ~2,400 rows per snapshot.
- Snapshots every 60s during 09:15–15:30 IST = 375 snapshots/day.
- ~900,000 rows/day. ~250 trading days/year. ~225M rows/year.

This is a TS-shaped workload: append-heavy, range-scanned by
`(underlying, strike, ts_range)`, and very rarely updated. Retention
will be capped — backtests beyond ~3 years of history are a niche
feature, not a core promise.

Four storage options were considered:

1. **Plain Postgres table.** Single ~225M-row table per year. Index
   bloat, slow `DELETE` for retention, autovacuum pressure on hot tail.
2. **TimescaleDB hypertable.** Purpose-built for this. Auto-partitioning,
   continuous aggregates, compression. Extra extension to install on the
   managed Postgres; one more piece of infra to operate. ADR-0006
   pattern is "no new infra unless forced" — we're not at the scale
   that forces this yet.
3. **S3 + Parquet, queried via DuckDB.** Cheap storage, great for
   analytic scans. But: every backtest becomes a Lambda or in-process
   DuckDB query, not a SQL JOIN. Backtest worker has to learn a second
   data path. JSONB strategies (ADR-0012) and chain queries would
   live in different systems.
4. **Partitioned Postgres** via native `PARTITION BY RANGE`.
   Manageable, all-Postgres, no new ops surface.

## Decisions

### D1 — `kairos.option_chains`, `PARTITION BY RANGE (snapshot_date)`, weekly partitions

Parent declared in migration `0025_kairos_schema.sql`:

```sql
CREATE TABLE kairos.option_chains (
  id             BIGSERIAL,
  provider       TEXT        NOT NULL,            -- 'kite' | 'dhan' | ...
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
  snapshot_date  DATE        NOT NULL GENERATED ALWAYS AS (snapshot_time::date) STORED,
  PRIMARY KEY (id, snapshot_date)
) PARTITION BY RANGE (snapshot_date);
```

`snapshot_date` is a `GENERATED` column so partition routing is
deterministic and code never has to set it explicitly.

Weekly grain — not daily, not monthly — for two reasons:

- Daily = ~250 partitions/year. Each partition has its own indexes;
  query planner overhead from too many child tables becomes real.
- Monthly = ~3M rows per partition. Big enough that "drop a month"
  loses too much granularity if we want to selectively retain.
- Weekly = ~52 partitions/year, ~700K rows each. Goldilocks.

### D2 — Local-to-partition indexes, not global

```sql
CREATE INDEX option_chains_lookup_idx
  ON kairos.option_chains
  (underlying, expiry_date, snapshot_date, strike, option_type);
```

Declared on the parent — Postgres propagates to each new child
partition. The leading column is `underlying` because every query in
the codebase filters by it first; `expiry_date` second because
backtests pin a specific expiry per leg; `snapshot_date` third
because partition pruning happens before the index is even consulted,
so this column being in the index is mostly so the planner can use
index-only scans for time-range slices.

No global index. Global indexes across partitions would slow inserts
and block the partition-drop retention path (D5).

### D3 — Greeks are **computed**, never stored

The exchange doesn't broadcast greeks. Vendors that "include greeks"
have computed them with Black-Scholes; we can do the same and save
four numeric columns × 225M rows/year ≈ ~9GB/year of storage.

The `internal/kairos/greeks` package computes `IV`, `delta`, `gamma`,
`theta`, `vega` from `(ltp, spot, strike, dte, type)` at query time
using the same Abramowitz-Stegun CND approximation already in
`kairos/app/options/chain-data.ts:cnd()` (mirrored 1:1 on the Go side
so test fixtures cross-validate).

Side benefit: when a provider's IV computation method differs from
ours, our backtests stay consistent because we use our formula
on the raw data.

### D4 — Partition pre-creation via cron

Postgres will not auto-create child partitions; an insert into a date
range with no child fails with `no partition of relation ... found`.

`cron-once kairos-create-partitions` runs on the 1st of each month
and the 25th of each month, idempotently creating any missing weekly
partitions for **the next 60 days**. Idempotency via `CREATE TABLE
IF NOT EXISTS kairos.option_chains_2026_w20 PARTITION OF ...`. Running
twice a month means a forgotten run still has a 30-day buffer before
inserts start failing — far longer than the cron's interval.

Naming: `kairos.option_chains_YYYY_wNN` where NN is the ISO week
number. Year is included to make `2027_w01` unambiguous.

### D5 — Retention by partition drop, not `DELETE`

A separate cron job (`prune-old-partitions`, run weekly) lists
partitions older than `KAIROS_RETENTION_WEEKS` (default 156 = 3
years) and drops them. `DROP TABLE` on a partition is O(1) — no
table scan, no autovacuum churn.

Users on the premium tier (ADR-0006) get access to history older
than 6 months; free users are capped to the most recent 6 months
at query-handler level (not at the storage level — the data is
still there, just not exposed).

### D6 — One ingest target table, multiple providers tagged

Every row carries a `provider` column. We could theoretically run
two providers in parallel for redundancy / cross-validation in the
future, and we'd want to be able to tell them apart at query time.

For now, one provider is active at a time (ADR-0011); the column
is there as forward-compatibility, not a current feature.

## Consequences

**Positive:**

- All data in one Postgres instance. Backtest worker uses the same
  `pgx` pool as the rest of sypher-api. No second data system to
  authenticate against, monitor, or operate.
- Retention is `DROP TABLE`, which is free.
- Partition pruning at the planner level means a query for last
  week's data touches one ~700K-row table, not the whole 225M-row
  parent. Indexes stay small per partition.
- Storage cost is bounded and predictable: roughly (225M rows × 80
  bytes/row) ≈ 18GB/year. Three years at full ingestion is ~54GB,
  comfortably inside a single-node Postgres.

**Negative:**

- We own partition creation. If the cron silently fails for >30 days,
  inserts start failing and the live `/options` page goes stale. The
  monthly + mid-monthly schedule + idempotency make this a low-risk
  failure mode, but it is one. Cron failures already alert via the
  existing `cron.New` error logger.
- No materialized views or rollups yet. Backtests over 1y of
  minute-data scan ~3M rows for a typical 2-leg strategy. Measured
  on dev: ~1.8s for a 1y NIFTY straddle backtest on the warm pool.
  Acceptable inside the async worker (ADR-0010). If this becomes a
  hot path, a `(underlying, expiry, strike, day)` materialized
  view of EOD-closing prices would cut the bulk of it.
- TimescaleDB's continuous aggregates and per-row compression are
  forfeit. If volume grows 10× (we add 10 more underlyings, or move
  to second-grain data), revisit and likely move to Timescale.
