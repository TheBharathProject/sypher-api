// Package null is a synthetic-data BrokerProvider used in tests and
// offline dev. ADR-0011 D8.
//
// Mirrors the Black-Scholes-based sample generator on the frontend
// (kairos/app/options/chain-data.ts) so the two are interchangeable —
// a backtest run against the null provider in CI produces the same
// shape of result the FE renders against the embedded sample.
//
// Hard-rejected in prod: NewProvider returns an error if cfg.Env=="prod"
// and KAIROS_DATA_PROVIDER=="null" — see kairos/server wiring.
package null

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

func init() {
	provider.Register("null", New)
}

// Provider is the synthetic data source.
type Provider struct {
	logger *slog.Logger
	env    string
}

// New constructs the null provider. Refuses to load in prod — the env
// vars say "use synthetic data" which is never right in prod.
func New(cfg *config.Config, _ *pgxpool.Pool, logger *slog.Logger) (provider.BrokerProvider, error) {
	if cfg.Env == "prod" {
		return nil, errors.New("null provider is not allowed in prod; set KAIROS_DATA_PROVIDER to a real broker")
	}
	return &Provider{logger: logger, env: cfg.Env}, nil
}

func (p *Provider) Name() string { return "null" }

func (p *Provider) IsReady(_ context.Context) error { return nil }

// underlyingSpot is the canonical "spot" we synthesise for each index.
// Real values aren't important; consistency across calls is.
func underlyingSpot(u provider.Underlying) (float64, int) {
	switch u {
	case provider.UnderlyingNIFTY:
		return 24198.85, 50
	case provider.UnderlyingBANKNIFTY:
		return 52340.20, 100
	case provider.UnderlyingSENSEX:
		return 79820.50, 200
	}
	return 0, 0
}

func (p *Provider) FetchSpot(_ context.Context, u provider.Underlying) (provider.Spot, error) {
	price, _ := underlyingSpot(u)
	if price == 0 {
		return provider.Spot{}, provider.ErrUnknownUnderlying
	}
	return provider.Spot{
		Underlying: u,
		Price:      price,
		Change:     142.4,
		ChangePct:  0.59,
		AsOf:       time.Now(),
	}, nil
}

func (p *Provider) FetchExpiries(_ context.Context, u provider.Underlying) ([]provider.Expiry, error) {
	if _, step := underlyingSpot(u); step == 0 {
		return nil, provider.ErrUnknownUnderlying
	}
	// Next 4 Thursdays for weekly, plus last Thursday of next month for monthly.
	out := make([]provider.Expiry, 0, 5)
	t := nextThursday(time.Now())
	for i := 0; i < 4; i++ {
		out = append(out, provider.Expiry{Date: t, Type: "weekly"})
		t = t.AddDate(0, 0, 7)
	}
	// Last Thursday of the *next* month — naive heuristic.
	out = append(out, provider.Expiry{Date: lastThursdayOfNextMonth(time.Now()), Type: "monthly"})
	return out, nil
}

func nextThursday(now time.Time) time.Time {
	d := now
	for d.Weekday() != time.Thursday {
		d = d.AddDate(0, 0, 1)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 15, 30, 0, 0, d.Location())
}

func lastThursdayOfNextMonth(now time.Time) time.Time {
	firstOfNext := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	firstOfAfter := firstOfNext.AddDate(0, 1, 0)
	d := firstOfAfter.AddDate(0, 0, -1)
	for d.Weekday() != time.Thursday {
		d = d.AddDate(0, 0, -1)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 15, 30, 0, 0, d.Location())
}

// FetchOptionChain synthesises a Black-Scholes-priced chain. ATM is the
// nearest strike to spot; OI peaks at ATM and decays away.
func (p *Provider) FetchOptionChain(_ context.Context, u provider.Underlying, expiry time.Time) ([]provider.ChainRow, error) {
	spot, step := underlyingSpot(u)
	if spot == 0 {
		return nil, provider.ErrUnknownUnderlying
	}
	atm := int(math.Round(spot/float64(step)) * float64(step))
	now := time.Now()
	dte := math.Max(1, expiry.Sub(now).Hours()/24)
	t := dte / 365.0

	// Seed deterministically per (underlying, expiry-day) so two calls in
	// the same minute return the same chain — handy for tests.
	seed := int64(atm) + expiry.Unix()/86400
	rng := rand.New(rand.NewSource(seed))

	out := make([]provider.ChainRow, 0, 30*2)
	for i := -15; i <= 15; i++ {
		strike := atm + i*step
		for _, side := range []provider.OptionType{provider.OptionTypeCE, provider.OptionTypePE} {
			row := synthesizeRow(u, expiry, strike, side, spot, t, rng)
			row.SnapshotTime = now
			row.Spot = spot
			out = append(out, row)
		}
	}
	return out, nil
}

