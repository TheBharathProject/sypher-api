# ADR-0013 — Kairos live chain via 60s polling (no WebSocket)

**Status:** Accepted
**Date:** 2026-05-17

## Context

The `/options` page in Kairos shows the live options chain for the
selected underlying. During market hours (09:15–15:30 IST on
trading days), the chain visibly changes — LTP ticks, OI shifts,
volume accumulates. We need to get those updates onto the page.

Three delivery models on the table:

1. **Frontend polls a REST endpoint.** Simplest. The endpoint
   reads from the same `kairos.option_chains` table the ingest
   cron writes into. Cadence picked by the client.
2. **Server-sent events (SSE).** One-way push. The frontend opens
   a long-lived `EventSource`; the server flushes a new chain
   whenever the ingest cron writes one.
3. **WebSocket.** Bidirectional, but for this use case mostly
   one-way push. Standard tooling around it.

The end-to-end latency budget is set by two upstream constraints:

- The ingest cron (ADR-0011) calls Kite's Quote API. Kite's quote
  endpoint is rate-limited to **1 req/sec**; we batch up to 500
  instruments per call, so one chain (~160 instruments) fits in
  one request. We picked 60s ingest cadence in
  ADR-0011, leaving headroom for other ingestions on the same
  quota.
- The Kite quote feed itself has a "few seconds" provider-side
  delay vs the exchange. Real-time it isn't.

So the **freshest data we can serve is at most ~60s old, by
construction**. There is no point delivering it to the browser
faster than that. A WebSocket pushing at 100Hz would deliver the
same 60-second-old snapshot 6000 times.

The user is a retail options trader using this for research and
backtesting, not a market-maker. Their decision loop is minutes,
not microseconds. 60s data is the same data they'd see on
Sensibull, Opstra, or NSE's own website.

## Decisions

### D1 — Frontend polls `GET /kairos/options/chain?underlying=X&expiry=Y` every 60s

When `/options` mounts:

1. Initial fetch on mount.
2. `setInterval(fetchChain, 60_000)` until unmount or
   `document.visibilityState === 'hidden'` (then suspend).
3. On `visibilitychange` back to 'visible', refresh immediately
   then resume the 60s interval.

`visibilitychange` handling is important — when the user has the
tab in the background, we stop polling. Saves both Kairos API
load and Kite quote calls (indirectly, because we're not pulling
from Kite per request — but it still saves DB reads).

### D2 — The endpoint reads from the DB, not from Kite directly

`GET /kairos/options/chain`:

1. Resolves `underlying` + `expiry` to a strike range (ATM ± 30).
2. `SELECT ... FROM kairos.option_chains WHERE underlying=$1 AND expiry_date=$2 AND snapshot_time = (SELECT MAX(snapshot_time) FROM ...)`.
   In practice: select the most recent snapshot_minute via an
   index-only scan (the lookup index from ADR-0009 D2 covers this).
3. Compute greeks per row (D5 of ADR-0011, the on-read Black-Scholes).
4. Return the normalised `ChainRow[]` shape (matches
   `kairos/app/options/chain-data.ts`).

The DB is the cache. No separate Redis. The hottest read path is
"most recent snapshot for one (underlying, expiry)" — a tiny query
that hits the lookup index. Measured on dev with one week of data
in: <5ms warm, <20ms cold.

### D3 — Ingest cron writes the snapshot once per minute

`internal/cron/jobs/kairos_snapshot.go` runs every 60s. On each
fire:

