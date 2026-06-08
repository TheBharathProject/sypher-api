package kite

import (
	"fmt"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// translateChain converts Kite's /quote response map keyed by
// "EXCHANGE:TRADINGSYMBOL" into our normalised ChainRow slice.
//
// The spot for the snapshot is the index symbol's last_price; everything
// else is the per-strike option quote.
func translateChain(
	u provider.Underlying,
	expiry time.Time,
	instr []instrument,
	data map[string]quoteEnvelope,
) []provider.ChainRow {
	// Spot lookup. If the spot isn't in the response (shouldn't happen
	// for a valid request), fall back to zero — downstream renders an
	// empty change badge.
	var spot float64
	if s, ok := indexSymbol(u); ok {
		if v, ok2 := data[s]; ok2 {
			spot = v.LastPrice
		}
	}
	now := time.Now()
	out := make([]provider.ChainRow, 0, len(instr))
	for _, ins := range instr {
		key := fmt.Sprintf("%s:%s", ins.Exchange, ins.TradingSymbol)
		q, ok := data[key]
		if !ok {
			continue
		}
		bid, ask := q.LastPrice, q.LastPrice
		if len(q.Depth.Buy) > 0 && q.Depth.Buy[0].Price > 0 {
			bid = q.Depth.Buy[0].Price
		}
		if len(q.Depth.Sell) > 0 && q.Depth.Sell[0].Price > 0 {
			ask = q.Depth.Sell[0].Price
		}
		out = append(out, provider.ChainRow{
			Underlying:   u,
			ExpiryDate:   expiry,
			Strike:       ins.Strike,
			OptionType:   ins.Type,
			SnapshotTime: now,
			Spot:         spot,
			LTP:          q.LastPrice,
			Bid:          bid,
			Ask:          ask,
			OI:           q.OI,
			OIChange:     q.OIDayChange,
			Volume:       q.Volume,
		})
	}
	return out
}

// translateCandles converts Kite's historical-data candle rows into our
// Candle slice. Kite returns each candle as an array:
//   [timestamp_string, open, high, low, close, volume]
//
// The slice has untyped any-values (because JSON has no tuple type)
// so we have to type-assert each element.
func translateCandles(raw [][]any) []provider.Candle {
	out := make([]provider.Candle, 0, len(raw))
	for _, c := range raw {
		if len(c) < 6 {
			continue
		}
		ts, _ := c[0].(string)
		t, err := time.Parse("2006-01-02T15:04:05-0700", ts)
		if err != nil {
			t, err = time.Parse("2006-01-02T15:04:05Z", ts)
			if err != nil {
				continue
			}
		}
		out = append(out, provider.Candle{
			Time:   t,
			Open:   toF(c[1]),
			High:   toF(c[2]),
			Low:    toF(c[3]),
			Close:  toF(c[4]),
			Volume: int64(toF(c[5])),
		})
	}
	return out
}

func toF(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}
