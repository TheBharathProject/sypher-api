package provider

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/config"
)

// BrokerProvider is the contract every data-feed implementation satisfies.
// See ADR-0011 D2 for why these specific five methods, and not more.
//
// The interface is intentionally small. Provider-specific superpowers
// (Kite WebSocket streaming, Upstox binary market feed, etc.) live as
// optional sibling interfaces that a caller can type-assert against —
// not on this contract. Adding a method here forces every provider to
// implement it, including the stubs, which defeats the point.
type BrokerProvider interface {
	// Name returns the canonical short name of the active provider.
	// Used in logs and stored on each kairos.option_chains row.
	Name() string

	// IsReady answers "can I serve a fetch right now?" Cron jobs and
	// handlers use this to decide whether to skip cleanly or proceed.
	// Cheap: no network call expected in the happy path. Implementations
	// usually just check whether their in-memory token cache is set
	// and unexpired.
	//
	// Returns nil when ready. Returns ErrAuthExpired or ErrNotConfigured
	// to communicate why not.
	IsReady(ctx context.Context) error

	// FetchOptionChain returns the chain for one underlying + expiry,
	// in normalised ChainRow shape. The provider is responsible for
	// translating its native symbol scheme (Kite tokens, Dhan security
	// ids, etc.) and pricing fields into ChainRow.
	//
	// The returned slice is unordered; callers that need order sort by
	// (strike asc, option_type) themselves.
	FetchOptionChain(ctx context.Context, underlying Underlying, expiry time.Time) ([]ChainRow, error)

	// FetchExpiries returns the list of currently-tradeable expiries
	// for an underlying. Refreshed daily, not per-snapshot — these
	// only change when an expiry rolls or a new monthly is listed.
	FetchExpiries(ctx context.Context, underlying Underlying) ([]Expiry, error)

	// FetchSpot returns the current spot price for an underlying.
	// Used by the ATM-strike resolver and by /dashboard market cards.
	FetchSpot(ctx context.Context, underlying Underlying) (Spot, error)

	// FetchHistoricalCandles is called by the backtest worker and by
	// the /charts page (via internal/kairos/marketdata).
	FetchHistoricalCandles(ctx context.Context, req HistoricalReq) ([]Candle, error)
}

// constructor is the shape every provider subpackage exports as New().
// Receives the full config (each provider reads its own env vars) and a
// pgxpool (Kite needs it for the encrypted provider_tokens row; stubs
// ignore it). The pool is also passed so a future provider could cache
// instrument lookup tables in its own table without changing this
// signature.
type constructor func(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) (BrokerProvider, error)

// registry maps provider names to their constructors. Filled in by
// factory.go in init(); separate file so adding a new provider is one
// line in one place.
var registry = map[string]constructor{}

// register is called from each provider subpackage's init() to wire
// itself into the factory without provider.go having to import it.
// Avoids the circular-dependency hazard of provider.go knowing about
// every concrete subpackage.
//
// Concretely: provider/kite/kite.go has an init() that calls
// provider.register("kite", kite.New). Same for dhan/upstox/angel/null.
func register(name string, fn constructor) {
	registry[name] = fn
}

// NewProvider returns the active BrokerProvider as selected by
// cfg.KairosDataProvider. Called once from server.Start.
//
// Default empty value resolves to "kite" because that's the launch
// provider per ADR-0011 D6. A blank env var in prod likely means a
// deploy forgot to set it; defaulting (rather than erroring) keeps the
// happy path frictionless and lets the IsReady check catch missing
// auth at request time.
//
// Returns ErrNotConfigured if the named provider isn't registered
// (i.e. the env value is a typo).
func NewProvider(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) (BrokerProvider, error) {
	name := cfg.KairosDataProvider
	if name == "" {
		name = "kite"
	}
	fn, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %v)", ErrNotConfigured, name, registeredNames())
	}
	prov, err := fn(cfg, pool, logger)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", name, err)
	}
	logger.Info("kairos provider initialised", "provider", prov.Name())
	return prov, nil
}

// Register is the public form of register used by provider subpackages
// in their init(). Exported so the import-driven registration pattern
// works across package boundaries.
func Register(name string, fn func(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) (BrokerProvider, error)) {
	register(name, fn)
}

// registeredNames returns the sorted list of currently-known provider
// names. Used in the error message when a typo'd KAIROS_DATA_PROVIDER
// hits NewProvider so the operator can fix it without grepping the code.
func registeredNames() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	return out
}
