// Package dhan is a placeholder for a Dhan-API BrokerProvider.
// ADR-0011 D4: stubs return ErrNotImplemented until wired up.
//
// To finish this implementation:
//
//   1. Add KAIROS_DHAN_ACCESS_TOKEN to config.Config and the env
//      handling in internal/config/config.go.
//   2. Implement the HTTP client using net/http (no SDK needed — Dhan's
//      API is straightforward REST).
//      Endpoints to wire:
//        - POST /v2/optionchain         → FetchOptionChain
//        - POST /v2/optionchain/expirylist → FetchExpiries
//        - POST /v2/marketfeed/ltp      → FetchSpot
//        - POST /v2/charts/historical   → FetchHistoricalCandles
//   3. Translate Dhan's security ids ↔ our (underlying, expiry, strike,
//      type) tuple. Dhan publishes a daily instruments CSV — cache it
//      in memory at init.
//   4. Auth: static access token, ~1 year lifetime. Stored encrypted in
//      kairos.provider_tokens like Kite, but the refresh story is just
//      "operator edits config when the token rolls."
//   5. Rate limit: Dhan publishes per-endpoint limits; respect them in
//      the client.
//
// Estimated effort: 5–10 hours once Kite is in place to use as a
// reference (the shape of the file is the same).
package dhan

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

func init() {
	provider.Register("dhan", New)
}

// Provider is the not-yet-implemented Dhan client.
type Provider struct {
	logger *slog.Logger
}

func New(_ *config.Config, _ *pgxpool.Pool, logger *slog.Logger) (provider.BrokerProvider, error) {
	logger.Warn("kairos provider 'dhan' is a stub — selecting it will cause cron/handlers to return ErrNotImplemented")
	return &Provider{logger: logger}, nil
}

func (p *Provider) Name() string { return "dhan" }

func (p *Provider) IsReady(_ context.Context) error { return provider.ErrNotImplemented }

func (p *Provider) FetchOptionChain(_ context.Context, _ provider.Underlying, _ time.Time) ([]provider.ChainRow, error) {
	return nil, provider.ErrNotImplemented
}

func (p *Provider) FetchExpiries(_ context.Context, _ provider.Underlying) ([]provider.Expiry, error) {
	return nil, provider.ErrNotImplemented
}

func (p *Provider) FetchSpot(_ context.Context, _ provider.Underlying) (provider.Spot, error) {
	return provider.Spot{}, provider.ErrNotImplemented
}

func (p *Provider) FetchHistoricalCandles(_ context.Context, _ provider.HistoricalReq) ([]provider.Candle, error) {
	return nil, provider.ErrNotImplemented
}