// synthesizeRow returns one ChainRow with realistic-feeling values. IV
// smile is hardcoded — ATM ~16%, wings up to 28% at ATM±15.
func synthesizeRow(u provider.Underlying, expiry time.Time, strike int, side provider.OptionType, spot, t float64, rng *rand.Rand) provider.ChainRow {
	moneyness := math.Abs(float64(strike) - spot)
	iv := 0.16 + 0.008*math.Sqrt(moneyness/100) // smile
	d1 := (math.Log(spot/float64(strike)) + (0.06+0.5*iv*iv)*t) / (iv * math.Sqrt(t))
	d2 := d1 - iv*math.Sqrt(t)
	var ltp float64
	if side == provider.OptionTypeCE {
		ltp = spot*cnd(d1) - float64(strike)*math.Exp(-0.06*t)*cnd(d2)
	} else {
		ltp = float64(strike)*math.Exp(-0.06*t)*cnd(-d2) - spot*cnd(-d1)
	}
	if ltp < 0.05 {
		ltp = 0.05
	}
	// OI peaks at ATM. Triangular falloff for simplicity.
	oiPeak := int64(2_500_000)
	oi := int64(float64(oiPeak) * math.Max(0, 1-math.Abs(float64(strike)-spot)/(15*float64(50))))
	if oi < 1000 {
		oi = 1000
	}
	jitter := rng.Float64()*0.04 - 0.02
	return provider.ChainRow{
		Underlying:   u,
		ExpiryDate:   expiry,
		Strike:       strike,
		OptionType:   side,
		LTP:          round2(ltp * (1 + jitter)),
		Bid:          round2(ltp * 0.995),
		Ask:          round2(ltp * 1.005),
		OI:           oi,
		OIChange:     int64(rng.Float64()*100_000 - 50_000),
		Volume:       int64(rng.Float64() * float64(oi/3)),
	}
}

// cnd is the cumulative normal distribution — Abramowitz & Stegun. Mirrored
// from kairos/app/options/chain-data.ts:cnd() so the FE and BE compute
// identical greeks. ADR-0011 D7.
func cnd(x float64) float64 {
	a1, a2, a3, a4, a5 := 0.31938153, -0.356563782, 1.781477937, -1.821255978, 1.330274429
	k := 1.0 / (1.0 + 0.2316419*math.Abs(x))
	poly := k * (a1 + k*(a2+k*(a3+k*(a4+k*a5))))
	nd := (1.0 / math.Sqrt(2*math.Pi)) * math.Exp(-0.5*x*x)
	r := 1.0 - nd*poly
	if x >= 0 {
		return r
	}
	return 1.0 - r
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// FetchHistoricalCandles returns a deterministic random walk for the
// requested instrument. Good enough for backtest worker unit tests.
func (p *Provider) FetchHistoricalCandles(_ context.Context, req provider.HistoricalReq) ([]provider.Candle, error) {
	if req.From.After(req.To) {
		return nil, errors.New("from > to")
	}
	step := time.Minute
	switch req.Interval {
	case "5minute":
		step = 5 * time.Minute
	case "15minute":
		step = 15 * time.Minute
	case "day":
		step = 24 * time.Hour
	}
	rng := rand.New(rand.NewSource(req.From.Unix()))
	price := 100.0
	var out []provider.Candle
	for t := req.From; !t.After(req.To); t = t.Add(step) {
		open := price
		change := (rng.Float64() - 0.5) * 2
		high := open + math.Abs(change)
		low := open - math.Abs(change)
		close := open + change
		out = append(out, provider.Candle{
			Time:   t,
			Open:   round2(open),
			High:   round2(high),
			Low:    round2(low),
			Close:  round2(close),
			Volume: int64(rng.Float64() * 1_000_000),
		})
		price = close
	}
	return out, nil
}