1. Check `provider.IsReady()` — skip if `ErrAuthExpired`.
2. Check IST clock — skip if outside 09:15–15:30 or on a weekend.
   (Indian holiday calendar handling is a TODO; a holiday means
   we'll fetch an empty/zero chain, which is fine — the page
   just shows yesterday's last snapshot.)
3. For each `(underlying, active expiry)` pair, call
   `provider.FetchOptionChain(ctx, ...)`.
4. Bulk-insert returned rows into `kairos.option_chains`. One
   transaction per underlying, one COPY-equivalent batch insert
   (via pgx `CopyFrom`) — ~160 rows in ~3ms.

If a fetch fails (rate-limit, network, provider error), log and
skip that one underlying; the next 60s tick retries. Don't fail
the whole cron run.

### D4 — Stale-tolerance: if no fresh data, return last-stored

The endpoint never blocks waiting for fresh ingest. If the most
recent snapshot is 30 minutes old (market closed, or token
expired), the endpoint returns that data plus a `staleness_seconds`
field in the response envelope:

```jsonc
{
  "underlying": "NIFTY",
  "spot": 24198.85,
  "expiry": "2026-05-29",
  "snapshot_time": "2026-05-17T15:29:00+05:30",
  "staleness_seconds": 1812,
  "rows": [ /* ChainRow[] */ ]
}
```

The frontend uses `staleness_seconds` to render a banner ("Last
update: 30 min ago — markets closed") instead of pretending the
data is live. No errors, just honest state.

### D5 — Per-symbol Last-Modified / 304s

Reduce wire bytes by setting `Last-Modified: <snapshot_time>` on
the response. When the frontend polls and the snapshot hasn't
changed (e.g., a polling tick that fell between two ingest ticks),
it sends `If-Modified-Since` and the API returns `304 Not Modified`
with no body.

This isn't free (we still need to find the latest snapshot's
timestamp), but the body of a NIFTY chain is ~25KB unminified —
a `304` on every other request roughly halves the page's data
usage over a session. Worth doing.

### D6 — No WebSocket, no SSE

**Why not WebSocket:**

- A WebSocket implementation needs reconnect logic on the client
  (network blips, mobile suspend, server restart). Polling has
  none — each request is independent.
- nginx config for WebSocket upgrade is non-trivial in our setup
  and means a third path through the reverse-proxy.
- Bidirectional isn't needed; nothing flows browser → server
  fast enough to want a long-lived socket.
- The "real-time" perceived advantage of WebSocket is a mirage
  here — the freshest data we have is 60s old.

**Why not SSE:**

- SSE is conceptually a fit (one-way server push) but has the
  same reconnect-handling story to write client-side.
- Idle SSE connections eat one HTTP/2 stream per user. At 100
  concurrent /options viewers, that's 100 long-lived streams
  the API is paying for, vs. 100 ~25KB poll responses every 60s
  (1.7 RPS).
- Browsers limit SSE connections per origin to 6; if the user has
  multiple Kairos tabs, this becomes a real constraint.

The non-decisions matter as much as the decision: by not opening
a long-lived channel, we keep the API stateless per request, the
nginx config flat, and the failure modes obvious. Network blip?
Next poll succeeds. Server restart? Next poll succeeds.

## Consequences

**Positive:**

- One code path (the polling client + the DB-backed endpoint),
  reused unchanged for `/options`, `/strategies` (the chain
  context inside the builder), and any future page that wants
  current chain data.
- The DB is both the durable record and the cache. No second
  caching layer to invalidate.
- Polling is cancellable, suspendable on tab hide, and
  per-request — all desirable properties that long-lived
  connections complicate.
- `304 Not Modified` keeps wire bytes low and means the polling
  load is dominated by header round-trips, not chain payloads.

**Negative:**

- Worst-case staleness is ~120s (60s ingest interval + 60s poll
  interval). A trader expecting true-real-time will find this
  too slow — but the upstream provider isn't truer-real-time
  either, so this is a constraint of the data source, not the
  delivery model.
- If we ever add a feature that genuinely needs sub-second
  updates (e.g., a paper-trading order book viewer), we'd build
  a separate WebSocket path for that page, not retrofit polling
  into push. That's fine — they're different problems.
- A user with the tab open all day generates ~390 chain requests
  during market hours (390 minutes × 1 req/min). At 1.7 RPS
  across all users, the DB still sees this as background noise.
  Worth monitoring if user count grows by 100×.
