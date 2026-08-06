package dhan

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// Dhan security master CSV: https://images.dhan.co/api-data/api-scrip-master.csv
//
// Relevant columns (detected dynamically; names may shift between releases):
//   SEM_EXM_EXCH_ID      — exchange ("NSE", "BSE", "NFO", "BFO", "IDX_I")
//   SEM_SMST_SECURITY_ID — numeric security ID used in all API calls
//   SEM_INSTRUMENT_NAME  — "EQUITY", "INDEX", "OPTIDX", "OPTSTK", "FUTIDX", …
//   SEM_ENTITY_NAME      — issuer / underlying name ("NIFTY", "RELIANCE IND LTD", …)
//   SEM_EXPIRY_DATE      — "YYYY-MM-DD HH:MM:SS" for derivatives, blank otherwise
//   SEM_STRIKE_PRICE     — float, blank/0 for non-options
//   SEM_OPTION_TYPE      — "CE", "PE", or "-"
//   SEM_LOT_UNITS        — lot size integer
//   SEM_TRADING_SYMBOL   — exchange trading symbol
//
// We load two slices:
//   all    — OPTIDX instruments on NFO + BFO for our three underlyings
//             (used by Expiries / ForSymbol / HistoricalReq lookup)
//   search — NSE equities + all exchange indices
//             (used by SearchInstruments)

const maxSearchResults = 50

// instrument is the in-memory representation of one options contract or
// equity / index instrument kept for API calls.
type instrument struct {
	SecurityID    int64
	Exchange      string              // "NFO" | "BFO" | "NSE" | "BSE" | "IDX_I"
	Segment       string              // Dhan API segment: "NSE_FO", "BSE_FO", "NSE_EQ", "BSE_EQ", "IDX_I"
	InstrType     string              // "OPTIDX" | "EQUITY" | "INDEX"
	TradingSymbol string              // e.g. "NIFTY25JAN24CE23000"
	Name          string              // underlying or company name
	Expiry        time.Time
	Strike        float64
	OptionType    provider.OptionType // "CE" | "PE" | ""
	LotSize       int
}

// searchRow is one searchable equity or index kept for SearchInstruments.
type searchRow struct {
	SecurityID    int64
	Exchange      string
	Segment       string
	TradingSymbol string
	Name          string
	Kind          string // "EQ" | "INDEX"
	LotSize       int
	symU, nameU   string // pre-uppercased for fast search
}

// instrumentIndex is the in-memory lookup built from the CSV.
type instrumentIndex struct {
	all      []instrument
	bySymbol map[string]instrument // "EXCHANGE:TRADINGSYMBOL" → instrument
	search   []searchRow
}

