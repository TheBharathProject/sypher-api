package marketdata

import (
	"fmt"
	"strings"
	"time"
)

// Static instrument lists for the board endpoints (indices, movers,
// sectors). All symbols below were verified against Kite's public
// instruments dump (https://api.kite.trade/instruments) on 2026-06-12.
//
// MEMBERSHIP CHANGES SEMI-ANNUALLY — NSE reconstitutes the NIFTY
// indices around March and September each year. There is no API for
// membership; update Nifty100 below BY HAND after each rejig
// (https://www.niftyindices.com → "Index Constituents"). A stale list
// degrades gracefully: Kite silently omits unknown symbols from quote
// responses, so movers simply compute over the valid subset.
//
// Symbol conventions (Kite "EXCHANGE:TRADINGSYMBOL", see
// provider/capabilities.go):
//   - Equities are bare NSE tradingsymbols ("RELIANCE"); the provider
//     interprets bare symbols as NSE, and bare reads better in the UI.
//   - Indices need the exchange prefix (SENSEX lives on BSE) and use
//     Kite's tradingsymbol, which is NOT always the official NSE index
//     name — notably MIDCPNIFTY: official name "NIFTY MIDCAP SELECT",
//     Kite tradingsymbol "NIFTY MID SELECT".

// Nifty100 is the NIFTY 100 constituent list (NIFTY 50 + NIFTY NEXT
// 50), bare NSE tradingsymbols, provider format. Movers quotes this
// whole list in one batch (Kite's /quote cap is 500). Post Tata Motors
// demerger the index carries both successor entities (TMCV commercial,
// TMPV passenger).
var Nifty100 = []string{
	// NIFTY 50
	"ADANIENT", "ADANIPORTS", "APOLLOHOSP", "ASIANPAINT", "AXISBANK",
	"BAJAJ-AUTO", "BAJAJFINSV", "BAJFINANCE", "BEL", "BHARTIARTL",
	"CIPLA", "COALINDIA", "DRREDDY", "EICHERMOT", "ETERNAL",
	"GRASIM", "HCLTECH", "HDFCBANK", "HDFCLIFE", "HINDALCO",
	"HINDUNILVR", "ICICIBANK", "INDIGO", "INFY", "ITC",
	"JIOFIN", "JSWSTEEL", "KOTAKBANK", "LT", "M&M",
	"MARUTI", "MAXHEALTH", "NESTLEIND", "NTPC", "ONGC",
	"POWERGRID", "RELIANCE", "SBILIFE", "SBIN", "SHRIRAMFIN",
	"SUNPHARMA", "TATACONSUM", "TATASTEEL", "TCS", "TECHM",
	"TITAN", "TMCV", "TMPV", "TRENT", "ULTRACEMCO",
	"WIPRO",
	// NIFTY NEXT 50
	"ABB", "ADANIENSOL", "ADANIGREEN", "ADANIPOWER", "AMBUJACEM",
	"BAJAJHFL", "BAJAJHLDNG", "BANKBARODA", "BOSCHLTD", "BPCL",
	"BRITANNIA", "CANBK", "CGPOWER", "CHOLAFIN", "DABUR",
	"DIVISLAB", "DLF", "DMART", "GAIL", "GODREJCP",
	"HAL", "HAVELLS", "HEROMOTOCO", "HINDPETRO", "HYUNDAI",
	"ICICIGI", "INDHOTEL", "INDUSINDBK", "IOC", "IRFC",
	"JINDALSTEL", "JSWENERGY", "LICI", "LODHA", "MOTHERSON",
	"NAUKRI", "PFC", "PIDILITIND", "PNB", "RECLTD",
	"SIEMENS", "SWIGGY", "TATAPOWER", "TORNTPHARM", "TVSMOTOR",
	"UNITDSPR", "VBL", "VEDL", "ZYDUSLIFE",
}

// SectoralIndices maps the dashboard's sector tiles to Kite NSE index
// symbols (all verified in the instruments dump, segment=INDICES).
var SectoralIndices = [...]struct {
	Name   string // short label the FE may derive from the symbol
	Symbol string // Kite "NSE:..." index tradingsymbol
}{
	{Name: "IT", Symbol: "NSE:NIFTY IT"},
	{Name: "BANK", Symbol: "NSE:NIFTY BANK"},
	{Name: "AUTO", Symbol: "NSE:NIFTY AUTO"},
	{Name: "PHARMA", Symbol: "NSE:NIFTY PHARMA"},
	{Name: "FMCG", Symbol: "NSE:NIFTY FMCG"},
	{Name: "METAL", Symbol: "NSE:NIFTY METAL"},
	{Name: "ENERGY", Symbol: "NSE:NIFTY ENERGY"},
	{Name: "REALTY", Symbol: "NSE:NIFTY REALTY"},
}

// istLocation is the market timezone, used to decide which USDINR
// monthly contract is "near". Same fallback strategy as the kite
// package's istLoc.
var istLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return time.FixedZone("IST", int((5*time.Hour + 30*time.Minute).Seconds()))
	}
	return loc
}()

// IndexSymbols returns the fixed list behind GET /kairos/marketdata/
// indices (spec §3.2): NIFTY 50, SENSEX, NIFTY BANK (BANKNIFTY),
// NIFTY FIN SERVICE (FINNIFTY), NIFTY MIDCAP SELECT (MIDCPNIFTY),
// INDIA VIX, USDINR. It takes now because the USDINR entry is a
// rolling monthly future, not a static symbol.
func IndexSymbols(now time.Time) []string {
	return []string{
		"NSE:NIFTY 50",
		"BSE:SENSEX",
		"NSE:NIFTY BANK",
		"NSE:NIFTY FIN SERVICE",
		"NSE:NIFTY MID SELECT", // official name: NIFTY MIDCAP SELECT (MIDCPNIFTY)
		"NSE:INDIA VIX",
		usdinrFuture(now),
	}
}

// usdinrFuture returns the Kite symbol of the near-month USDINR
// future, e.g. "CDS:USDINR26JUNFUT". Kite carries NO spot USDINR
// instrument (verified against the dump: only CDS futures/options),
// so the dashboard quotes the near-month future — it tracks spot to
// within the small cost-of-carry premium.
//
// Monthly USDINR contracts expire two business days before the last
// working day of the month (roughly the 25th–29th). We roll to the
// next month's contract from the 25th (IST) onward: a few days early
// at worst, which is harmless, versus quoting an expired contract,
// which Kite answers by omitting the symbol.
func usdinrFuture(now time.Time) string {
	t := now.In(istLocation)
	if t.Day() >= 25 {
		// First of the next month — avoids AddDate's day-overflow
		// normalisation (Jan 31 + 1 month = Mar 3).
		t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, istLocation)
	}
	return fmt.Sprintf("CDS:USDINR%s%sFUT", t.Format("06"), strings.ToUpper(t.Format("Jan")))
}
