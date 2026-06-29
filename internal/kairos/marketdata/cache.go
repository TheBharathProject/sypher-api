// Package marketdata serves /kairos/marketdata/* — live quotes,
// historical candles, symbol search, index/sector boards and movers —
// by fronting the active provider.BrokerProvider with small in-memory
// read-through TTL caches (spec §3.2, ADR-0014 cache model). No Redis:
// a single API process and second-scale TTLs make process-local maps
// the right size of hammer.
package marketdata

import (
	"context"
	"sync"
	"time"
)

// TTLCache is a generic read-through TTL cache with per-key
// singleflight. Concurrent Gets for the same missing key share one
// loader call; the rest park on the in-flight call's done channel.
//
// Eviction: there is deliberately NO background eviction goroutine.
// The cache is read-through only and its key spaces are bounded in
// practice — quote keys are joined symbol lists drawn from a fixed
// instrument universe (indices, sectors, Nifty100, user watchlists),
// candle keys are (symbol, whitelisted interval, range) tuples, and
// search keys are short user queries capped by the handler. Stale
// entries are overwritten in place on the next load of the same key.
// As a safety net against the one user-controlled key space (search
// queries), an inline sweep drops expired entries when the map grows
// past sweepThreshold — still no timer, no goroutine, no lifecycle to
// manage; the cost is paid by the occasional writer that crossed the
// threshold.
type TTLCache[K comparable, V any] struct {
	ttl time.Duration

	// now is the clock; time.Now in production, swapped for a fake in
	// tests so TTL expiry is deterministic.
	now func() time.Time

	mu       sync.Mutex
	entries  map[K]cacheEntry[V]
	inflight map[K]*inflightCall[V]
}

type cacheEntry[V any] struct {
	val       V
	expiresAt time.Time
}

// inflightCall is one singleflight slot. done is closed exactly once,
// after val/err are set — waiters read them only after <-done, so no
// further synchronisation is needed on the fields.
type inflightCall[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// sweepThreshold is the entry count past which a successful load also
// sweeps expired entries. Generous: at ~hundreds of bytes per entry
// this bounds the steady-state cache to a few MB before any sweep.
const sweepThreshold = 4096

// NewTTLCache returns a cache whose entries are fresh for ttl after a
// successful load.
func NewTTLCache[K comparable, V any](ttl time.Duration) *TTLCache[K, V] {
	return &TTLCache[K, V]{
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[K]cacheEntry[V]),
		inflight: make(map[K]*inflightCall[V]),
	}
}

// Get returns the cached value for key when fresh (now < expiresAt).
// On a miss it runs loader — once across all concurrent callers of the
// same key — caches a successful result for the TTL, and returns it.
//
// Errors are never cached: the next Get after a failed load retries
// the loader. Waiters joining an in-flight load receive whatever that
// load returns (value or error); a waiter whose own ctx is cancelled
// while parked returns ctx.Err() without affecting the load, which
// keeps running on the first caller's ctx.
func (c *TTLCache[K, V]) Get(ctx context.Context, key K, loader func(ctx context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Before(e.expiresAt) {
		c.mu.Unlock()
		return e.val, nil
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.val, call.err
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err()
		}
	}
	call := &inflightCall[V]{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	call.val, call.err = loader(ctx)

	c.mu.Lock()
	delete(c.inflight, key)
	if call.err == nil {
		c.maybeSweepLocked()
		c.entries[key] = cacheEntry[V]{val: call.val, expiresAt: c.now().Add(c.ttl)}
	}
	c.mu.Unlock()
	close(call.done)
	return call.val, call.err
}

// maybeSweepLocked drops expired entries once the map has grown past
// sweepThreshold. Called with c.mu held, on the (rare) successful-load
// path only — see the eviction note on TTLCache.
func (c *TTLCache[K, V]) maybeSweepLocked() {
	if len(c.entries) < sweepThreshold {
		return
	}
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}
