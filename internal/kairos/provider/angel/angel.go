// Package angel is a placeholder for an Angel One (SmartAPI) BrokerProvider.
// ADR-0011 D4: stubs return ErrNotImplemented until wired up.
//
// To finish this implementation:
//
//   1. Add KAIROS_ANGEL_API_KEY, KAIROS_ANGEL_CLIENT_CODE,
//      KAIROS_ANGEL_PASSWORD, KAIROS_ANGEL_TOTP_SECRET to config.
//   2. Auth flow: TOTP-based.
//        - POST /rest/auth/angelbroking/user/v1/loginByPassword with
//          totp = generated from TOTP secret.
//        - Receive jwtToken, refreshToken, feedToken.
//        - jwtToken is daily; refresh via /rest/auth/angelbroking/jwt/v1/generateTokens.
//   3. Endpoints:
//        - POST /rest/secure/angelbroking/market/v1/optionChain → FetchOptionChain
//        - POST /rest/secure/angelbroking/historical/v1/getCandleData → FetchHistoricalCandles
//        - POST /rest/secure/angelbroking/market/v1/quote → FetchSpot
//   4. Angel publishes a daily instruments JSON
//      (https://margincalculator.angelbroking.com/OpenAPI_File/files/OpenAPIScripMaster.json)
//      — cache at init for token ↔ tradingsymbol resolution.
//   5. TOTP enables full automation: the daily refresh can be a cron
//      job, no human in the loop. This is the only provider where
//      "set and forget" is genuinely possible.
//
// Estimated effort: 7–12 hours (TOTP makes it slightly more complex
// than Kite's request-token flow).
package angel

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

func init() {
	provider.Register("angel", New)
}

type Provider struct {
	logger *slog.Logger
}

func New(_ *config.Config, _ *pgxpool.Pool, logger *slog.Logger) (provider.BrokerProvider, error) {
	logger.Warn("kairos provider 'angel' is a stub — selecting it will cause cron/handlers to return ErrNotImplemented")
	return &Provider{logger: logger}, nil
}

func (p *Provider) Name() string { return "angel" }

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
