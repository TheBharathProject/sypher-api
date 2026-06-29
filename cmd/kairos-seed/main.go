// Command kairos-seed populates the kairos schema with deterministic
// development fixtures so the FE has data to render without a live
// broker provider:
//
//   - option_chains: per trading day (Sat/Sun skipped, last -days days)
//     per underlying (NIFTY / BANKNIFTY / SENSEX), snapshots at 09:20,
//     12:00 and 15:15 IST. Spot follows a deterministic random walk
//     (rand.NewSource(42)); ATM ± 12 strikes, CE+PE priced via
//     greeks.BSPrice with a simple vol smile; hump-shaped OI and
//     deterministic volume. Rows are inserted through the exact same
//     statement shape the ingest path uses (Store.BulkInsertChain),
//     with provider = "seed".
//   - ingest_runs: one row per snapshot tick (job "options-snapshot",
//     detail "seed") — same bookkeeping SnapshotChains performs.
//   - equity_fundamentals: the screener rows ported VERBATIM from
//     kairos/app/screener/sample-data.ts (upserted, so re-runs refresh
//     in place).
//   - announcements: 12 rows dated relative to now, deduped on nse_id
//     (SEED-ANN-NNN), so re-runs are no-ops.
//
// Because kairos.option_chains is partitioned weekly and migrations
// only create current/future partitions, the command first ensures
// child partitions exist for every seeded week (same statement shape
// as Store.EnsurePartitions).
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/kairos-seed -days 10
//	go run ./cmd/kairos-seed -days 5 -database-url postgres://...
//
// Re-running duplicates option_chains rows (the table has no natural
// unique key), so the command warns when prior seed runs are detected
// (ingest_runs detail = 'seed'). Dev fixtures only — never run this
// against production.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/kairos"
	"github.com/TheBharathProject/sypher-api/internal/kairos/greeks"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// istZone is Asia/Kolkata as a fixed offset (+05:30) so the binary
// doesn't depend on the host's tzdata.
var istZone = time.FixedZone("IST", 5*3600+30*60)

// strikeBand is how many strikes each side of ATM get seeded (± 12,
// i.e. 25 strikes per side pair per snapshot).
const strikeBand = 12

// seedProvider is the provider name stamped on seeded option_chains
// rows — distinct from any real broker so seed data is identifiable.
const seedProvider = "seed"

// snapshotTimes are the IST wall-clock ticks seeded per trading day,
// mirroring a sparse version of the real 5-min snapshot cron: open-ish,
// midday, close-ish.
var snapshotTimes = [][2]int{{9, 20}, {12, 0}, {15, 15}}

// underlyingSpec is one index's seeding parameters.
type underlyingSpec struct {
	name     provider.Underlying
	baseSpot float64 // walk starting point
	step     int     // strike interval
	lot      int     // contract lot size (OI/volume granularity)
}

