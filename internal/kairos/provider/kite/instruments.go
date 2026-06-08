package kite

import (
	"encoding/csv"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// Kite's instruments CSV documents:
// https://kite.trade/docs/connect/v3/market-quotes/#retrieving-full-list-of-instruments
//
// Columns we use:
//   instrument_token, exchange_token, tradingsymbol, name, last_price,
//   expiry, strike, tick_size, lot_size, instrument_type, segment, exchange.
//
// We only care about index options on NFO + BFO:
//   - NIFTY  (NFO segment, name="NIFTY")
//   - BANKNIFTY (NFO segment, name="BANKNIFTY")
//   - SENSEX (BFO segment, name="SENSEX")

// instrument is the slimmed-down row we keep in memory. The full CSV
// has ~80k rows; we discard everything that isn't an index option on a
// supported underlying.
type instrument struct {
	Token         int64
	Exchange      string // "NFO" | "BFO"
	TradingSymbol string // e.g. "NIFTY25MAY24200CE"
	Name          string // e.g. "NIFTY"
	Expiry        time.Time
	Strike        int
	Type          provider.OptionType
	LotSize       int
}

// instrumentIndex is the in-memory lookup structure populated from the
// Kite instruments CSV. Refreshed every 12h (kite.go:ensureInstruments).
type instrumentIndex struct {
	all      []instrument
	byToken  map[int64]instrument
	bySymbol map[string]int64 // exchange:tradingsymbol → token
}

// parseInstruments reads the Kite CSV from r and returns the filtered
// index. Errors are returned for malformed CSVs but unknown / unhandled
// rows are silently skipped — we don't need to load equity, futures, or
// currency derivatives.
func parseInstruments(r io.Reader) (instrumentIndex, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate variable widths
	idx := instrumentIndex{
		byToken:  map[int64]instrument{},
		bySymbol: map[string]int64{},
	}
	// First row is the header.
	header, err := cr.Read()
	if err != nil {
		return idx, err
	}
	col := map[string]int{}
	for i, c := range header {
		col[strings.TrimSpace(c)] = i
	}
	required := []string{"instrument_token", "tradingsymbol", "name", "expiry", "strike", "instrument_type", "exchange", "lot_size"}
	for _, c := range required {
		if _, ok := col[c]; !ok {
			return idx, errors.New("kite instruments CSV missing column: " + c)
		}
	}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return idx, err
		}
		exchange := rec[col["exchange"]]
		if exchange != "NFO" && exchange != "BFO" {
			continue
		}
		name := rec[col["name"]]
		if name != "NIFTY" && name != "BANKNIFTY" && name != "SENSEX" {
			continue
		}
		instrType := rec[col["instrument_type"]]
		if instrType != "CE" && instrType != "PE" {
			continue
		}
		tokStr := rec[col["instrument_token"]]
		tok, err := strconv.ParseInt(tokStr, 10, 64)
		if err != nil {
			continue
		}
		expStr := rec[col["expiry"]]
		exp, err := parseExpiry(expStr)
		if err != nil {
			continue
		}
		strikeStr := rec[col["strike"]]
		strikeF, err := strconv.ParseFloat(strikeStr, 64)
		if err != nil {
			continue
		}
		lotSize, _ := strconv.Atoi(rec[col["lot_size"]])
		ins := instrument{
			Token:         tok,
			Exchange:      exchange,
			TradingSymbol: rec[col["tradingsymbol"]],
			Name:          name,
			Expiry:        exp,
			Strike:        int(strikeF),
			Type:          provider.OptionType(instrType),
			LotSize:       lotSize,
		}
		idx.all = append(idx.all, ins)
		idx.byToken[tok] = ins
		idx.bySymbol[exchange+":"+ins.TradingSymbol] = tok
	}
	return idx, nil
}

// parseExpiry handles Kite's date format. They use 2006-01-02 in the
// CSV but sometimes a header gives "DD-Mon-YYYY"; we accept both.
func parseExpiry(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", "02-Jan-2006", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("unrecognised expiry format")
}

// Expiries returns the unique tradeable expiries for an underlying,
// sorted ascending. Type is inferred by "last Thursday of month"
// heuristic — every weekly expiry is "weekly", the monthly one is the
// final Thursday of its month (Kite doesn't label them).
func (idx *instrumentIndex) Expiries(u provider.Underlying) []provider.Expiry {
	name := underlyingName(u)
	seen := map[time.Time]struct{}{}
	var dates []time.Time
	for _, ins := range idx.all {
		if ins.Name != name {
			continue
		}
		if _, ok := seen[ins.Expiry]; ok {
			continue
		}
		seen[ins.Expiry] = struct{}{}
		dates = append(dates, ins.Expiry)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	monthlies := map[time.Time]struct{}{}
	// Group by year-month and tag the latest in each month as monthly.
	byMonth := map[string]time.Time{}
	for _, d := range dates {
		k := d.Format("2006-01")
		if cur, ok := byMonth[k]; !ok || d.After(cur) {
			byMonth[k] = d
		}
	}
	for _, d := range byMonth {
		monthlies[d] = struct{}{}
	}
	out := make([]provider.Expiry, 0, len(dates))
	for _, d := range dates {
		typ := "weekly"
		if _, ok := monthlies[d]; ok {
			typ = "monthly"
		}
		out = append(out, provider.Expiry{Date: d, Type: typ})
	}
	return out
}

// OptionInstruments returns all CE+PE instruments for an underlying on
// a given expiry, ordered by strike asc then CE before PE.
func (idx *instrumentIndex) OptionInstruments(u provider.Underlying, expiry time.Time) []instrument {
	name := underlyingName(u)
	target := expiry.UTC().Format("2006-01-02")
	var out []instrument
	for _, ins := range idx.all {
		if ins.Name != name {
			continue
		}
		if ins.Expiry.UTC().Format("2006-01-02") != target {
			continue
		}
		out = append(out, ins)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Strike != out[j].Strike {
			return out[i].Strike < out[j].Strike
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// TokenForSymbol resolves a Kite "EXCHANGE:TRADINGSYMBOL" to its
// numeric instrument_token. Used by FetchHistoricalCandles when the
// backtest worker passes a symbol in.
func (idx *instrumentIndex) TokenForSymbol(sym string) (int64, bool) {
	tok, ok := idx.bySymbol[sym]
	return tok, ok
}

// underlyingName converts our Underlying enum into Kite's `name` field.
// For SENSEX, the BFO contract's `name` is "SENSEX". For Nifty and
// BankNifty it's the same string.
func underlyingName(u provider.Underlying) string {
	switch u {
	case provider.UnderlyingNIFTY:
		return "NIFTY"
	case provider.UnderlyingBANKNIFTY:
		return "BANKNIFTY"
	case provider.UnderlyingSENSEX:
		return "SENSEX"
	}
	return ""
}
