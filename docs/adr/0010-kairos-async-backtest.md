# ADR-0010 — Kairos backtest jobs (async, in-process worker pool)

**Status:** Accepted
**Date:** 2026-05-17

## Context

A Kairos "backtest" replays a user-defined options strategy
(multi-leg, with entry/exit times, stop-loss, target) against the
historical chain stored per ADR-0009. The workload per backtest:

- One trading day's iteration: pull entry-time and exit-time rows
  from `kairos.option_chains` for each leg, compute P&L, apply
  costs (STT, brokerage, slippage, SEBI fee).
- One year of backtesting = ~250 trading days = ~250 iterations.
- Wall clock: 1.5–8 seconds on the warm pool for typical 2–4-leg
  strategies. 1-year, 4-leg condors land around the upper bound.

A synchronous HTTP handler is not viable. The Next.js fetch
timeout, nginx idle timeout, and user attention all expire before
the worst case finishes. Even the 1.5s lower bound is too long for
a UI to sit on; the user has no signal whether the request is
working.

Three execution models were on the table:

1. **Synchronous handler.** Simplest. Eliminated for the latency
   reasons above.
2. **In-process worker pool with in-memory queue.** Submit returns
   immediately; workers pick from a Go channel. Status persisted in
   the DB so the user can poll across page refreshes.
3. **External worker** — separate binary, Postgres `LISTEN/NOTIFY`
   or Redis Streams for hand-off. Resilient to API process
   restarts; jobs survive.

ADR-0006 set the precedent: don't introduce a new piece of infra
unless current scale forces it. We have one Go binary, one
Postgres, one frontend. Adding a worker process means a second
deploy target, a second log stream, a second OOM risk, and a
durable queue.

User base today is zero. Even with 50 concurrent backtest users
generating jobs at the speed humans can click "Run", the in-process
pool handles it. The cost of a restart-during-job is "user clicks
Run again" — not "lost user data".

## Decisions

### D1 — In-process worker pool of size 3, buffered channel of size 100

```go
// internal/kairos/worker.go
type Pool struct {
  ch     chan Job
  store  *Store
  prov   provider.BrokerProvider
  logger *slog.Logger
}

func NewPool(store *Store, prov provider.BrokerProvider, logger *slog.Logger) *Pool {
  return &Pool{ch: make(chan Job, 100), store: store, prov: prov, logger: logger}
}

func (p *Pool) Start(ctx context.Context, n int) {
  for i := 0; i < n; i++ {
    go p.worker(ctx, i)
  }
}
```

Workers are goroutines started from `server.Start`. They listen on
the shared channel and pull `kairos.backtests` rows where
`status='pending'` on startup so a deploy doesn't drop in-flight
jobs that hadn't been picked up yet (D5).

Pool size 3 because that's well below the Kite historical data
rate limit (3 req/sec for the active provider per ADR-0011) — a
fourth worker would be rate-limit-blocked on historical fetches.

### D2 — `POST /kairos/backtest` returns 202 with `{ id }`, never blocks

The handler:

1. Validates strategy payload (matches `Strategy` Go struct).
2. Inserts a row in `kairos.backtests` with `status='pending'`,
   `result=NULL`. Returns the row's UUID immediately.
3. Sends the job on the channel — non-blocking via `select`/`default`.
   If the channel is full, the row stays `pending`; a worker picks
   it up via the periodic sweep (D5).

Returning **before** the channel send completes is intentional:
a slow downstream worker should never block an HTTP handler.

### D3 — Frontend polls `GET /kairos/backtest/:id` every 2 seconds

A backtest takes 1.5–8 seconds. 2-second polling means at worst
the user sees the "done" state ~10 seconds after job completion;
at best, ~1 second after. This is the right cadence — 1s polling
floods the API for no win, 5s polling adds perceived latency
without saving anything material.

The page reuses the existing fetch pattern from
`kairos/app/strategies/page.tsx` — no new client-side infra
(no SWR, no react-query). A `useEffect` with `setInterval` until
`status === 'done' || status === 'failed'`.

### D4 — `kairos.backtests` row is the durable state

Schema (in migration 0025):

