// Package provider is the broker-agnostic data-feed abstraction for Kairos.
//
// See docs/adr/0011-kairos-provider-abstraction.md for the design.
//
// Types in this file are the normalised over-the-wire shapes the rest of
// the kairos package consumes. Provider implementations translate from
// their native API responses into these shapes (e.g. provider/kite/translate.go).
//
// The shapes are intentionally 1:1 with the frontend in
// kairos/app/options/chain-data.ts so the wire format is the same shape
// the React components already render.
package provider

import "time"

// Underlying is one of the three indices Kairos tracks. Constants are
// the canonical short names used in DB rows and API responses.
type Underlying string

const (
	UnderlyingNIFTY     Underlying = "NIFTY"
	UnderlyingBANKNIFTY Underlying = "BANKNIFTY"
	UnderlyingSENSEX    Underlying = "SENSEX"
)

// AllUnderlyings returns the indices the ingest cron iterates over.
// Order is deterministic for stable logging.
func AllUnderlyings() []Underlying {
	return []Underlying{UnderlyingNIFTY, UnderlyingBANKNIFTY, UnderlyingSENSEX}
}

// OptionType is CE or PE. Two characters because that's what NSE uses and
// what every Indian options tool puts in its UI.
type OptionType string

const (
	OptionTypeCE OptionType = "CE"
	OptionTypePE OptionType = "PE"
)

// ChainRow is one strike on one side at one snapshot time. Mirrors the
// `OptionData` shape in kairos/app/options/chain-data.ts.
//
// Greeks (IV, delta, gamma, theta, vega) are deliberately not on this
// type — they're computed at read time from (ltp, spot, strike, dte)
// via internal/kairos/greeks. ADR-0009 D3.
type ChainRow struct {
	Underlying    Underlying `json:"underlying"`
	ExpiryDate    time.Time  `json:"expiry_date"`
	Strike        int        `json:"strike"`
	OptionType    OptionType `json:"option_type"`
	SnapshotTime  time.Time  `json:"snapshot_time"`
	Spot          float64    `json:"spot"`
	LTP           float64    `json:"ltp"`
	Bid           float64    `json:"bid"`
	Ask           float64    `json:"ask"`
	OI            int64      `json:"oi"`
	OIChange      int64      `json:"oi_change"`
	Volume        int64      `json:"volume"`
}

// Expiry is one tradeable expiry date for an underlying. Type is the
// expiry's cadence — used by the strategy DSL's `expiryRule`
// (WEEKLY|NEXT_WEEKLY|MONTHLY) to resolve a leg to a specific date.
type Expiry struct {
	Date time.Time `json:"date"`
	Type string    `json:"type"` // "weekly" | "monthly"
}

// Spot is the current spot price and a hint at intraday change.
// Change is in absolute points; ChangePct in percent (e.g. 0.59 = +0.59%).
type Spot struct {
	Underlying Underlying `json:"underlying"`
	Price      float64    `json:"price"`
	Change     float64    `json:"change"`
	ChangePct  float64    `json:"change_pct"`
	AsOf       time.Time  `json:"as_of"`
}

// Candle is one OHLCV bar. Used by the backtest worker (when running on
// equity reference data) and by the /charts page for non-options
// instruments (which go through internal/kairos/marketdata, ADR-0014).
type Candle struct {
	Time   time.Time `json:"time"`
	Open   float64   `json:"open"`
	High   float64   `json:"high"`
	Low    float64   `json:"low"`
	Close  float64   `json:"close"`
	Volume int64     `json:"volume"`
}

// HistoricalReq is the parameter bundle for FetchHistoricalCandles.
// Interval matches Kite's vocabulary (which Dhan/Upstox/Angel either
// share or trivially translate to): "minute" | "3minute" | "5minute" |
// "10minute" | "15minute" | "30minute" | "60minute" | "day".
//
// Instrument is the provider-native symbol — the caller is responsible
// for resolving the right token (Kite has its own scheme; Dhan has
// theirs). Currently the only caller is the backtest worker, which
// resolves the instrument via a chain query against the DB, then asks
// the provider for that instrument's candle history.
type HistoricalReq struct {
	Instrument string
	Interval   string
	From       time.Time
	To         time.Time
}
