# ADR-0014 — Kairos storage boundary (only options data persists)

**Status:** Accepted
**Date:** 2026-05-17

## Context

Kairos has 22 routes. Several of them — `/charts`, `/dashboard`,
`/watchlist`, `/screener`, `/fundamentals`, `/announcements` —
display equity or market-wide data (stock OHLCV, live spot prices,
fundamentals, corporate actions). Only one page,
`/options`, plus its backing feature `/strategies` (backtester),
need data we ourselves generate and own.

The question this ADR resolves: **which datasets does Kairos
persist in its own database, and which ones flow through on
demand?**

The asymmetry between options and equity data:

- **Options chain history at 1m granularity is not commercially
  available to retail in India for less than ~₹3,000–8,000/mo.**
  Sensibull, Opstra, and similar tools have collected this
  themselves over years; it's their moat. Backtesting that
  reproduces real option prices needs this data. If we collect it
  ourselves via the 60s ingest cron (ADR-0011), every passing
  day adds to a compounding asset.
- **Equity OHLCV, live quotes, fundamentals, and announcements
  are commodity data.** Kite/Dhan/Upstox/Angel all give us this
  on demand, free with a basic subscription. Many free APIs
  (Yahoo, NSE bhavcopy, etc.) also serve it. Persisting it gives
  no compounding edge — the same data is available to anyone with
  a broker key.

Storing equity data anyway costs ~the same DB size as options
data at 1m granularity (~2,000 actively-traded NSE stocks × 375
ticks/day × 250 days = ~187M rows/year). Same operational
weight; zero moat upside. The decision is therefore obvious
once it's framed.

## Decisions

### D1 — Only `kairos.option_chains` carries persistent market data

The four tables in `kairos` schema (migration 0025):

| Table | What it stores | Why it's stored |
|---|---|---|
| `kairos.option_chains` | Minute-by-minute option snapshots (LTP, OI, volume) | The moat: cannot be re-fetched once expired |
| `kairos.strategies` | User-saved strategy DSL (legs, times, SL%, target%) | User-owned data; must persist across sessions |
| `kairos.backtests` | Backtest job + result JSONB | User-owned data; reproducibility requires legs snapshot |
| `kairos.provider_tokens` | Encrypted broker access tokens | Cross-restart auth state |

No `kairos.stocks`, no `kairos.fundamentals`, no
`kairos.announcements`, no `kairos.candles`. If a route needs
equity data, it goes through D2.

### D2 — Equity/market data is a read-through cache in
`internal/kairos/marketdata`

A new package separate from `provider/`:

```
internal/kairos/marketdata/
  cache.go      // in-memory cache, sync.Map + LRU eviction
  quote.go      // GetQuote(symbol) → live last-trade, 5s TTL
  candles.go    // GetCandles(symbol, interval, from, to) → OHLCV
  fundamentals.go // GetFundamentals(symbol) → P/E, ROE, etc.
```

Cache lookup order:

1. In-memory hit and within TTL → return.
2. Miss / stale → call `provider.BrokerProvider` method (e.g.
   `FetchHistoricalCandles`, `FetchSpot`) or, for non-broker
   data, the appropriate external fetcher.
3. Store result in cache with type-specific TTL.
4. Return.

### D3 — TTLs by data type

