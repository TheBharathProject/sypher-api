package provider

import "context"

// Optional provider capabilities.
//
// Per the BrokerProvider doc in provider.go, provider-specific
// superpowers live as optional sibling interfaces — NOT as methods on
// BrokerProvider. Adding a method to BrokerProvider forces every
// provider (including the dhan/upstox/angel/null stubs) to implement
// it; a sibling interface lets a provider opt in by simply having the
// method.
//
// Callers discover a capability with a type assertion:
//
//	if qf, ok := prov.(provider.QuoteFetcher); ok {
//		quotes, err := qf.FetchQuotes(ctx, symbols)
//		...
//	}
//	// else: capability unavailable — degrade gracefully
//	// (e.g. respond 503 or fall back), don't crash.
//
// Implementations should also add a compile-time assertion next to the
// method so a signature drift is caught at build time:
//
//	var _ provider.QuoteFetcher = (*Provider)(nil)

// QuoteFetcher is the optional live-quote capability. Implemented by
// kite (batched /quote calls); the stubs deliberately don't have it.
//
// Symbols are "EXCHANGE:TRADINGSYMBOL" (e.g. "NSE:RELIANCE",
// "NSE:NIFTY 50"); a bare "RELIANCE" is interpreted as NSE by
// implementations. The returned slice preserves input order; symbols
// the provider doesn't recognise are silently omitted (no error), so
// the result may be shorter than the input.
type QuoteFetcher interface {
	FetchQuotes(ctx context.Context, symbols []string) ([]Quote, error)
}

// InstrumentSearcher is the optional symbol-search capability backing
// GET /kairos/marketdata/search. Implemented by kite against its daily
// instruments dump; the stubs deliberately don't have it.
//
// query is matched case-insensitively against symbol and name, with
// prefix matches ranked above contains matches. limit caps the result
// count; implementations clamp it to a sane maximum (50).
type InstrumentSearcher interface {
	SearchInstruments(ctx context.Context, query string, limit int) ([]Instrument, error)
}
