// Package greeks is the single Black-Scholes implementation for Kairos.
//
// ADR-0009 D3: greeks (IV, delta, gamma, theta, vega) are never stored
// in kairos.option_chains — they are computed at read time from
// (ltp, spot, strike, tte). Every consumer (chain enrichment, intraday
// analytics, paper trading) goes through this package so the numbers
// agree everywhere.
//
// Conventions:
//   - vol and IV are fractions (0.18 = 18%); callers that want percent
//     multiply by 100 at the presentation layer.
//   - tte is in years (calendar days / 365).
//   - theta is per calendar day; vega is per 1 vol-point (1%).
//   - delta is SIGNED — negative for puts. Presentation layers that
//     render both sides positive take abs() themselves.
package greeks

import (
	"errors"
	"math"
)

// RiskFreeRate is the annualised risk-free rate used across Kairos when
// no caller-specific rate applies. Roughly the RBI repo rate; revised
// rarely enough that a const beats a config knob.
const RiskFreeRate = 0.065

// ErrPriceOutOfBounds is returned by ImpliedVol when the observed price
// violates no-arbitrage bounds, so no volatility can reproduce it.
var ErrPriceOutOfBounds = errors.New("greeks: price outside no-arbitrage bounds")

// normCDF is the standard normal cumulative distribution function,
// via math.Erf (no external stats dependency).
func normCDF(x float64) float64 {
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

// normPDF is the standard normal density.
func normPDF(x float64) float64 {
	return math.Exp(-x*x/2) / math.Sqrt(2*math.Pi)
}

// d1d2 returns the Black-Scholes d1 and d2 terms.
func d1d2(spot, strike, r, vol, tte float64) (float64, float64) {
	d1 := (math.Log(spot/strike) + (r+vol*vol/2)*tte) / (vol * math.Sqrt(tte))
	return d1, d1 - vol*math.Sqrt(tte)
}

// BSPrice returns the Black-Scholes price of a European option.
// Degenerate inputs (tte or vol <= 0) collapse to discounted intrinsic
// value, the model's limit as variance goes to zero.
func BSPrice(isCall bool, spot, strike, r, vol, tte float64) float64 {
	if tte <= 0 || vol <= 0 || spot <= 0 || strike <= 0 {
		disc := strike * math.Exp(-r*tte)
		if isCall {
			return math.Max(spot-disc, 0)
		}
		return math.Max(disc-spot, 0)
	}
	d1, d2 := d1d2(spot, strike, r, vol, tte)
	disc := strike * math.Exp(-r*tte)
	if isCall {
		return spot*normCDF(d1) - disc*normCDF(d2)
	}
	return disc*normCDF(-d2) - spot*normCDF(-d1)
}

// ImpliedVol inverts BSPrice for vol by bisection on [0.01, 5.0]
// (80 iterations — interval width ~5/2^80, far below any tolerance we
// care about). Returns ErrPriceOutOfBounds when the price cannot be
// produced by any vol: below discounted intrinsic, or above the
// option's value ceiling (spot for calls, discounted strike for puts).
func ImpliedVol(isCall bool, price, spot, strike, r, tte float64) (float64, error) {
	if spot <= 0 || strike <= 0 || tte <= 0 || price <= 0 {
		return 0, ErrPriceOutOfBounds
	}
	disc := strike * math.Exp(-r*tte)
	var lower, upper float64
	if isCall {
		lower, upper = math.Max(spot-disc, 0), spot
	} else {
		lower, upper = math.Max(disc-spot, 0), disc
	}
	if price < lower || price > upper {
		return 0, ErrPriceOutOfBounds
	}
	lo, hi := 0.01, 5.0
	for i := 0; i < 80; i++ {
		mid := (lo + hi) / 2
		if BSPrice(isCall, spot, strike, r, mid, tte) > price {
			hi = mid
		} else {
			lo = mid
		}
	}
	return (lo + hi) / 2, nil
}

// Greeks is one option's sensitivities at a given vol. IV echoes the
// vol the greeks were computed at (a fraction, e.g. 0.18).
type Greeks struct {
	IV    float64
	Delta float64
	Gamma float64
	Theta float64
	Vega  float64
}

// Compute returns the Black-Scholes greeks. Theta is per calendar day,
// vega per 1 vol-point, delta signed (negative for puts). Degenerate
// inputs return a zero-value Greeks (only IV echoed) rather than NaN.
func Compute(isCall bool, spot, strike, r, vol, tte float64) Greeks {
	g := Greeks{IV: vol}
	if tte <= 0 || vol <= 0 || spot <= 0 || strike <= 0 {
		return g
	}
	d1, d2 := d1d2(spot, strike, r, vol, tte)
	pdf := normPDF(d1)
	sqrtT := math.Sqrt(tte)
	disc := strike * math.Exp(-r*tte)

	if isCall {
		g.Delta = normCDF(d1)
	} else {
		g.Delta = normCDF(d1) - 1
	}
	g.Gamma = pdf / (spot * vol * sqrtT)
	// Vega per 1 vol-point: dPrice/dVol is for a 1.0 (=100%) move, so /100.
	g.Vega = spot * pdf * sqrtT / 100
	// Theta per calendar day: annual theta / 365.
	if isCall {
		g.Theta = (-spot*pdf*vol/(2*sqrtT) - r*disc*normCDF(d2)) / 365
	} else {
		g.Theta = (-spot*pdf*vol/(2*sqrtT) + r*disc*normCDF(-d2)) / 365
	}
	return g
}