| Data | TTL | Reason |
|---|---|---|
| Live quote (last-trade) | 5s | Matches the perceptual freshness of broker quote feeds |
| Intraday OHLCV (today's bars) | 30s | Same minute may add more ticks; let it settle |
| Daily OHLCV (historical days) | 6h | Settled data, won't change today |
| Fundamentals (P/E, ROE, etc.) | 24h | Quarterly reporting cadence; no need to be fresher |
| Announcements | 24h | Polled by a separate daily job; cache between polls |

Negative caching (a 404 from the provider) gets a 60s TTL — long
enough to absorb a hammering frontend, short enough to recover
when the symbol becomes available.

### D4 — Cache is in-process, sized + LRU-evicted, no Redis

`sync.Map` keyed by `"<datatype>:<symbol>:<args>"` with a
side-channel size-bounded LRU. Max ~5,000 entries (enough for
the top ~2,000 NSE stocks across 2–3 data types). Eviction is
size-driven first, TTL-driven second.

**Why not Redis:** one binary, one Postgres, one frontend — the
ADR-0006 "no new infra" pattern. The cache is a performance
optimisation, not a correctness requirement. Lost on restart;
warm-up takes seconds.

**Cross-pod consistency:** we run one API pod today. If we
horizontally scale, two pods will have independent caches and
both will hit the provider. Acceptable until traffic grows;
revisit with a shared Redis only when the provider's rate limits
start biting.

### D5 — Fundamentals & announcements: external source TBD

Brokers don't reliably serve fundamentals or corporate
announcements. Options for fundamentals:

- **NSE bhavcopy + manual extraction.** Free, requires parsing.
- **screener.in scrape.** Not licensed; brittle.
- **A paid API** (Mosaic, Tijori, etc.). ~₹2,000–5,000/mo if it
  comes to that.

Until one is picked, `/screener` and `/fundamentals` stay on
sample data (the 20-stock dataset already shipped in
`kairos/app/screener/sample-data.ts`). The `marketdata.GetFundamentals`
function returns `ErrNotConfigured` and the handlers return
sample data when in dev or 503 in prod.

Announcements: NSE publishes a JSON endpoint
(`/api/corporate-announcements`) that's scrapeable with the same
session-cookie pattern as their options chain. A daily cron job
to refresh this is straightforward and is queued for after the
options pipeline is live.

### D6 — Per-user broker data (portfolio, orders) stays out of D2

Holdings, positions, orders are user-specific. They cannot share
a cache across users; they require per-user OAuth (not the
shared platform key). Those endpoints will be a separate
package (`internal/kairos/user_broker/`) when wired, with no
caching — every read goes to the user's broker. Out of scope
for this ADR; flagged so it's not lumped in.

### D7 — `/charts` page is a marketdata pass-through

When `/charts` requests OHLCV for RELIANCE on a 5-min timeframe:

1. `GET /kairos/marketdata/candles?symbol=RELIANCE&interval=5minute&from=...&to=...`
2. Handler calls `marketdata.GetCandles(...)`.
3. Cache miss → `provider.FetchHistoricalCandles(...)` → Kite REST
   call (~150ms).
4. Result cached, returned to frontend.
5. Frontend renders with TradingView Lightweight Charts (or
   equivalent — the existing fake SVG in `app/charts/page.tsx`
   is replaced when the feature ships).

Subsequent users in the next 6 hours requesting the same
(symbol, interval, range) → cache hit, <1ms.

## Consequences

**Positive:**

- One persistent dataset to operate, monitor, back up, retain,
  prune (ADR-0009). DB size is bounded and predictable.
- The DB is the moat. Anything we lose access to from the
  provider (rate-limited, key revoked, vendor switch) doesn't
  cost us the options history — that's already in our DB.
- New equity-dependent features ship without a migration. A
  page that needs sector P/E ratios just adds a `marketdata`
  method.
- Cost scales with users and traffic, not with stored data
  volume. Kite quote calls are free under the subscription;
  cache hits cost zero.

**Negative:**

- Equity pages have a hard dependency on the live provider.
  Kite outage = `/charts` broken (`/options` still works because
  its data is in our DB). Mitigation: graceful degradation in
  the marketdata layer (return last-cached, even past TTL, with
  a `staleness_seconds` field — same pattern as ADR-0013 D4).
- Fundamentals & announcements wait on a source-of-truth
  decision before going live. The pages stay on sample data
  until then. This is a visible product gap, but a pragmatic
  one — picking the wrong source and persisting it costs more
  than waiting.
- Per-pod cache means we don't get a perfect cache hit ratio at
  multi-pod scale. Acceptable now; predictable upgrade path
  (Redis).
- Future feature that wants long-range equity backtesting (a
  stock-only strategy backtester) would either need to fetch
  candles per backtest run (slow, rate-limit risk) or persist
  selectively. Revisit then.
