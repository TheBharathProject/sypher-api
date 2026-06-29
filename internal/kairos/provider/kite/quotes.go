package kite

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// Optional capabilities (provider/capabilities.go). Compile-time
// assertions so a signature drift breaks the build, not a type assert
// at runtime.
var (
	_ provider.QuoteFetcher       = (*Provider)(nil)
	_ provider.InstrumentSearcher = (*Provider)(nil)
)

// kiteQuoteBatchMax is Kite's documented cap on instruments per /quote
// call (https://kite.trade/docs/connect/v3/market-quotes/).
const kiteQuoteBatchMax = 500

// istLoc is the zone Kite's naive "2006-01-02 15:04:05" timestamps are
// in. Same fallback strategy as nextKiteExpiry (kite.go).
var istLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return time.FixedZone("IST", int((5*time.Hour + 30*time.Minute).Seconds()))
	}
	return loc
}()

// FetchQuotes implements provider.QuoteFetcher via Kite's /quote
// endpoint, batching at most kiteQuoteBatchMax symbols per call. Each
// batch waits on the shared 1 rps quote limiter, so a 600-symbol
// request costs two limiter slots.
//
// Symbols may be "EXCHANGE:TRADINGSYMBOL" or bare ("RELIANCE" →
// "NSE:RELIANCE"). The returned slice preserves input order; symbols
// Kite doesn't recognise are dropped from its response and therefore
// from ours — no error.
func (p *Provider) FetchQuotes(ctx context.Context, symbols []string) ([]provider.Quote, error) {
	out := make([]provider.Quote, 0, len(symbols))
	for start := 0; start < len(symbols); start += kiteQuoteBatchMax {
		batch := symbols[start:min(start+kiteQuoteBatchMax, len(symbols))]
		quotes, err := p.fetchQuoteBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		out = append(out, quotes...)
	}
	return out, nil
}

// fetchQuoteBatch does one /quote call for ≤ kiteQuoteBatchMax symbols.
func (p *Provider) fetchQuoteBatch(ctx context.Context, symbols []string) ([]provider.Quote, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	if len(symbols) > kiteQuoteBatchMax {
		return nil, fmt.Errorf("kite: quote batch %d exceeds cap %d", len(symbols), kiteQuoteBatchMax)
	}
	if err := p.quoteLim.Wait(ctx); err != nil {
		return nil, err
	}
	q := url.Values{}
	keys := make([]string, len(symbols))
	for i, s := range symbols {
		keys[i] = kiteQuoteKey(s)
		q.Add("i", keys[i])
	}
	req, err := p.newAuthedReq(ctx, "GET", "/quote?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Status string               `json:"status"`
		Data   map[string]fullQuote `json:"data"`
	}
	if err := p.do(req, &resp); err != nil {
		return nil, err
	}
	out := make([]provider.Quote, 0, len(symbols))
	for i, s := range symbols {
		fq, ok := resp.Data[keys[i]]
		if !ok {
			continue // Kite omits unknown symbols rather than erroring
		}
		out = append(out, translateQuote(s, fq))
	}
	return out, nil
}

// kiteQuoteKey normalises a caller symbol into Kite's
// "EXCHANGE:TRADINGSYMBOL" form. Bare symbols default to NSE.
func kiteQuoteKey(sym string) string {
	if strings.Contains(sym, ":") {
		return sym
	}
	return "NSE:" + sym
}

// fullQuote is the subset of Kite's full /quote envelope that Quote
// needs. Kept separate from quoteEnvelope (kite.go) — that one is
// shaped for the option-chain path (depth, OI) and doesn't carry ohlc
// or the exchange timestamp.
type fullQuote struct {
	LastPrice float64 `json:"last_price"`
	NetChange float64 `json:"net_change"`
	Volume    int64   `json:"volume"`
	Timestamp string  `json:"timestamp"` // "2006-01-02 15:04:05", IST, may be empty
	OHLC      struct {
		Open  float64 `json:"open"`
		High  float64 `json:"high"`
		Low   float64 `json:"low"`
		Close float64 `json:"close"`
	} `json:"ohlc"`
}

// translateQuote maps one Kite full quote onto the normalised
// provider.Quote. symbol is echoed back exactly as the caller passed
// it, so the handler's response matches what the FE asked for.
//
// Change/ChangePct are computed against ohlc.close (Kite's previous
// session close). When close is 0 (fresh listings, some indices) we
// fall back to Kite's own net_change and leave ChangePct at 0 rather
// than divide by zero.
func translateQuote(symbol string, fq fullQuote) provider.Quote {
	change := fq.NetChange
	var changePct float64
	if fq.OHLC.Close != 0 {
		change = fq.LastPrice - fq.OHLC.Close
		changePct = change / fq.OHLC.Close * 100
	}
	ts := time.Now()
	if fq.Timestamp != "" {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", fq.Timestamp, istLoc); err == nil {
			ts = t
		}
	}
	return provider.Quote{
		Symbol:    symbol,
		Last:      fq.LastPrice,
		Change:    change,
		ChangePct: changePct,
		Volume:    fq.Volume,
		OHLC: provider.OHLC{
			O: fq.OHLC.Open,
			H: fq.OHLC.High,
			L: fq.OHLC.Low,
			C: fq.OHLC.Close,
		},
		TS: ts,
	}
}
