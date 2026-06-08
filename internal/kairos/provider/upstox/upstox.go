// Package upstox is a placeholder for an Upstox-API BrokerProvider.
// ADR-0011 D4: stubs return ErrNotImplemented until wired up.
//
// To finish this implementation:
//
//   1. Add KAIROS_UPSTOX_CLIENT_ID, KAIROS_UPSTOX_CLIENT_SECRET, and the
//      callback URL config to config.Config.
//   2. Implement the HTTP client using net/http. Endpoints to wire:
//        - GET /v2/option/contract       → FetchExpiries
//        - GET /v2/option/chain          → FetchOptionChain
//        - GET /v2/market-quote/ltp      → FetchSpot
//        - GET /v3/historical-candle/    → FetchHistoricalCandles
//   3. Auth: standard OAuth 2.0 with refresh_token. Implement the
//      refresh flow inside the provider so callers don't have to think
//      about it.
//   4. Symbol scheme: Upstox uses instrument_key like
//      "NSE_FO|<token>" — keep a translator next to translate.go.
//   5. Rate limit: 25 requests/sec, 250 requests/min, 1000 requests/30min.
//      Use a token-bucket limiter.
//
// Estimated effort: 5–10 hours once Kite is in place to use as a
// reference.
package upstox

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

func init() {
	provider.Register("upstox", New)
}

type Provider struct {
	logger *slog.Logger
}

func New(_ *config.Config, _ *pgxpool.Pool, logger *slog.Logger) (provider.BrokerProvider, error) {
	logger.Warn("kairos provider 'upstox' is a stub — selecting it will cause cron/handlers to return ErrNotImplemented")
	return &Provider{logger: logger}, nil
}

func (p *Provider) Name() string { return "upstox" }

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
