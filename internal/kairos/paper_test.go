package kairos

import (
	"math"
	"testing"
)

// almostEq compares money values with a tolerance well under a paisa.
func almostEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

// TestApplyFillLong walks a long position through build-up, averaging
// and a partial close:
//
//	BUY 75 @100            → qty 75,  avg 100
//	BUY 75 @110            → qty 150, avg 105 (weighted average)
//	SELL 75 @120           → qty 75,  avg 105, realized 75×(120−105) = 1125
func TestApplyFillLong(t *testing.T) {
	pos := PaperPosition{}

	pos = ApplyFill(pos, "BUY", 75, 100)
	if pos.Qty != 75 || !almostEq(pos.AvgPrice, 100) || !almostEq(pos.RealizedPnL, 0) {
		t.Fatalf("after BUY 75@100: got qty=%d avg=%v realized=%v, want 75/100/0",
			pos.Qty, pos.AvgPrice, pos.RealizedPnL)
	}

	pos = ApplyFill(pos, "BUY", 75, 110)
	if pos.Qty != 150 || !almostEq(pos.AvgPrice, 105) || !almostEq(pos.RealizedPnL, 0) {
		t.Fatalf("after BUY 75@110: got qty=%d avg=%v realized=%v, want 150/105/0",
			pos.Qty, pos.AvgPrice, pos.RealizedPnL)
	}

	pos = ApplyFill(pos, "SELL", 75, 120)
	if pos.Qty != 75 {
		t.Fatalf("after SELL 75@120: got qty=%d, want 75", pos.Qty)
	}
	if !almostEq(pos.AvgPrice, 105) {
		t.Fatalf("after SELL 75@120: avg should stay 105 on a partial close, got %v", pos.AvgPrice)
	}
	if want := 75.0 * 15.0; !almostEq(pos.RealizedPnL, want) {
		t.Fatalf("after SELL 75@120: got realized=%v, want %v", pos.RealizedPnL, want)
	}
}

// TestApplyFillFlipShort crosses zero in one fill: from long 75 @100, a
// SELL of 150 @90 closes the long (realizing 75×(90−100) = −750) and
// opens a 75-lot short whose avg resets to the fill price.
func TestApplyFillFlipShort(t *testing.T) {
	pos := PaperPosition{Qty: 75, AvgPrice: 100}

	pos = ApplyFill(pos, "SELL", 150, 90)
	if pos.Qty != -75 {
		t.Fatalf("got qty=%d, want -75", pos.Qty)
	}
	if !almostEq(pos.AvgPrice, 90) {
		t.Fatalf("avg must reset to the fill price when crossing zero: got %v, want 90", pos.AvgPrice)
	}
	if !almostEq(pos.RealizedPnL, -750) {
		t.Fatalf("got realized=%v, want -750", pos.RealizedPnL)
	}
}

// TestApplyFillCoverShort is the mirror: short 75 @100, BUY 75 @90
// covers fully and realizes 75×(100−90) = +750.
func TestApplyFillCoverShort(t *testing.T) {
	pos := PaperPosition{Qty: -75, AvgPrice: 100}

	pos = ApplyFill(pos, "BUY", 75, 90)
	if pos.Qty != 0 {
		t.Fatalf("got qty=%d, want 0", pos.Qty)
	}
	if !almostEq(pos.RealizedPnL, 750) {
		t.Fatalf("got realized=%v, want 750", pos.RealizedPnL)
	}
}

// TestLimitCross pins limit semantics: a BUY limit fills when the
// market trades at or below the limit; a SELL limit fills at or above.
func TestLimitCross(t *testing.T) {
	cases := []struct {
		side  string
		limit float64
		last  float64
		want  bool
	}{
		{"BUY", 100, 99.5, true},   // market below buy limit → fills
		{"BUY", 100, 100, true},    // exactly at limit → fills
		{"BUY", 100, 100.5, false}, // above limit → waits
		{"SELL", 100, 99.5, false}, // market below sell limit → waits
		{"SELL", 100, 100, true},   // exactly at limit → fills
		{"SELL", 100, 101, true},   // above limit → fills
	}
	for _, c := range cases {
		if got := LimitCrossed(c.side, c.limit, c.last); got != c.want {
			t.Errorf("LimitCrossed(%s, limit=%v, last=%v) = %v, want %v",
				c.side, c.limit, c.last, got, c.want)
		}
	}
}
