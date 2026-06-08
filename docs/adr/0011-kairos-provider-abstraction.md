# ADR-0011 — Kairos data provider abstraction (BrokerProvider interface)

**Status:** Accepted
**Date:** 2026-05-17

## Context

Indian options data has multiple roughly-fungible providers, each
with different commercial terms, auth flows, rate limits, and API
shapes:

| Provider | Cost | Live data | Historical | Auth shape |
|---|---|---|---|---|
| Zerodha Kite Connect | ₹500/mo | yes (1 rps quote) | yes (3 rps candle) | request_token → access_token, expires daily 06:00 IST |
| Dhan | free | yes | yes (limited) | static access_token, ~1 year |
| Upstox | free | yes | yes | OAuth 2.0 with refresh_token |
| Angel One (SmartAPI) | free | yes | yes | TOTP + daily access_token |

The product imperative from the owner: **start on Kite, but be
able to swap providers by flipping a config value.** Reasons one
might switch:

- Cost (₹500/mo adds up; if Dhan/Upstox feature-parity becomes
  acceptable, drop the recurring spend).
- Reliability (vendor outage → fail over to another).
- Capability (a future feature like options-WebSocket streaming
  is well-supported by Kite but not by another vendor — or
  vice-versa).
- ToS changes (providers periodically change what's allowed in a
  multi-user setting).

A second consideration: the chain data is **not user-specific**.
Every Kairos user sees the same NIFTY chain. The live `/options`
page is a read of a shared dataset, ingested once by a server-side
cron and served to all users. This is materially different from
trading actions (placing an order is per-user); the data path and
the trading path will eventually need separate authorisation
models. This ADR scopes only the data path.

Three candidate designs:

1. **Direct usage.** `internal/kairos/handler.go` imports a Kite
   SDK and calls it directly. Swapping = code change everywhere
   the SDK is referenced.
2. **Thin function map.** A registry of function pointers keyed by
   provider name. No interface, just `map[string]func(...)`.
   Avoids the "interface" word but ends up needing the same shape
   information and breaks when providers want internal state
   (token caches, rate limiters).
3. **Interface + factory.** A Go interface listing the operations
   the rest of the system needs. Each provider in its own
   subpackage. A factory function selects an impl based on an env
   var at startup. The rest of the system depends only on the
   interface.

Option 3 is the only one that keeps storage, handlers, cron, and
worker code provider-agnostic and lets a provider's internal state
(token cache, rate limiter, instrument lookup table) live in one
place.

## Decisions

### D1 — `BrokerProvider` interface, one subpackage per provider

```
internal/kairos/provider/
  provider.go      // interface, factory
  types.go         // ChainRow, Expiry, Spot, Candle, HistoricalReq
  errors.go        // ErrAuthExpired, ErrNotImplemented, ErrRateLimited
  kite/            // Kite Connect implementation
    kite.go
    auth.go
    translate.go
    instruments.go
  dhan/            // Stub — returns ErrNotImplemented
    dhan.go
  upstox/          // Stub
    upstox.go
  angel/           // Stub
    angel.go
```

The interface is in `provider.go`. The rest of the kairos package
(`handler.go`, `worker.go`, `store.go`) imports
`internal/kairos/provider` and refers only to
`provider.BrokerProvider`. Nothing outside the implementation
subpackage imports `provider/kite`.

### D2 — The interface itself

```go
package provider

type BrokerProvider interface {
    // Name returns the canonical short name of the active provider.
    // Used in logs and stored on each chain row.
    Name() string

    // IsReady answers "can I serve a fetch right now?". Cron uses this
    // to skip cleanly when auth is missing or expired. Cheap (no
    // network call expected in the happy path).
    IsReady(ctx context.Context) error

    // FetchOptionChain returns the chain for one underlying + expiry.
    // The returned ChainRow slice is in our normalised shape (matches
    // kairos/app/options/chain-data.ts on the frontend, 1:1).
    FetchOptionChain(ctx context.Context, underlying string, expiry time.Time) ([]ChainRow, error)

    // FetchExpiries returns the list of currently-tradeable expiries
    // for an underlying. Refreshed daily by a separate cron.
    FetchExpiries(ctx context.Context, underlying string) ([]Expiry, error)

    // FetchSpot returns the current spot price for an underlying.
    // Used to compute ATM strike and to enrich chain rows.
    FetchSpot(ctx context.Context, underlying string) (Spot, error)

    // FetchHistoricalCandles is used by the backtest worker.
    // Interval is one of: "minute", "3minute", "5minute", "day".
    FetchHistoricalCandles(ctx context.Context, req HistoricalReq) ([]Candle, error)
}
```