// parseInstruments reads the Dhan security master CSV and returns the
// filtered index. Unknown/unhandled rows are silently skipped.
func parseInstruments(r io.Reader) (instrumentIndex, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	idx := instrumentIndex{bySymbol: map[string]instrument{}}

	header, err := cr.Read()
	if err != nil {
		return idx, err
	}
	col := map[string]int{}
	for i, c := range header {
		col[strings.TrimSpace(c)] = i
	}
	required := []string{
		"SEM_EXM_EXCH_ID", "SEM_SMST_SECURITY_ID", "SEM_INSTRUMENT_NAME",
		"SEM_ENTITY_NAME", "SEM_TRADING_SYMBOL", "SEM_LOT_UNITS",
	}
	for _, c := range required {
		if _, ok := col[c]; !ok {
			return idx, errors.New("dhan instruments CSV missing column: " + c)
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
		exch := field(rec, col, "SEM_EXM_EXCH_ID")
		instrName := field(rec, col, "SEM_INSTRUMENT_NAME")
		entityName := field(rec, col, "SEM_ENTITY_NAME")

		// Searchable equities and indices.
		if sr, ok := toSearchRow(rec, col, exch, instrName, entityName); ok {
			idx.search = append(idx.search, sr)
			continue
		}

		// Index options on NFO (NIFTY, BANKNIFTY) and BFO (SENSEX).
		if exch != "NFO" && exch != "BFO" {
			continue
		}
		if instrName != "OPTIDX" {
			continue
		}
		name := strings.ToUpper(entityName)
		if name != "NIFTY" && name != "BANKNIFTY" && name != "SENSEX" {
			continue
		}

		secIDStr := field(rec, col, "SEM_SMST_SECURITY_ID")
		secID, err := strconv.ParseInt(secIDStr, 10, 64)
		if err != nil {
			continue
		}
		tradingSym := field(rec, col, "SEM_TRADING_SYMBOL")
		lotSize, _ := strconv.Atoi(field(rec, col, "SEM_LOT_UNITS"))

		expiry, err := parseDhanExpiry(field(rec, col, "SEM_EXPIRY_DATE"))
		if err != nil {
			continue
		}
		strikeF, _ := strconv.ParseFloat(field(rec, col, "SEM_STRIKE_PRICE"), 64)
		optType := strings.ToUpper(field(rec, col, "SEM_OPTION_TYPE"))
		if optType != "CE" && optType != "PE" {
			continue
		}

		seg := exchangeToSegment(exch)
		ins := instrument{
			SecurityID:    secID,
			Exchange:      exch,
			Segment:       seg,
			InstrType:     "OPTIDX",
			TradingSymbol: tradingSym,
			Name:          name,
			Expiry:        expiry,
			Strike:        strikeF,
			OptionType:    provider.OptionType(optType),
			LotSize:       lotSize,
		}
		idx.all = append(idx.all, ins)
		idx.bySymbol[exch+":"+tradingSym] = ins
	}
	return idx, nil
}

// toSearchRow classifies one CSV record as a searchable equity or index.
// Returns ok=false for options, futures, currencies, MCX, etc.
func toSearchRow(rec []string, col map[string]int, exch, instrName, entityName string) (searchRow, bool) {
	var kind string
	var seg string
	switch {
	case instrName == "INDEX":
		kind = "INDEX"
		seg = exchangeToSegment(exch)
	case exch == "NSE" && instrName == "EQUITY":
		kind = "EQ"
		seg = "NSE_EQ"
	case exch == "BSE" && instrName == "EQUITY":
		kind = "EQ"
		seg = "BSE_EQ"
	default:
		return searchRow{}, false
	}
	secIDStr := field(rec, col, "SEM_SMST_SECURITY_ID")
	secID, err := strconv.ParseInt(secIDStr, 10, 64)
	if err != nil {
		return searchRow{}, false
	}
	tradingSym := field(rec, col, "SEM_TRADING_SYMBOL")
	lotSize, _ := strconv.Atoi(field(rec, col, "SEM_LOT_UNITS"))
	name := entityName
	return searchRow{
		SecurityID:    secID,
		Exchange:      exch,
		Segment:       seg,
		TradingSymbol: tradingSym,
		Name:          name,
		Kind:          kind,
		LotSize:       lotSize,
		symU:          strings.ToUpper(tradingSym),
		nameU:         strings.ToUpper(name),
	}, true
}

// ─────────────────────────────────────────────────────────────────────────
// Index query methods
// ─────────────────────────────────────────────────────────────────────────

// Expiries returns the unique tradeable expiries for an underlying, sorted
// ascending. The latest expiry in each calendar month is tagged "monthly";
// all others are "weekly".
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

	byMonth := map[string]time.Time{}
	for _, d := range dates {
		k := d.Format("2006-01")
		if cur, ok := byMonth[k]; !ok || d.After(cur) {
			byMonth[k] = d
		}
	}
	monthlies := map[time.Time]struct{}{}
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

// ForSymbol resolves "EXCHANGE:TRADINGSYMBOL" to a Dhan instrument.
// Also tries the search rows (for equities/indices) if not found in
// the options cache.
func (idx *instrumentIndex) ForSymbol(sym string) (instrument, bool) {
	if ins, ok := idx.bySymbol[sym]; ok {
		return ins, true
	}
	// Try matching a search row (equity or index) for FetchQuotes / FetchHistoricalCandles.
	parts := strings.SplitN(sym, ":", 2)
	if len(parts) != 2 {
		return instrument{}, false
	}
	exchU := strings.ToUpper(parts[0])
	symU := strings.ToUpper(parts[1])
	for _, sr := range idx.search {
		if strings.ToUpper(sr.Exchange) == exchU && strings.ToUpper(sr.TradingSymbol) == symU {
			return instrument{
				SecurityID:    sr.SecurityID,
				Exchange:      sr.Exchange,
				Segment:       sr.Segment,
				InstrType:     sr.Kind,
				TradingSymbol: sr.TradingSymbol,
				Name:          sr.Name,
			}, true
		}
	}
	return instrument{}, false
}

// Search implements prefix-then-contains matching over the searchable
// rows (equities and indices). Empty/whitespace query returns nil.
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
		if len(out) >= limit {
			break
		}
		out = append(out, provider.Instrument{
			Symbol:   r.TradingSymbol,
			Name:     r.Name,
			Exchange: r.Exchange,
			Kind:     r.Kind,
			LotSize:  r.LotSize,
			Token:    fmt.Sprintf("%d", r.SecurityID),
		})
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────

// exchangeToSegment maps the CSV exchange code to the Dhan API segment string.
func exchangeToSegment(exch string) string {
	switch strings.ToUpper(exch) {
	case "NFO":
		return "NSE_FO"
	case "BFO":
		return "BSE_FO"
	case "NSE":
		return "NSE_EQ"
	case "BSE":
		return "BSE_EQ"
	case "IDX_I", "IDX":
		return "IDX_I"
	case "MCX":
		return "MCX_COMM"
	case "NSE_CURRENCY", "CDS":
		return "NSE_CURRENCY"
	default:
		return exch
	}
}

// underlyingName converts our Underlying enum to Dhan's SEM_ENTITY_NAME.
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

// parseDhanExpiry handles Dhan's "YYYY-MM-DD HH:MM:SS" expiry format and
// falls back to plain "YYYY-MM-DD".
func parseDhanExpiry(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty expiry")
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised expiry format: %q", s)
}

// field returns the CSV cell for a named column, or "" if the column or
// cell is absent. Trims leading/trailing whitespace.
func field(rec []string, col map[string]int, name string) string {
	i, ok := col[name]
	if !ok || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}
