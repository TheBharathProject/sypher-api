package kairos

import "net/http"

// RegisterRoutes wires every /kairos/* path. Auth middleware is passed
// in so this package doesn't need to know about JWT secrets — server.go
// composes the dependency graph (same pattern as jobtracker/routes.go).
//
// Public routes (no auth):
//   GET  /kairos/provider/kite/login    — OAuth start, redirects to Zerodha
//   GET  /kairos/provider/kite/callback — OAuth callback from Zerodha
//
// All others require a valid JWT (the same one used by /pegasus/*);
// /kairos/admin/* additionally requires the platform admin flag
// (auth.RequireAdmin — 403 admin_required for ordinary users).
func RegisterRoutes(mux *http.ServeMux, h *Handler, requireUser, requireAdmin func(http.Handler) http.Handler) {
	g := func(method, path string, fn http.HandlerFunc) {
		mux.Handle(method+" "+path, requireUser(fn))
	}
	ga := func(method, path string, fn http.HandlerFunc) {
		mux.Handle(method+" "+path, requireAdmin(fn))
	}

	// Options chain — read-only, auth-gated.
	g("GET", "/kairos/options/underlyings", h.GetUnderlyings)
	g("GET", "/kairos/options/expiries", h.GetExpiries)
	g("GET", "/kairos/options/chain", h.GetChain)
	g("GET", "/kairos/options/chain/timestamps", h.ChainTimestamps)
	g("GET", "/kairos/options/analytics/intraday", h.IntradayAnalytics)

	// Strategies — CRUD on user-saved strategies.
	g("POST", "/kairos/strategies", h.SaveStrategy)
	g("GET", "/kairos/strategies", h.ListStrategies)
	g("DELETE", "/kairos/strategies/{id}", h.DeleteStrategy)

	// Backtests — async submit + poll.
	g("POST", "/kairos/backtest", h.SubmitBacktest)
	g("GET", "/kairos/backtest/{id}", h.GetBacktest)
	g("GET", "/kairos/backtests", h.ListBacktests)

	// Watchlists — per-user lists plus item add/remove.
	g("GET", "/kairos/watchlists", h.ListWatchlists)
	g("POST", "/kairos/watchlists", h.CreateWatchlist)
	g("DELETE", "/kairos/watchlists/{id}", h.DeleteWatchlist)
	g("POST", "/kairos/watchlists/{id}/items", h.AddWatchlistItem)
	g("DELETE", "/kairos/watchlists/{id}/items/{itemId}", h.RemoveWatchlistItem)

	// Alerts — price/pct-change rules, evaluated by the alerts cron.
	g("GET", "/kairos/alerts", h.ListAlerts)
	g("POST", "/kairos/alerts", h.CreateAlert)
	g("PATCH", "/kairos/alerts/{id}", h.UpdateAlert)
	g("DELETE", "/kairos/alerts/{id}", h.DeleteAlert)

	// Screener + fundamentals — stored ratios, best-effort live prices.
	g("GET", "/kairos/screener", h.Screener)
	g("GET", "/kairos/fundamentals/{symbol}", h.GetFundamental)

	// Announcements — NSE corporate filings feed (read side; ingestion
	// is the kairos-announcements cron).
	g("GET", "/kairos/announcements", h.ListAnnouncements)

	// Paper trading — simulated account, orders, positions (spec §3.5).
	g("GET", "/kairos/paper/account", h.GetPaperAccount)
	g("POST", "/kairos/paper/reset", h.ResetPaperAccount)
	g("POST", "/kairos/paper/orders", h.PlacePaperOrder)
	g("GET", "/kairos/paper/orders", h.ListPaperOrders)
	g("DELETE", "/kairos/paper/orders/{id}", h.CancelPaperOrder)
	g("GET", "/kairos/paper/positions", h.ListPaperPositions)
	g("POST", "/kairos/paper/positions/{id}/close", h.ClosePaperPosition)

	// Dark-launch live orders — registered but refuses (403 while
	// KAIROS_LIVE_TRADING is off, 501 once on; spec §3.5).
	g("POST", "/kairos/orders", h.RealOrders)

	// Provider status — used by the FE to show "refresh kite" banner.
	g("GET", "/kairos/provider/status", h.ProviderStatus)

	// Admin — RequireAdmin on top of RequireUser (spec §3.7).
	// Snapshot is the one-shot ingest trigger bypassing the cron's
	// market-hours gate; useful for first-day seeding and e2e checks.
	ga("POST", "/kairos/admin/snapshot", h.AdminSnapshot)
	ga("GET", "/kairos/admin/health", h.AdminHealth)
	ga("GET", "/kairos/admin/coverage", h.AdminCoverage)
	ga("GET", "/kairos/admin/coverage/day", h.AdminCoverageDay)
	ga("GET", "/kairos/admin/export/chains", h.AdminExportChains)
	ga("GET", "/kairos/admin/ingest-runs", h.AdminIngestRuns)
	ga("GET", "/kairos/admin/partitions", h.AdminPartitions)
	ga("GET", "/kairos/admin/backtests", h.AdminBacktests)
	ga("POST", "/kairos/admin/backtests/{id}/retry", h.AdminRetryBacktest)
	ga("GET", "/kairos/admin/users", h.AdminUsers)
	ga("PATCH", "/kairos/admin/users/{id}", h.AdminUpdateUser)
	ga("POST", "/kairos/admin/fundamentals/upload", h.AdminUploadFundamentals)

	// Kite OAuth — both public. The login endpoint redirects to Zerodha;
	// the callback exchanges the request_token. Kite signs neither, so
	// auth gating buys nothing — the protection is that exchange fails
	// for invalid tokens.
	mux.HandleFunc("GET /kairos/provider/kite/login", h.KiteLoginStart)
	mux.HandleFunc("GET /kairos/provider/kite/callback", h.KiteCallback)
}