Five methods is the smallest set that lets cron + worker do their
jobs. No `PlaceOrder`, no `GetPositions` — those are trading
operations and will live on a different interface
(`BrokerTrading` or per-user OAuth, not yet decided).

Anything provider-specific that a caller might want
(WebSocket streaming, instrument tokens, native expiry strings)
is **not on the interface**. If a feature needs it, that feature
gets a separate interface that some providers implement and
others don't — discovered via a type assertion at the call site:

```go
if streamer, ok := prov.(provider.ChainStreamer); ok {
  go streamer.StreamChain(ctx, ...)
}
```

This keeps the core interface small without forcing every provider
to implement everything.

### D3 — `NewProvider(cfg)` factory in `provider.go`

```go
func NewProvider(cfg *config.Config, logger *slog.Logger) (BrokerProvider, error) {
    switch cfg.KairosDataProvider {
    case "kite", "":  // default
        return kite.New(cfg, logger)
    case "dhan":
        return dhan.New(cfg, logger)
    case "upstox":
        return upstox.New(cfg, logger)
    case "angel":
        return angel.New(cfg, logger)
    default:
        return nil, fmt.Errorf("unknown KAIROS_DATA_PROVIDER: %q", cfg.KairosDataProvider)
    }
}
```

The factory is called exactly once, in `server.Start`. The returned
`BrokerProvider` is stored on the kairos `Handler` and passed to
the cron jobs and the worker pool. The active provider is fixed
for the life of the process.

Switching providers requires a config change + a restart. This is
intentional — we don't want a partial cutover where some inserts
say `provider='kite'` and others say `provider='dhan'` for the
same snapshot tick. The `provider` column on `option_chains`
(ADR-0009 D6) is for the future case where we might run multiple
providers in parallel; for now it just records what the active
one was at write time.

### D4 — Each provider owns its own auth lifecycle

The interface offers `IsReady()`. Implementations:

**Kite** (`provider/kite/auth.go`):

- Loads `KAIROS_KITE_API_KEY`, `KAIROS_KITE_API_SECRET` at startup.
- Reads the current access token from `kairos.provider_tokens`
  on first need (cached in memory, refreshed via DB pubsub on update).
- `IsReady` returns `ErrAuthExpired` when the token is missing or
  the last 401 from Kite happened within the current session.
- Exposes a separate OAuth flow on the HTTP layer:
  `GET /kairos/provider/kite/login` redirects to Kite,
  `POST /kairos/provider/kite/callback` exchanges request_token for
  access_token and writes it to `kairos.provider_tokens`. The
  in-memory cache invalidates on the next DB read.

**Dhan / Upstox / Angel (stubs):**

- Implement the interface, all methods return `ErrNotImplemented`
  except `Name()` and `IsReady()` (which returns
  `ErrNotImplemented` too — so cron logs "provider not ready" and
  skips, not crashes).
- Each stub file has a single TODO block at the top listing the
  exact API endpoints to wire and the rough auth flow. Filling
  one in is a ~5–10 hour task, not a project.

### D5 — `kairos.provider_tokens` is a single small table

