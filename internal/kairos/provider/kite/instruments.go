package kite

import (
	"context"
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
// We keep two slices of the dump in memory:
//   - index options on NFO + BFO (the chain/expiry path):
//       NIFTY (NFO), BANKNIFTY (NFO), SENSEX (BFO)
//   - searchable cash instruments (the SearchInstruments capability):
//       NSE equities (exchange=NSE, segment=NSE, instrument_type=EQ)
//       and indices (segment=INDICES on NSE/BSE — BSE included so
//       SENSEX is findable)

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

// searchRow is one searchable cash instrument (NSE equity or index)
// kept for the SearchInstruments capability. Separate from instrument
// so the option-chain lookups (Expiries/OptionInstruments) never have
// to skip over equities.
//
// symU/nameU are pre-uppercased copies of TradingSymbol/Name so each
// search query doesn't re-fold ~2k strings.
type searchRow struct {
	Token         int64
	Exchange      string // "NSE" | "BSE"
	TradingSymbol string // e.g. "RELIANCE", "NIFTY 50"
	Name          string // e.g. "RELIANCE INDUSTRIES"
	Kind          string // "EQ" | "INDEX"
	LotSize       int
	symU, nameU   string
}

// instrumentIndex is the in-memory lookup structure populated from the
// Kite instruments CSV. Refreshed every 12h (kite.go:ensureInstruments).
type instrumentIndex struct {
	all      []instrument
	byToken  map[int64]instrument
	bySymbol map[string]int64 // exchange:tradingsymbol → token
	search   []searchRow      // NSE equities + indices, for SearchInstruments
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
	required := []string{"instrument_token", "tradingsymbol", "name", "expiry", "strike", "instrument_type", "exchange", "segment", "lot_size"}
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
		// Searchable cash instruments (NSE EQ + indices) go in the
		// search slice; everything below is the index-options path.
		if row, ok := searchableRow(rec, col, exchange); ok {
			idx.search = append(idx.search, row)
			continue
		}
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

// searchableRow classifies one CSV record as a searchable cash
// instrument. Returns ok=false for anything that isn't an NSE equity
// or an NSE/BSE index — those rows fall through to the options path.
func searchableRow(rec []string, col map[string]int, exchange string) (searchRow, bool) {
	segment := rec[col["segment"]]
	instrType := rec[col["instrument_type"]]
	var kind string
	switch {
	case segment == "INDICES" && (exchange == "NSE" || exchange == "BSE"):
		kind = "INDEX"
	case exchange == "NSE" && segment == "NSE" && instrType == "EQ":
		kind = "EQ"
	default:
		return searchRow{}, false
	}
	tok, err := strconv.ParseInt(rec[col["instrument_token"]], 10, 64)
	if err != nil {
		return searchRow{}, false
	}
	lotSize, _ := strconv.Atoi(rec[col["lot_size"]])
	sym := rec[col["tradingsymbol"]]
	name := rec[col["name"]]
	return searchRow{
		Token:         tok,
		Exchange:      exchange,
		TradingSymbol: sym,
		Name:          name,
		Kind:          kind,
		LotSize:       lotSize,
		symU:          strings.ToUpper(sym),
		nameU:         strings.ToUpper(name),
	}, true
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

// maxSearchResults is the hard cap on SearchInstruments results,
// regardless of the limit the caller asks for.
const maxSearchResults = 50

// SearchInstruments implements provider.InstrumentSearcher against the
// in-memory instruments cache — no network call once the cache is warm
// (ensureInstruments refreshes it every 12h).
//
// Matching is case-insensitive over tradingsymbol and name; prefix
// matches rank above contains matches, alphabetical by symbol within
// each rank. Only NSE equities and indices are searchable (see
// searchableRow). limit is clamped to (0, maxSearchResults].
func (p *Provider) SearchInstruments(ctx context.Context, query string, limit int) ([]provider.Instrument, error) {
	if err := p.ensureInstruments(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxSearchResults {
		limit = maxSearchResults
	}
	p.instrumentsMu.RLock()
	defer p.instrumentsMu.RUnlock()
	return p.instruments.Search(query, limit), nil
}

// Search scans the searchable rows for query. See SearchInstruments
// for the ranking contract. An empty/whitespace query returns nil —
// we never dump the whole universe.
func (idx *instrumentIndex) Search(query string, limit int) []provider.Instrument {
	q := strings.ToUpper(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var prefix, contains []searchRow
	for _, r := range idx.search {
		switch {
		case strings.HasPrefix(r.symU, q) || strings.HasPrefix(r.nameU, q):
			prefix = append(prefix, r)
		case strings.Contains(r.symU, q) || strings.Contains(r.nameU, q):
			contains = append(contains, r)
		}
	}
	bySymbol := func(rows []searchRow) {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].TradingSymbol != rows[j].TradingSymbol {
				return rows[i].TradingSymbol < rows[j].TradingSymbol
			}
			return rows[i].Exchange < rows[j].Exchange
		})
	}
	bySymbol(prefix)
	bySymbol(contains)
	out := make([]provider.Instrument, 0, limit)
	for _, r := range append(prefix, contains...) {
		if len(out) == limit {
			break
		}
		out = append(out, provider.Instrument{
			Symbol:   r.TradingSymbol,
			Name:     r.Name,
			Exchange: r.Exchange,
			Kind:     r.Kind,
			LotSize:  r.LotSize,
			Token:    strconv.FormatInt(r.Token, 10),
		})
	}
	return out
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
