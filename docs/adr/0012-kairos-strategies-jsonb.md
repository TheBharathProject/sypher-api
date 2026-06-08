# ADR-0012 — Kairos strategies & backtests use JSONB for variable shape

**Status:** Accepted
**Date:** 2026-05-17

## Context

A Kairos strategy is a small structured object:

- A name and an underlying.
- An array of **legs**. Each leg has a side (BUY/SELL), option
  type (CE/PE), strike rule (`ATM`, `ATM+1`, ..., `ATM-3`),
  expiry rule (`WEEKLY`, `NEXT_WEEKLY`, `MONTHLY`), and lot count.
- Entry time, exit time.
- Stop-loss percentage and target percentage.

The shape is going to grow. Near-term additions on the roadmap:

- Per-leg stop-loss (today SL is whole-strategy).
- Trailing stop-loss (a new field with sub-fields: trigger %, step %).
- Time-based exit conditions ("close at 14:30 if leg PnL > X").
- Hedge legs (a leg with a different sizing rule).
- Underlying-relative entry conditions ("only enter if VIX > 16").

If we model legs as a normalised `kairos.strategy_legs` table, every
one of those additions is a migration. If we model the time/SL/target
sub-objects as columns, every change is a migration. Strategies and
their legs are always read together (you never want a strategy
without its legs, and you never query "give me all CE legs across
all users") — so the normalisation argument has no payoff here.

`kairos.backtests` has the same shape problem in two places:

- The legs of the strategy that was run (a strategy can be edited
  after a backtest; we capture the legs as-of-backtest, so the
  result is reproducible).
- The result blob: `totalPnl`, `winRate`, `sharpe`, `maxDrawdown`,
  and a `trades` array of day-by-day P&L rows. The shape of
  `trades[i]` evolves as we add cost-model details (per-leg fees,
  slippage breakdown, etc.).

PostgreSQL's `JSONB` (binary, indexable, validated-at-write) is
designed exactly for this — a structured payload whose shape lives
in application code, not the DB schema.

## Decisions

### D1 — `strategies.legs JSONB NOT NULL`

```sql
CREATE TABLE kairos.strategies (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     UUID        NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  name        TEXT        NOT NULL,
  underlying  TEXT        NOT NULL,         -- 'NIFTY' | 'BANKNIFTY' | 'SENSEX'
  legs        JSONB       NOT NULL,
  entry_time  TEXT        NOT NULL DEFAULT '09:20',
  exit_time   TEXT        NOT NULL DEFAULT '15:15',
  stop_loss   NUMERIC(5,2) NOT NULL DEFAULT 0,
  target      NUMERIC(5,2) NOT NULL DEFAULT 0,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX strategies_user_idx ON kairos.strategies (user_id, created_at DESC);
```

`legs` is a JSONB array. The shape today:

```jsonc
[
  { "side": "SELL", "optType": "CE", "strikeRule": "ATM", "expiryRule": "WEEKLY", "lots": 1 },
  { "side": "SELL", "optType": "PE", "strikeRule": "ATM", "expiryRule": "WEEKLY", "lots": 1 }
]
```

Validation lives in Go (`internal/kairos/types.go:ValidateLegs`).
The schema is the Go struct; the DB only knows it's a JSONB array.

### D2 — `backtests.legs JSONB`, `backtests.result JSONB`

The backtest row captures the strategy state at run time **and**
the result. Schema in migration 0025, called out in ADR-0010 D4.

`legs` is a copy of the strategy's legs (not a foreign key) — the
strategy may have been edited or deleted since the backtest ran;
the result is meaningless without the leg definitions, so we
freeze them in.

`result` shape:

```jsonc
{
  "totalPnl": 24450.0,
  "winRate": 0.62,
  "sharpe": 1.34,
  "maxDrawdown": -8200.0,
  "trades": [
    { "date": "2025-08-12", "entry": 245.5, "exit": 211.0, "grossPnl": 2587.5, "costs": 87.4, "netPnl": 2500.1, "outcome": "win" },
    ...
  ]
}
```

The frontend (`kairos/app/strategies/page.tsx`) consumes this
directly — same TypeScript types whether the data came from the
old synthetic generator or the real backtest worker.

### D3 — JSONB, not JSON (text)

`JSONB` enforces well-formed JSON at write time and stores it in a
binary format that supports indexing, sub-document containment
queries (`legs @> '[{"optType": "CE"}]'::jsonb`), and key extraction
without re-parsing.

We don't need most of that today. But the cost is zero (the binary
format is also faster to read) and the option value (for an
admin debugging tool, or a future "find all strategies that contain
a SELL CE leg" feature) is real.

### D4 — Validation is Go-side, not DB-side

No `CHECK (jsonb_typeof(legs) = 'array')`, no `jsonschema` extension,
no triggers. The Go layer owns the schema:

```go
type Leg struct {
    Side       string `json:"side"`        // BUY | SELL
    OptType    string `json:"optType"`     // CE | PE
    StrikeRule string `json:"strikeRule"`  // ATM | ATM+1 | ATM-1 | ATM+2 | ATM-2 | ATM+3 | ATM-3
    ExpiryRule string `json:"expiryRule"`  // WEEKLY | NEXT_WEEKLY | MONTHLY
    Lots       int    `json:"lots"`
}

func ValidateLegs(legs []Leg) error { /* exhaustive checks */ }
```

DB-side validation is too rigid (any rule change becomes a
migration) and not richer than what the Go validation already does
(can't express "lots > 0 AND <= 100"). Keep validation co-located
with the code that produces and consumes the data.

### D5 — No per-leg index. Strategies are filtered by user, not by leg contents

`strategies_user_idx (user_id, created_at DESC)` covers every
listing query the UI makes. We do **not** index inside `legs` —
no one's asking "which strategies contain a particular leg
shape" yet.

When such a query becomes a feature, we can add a GIN index on
`legs` with one line:

```sql
CREATE INDEX strategies_legs_gin ON kairos.strategies USING GIN (legs);
```

That's a future move, not a current one.

### D6 — Migrations evolve JSONB shapes via data migration, not DDL

When we add `trailingStopLoss` to a leg, we don't migrate the
column type. We do (in order):

1. Update the Go `Leg` struct with the new field (omitempty).
2. Ship. Newly-saved legs include the new field; old legs
   don't.
3. The reader code (`internal/kairos/types.go`) defaults missing
   fields to a sensible zero.
4. Optionally, a background backfill writes the default into
   existing rows so the JSONB is self-describing — but only when
   we want it.

This is the **only** sane way to evolve a schema-on-write
JSONB column. Anything else (CHECK constraints, jsonschema
columns, etc.) makes step 1 a multi-day migration project.

## Consequences

**Positive:**

- Strategy and result shapes can grow without a migration.
- One JSONB read per strategy load; no JOIN to a `legs` table; no
  N+1 risk in the listing query.
- Reproducibility of historical backtests is built in: the
  backtest row is self-contained.
- GIN-index escape hatch is there if we ever want to query inside
  the JSONB.

**Negative:**

- Schema lives in code. A new dev reading the SQL doesn't see
  what `legs` should look like — they have to find the Go struct.
  Mitigated by an exported `LegSchema` comment block in
  `types.go` and a brief schema cheat-sheet in
  `docs/adr/0012-kairos-strategies-jsonb.md` (this file).
- Bad data is detectable only at read time. A direct SQL insert
  could bypass `ValidateLegs`. Acceptable — all writes go through
  the kairos handler in normal operation.
- Cross-cutting analytics ("median strategy depth across all
  users") need to unnest the JSONB rather than do a simple
  `count(*) FROM strategy_legs`. Easy with `jsonb_array_elements`
  but mildly less obvious to a SQL-first analyst.