```sql
CREATE TABLE kairos.provider_tokens (
  provider     TEXT        PRIMARY KEY,    -- 'kite' | 'dhan' | ...
  access_token TEXT        NOT NULL,
  refresh_token TEXT,
  expires_at   TIMESTAMPTZ,
  metadata     JSONB       NOT NULL DEFAULT '{}'::jsonb,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Tokens are encrypted at rest using the existing `internal/security/aead`
helpers (same pattern as resume data in ADR-0008). The `metadata`
JSONB holds provider-specific extras — Kite uses it for the user_id
returned by `/session/token`; Upstox would use it for the refresh
token; Angel for the TOTP-derived feed_token. Each provider knows
its own shape.

One row per provider. Switching the active provider doesn't
overwrite the old row; the old token is still there if we switch
back. The encryption key is the same `JWT_SECRET`-derived key
already in use.

### D6 — Server-side ingest with a shared key, not per-user OAuth (data path only)

The shared chain data is fetched once per minute using the
Sypher-operator's broker key, stored in our DB, served to every
Kairos user.

**Why not per-user OAuth for the chain:**

- Live chains are not user-specific data. Forcing OAuth means
  forcing every Kairos user to also have a brokerage account, which
  excludes paper-trading users entirely.
- 100 users × 1 API call per minute × Kite's 1-rps quote limit =
  immediate rate-limit failures. The shared model gives us a
  single quota to manage.
- Provider ToS for the **data** APIs typically permits this pattern
  (the operator is the user; chain data is public market data
  republished). Trading APIs (order placement, holdings) are a
  different matter — those require per-user auth and are not in
  scope here.

**What this means for billing / spend:** the ₹500/mo Kite fee is
borne by the platform, not passed through per-user. Recouped via
the premium tier (ADR-0006) and per-feature gates on backtest
depth (ADR-0010 D7).

### D7 — Greeks are computed on read, identically across providers

`internal/kairos/greeks/blackscholes.go` is the single source of
truth for `IV`, `delta`, `gamma`, `theta`, `vega`. Providers
deliver `(ltp, oi, volume)`; greeks fall out the same way no
matter who the provider is. This means a backtest is reproducible
across provider switches.

Mirrored to `kairos/app/options/chain-data.ts` for client-side
computation when needed (the chain page can render computed
greeks without a round-trip).

### D8 — A "no-op" provider for tests and offline dev

```
internal/kairos/provider/null/null.go
```

Returns deterministic synthetic chain data driven by the same
Black-Scholes generator the frontend's `chain-data.ts` uses. Used
by:

- `go test ./internal/kairos/...` — backtest worker test runs end-
  to-end without needing a live Kite token.
- Local dev (`KAIROS_DATA_PROVIDER=null`) when working on UI / API
  without a Kite key.

Not exposed via prod config (the factory rejects `null` if
`cfg.Env == "prod"`).

## Consequences

**Positive:**

- Provider switch is a config + restart. No code change.
- Storage, cron, handler, worker code is provider-agnostic by
  construction; the type system enforces this — none of those
  files can compile if they import a concrete provider.
- The shared-ingest model means one broker subscription serves
  unlimited Kairos users. Cost scales with us, not with users.
- Stubs for non-Kite providers are committed but inert. We can
  ship the live product on Kite today and fill in Dhan/Upstox at
  the moment we decide to migrate, with no scaffolding work to
  redo.
- Greeks math is one file. A bug in the IV formula is one fix,
  not four (one per provider).

**Negative:**

- The interface is a least-common-denominator. Provider-specific
  superpowers (Kite's WebSocket, Upstox's market-feed binary
  format) require either type assertions (D2) or out-of-band
  code paths. Acceptable for now; revisit when one of those
  features becomes a product requirement.
- A provider swap loses in-memory token caches and rate-limit
  state. Practically: a swap from Kite to Dhan triggers a fresh
  Dhan auth and a few minutes of warm-up before the chain refills.
  Not a user-visible issue at our cadence.
- The `provider` column on `option_chains` adds 5 bytes per row.
  At 225M rows/year, ~1GB/year of "tax". Acceptable.
- One operational risk specific to Kite: the daily 06:00 IST
  token expiry. If the operator forgets to refresh, ingestion
  stops. Mitigation: an existing alert path on cron errors
  (ADR-0001-style) fires when `IsReady` returns
  `ErrAuthExpired` for >15 minutes during market hours; a future
  improvement is an automated TOTP-driven refresh, but that's
  not in scope here.
