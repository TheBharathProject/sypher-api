package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos"
)

// announcementsFetcher is the subset of *kairos.NSEClient this cron
// needs.
type announcementsFetcher interface {
	FetchAnnouncements(ctx context.Context) ([]kairos.Announcement, error)
}

// announcementsStore is the subset of *kairos.Store this cron needs.
type announcementsStore interface {
	InsertAnnouncements(ctx context.Context, rows []kairos.Announcement) (int, error)
	RecordIngestRun(ctx context.Context, job string, startedAt, finishedAt time.Time, ok bool, rowsWritten int, detail string) error
}

// announceNow feeds the market-day gate. A package var so tests can
// pin the clock to a known weekday/weekend; the audit timestamps
// below stay on time.Now directly (they're metadata, not behaviour).
var announceNow = time.Now

// KairosAnnouncements returns a cron func that pulls the NSE
// corporate-announcements feed and inserts new rows (deduped on
// nse_id). Register with EveryN(10*time.Minute) — spec §3.4, every
// 10 minutes on market days.
//
// Skips cleanly on non-market days (weekends, IST) with a throttled
// log line. NSE fetch failures (their WAF 403s routinely) are logged
// and recorded as a failed "announcements" ingest run, then swallowed
// — the next tick retries; nothing panics. DB insert failures record
// the failed run too but propagate, since those are ours to notice.
func KairosAnnouncements(store announcementsStore, client announcementsFetcher, logger *slog.Logger) func(context.Context) error {
	// 10-min cadence → 144 ticks/day. Log every Nth weekend skip so
	// the gate is visible without flooding journals (same throttle
	// idea as KairosOptionsSnapshot).
	var skipCount int
	const skipLogEvery = 36 // one line every 6 hours while skipping
	return func(ctx context.Context) error {
		const job = "announcements"
		if !isMarketDayIST(announceNow()) {
			skipCount++
			if skipCount%skipLogEvery == 1 {
				logger.Info("kairos announcements skipped",
					"reason", "not a market day",
					"next_log_in_ticks", skipLogEvery)
			}
			return nil
		}
		skipCount = 0

		startedAt := time.Now().UTC()
		anns, err := client.FetchAnnouncements(ctx)
		if err != nil {
			logger.Warn("kairos announcements fetch failed", "err", err)
			recordAnnouncementsRun(ctx, store, logger, startedAt, false, 0, "fetch: "+err.Error())
			return nil // expected NSE flakiness; recorded, retry next tick
		}
		inserted, err := store.InsertAnnouncements(ctx, anns)
		if err != nil {
			logger.Warn("kairos announcements insert failed", "err", err)
			recordAnnouncementsRun(ctx, store, logger, startedAt, false, inserted, "insert: "+err.Error())
			return fmt.Errorf("kairos announcements insert: %w", err)
		}
		if inserted > 0 {
			logger.Info("kairos announcements ingested",
				"fetched", len(anns), "inserted", inserted)
		}
		recordAnnouncementsRun(ctx, store, logger, startedAt, true, inserted, "")
		return nil
	}
}

// recordAnnouncementsRun writes the ingest_runs audit row,
// best-effort, on a detached context so the row still lands when the
// tick's ctx was cancelled mid-fetch (mirrors kairos.SnapshotChains).
func recordAnnouncementsRun(ctx context.Context, store announcementsStore, logger *slog.Logger,
	startedAt time.Time, ok bool, rows int, detail string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.RecordIngestRun(rctx, "announcements", startedAt, time.Now().UTC(), ok, rows, detail); err != nil {
		logger.Warn("kairos ingest run record failed", "job", "announcements", "err", err)
	}
}

// isMarketDayIST reports whether the time falls on an NSE trading
// weekday (IST). Unlike the options snapshot's market-hours gate,
// announcements keep landing after the closing bell, so only the
// weekend is excluded. Exchange holidays are a known TODO shared with
// isWithinMarketHoursIST (kairos_snapshot.go).
func isMarketDayIST(now time.Time) bool {
	wd := now.In(IST).Weekday()
	return wd != time.Saturday && wd != time.Sunday
}
