package kairos

// Cost model constants (Indian options, sell-side STT + brokerage).
const (
	sttPctSell      = 0.0625 / 100.0 // 0.0625% STT on sell-side premium
	exchangeFeePct  = 0.000345       // 0.0345% (approximation across NSE/BSE)
	sebiTurnoverPct = 0.000010       // 0.0001% turnover charge
	gstOnFeesPct    = 0.18           // 18% GST on (brokerage + exchange + SEBI)
	brokerageFlat   = 20.0           // ₹20 / executed order (Zerodha-style)
	SlippageTicks   = 1              // 1-tick slippage per fill
	TickSize        = 0.05           // ₹0.05 per tick (standard for NSE options)
)

// OrderCosts describes a single executed order (one fill) for cost
// computation: side ("BUY" or "SELL"), per-unit premium, and quantity
// (lots × lot size).
type OrderCosts struct {
	Side    string
	Premium float64
	Qty     float64
}

// CostBreakdown itemises the charges for a single executed order.
type CostBreakdown struct {
	STT       float64
	Exchange  float64
	SEBI      float64
	GST       float64
	Brokerage float64
	Total     float64
}

// ComputeOrderCosts applies the Indian options cost model to one executed
// order: STT on sell-side premium only, exchange + SEBI turnover charges,
// flat brokerage per order, and GST on (brokerage + exchange + SEBI).
// Costs are approximate; real Zerodha varies a few rupees per trade from
// these numbers but the order of magnitude is correct.
func ComputeOrderCosts(o OrderCosts) CostBreakdown {
	turnover := o.Premium * o.Qty

	var stt float64
	if o.Side == "SELL" {
		stt = turnover * sttPctSell
	}
	exch := turnover * exchangeFeePct
	sebi := turnover * sebiTurnoverPct
	brokerage := float64(brokerageFlat)
	gst := (brokerage + exch + sebi) * gstOnFeesPct

	return CostBreakdown{
		STT:       stt,
		Exchange:  exch,
		SEBI:      sebi,
		GST:       gst,
		Brokerage: brokerage,
		Total:     stt + exch + sebi + brokerage + gst,
	}
}
