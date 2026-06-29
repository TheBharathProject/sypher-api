package marketdata

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a hand-cranked clock injected via TTLCache.now so tests
// control TTL expiry deterministically (no sleeps).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func TestTTLCacheReturnsCachedValueWithinTTL(t *testing.T) {
	clk := newFakeClock()
	c := NewTTLCache[string, int](5 * time.Second)
	c.now = clk.Now

	var calls atomic.Int32
	loader := func(ctx context.Context) (int, error) {
		calls.Add(1)
		return 42, nil
	}

	got, err := c.Get(context.Background(), "k", loader)
	if err != nil {
		t.Fatalf("first Get: unexpected error: %v", err)
	}
	if got != 42 {
		t.Fatalf("first Get: got %d, want 42", got)
	}

	// Just inside the TTL — must serve from cache, no second load.
	clk.Advance(4 * time.Second)
	got, err = c.Get(context.Background(), "k", loader)
	if err != nil {
		t.Fatalf("second Get: unexpected error: %v", err)
	}
	if got != 42 {
		t.Fatalf("second Get: got %d, want 42", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("loader called %d times within TTL, want 1", n)
	}
}

func TestTTLCacheRefetchesAfterExpiry(t *testing.T) {
	clk := newFakeClock()
	c := NewTTLCache[string, int](5 * time.Second)
	c.now = clk.Now

	var calls atomic.Int32
	loader := func(ctx context.Context) (int, error) {
		return int(calls.Add(1)) * 100, nil
	}

	got, err := c.Get(context.Background(), "k", loader)
	if err != nil {
		t.Fatalf("first Get: unexpected error: %v", err)
	}
	if got != 100 {
		t.Fatalf("first Get: got %d, want 100", got)
	}

	// At exactly the TTL boundary the entry is stale (fresh means
	// now < expiresAt), so this must refetch.
	clk.Advance(5 * time.Second)
	got, err = c.Get(context.Background(), "k", loader)
	if err != nil {
		t.Fatalf("post-expiry Get: unexpected error: %v", err)
	}
	if got != 200 {
		t.Fatalf("post-expiry Get: got %d, want 200 (fresh load)", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("loader called %d times across expiry, want 2", n)
	}
}

func TestTTLCacheDoesNotCacheErrors(t *testing.T) {
	clk := newFakeClock()
	c := NewTTLCache[string, int](5 * time.Second)
	c.now = clk.Now

	boom := errors.New("boom")
	var calls atomic.Int32
	loader := func(ctx context.Context) (int, error) {
		if calls.Add(1) == 1 {
			return 0, boom
		}
		return 7, nil
	}

	if _, err := c.Get(context.Background(), "k", loader); !errors.Is(err, boom) {
		t.Fatalf("first Get: got err %v, want boom", err)
	}
	// Errors must not be cached: the very next Get retries the loader.
	got, err := c.Get(context.Background(), "k", loader)
	if err != nil {
		t.Fatalf("retry Get: unexpected error: %v", err)
	}
	if got != 7 {
		t.Fatalf("retry Get: got %d, want 7", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("loader called %d times, want 2", n)
	}
}

func TestTTLCacheSingleflightsConcurrentMisses(t *testing.T) {
	clk := newFakeClock()
	c := NewTTLCache[string, int](5 * time.Second)
	c.now = clk.Now

	var calls atomic.Int32
	started := make(chan struct{}) // closed once the (single) loader is running
	release := make(chan struct{}) // loader blocks here until we let it finish
	loader := func(ctx context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return 42, nil
	}

	const goroutines = 10
	results := make([]int, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], errs[0] = c.Get(context.Background(), "k", loader)
	}()

	// Wait until the first call's loader is definitely in flight — at
	// that point the flight is registered, so every Get below either
	// joins it or (if it arrives after completion) hits the cache.
	// Either way: exactly one loader call.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("loader never started")
	}

	for i := 1; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Get(context.Background(), "k", loader)
		}(i)
	}

	close(release)
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if results[i] != 42 {
			t.Fatalf("goroutine %d: got %d, want 42", i, results[i])
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("loader called %d times across %d concurrent Gets, want exactly 1", n, goroutines)
	}
}

func TestTTLCacheWaiterHonoursContextCancellation(t *testing.T) {
	clk := newFakeClock()
	c := NewTTLCache[string, int](5 * time.Second)
	c.now = clk.Now

	started := make(chan struct{})
	release := make(chan struct{})
	loader := func(ctx context.Context) (int, error) {
		close(started)
		<-release
		return 1, nil
	}

	go c.Get(context.Background(), "k", loader) //nolint:errcheck // result observed via cache state below
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, "k", func(context.Context) (int, error) { return 0, nil })
		waiterErr <- err
	}()
	cancel()

	select {
	case err := <-waiterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter got err %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled waiter never returned")
	}
	close(release)
}