```sql
CREATE TABLE kairos.backtests (
  id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID        NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  strategy_id  UUID        REFERENCES kairos.strategies(id) ON DELETE SET NULL,
  underlying   TEXT        NOT NULL,
  legs         JSONB       NOT NULL,
  from_date    DATE        NOT NULL,
  to_date      DATE        NOT NULL,
  entry_time   TEXT        NOT NULL,
  exit_time    TEXT        NOT NULL,
  stop_loss    NUMERIC(5,2),
  target       NUMERIC(5,2),
  status       TEXT        NOT NULL DEFAULT 'pending',  -- pending|running|done|failed
  result       JSONB,
  error        TEXT,
  started_at   TIMESTAMPTZ,
  finished_at  TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

The row is the only persistent record of the job. The channel is a
hand-off mechanism, not a queue of truth. Worker on pickup:

1. `UPDATE backtests SET status='running', started_at=NOW() WHERE id=$1 AND status='pending'`.
   Returns 1 row → claimed; 0 rows → someone else got it (this
   protects against double-execution across the startup sweep and
   the channel).
2. Run the simulation.
3. `UPDATE backtests SET status='done', result=$1, finished_at=NOW() WHERE id=$2`,
   or `status='failed', error=$1` on err.

`result` is a JSONB of `{ totalPnl, winRate, sharpe, maxDrawdown,
trades: [...] }` — same shape the frontend already renders from
synthetic data.

### D5 — Startup sweep recovers `pending` rows from previous run

On boot, `Pool.Start` does:

```sql
UPDATE kairos.backtests
SET status='pending'
WHERE status='running' AND started_at < NOW() - INTERVAL '5 minutes';
```

(Anything stuck "running" for >5min is a worker that died; reset
it.) Then it selects up to 100 oldest `pending` rows and pushes
them onto the channel. After that, only the live
`POST /kairos/backtest` path enqueues.

This is the durability story. A restart mid-job means the job
goes back to `pending` after 5 minutes and re-runs. The user
might see the same backtest take longer than usual, but no data
is lost — backtests are deterministic given fixed inputs.

### D6 — No external worker process, no LISTEN/NOTIFY, no Redis

These get revisited when one of:

- Backtest queue depth regularly exceeds 100 (the channel buffer).
- Worker pool can't keep up: average pickup latency > 30s.
- A backtest needs to run >60s — at which point the HTTP keepalive
  for the polling client is more interesting than the worker
  architecture anyway.

None of those are true now. Defer.

### D7 — Free vs premium gate on backtest depth, not concurrency

Free users: at most 6 months of historical range per backtest, at
most 5 backtests in a 24h rolling window.
Premium users: 3 years, unlimited backtests.

These are checked in the handler before the row is inserted.
Counter for "5 in 24h" is `SELECT count(*) FROM backtests WHERE user_id=$1 AND created_at > NOW() - INTERVAL '24 hours'`.
No separate rate-limit table — the row already exists.

## Consequences

**Positive:**

- One binary, one deploy. Backtest workers come up when the API
  comes up.
- No queue infrastructure. The DB row is the source of truth; the
  channel is just a hint.
- Polling at 2s is dumb and reliable. No WebSocket lifecycle, no
  reconnect logic, no nginx config for long-lived connections.
- Backtests are crash-recoverable. A worker dying mid-simulation
  leaves the row in `running`; the startup sweep flips it back
  after 5 minutes and another worker picks it up. The user sees
  delay, not data loss.

**Negative:**

- The worker pool runs inside the same process as the HTTP server.
  A pathological backtest that pins a CPU also slows HTTP request
  handling for everything else (Pegasus job-tracker, auth, etc.).
  Mitigation: pool size 3 caps the damage to at most 3 cores; the
  Go scheduler still gives time to HTTP handlers. Acceptable on a
  multi-core box.
- An aggressive user could enqueue many backtests quickly; the
  per-user counter caps premium at "unlimited" which is fine but
  not free of resource cost. Throttle at handler level only
  becomes necessary if we see abuse.
- The 2s polling cadence on a popular page means every active user
  doing a backtest contributes 0.5 RPS to the API. For 100
  concurrent backtests, that's 50 RPS purely on the polling path
  — trivial, but worth noting if the user base grows by 10×.
