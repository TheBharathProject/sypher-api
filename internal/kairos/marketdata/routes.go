package marketdata

import "net/http"

// RegisterRoutes wires every /kairos/marketdata/* path onto the mux.
// Auth middleware is passed in so this package doesn't need to know
// about JWT secrets (same pattern as jobtracker/routes.go and
// kairos/routes.go); server.go composes the dependency graph.
//
// All endpoints are read-only GETs and all require a valid user JWT —
// the data is licensed via the operator's broker account, not public.
func RegisterRoutes(mux *http.ServeMux, h *Handler, requireUser func(http.Handler) http.Handler) {
	g := func(method, path string, fn http.HandlerFunc) {
		mux.Handle(method+" "+path, requireUser(fn))
	}

	g("GET", "/kairos/marketdata/quotes", h.GetQuotes)
	g("GET", "/kairos/marketdata/candles", h.GetCandles)
	g("GET", "/kairos/marketdata/indices", h.GetIndices)
	g("GET", "/kairos/marketdata/search", h.Search)
	g("GET", "/kairos/marketdata/movers", h.GetMovers)
	g("GET", "/kairos/marketdata/sectors", h.GetSectors)
}
