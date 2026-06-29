package marketdata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// ErrUnsupported is returned when the active provider lacks the
// optional capability an endpoint needs (provider/capabilities.go) —
// e.g. the dhan/upstox/angel/null stubs have neither QuoteFetcher nor
// InstrumentSearcher. Handlers map it to 503 provider_unavailable.
var ErrUnsupported = errors.New("marketdata: capability not supported by active provider")

// errBadInterval is returned for an interval outside the UI whitelist.
// The handler validates first, so reaching this from HTTP means a
// handler bug; it exists so direct Service callers fail loudly too.
var errBadInterval = errors.New("marketdata: unknown candle interval")

// Cache TTLs (spec §3.2). Quotes are near-live; intraday candles only
// move once per bar anyway; daily candles change once a session;
// search results change only on the provider's daily instruments dump.
const (
	quotesTTL         = 5 * time.Second
	intradayCandleTTL = 30 * time.Second
	dailyCandleTTL    = 6 * time.Hour
	searchTTL         = 60 * time.Second
)

// kiteMinuteWindow is the chunk size for minute-granularity historical
// requests. Kite caps minute data at 60 days per request (its other
// intraday intervals allow more, but 60 is ≤ all of their caps, so one
// chunk size serves every intraday interval).
const kiteMinuteWindow = 60 * 24 * time.Hour

// moversCount is how many gainers and losers Movers returns.
const moversCount = 8

// searchLimit caps Search results; the provider clamps to 50 anyway,
// 20 is plenty for a command-palette dropdown.
const searchLimit = 20

// uiIntervals maps the FE interval vocabulary (kairos-api.ts
// ApiCandleInterval) onto Kite's historical-API vocabulary, which the
// other providers either share or translate (provider.HistoricalReq).
var uiIntervals = map[string]string{
	"1m":  "minute",
	"5m":  "5minute",
	"15m": "15minute",
	"1h":  "60minute",
	"1d":  "day",
}

// ValidInterval reports whether interval is one of the UI vocabulary
// values. Used by the handler's whitelist check.
func ValidInterval(interval string) bool {
	_, ok := uiIntervals[interval]
	return ok
}

// Movers is the payload of GET /kairos/marketdata/movers — top and
// bottom moversCount of the Nifty100 by percent change. JSON tags
// match kairos-api.ts fetchMovers exactly.
type Movers struct {
	Gainers []provider.Quote `json:"gainers"`
	Losers  []provider.Quote `json:"losers"`
}

// Service fronts the provider with the TTL caches. One instance per
// process, shared across requests — the caches are what make the
// dashboard's 60s polling affordable against Kite's rate limits.
type Service struct {
	prov   provider.BrokerProvider
	logger *slog.Logger

	quotes   *TTLCache[string, []provider.Quote]
	intraday *TTLCache[string, []provider.Candle]
	daily    *TTLCache[string, []provider.Candle]
	search   *TTLCache[string, []provider.Instrument]

	// now feeds IndexSymbols' rolling USDINR contract; swappable in
	// tests like TTLCache.now.
	now func() time.Time
}

// NewService wires a Service over the active provider.
func NewService(prov provider.BrokerProvider, logger *slog.Logger) *Service {
	return &Service{
		prov:     prov,
		logger:   logger,
		quotes:   NewTTLCache[string, []provider.Quote](quotesTTL),
		intraday: NewTTLCache[string, []provider.Candle](intradayCandleTTL),
		daily:    NewTTLCache[string, []provider.Candle](dailyCandleTTL),
		search:   NewTTLCache[string, []provider.Instrument](searchTTL),
		now:      time.Now,
	}
}

// Quotes returns live quotes for symbols via the optional QuoteFetcher
// capability; ErrUnsupported when the active provider doesn't have it.
// Cached for quotesTTL keyed on the exact symbol list — the FE asks
// for stable sets (indices board, sectors board, a user's watchlist),
// so identical lists recur every poll tick.
func (s *Service) Quotes(ctx context.Context, symbols []string) ([]provider.Quote, error) {
	qf, ok := s.prov.(provider.QuoteFetcher)
	if !ok {
		return nil, fmt.Errorf("%w: quotes (provider %s)", ErrUnsupported, s.prov.Name())
	}
	key := strings.Join(symbols, ",")
	return s.quotes.Get(ctx, key, func(ctx context.Context) ([]provider.Quote, error) {
		return qf.FetchQuotes(ctx, symbols)
	})
}