var underlyings = []underlyingSpec{
	{provider.UnderlyingNIFTY, 24200, 50, 75},
	{provider.UnderlyingBANKNIFTY, 52300, 100, 35},
	{provider.UnderlyingSENSEX, 79800, 100, 20},
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	days := flag.Int("days", 10, "number of trading days to seed (Sat/Sun are skipped)")
	dbURL := flag.String("database-url", "", "Postgres connection string (defaults to the DATABASE_URL env var)")
	flag.Parse()

	if *days <= 0 {
		fmt.Fprintln(os.Stderr, "kairos-seed: -days must be > 0")
		os.Exit(2)
	}
	url := *dbURL
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		fmt.Fprintln(os.Stderr, "kairos-seed: no database URL (set DATABASE_URL or pass -database-url)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		logger.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		logger.Error("db ping", "err", err)
		os.Exit(1)
	}

	store := kairos.NewStore(pool)

	// Prior seed detection: every seed run leaves ingest_runs rows with
	// detail='seed'. option_chains has no unique key, so a re-run
	// duplicates chain rows — warn, then proceed (fundamentals upsert
	// and announcements dedupe are safe either way).
	var priorRuns int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM kairos.ingest_runs WHERE detail = 'seed'`,
	).Scan(&priorRuns); err != nil {
		logger.Error("seed-run check", "err", err)
		os.Exit(1)
	}
	if priorRuns > 0 {
		logger.Warn("existing seed rows detected — re-running will DUPLICATE option_chains rows "+
			"(fundamentals upsert and announcements dedupe are unaffected)",
			"prior_seed_ingest_runs", priorRuns)
	}

	tradingDays := lastTradingDays(*days)

	// Partitions first: option_chains is partitioned weekly and the
	// migration/cron only cover current + future weeks, while we are
	// about to insert into past weeks.
	partitions, err := ensurePartitions(ctx, pool, tradingDays)
	if err != nil {
		logger.Error("ensure partitions", "err", err)
		os.Exit(1)
	}

	chainRows, runRows, err := seedChains(ctx, store, tradingDays)
	if err != nil {
		logger.Error("seed option chains", "err", err)
		os.Exit(1)
	}

	funds := seedFundamentals()
	fundN, err := store.UpsertFundamentals(ctx, funds)
	if err != nil {
		logger.Error("seed fundamentals", "err", err, "written", fundN)
		os.Exit(1)
	}

	anns := seedAnnouncements(time.Now().UTC())
	annNew, err := store.InsertAnnouncements(ctx, anns)
	if err != nil {
		logger.Error("seed announcements", "err", err, "inserted", annNew)
		os.Exit(1)
	}

	first := tradingDays[0].Format("2006-01-02")
	last := tradingDays[len(tradingDays)-1].Format("2006-01-02")
	fmt.Println("kairos-seed summary:")
	fmt.Printf("  trading days:             %d (%s .. %s)\n", len(tradingDays), first, last)
	fmt.Printf("  partitions ensured:       %d\n", partitions)
	fmt.Printf("  option_chains rows:       %d\n", chainRows)
	fmt.Printf("  ingest_runs rows:         %d\n", runRows)
	fmt.Printf("  equity_fundamentals rows: %d\n", fundN)
	fmt.Printf("  announcements:            %d inserted, %d duplicates skipped\n", annNew, len(anns)-annNew)
}

// ─────────────────────────────────────────────────────────────────────────
// Option chains
// ─────────────────────────────────────────────────────────────────────────

// seedChains walks the trading days chronologically and, per snapshot
// tick, inserts one chain per underlying plus the ingest_runs audit row
// — the same shape a real SnapshotChains pass leaves behind.
//
// The spot walk consumes the rng in a fixed order (day → tick →
// underlying), so the whole dataset is reproducible from seed 42.
func seedChains(ctx context.Context, store *kairos.Store, tradingDays []time.Time) (chainRows int64, runRows int, err error) {
	rng := rand.New(rand.NewSource(42))

	spot := map[provider.Underlying]float64{}
	for _, u := range underlyings {
		spot[u.name] = u.baseSpot
	}

	for _, day := range tradingDays {
		expiry, tte := monthlyExpiry(day)
		for snapIdx, hm := range snapshotTimes {
			ts := time.Date(day.Year(), day.Month(), day.Day(), hm[0], hm[1], 0, 0, istZone)
			var tickRows int64
			for _, u := range underlyings {
				// ±0.2% step per tick — gentle deterministic drift.
				spot[u.name] *= 1 + (rng.Float64()-0.5)*0.004
				rows := buildChain(u, spot[u.name], ts, snapIdx, expiry, tte)
				n, ierr := store.BulkInsertChain(ctx, seedProvider, rows)
				if ierr != nil {
					return chainRows, runRows, fmt.Errorf("insert %s %s: %w",
						u.name, ts.Format(time.RFC3339), ierr)
				}
				tickRows += n
			}
			chainRows += tickRows
			// One audit row per snapshot tick, exactly like
			// SnapshotChains: job options-snapshot, ok, rows written.
			// detail='seed' is the marker re-runs are detected by.
			if rerr := store.RecordIngestRun(ctx, "options-snapshot",
				ts.UTC(), ts.UTC().Add(2*time.Second), true, int(tickRows), "seed"); rerr != nil {
				return chainRows, runRows, fmt.Errorf("record ingest run %s: %w",
					ts.Format(time.RFC3339), rerr)
			}
			runRows++
		}
	}
	return chainRows, runRows, nil
}

// buildChain produces CE+PE rows for ATM ± strikeBand strikes at one
// snapshot. LTP is Black-Scholes with a linear vol smile
// (0.14 + 0.0008 per strike-step away from ATM); bid/ask straddle LTP
// by one 0.05 tick (bid floored at 0.05); OI is a deterministic hump
// (CE peak above ATM, PE below) that grows through the day; volume is
// a deterministic ATM-centred hump that accumulates by tick.
func buildChain(u underlyingSpec, spot float64, ts time.Time, snapIdx int, expiry time.Time, tte float64) []provider.ChainRow {
	atm := int(math.Round(spot/float64(u.step))) * u.step
	rows := make([]provider.ChainRow, 0, 2*(2*strikeBand+1))
	for off := -strikeBand; off <= strikeBand; off++ {
		strike := atm + off*u.step
		vol := 0.14 + 0.0008*math.Abs(float64(strike-atm))/float64(u.step)
		for _, side := range []provider.OptionType{provider.OptionTypeCE, provider.OptionTypePE} {
			isCall := side == provider.OptionTypeCE
			ltp := greeks.BSPrice(isCall, spot, float64(strike), greeks.RiskFreeRate, vol, tte)
			ltp = math.Round(ltp*100) / 100
			if ltp < 0.05 {
				ltp = 0.05 // exchange tick floor; also keeps nullF from NULLing deep OTM rows
			}
			bid := math.Max(math.Round((ltp-0.05)*100)/100, 0.05)
			ask := math.Round((ltp+0.05)*100) / 100
			oi := seedOI(off, u.lot, isCall, snapIdx)
			rows = append(rows, provider.ChainRow{
				Underlying:   u.name,
				ExpiryDate:   expiry,
				Strike:       strike,
				OptionType:   side,
				SnapshotTime: ts,
				Spot:         math.Round(spot*100) / 100,
				LTP:          ltp,
				Bid:          bid,
				Ask:          ask,
				OI:           oi,
				OIChange:     oi - seedOI(off, u.lot, isCall, 0), // vs day's first tick
				Volume:       seedVolume(off, u.lot, snapIdx),
			})
		}
	}
	return rows
}

// seedOI is the deterministic open-interest hump: a Gaussian in strike
// offset, centred 2 steps above ATM for calls and 2 below for puts
// (the usual resistance/support shape), scaled by lot size and growing
// 5% per intraday tick so oi_change is non-zero after the first one.
func seedOI(off, lot int, isCall bool, snapIdx int) int64 {
	center := 2.0
	if !isCall {
		center = -2.0
	}
	x := float64(off) - center
	lots := (18000.0*math.Exp(-x*x/40.0) + 1200.0) * (1.0 + 0.05*float64(snapIdx))
	return int64(lots) * int64(lot)
}

// seedVolume is the deterministic traded-volume hump: ATM-centred,
// accumulating through the day (each tick reports the running total,
// the way exchanges publish volume).
func seedVolume(off, lot, snapIdx int) int64 {
	x := float64(off)
	lots := (9000.0*math.Exp(-x*x/30.0) + 400.0) * float64(snapIdx+1)
	return int64(lots) * int64(lot)
}

// lastTradingDays returns the most recent n weekdays (today included
// when it's a weekday) as IST midnights, in chronological order.
// Exchange holidays are not modelled — this is dev fixture data.
func lastTradingDays(n int) []time.Time {
	out := make([]time.Time, 0, n)
	d := time.Now().In(istZone)
	d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, istZone)
	for len(out) < n {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, d)
		}
		d = d.AddDate(0, 0, -1)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// monthlyExpiry resolves a trading day to its monthly expiry — the
// last Thursday of the day's month, rolling to next month's when the
// day is already past it — plus the time-to-expiry in years (calendar
// days / 365, floored at 1 day so expiry-day pricing doesn't collapse
// to intrinsic).
//
// The expiry is returned as a UTC midnight: the column is DATE, and a
// midnight in any non-UTC zone could land on the wrong day after pgx's
// timezone conversion.
func monthlyExpiry(day time.Time) (time.Time, float64) {
	dayDate := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	exp := lastThursday(day.Year(), day.Month())
	if dayDate.After(exp) {
		exp = lastThursday(day.Year(), day.Month()+1) // time.Date normalises month 13
	}
	d := int(exp.Sub(dayDate).Hours() / 24)
	if d < 1 {
		d = 1
	}
	return exp, float64(d) / 365.0
}

// lastThursday returns the last Thursday of (year, month) as a UTC
// midnight. Month overflow is normalised by time.Date.
func lastThursday(year int, month time.Month) time.Time {
	d := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, -1) // last day of month
	for d.Weekday() != time.Thursday {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// ─────────────────────────────────────────────────────────────────────────
// Partitions
// ─────────────────────────────────────────────────────────────────────────

// ensurePartitions creates the weekly option_chains child partitions
// covering every seeded snapshot. Same statement shape and naming as
// Store.EnsurePartitions (kairos.option_chains_<isoyear>_w<isoweek>),
// but driven by the seeded dates — which lie in the past, where the
// rolling-window cron never looks.
func ensurePartitions(ctx context.Context, pool *pgxpool.Pool, tradingDays []time.Time) (int, error) {
	seen := map[string]bool{}
	created := 0
	for _, day := range tradingDays {
		// Use the first tick's UTC instant to pick the week: market
		// hours (IST) always land inside the same UTC calendar day.
		ts := time.Date(day.Year(), day.Month(), day.Day(),
			snapshotTimes[0][0], snapshotTimes[0][1], 0, 0, istZone).UTC()
		start := mondayOf(ts)
		year, week := start.ISOWeek()
		child := fmt.Sprintf("kairos.option_chains_%d_w%02d", year, week)
		if seen[child] {
			continue
		}
		seen[child] = true
		startLit := start.Format("2006-01-02 15:04:05-07")
		endLit := start.AddDate(0, 0, 7).Format("2006-01-02 15:04:05-07")
		stmt := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF kairos.option_chains FOR VALUES FROM ('%s') TO ('%s')",
			child, startLit, endLit,
		)
		tag, err := pool.Exec(ctx, stmt)
		if err != nil {
			return created, fmt.Errorf("create partition %s: %w", child, err)
		}
		if strings.Contains(tag.String(), "CREATE") {
			created++
		}
	}
	return created, nil
}

// mondayOf mirrors the unexported helper in internal/kairos/store.go:
// the UTC midnight starting t's ISO week.
func mondayOf(t time.Time) time.Time {
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return time.Date(t.Year(), t.Month(), t.Day()-(wd-1), 0, 0, 0, 0, time.UTC)
}

// ─────────────────────────────────────────────────────────────────────────
// Screener fundamentals
// ─────────────────────────────────────────────────────────────────────────

// seedFundamentals returns the screener rows ported VERBATIM from
// kairos/app/screener/sample-data.ts (recovered via
// `git show HEAD:app/screener/sample-data.ts`): symbol, name, sector,
// mktCap→MktCapCr, pe, roce, roe, de, npY1, npY2, opm,
// cagr3P→Cagr3Profit, cagr3S→Cagr3Sales. TS `null` → nil pointer
// (NULL column). The TS `price` field is dropped — live prices come
// from marketdata, not equity_fundamentals (migration 0027).
func seedFundamentals() []kairos.FundamentalsRow {
	f := func(v float64) *float64 { return &v }
	return []kairos.FundamentalsRow{
		{Symbol: "RELIANCE", Name: "Reliance Industries", Sector: "Energy", MktCapCr: f(1958), PE: f(28.4), ROCE: f(12.3), ROE: f(10.1), DE: f(0.41), NpY1: f(67845), NpY2: f(73958), OPM: f(15.2), Cagr3Profit: f(18.4), Cagr3Sales: f(11.2)},
		{Symbol: "TCS", Name: "Tata Consultancy Svcs", Sector: "IT", MktCapCr: f(1288), PE: f(26.1), ROCE: f(51.8), ROE: f(49.3), DE: f(0.0), NpY1: f(42303), NpY2: f(46099), OPM: f(25.6), Cagr3Profit: f(9.8), Cagr3Sales: f(13.4)},
		{Symbol: "HDFCBANK", Name: "HDFC Bank", Sector: "Banking", MktCapCr: f(1308), PE: f(17.2), ROCE: f(9.8), ROE: f(16.4), DE: nil, NpY1: f(44109), NpY2: f(58477), OPM: nil, Cagr3Profit: f(22.8), Cagr3Sales: f(19.1)},
		{Symbol: "INFY", Name: "Infosys", Sector: "IT", MktCapCr: f(648), PE: f(22.8), ROCE: f(37.4), ROE: f(32.1), DE: f(0.0), NpY1: f(24108), NpY2: f(26233), OPM: f(21.1), Cagr3Profit: f(11.2), Cagr3Sales: f(10.8)},
		{Symbol: "BAJFINANCE", Name: "Bajaj Finance", Sector: "Finance", MktCapCr: f(435), PE: f(32.5), ROCE: f(11.4), ROE: f(20.7), DE: f(3.8), NpY1: f(11508), NpY2: f(14451), OPM: nil, Cagr3Profit: f(24.1), Cagr3Sales: f(28.3)},
		{Symbol: "ASIANPAINT", Name: "Asian Paints", Sector: "Consumer", MktCapCr: f(238), PE: f(51.2), ROCE: f(32.1), ROE: f(28.4), DE: f(0.07), NpY1: f(3974), NpY2: f(3998), OPM: f(16.4), Cagr3Profit: f(12.4), Cagr3Sales: f(11.8)},
		{Symbol: "WIPRO", Name: "Wipro", Sector: "IT", MktCapCr: f(241), PE: f(19.4), ROCE: f(17.8), ROE: f(15.9), DE: f(0.11), NpY1: f(11496), NpY2: f(12014), OPM: f(17.4), Cagr3Profit: f(8.4), Cagr3Sales: f(7.2)},
		{Symbol: "NESTLEIND", Name: "Nestle India", Sector: "Consumer", MktCapCr: f(218), PE: f(74.1), ROCE: f(88.4), ROE: f(119.4), DE: f(0.0), NpY1: f(2724), NpY2: f(3290), OPM: f(22.8), Cagr3Profit: f(17.8), Cagr3Sales: f(16.1)},
		{Symbol: "TITAN", Name: "Titan Company", Sector: "Consumer", MktCapCr: f(297), PE: f(88.2), ROCE: f(24.4), ROE: f(31.4), DE: f(0.04), NpY1: f(3493), NpY2: f(4046), OPM: f(11.2), Cagr3Profit: f(22.1), Cagr3Sales: f(24.8)},
		{Symbol: "SUNPHARMA", Name: "Sun Pharma", Sector: "Pharma", MktCapCr: f(403), PE: f(33.4), ROCE: f(19.8), ROE: f(18.4), DE: f(0.06), NpY1: f(10831), NpY2: f(12942), OPM: f(26.4), Cagr3Profit: f(18.8), Cagr3Sales: f(10.4)},
		{Symbol: "KOTAKBANK", Name: "Kotak Mahindra Bank", Sector: "Banking", MktCapCr: f(361), PE: f(18.4), ROCE: f(8.4), ROE: f(13.8), DE: nil, NpY1: f(12283), NpY2: f(15013), OPM: nil, Cagr3Profit: f(18.4), Cagr3Sales: f(21.2)},
		{Symbol: "MARUTI", Name: "Maruti Suzuki", Sector: "Auto", MktCapCr: f(381), PE: f(28.8), ROCE: f(20.4), ROE: f(17.8), DE: f(0.0), NpY1: f(10040), NpY2: f(14025), OPM: f(12.4), Cagr3Profit: f(28.4), Cagr3Sales: f(16.4)},
		{Symbol: "ADANIPORTS", Name: "Adani Ports & SEZ", Sector: "Infra", MktCapCr: f(266), PE: f(25.4), ROCE: f(13.8), ROE: f(14.4), DE: f(0.84), NpY1: f(5985), NpY2: f(8028), OPM: f(58.4), Cagr3Profit: f(32.4), Cagr3Sales: f(18.4)},
		{Symbol: "HCLTECH", Name: "HCL Technologies", Sector: "IT", MktCapCr: f(425), PE: f(22.4), ROCE: f(29.8), ROE: f(25.4), DE: f(0.02), NpY1: f(13898), NpY2: f(15710), OPM: f(18.8), Cagr3Profit: f(13.4), Cagr3Sales: f(12.8)},
		{Symbol: "LTIM", Name: "LTIMindtree", Sector: "IT", MktCapCr: f(155), PE: f(31.2), ROCE: f(34.8), ROE: f(28.4), DE: f(0.0), NpY1: f(3753), NpY2: f(4588), OPM: f(16.8), Cagr3Profit: f(19.8), Cagr3Sales: f(17.4)},
		{Symbol: "IRFC", Name: "Indian Railway Fin Corp", Sector: "Finance", MktCapCr: f(226), PE: f(29.4), ROCE: f(7.8), ROE: f(19.4), DE: f(9.2), NpY1: f(4636), NpY2: f(6412), OPM: nil, Cagr3Profit: f(19.4), Cagr3Sales: f(18.8)},
		{Symbol: "POLICYBZR", Name: "PB Fintech", Sector: "Fintech", MktCapCr: f(77), PE: nil, ROCE: f(12.4), ROE: f(8.4), DE: f(0.0), NpY1: f(-488), NpY2: f(1448), OPM: f(8.4), Cagr3Profit: nil, Cagr3Sales: f(42.4)},
		{Symbol: "ZOMATO", Name: "Zomato", Sector: "Fintech", MktCapCr: f(210), PE: nil, ROCE: f(8.4), ROE: f(6.8), DE: f(0.0), NpY1: f(-971), NpY2: f(2224), OPM: f(4.8), Cagr3Profit: nil, Cagr3Sales: f(74.8)},
		{Symbol: "NITCO", Name: "Nitco Ltd", Sector: "Consumer", MktCapCr: f(2.2), PE: f(66.8), ROCE: f(7.1), ROE: f(10.8), DE: nil, NpY1: f(29), NpY2: f(-741), OPM: f(4.6), Cagr3Profit: nil, Cagr3Sales: nil},
		{Symbol: "PAGEIND", Name: "Page Industries", Sector: "Consumer", MktCapCr: f(46), PE: f(52.4), ROCE: f(48.4), ROE: f(44.4), DE: f(0.01), NpY1: f(601), NpY2: f(732), OPM: f(19.4), Cagr3Profit: f(8.4), Cagr3Sales: f(9.8)},
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Announcements
// ─────────────────────────────────────────────────────────────────────────

// seedAnnouncements returns 12 announcements dated relative to now,
// spread over the last ~3 days, newest first. nse_id is a stable
// SEED-ANN-NNN so InsertAnnouncements' ON CONFLICT dedupe makes
// re-runs no-ops.
func seedAnnouncements(now time.Time) []kairos.Announcement {
	raw := json.RawMessage(`{"seed":true}`)
	items := []struct {
		symbol, company, category, headline string
		hoursAgo                            int
	}{
		{"RELIANCE", "Reliance Industries", "Board Meeting", "Reliance Industries: outcome of board meeting — capex plan for new energy business approved", 2},
		{"TCS", "Tata Consultancy Svcs", "Financial Results", "TCS announces quarterly results: net profit up 8.9% YoY, ₹28/share interim dividend", 5},
		{"HDFCBANK", "HDFC Bank", "Allotment", "HDFC Bank: allotment of equity shares under ESOP scheme", 9},
		{"INFY", "Infosys", "Press Release", "Infosys signs strategic collaboration for enterprise AI platform rollout", 14},
		{"MARUTI", "Maruti Suzuki", "Press Release", "Maruti Suzuki reports monthly production volumes; utilisation at 94%", 20},
		{"SUNPHARMA", "Sun Pharma", "Regulatory", "Sun Pharma receives USFDA approval for generic oncology formulation", 26},
		{"TITAN", "Titan Company", "Investor Presentation", "Titan Company: investor presentation on jewellery segment expansion", 33},
		{"ADANIPORTS", "Adani Ports & SEZ", "Press Release", "Adani Ports handles record monthly cargo volumes across terminals", 41},
		{"ZOMATO", "Zomato", "Board Meeting", "Zomato board approves grant of stock options under ESOP 2021", 48},
		{"NESTLEIND", "Nestle India", "Dividend", "Nestle India declares interim dividend of ₹12 per equity share", 55},
		{"WIPRO", "Wipro", "Press Release", "Wipro wins multi-year managed services engagement with European bank", 63},
		{"BAJFINANCE", "Bajaj Finance", "Press Release", "Bajaj Finance: AUM crosses milestone; asset quality stable in quarterly update", 70},
	}
	out := make([]kairos.Announcement, 0, len(items))
	for i, it := range items {
		out = append(out, kairos.Announcement{
			NseID:       fmt.Sprintf("SEED-ANN-%03d", i+1),
			Symbol:      it.symbol,
			Company:     it.company,
			Category:    it.category,
			Headline:    it.headline,
			AnnouncedAt: now.Add(-time.Duration(it.hoursAgo) * time.Hour),
			Raw:         raw,
		})
	}
	return out
}
