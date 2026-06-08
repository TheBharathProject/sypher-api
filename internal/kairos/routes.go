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
// All others require a valid JWT (the same one used by /pegasus/*).
func RegisterRoutes(mux *http.ServeMux, h *Handler, requireUser func(http.Handler) http.Handler) {
	g := func(method, path string, fn http.HandlerFunc) {
		mux.Handle(method+" "+path, requireUser(fn))
	}

	// Options chain — read-only, auth-gated.
	g("GET", "/kairos/options/underlyings", h.GetUnderlyings)
	g("GET", "/kairos/options/expiries", h.GetExpiries)
	g("GET", "/kairos/options/chain", h.GetChain)

	// Strategies — CRUD on user-saved strategies.
	g("POST", "/kairos/strategies", h.SaveStrategy)
	g("GET", "/kairos/strategies", h.ListStrategies)
	g("DELETE", "/kairos/strategies/{id}", h.DeleteStrategy)

	// Backtests — async submit + poll.
	g("POST", "/kairos/backtest", h.SubmitBacktest)
	g("GET", "/kairos/backtest/{id}", h.GetBacktest)
	g("GET", "/kairos/backtests", h.ListBacktests)

	// Provider status — used by the FE to show "refresh kite" banner.
	g("GET", "/kairos/provider/status", h.ProviderStatus)

	// Admin one-shot snapshot trigger. Bypasses the cron's market-hours
	// gate. Useful for first-day seeding and end-to-end verification.
	g("POST", "/kairos/admin/snapshot", h.AdminSnapshot)

	// Kite OAuth — both public. The login endpoint redirects to Zerodha;
	// the callback exchanges the request_token. Kite signs neither, so
	// auth gating buys nothing — the protection is that exchange fails
	// for invalid tokens.
	mux.HandleFunc("GET /kairos/provider/kite/login", h.KiteLoginStart)
	mux.HandleFunc("GET /kairos/provider/kite/callback", h.KiteCallback)
}
