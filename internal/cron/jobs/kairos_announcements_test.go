package jobs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos"
)

// ── fakes for the narrow announcementsFetcher/announcementsStore seams ──

type fakeAnnFetcher struct {
	anns  []kairos.Announcement
	err   error
	calls int
}

func (f *fakeAnnFetcher) FetchAnnouncements(context.Context) ([]kairos.Announcement, error) {
	f.calls++
	return f.anns, f.err
}

type annRun struct {
	job    string
	ok     bool
	rows   int
	detail string
}

type fakeAnnStore struct {
	insertN   int // rows reported inserted on success
	insertErr error
	batches   [][]kairos.Announcement
	runs      []annRun
	runCtxErr []error // ctx.Err() observed when each run was recorded
}

func (f *fakeAnnStore) InsertAnnouncements(_ context.Context, rows []kairos.Announcement) (int, error) {
	f.batches = append(f.batches, rows)
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	return f.insertN, nil
}

func (f *fakeAnnStore) RecordIngestRun(ctx context.Context, job string, _, _ time.Time, ok bool, rows int, detail string) error {
	f.runs = append(f.runs, annRun{job: job, ok: ok, rows: rows, detail: detail})
	f.runCtxErr = append(f.runCtxErr, ctx.Err())
	return nil
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// pinAnnounceNow fixes the market-day gate's clock for one test so the
// suite passes identically on a Tuesday and a Sunday.
func pinAnnounceNow(t *testing.T, fixed time.Time) {
	t.Helper()
	prev := announceNow
	announceNow = func() time.Time { return fixed }
	t.Cleanup(func() { announceNow = prev })
}

// 2026-06-10 is a Wednesday, 2026-06-13 a Saturday (both IST).
var (
	wednesdayIST = time.Date(2026, time.June, 10, 12, 0, 0, 0, IST)
	saturdayIST  = time.Date(2026, time.June, 13, 12, 0, 0, 0, IST)
)

// TestIsMarketDayIST pins the weekend gate, including the day flip at
// the UTC/IST boundary — the gate must read the IST calendar, not the
// server's.
func TestIsMarketDayIST(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"weekday IST", wednesdayIST, true},
		{"saturday IST", saturdayIST, false},
		{"sunday IST", time.Date(2026, time.June, 14, 12, 0, 0, 0, IST), false},
		// 20:00 UTC Friday is already 01:30 IST Saturday — closed.
		{"friday UTC is saturday IST", time.Date(2026, time.June, 12, 20, 0, 0, 0, time.UTC), false},
		// 19:00 UTC Sunday is 00:30 IST Monday — market day.
		{"sunday UTC is monday IST", time.Date(2026, time.June, 14, 19, 0, 0, 0, time.UTC), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isMarketDayIST(c.now); got != c.want {
				t.Errorf("isMarketDayIST(%s) = %v, want %v", c.now, got, c.want)
			}
		})
	}
}

// TestKairosAnnouncementsWeekendSkip: on a Saturday the tick is a
// no-op — no fetch, no insert, no ingest-run row, nil error.
func TestKairosAnnouncementsWeekendSkip(t *testing.T) {
	pinAnnounceNow(t, saturdayIST)
	fetcher := &fakeAnnFetcher{}
	store := &fakeAnnStore{}
	fn := KairosAnnouncements(store, fetcher, discardLogger())

	if err := fn(context.Background()); err != nil {
		t.Fatalf("weekend tick returned %v, want nil", err)
	}
	if fetcher.calls != 0 {
		t.Errorf("fetch calls = %d, want 0 on a weekend", fetcher.calls)
	}
	if len(store.batches) != 0 || len(store.runs) != 0 {
		t.Errorf("store touched on a weekend: batches=%d runs=%d", len(store.batches), len(store.runs))
	}
}

// TestKairosAnnouncementsSuccess: fetched rows go to the store in one
// batch and the run is recorded ok under job "announcements" with the
// *inserted* (deduped) count, not the fetched count.
func TestKairosAnnouncementsSuccess(t *testing.T) {
	pinAnnounceNow(t, wednesdayIST)
	fetcher := &fakeAnnFetcher{anns: []kairos.Announcement{
		{NseID: "1", Headline: "A"},
		{NseID: "2", Headline: "B"},
		{NseID: "3", Headline: "C"},
	}}
	store := &fakeAnnStore{insertN: 2} // one row was a dupe
	fn := KairosAnnouncements(store, fetcher, discardLogger())

	if err := fn(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if fetcher.calls != 1 {
		t.Errorf("fetch calls = %d, want 1", fetcher.calls)
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 3 {
		t.Fatalf("insert batches = %v, want one batch of 3", store.batches)
	}
	if len(store.runs) != 1 {
		t.Fatalf("runs = %+v, want exactly one", store.runs)
	}
	if run := store.runs[0]; run.job != "announcements" || !run.ok || run.rows != 2 || run.detail != "" {
		t.Errorf("run = %+v, want {announcements ok rows:2 detail:\"\"}", run)
	}
}

// TestKairosAnnouncementsFetchFailure: NSE flakiness is swallowed —
// nil error so the cron keeps ticking — but the failed run is still
// recorded with a fetch detail, on a context detached from the
// (possibly cancelled) tick context.
func TestKairosAnnouncementsFetchFailure(t *testing.T) {
	pinAnnounceNow(t, wednesdayIST)
	fetcher := &fakeAnnFetcher{err: &kairos.NSEError{Op: "fetch", Status: 403}}
	store := &fakeAnnStore{}
	fn := KairosAnnouncements(store, fetcher, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate the tick's ctx dying mid-fetch
	if err := fn(ctx); err != nil {
		t.Fatalf("fetch failure must be swallowed, got %v", err)
	}
	if len(store.batches) != 0 {
		t.Error("insert must not run after a failed fetch")
	}
	if len(store.runs) != 1 {
		t.Fatalf("runs = %+v, want exactly one failed row", store.runs)
	}
	run := store.runs[0]
	if run.job != "announcements" || run.ok || run.rows != 0 {
		t.Errorf("run = %+v, want failed announcements row with 0 rows", run)
	}
	if !strings.HasPrefix(run.detail, "fetch: ") {
		t.Errorf("run detail = %q, want fetch: prefix", run.detail)
	}
	if store.runCtxErr[0] != nil {
		t.Errorf("record ctx err = %v, want nil (detached from cancelled tick ctx)", store.runCtxErr[0])
	}
}

// TestKairosAnnouncementsInsertFailure: a DB failure is ours — the
// error propagates (wrapping the cause) and the failed run is
// recorded with an insert detail.
func TestKairosAnnouncementsInsertFailure(t *testing.T) {
	pinAnnounceNow(t, wednesdayIST)
	boom := errors.New("boom")
	fetcher := &fakeAnnFetcher{anns: []kairos.Announcement{{NseID: "1", Headline: "A"}}}
	store := &fakeAnnStore{insertErr: boom}
	fn := KairosAnnouncements(store, fetcher, discardLogger())

	err := fn(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrap of insert error", err)
	}
	if len(store.runs) != 1 {
		t.Fatalf("runs = %+v, want exactly one failed row", store.runs)
	}
	run := store.runs[0]
	if run.job != "announcements" || run.ok {
		t.Errorf("run = %+v, want failed announcements row", run)
	}
	if !strings.HasPrefix(run.detail, "insert: ") {
		t.Errorf("run detail = %q, want insert: prefix", run.detail)
	}
}
