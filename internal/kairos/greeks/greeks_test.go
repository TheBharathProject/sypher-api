package greeks

import (
	"math"
	"testing"
)

// TestImpliedVolRoundTrip prices a call at a known vol, then recovers
// that vol from the price. Bisection on [0.01, 5.0] over 80 iterations
// leaves an interval far below the 1e-4 tolerance.
func TestImpliedVolRoundTrip(t *testing.T) {
	const (
		spot   = 24000.0
		strike = 24200.0
		r      = 0.065
		vol    = 0.18
		tte    = 14.0 / 365.0
	)
	price := BSPrice(true, spot, strike, r, vol, tte)
	if price <= 0 {
		t.Fatalf("BSPrice returned non-positive price %v", price)
	}
	iv, err := ImpliedVol(true, price, spot, strike, r, tte)
	if err != nil {
		t.Fatalf("ImpliedVol: %v", err)
	}
	if diff := math.Abs(iv - vol); diff > 1e-4 {
		t.Fatalf("recovered iv %v, want %v (|diff|=%v > 1e-4)", iv, vol, diff)
	}
}

// TestPutCallParity asserts c - p == spot - strike*e^(-r*tte) — exact
// for any Black-Scholes implementation, so a tight 1e-6 tolerance.
func TestPutCallParity(t *testing.T) {
	const (
		spot   = 24000.0
		strike = 24000.0
		r      = 0.065
		vol    = 0.20
		tte    = 30.0 / 365.0
	)
	c := BSPrice(true, spot, strike, r, vol, tte)
	p := BSPrice(false, spot, strike, r, vol, tte)
	parity := c - p - (spot - strike*math.Exp(-r*tte))
	if math.Abs(parity) > 1e-6 {
		t.Fatalf("put-call parity violated: c=%v p=%v residual=%v", c, p, parity)
	}
}

// TestGreeksSanity checks an ATM call's greeks have the textbook signs
// and magnitudes: delta near 0.5 (slightly above with positive rates),
// long options have positive gamma and vega, and they decay (theta < 0).
func TestGreeksSanity(t *testing.T) {
	g := Compute(true, 24000, 24000, 0.065, 0.20, 30.0/365.0)
	if g.Delta <= 0.45 || g.Delta >= 0.60 {
		t.Errorf("ATM call delta = %v, want in (0.45, 0.60)", g.Delta)
	}
	if g.Gamma <= 0 {
		t.Errorf("gamma = %v, want > 0", g.Gamma)
	}
	if g.Vega <= 0 {
		t.Errorf("vega = %v, want > 0", g.Vega)
	}
	if g.Theta >= 0 {
		t.Errorf("theta = %v, want < 0", g.Theta)
	}
}