// Candles returns OHLCV bars for symbol between from and to at a UI
// interval (1m|5m|15m|1h|1d). Daily candles cache for 6h, intraday
// for 30s. Intraday requests are chunked into ≤60-day windows to stay
// under Kite's per-request minute-data cap; chunks are concatenated
// with a boundary-duplicate guard (Kite's from/to are inclusive).
func (s *Service) Candles(ctx context.Context, symbol, interval string, from, to time.Time) ([]provider.Candle, error) {
	kiteInterval, ok := uiIntervals[interval]
	if !ok {
		return nil, fmt.Errorf("%w: %q", errBadInterval, interval)
	}
	cache := s.intraday
	if interval == "1d" {
		cache = s.daily
	}
	key := fmt.Sprintf("%s|%s|%d|%d", symbol, interval, from.Unix(), to.Unix())
	return cache.Get(ctx, key, func(ctx context.Context) ([]provider.Candle, error) {
		if kiteInterval == "day" {
			// Kite allows ~2000 days of daily bars per request; the
			// handler's spans never approach that, so no chunking.
			return s.prov.FetchHistoricalCandles(ctx, provider.HistoricalReq{
				Instrument: symbol, Interval: kiteInterval, From: from, To: to,
			})
		}
		var out []provider.Candle
		for start := from; start.Before(to); {
			end := start.Add(kiteMinuteWindow)
			if end.After(to) {
				end = to
			}
			chunk, err := s.prov.FetchHistoricalCandles(ctx, provider.HistoricalReq{
				Instrument: symbol, Interval: kiteInterval, From: start, To: end,
			})
			if err != nil {
				return nil, err
			}
			for _, c := range chunk {
				// Drop a candle re-served at the inclusive window edge.
				if n := len(out); n > 0 && !c.Time.After(out[n-1].Time) {
					continue
				}
				out = append(out, c)
			}
			start = end
		}
		return out, nil
	})
}

// Search proxies the optional InstrumentSearcher capability;
// ErrUnsupported when absent. Keyed on the folded query so "rel" and
// "REL" share an entry.
func (s *Service) Search(ctx context.Context, q string) ([]provider.Instrument, error) {
	is, ok := s.prov.(provider.InstrumentSearcher)
	if !ok {
		return nil, fmt.Errorf("%w: search (provider %s)", ErrUnsupported, s.prov.Name())
	}
	key := strings.ToUpper(strings.TrimSpace(q))
	return s.search.Get(ctx, key, func(ctx context.Context) ([]provider.Instrument, error) {
		return is.SearchInstruments(ctx, q, searchLimit)
	})
}

// Indices returns quotes for the fixed dashboard index set
// (constituents.go IndexSymbols).
func (s *Service) Indices(ctx context.Context) ([]provider.Quote, error) {
	return s.Quotes(ctx, IndexSymbols(s.now()))
}

// Movers quotes the whole Nifty100 and returns the top and bottom
// moversCount by ChangePct. Symbols Kite doesn't recognise (stale
// constituents) are simply absent from the quote response, so a stale
// Nifty100 list shrinks the field rather than erroring.
func (s *Service) Movers(ctx context.Context) (Movers, error) {
	quotes, err := s.Quotes(ctx, Nifty100)
	if err != nil {
		return Movers{}, err
	}
	sorted := make([]provider.Quote, len(quotes))
	copy(sorted, quotes)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].ChangePct > sorted[j].ChangePct
	})
	n := len(sorted)
	g := min(moversCount, n)
	gainers := make([]provider.Quote, g)
	copy(gainers, sorted[:g])
	losers := make([]provider.Quote, 0, g)
	for i := n - 1; i >= 0 && len(losers) < moversCount; i-- {
		losers = append(losers, sorted[i]) // worst first
	}
	return Movers{Gainers: gainers, Losers: losers}, nil
}

// Sectors returns quotes for the NSE sectoral indices
// (constituents.go SectoralIndices).
func (s *Service) Sectors(ctx context.Context) ([]provider.Quote, error) {
	symbols := make([]string, len(SectoralIndices))
	for i, sec := range SectoralIndices {
		symbols[i] = sec.Symbol
	}
	return s.Quotes(ctx, symbols)
}
