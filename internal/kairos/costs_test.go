package kairos

import (
	"math"
	"testing"
)

// TestOrderCosts verifies the per-order cost breakdown for a sell order:
// SELL 1 lot NIFTY (Premium 100, Qty 75) → turnover 7500. Every component
// is computed from the same constants the implementation uses, so this
// test pins the arithmetic, not magic numbers.
func TestOrderCosts(t *testing.T) {
	got := ComputeOrderCosts(OrderCosts{Side: "SELL", Premium: 100, Qty: 75})

	turnover := 100.0 * 75.0 // 7500
	wantSTT := turnover * sttPctSell
	wantExchange := turnover * exchangeFeePct
	wantSEBI := turnover * sebiTurnoverPct
	wantBrokerage := float64(brokerageFlat)
	wantGST := (wantBrokerage + wantExchange + wantSEBI) * gstOnFeesPct
	wantTotal := wantSTT + wantExchange + wantSEBI + wantBrokerage + wantGST

	const eps = 1e-9
	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"STT", got.STT, wantSTT},
		{"Exchange", got.Exchange, wantExchange},
		{"SEBI", got.SEBI, wantSEBI},
		{"GST", got.GST, wantGST},
		{"Brokerage", got.Brokerage, wantBrokerage},
		{"Total", got.Total, wantTotal},
	}
	for _, c := range checks {
		if math.Abs(c.got-c.want) > eps {
			t.Errorf("%s = %.12f, want %.12f", c.name, c.got, c.want)
		}
	}
}

// TestOrderCostsBuyNoSTT verifies that buy orders carry no STT (STT is
// charged on the sell-side premium only).
func TestOrderCostsBuyNoSTT(t *testing.T) {
	got := ComputeOrderCosts(OrderCosts{Side: "BUY", Premium: 100, Qty: 75})
	if got.STT != 0 {
		t.Errorf("STT on BUY = %.12f, want 0", got.STT)
	}

	turnover := 100.0 * 75.0
	wantExchange := turnover * exchangeFeePct
	wantSEBI := turnover * sebiTurnoverPct
	wantBrokerage := float64(brokerageFlat)
	wantGST := (wantBrokerage + wantExchange + wantSEBI) * gstOnFeesPct
	wantTotal := wantExchange + wantSEBI + wantBrokerage + wantGST
	if math.Abs(got.Total-wantTotal) > 1e-9 {
		t.Errorf("Total = %.12f, want %.12f", got.Total, wantTotal)
	}
}
