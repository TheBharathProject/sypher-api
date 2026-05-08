package jobs

import (
	"testing"
	"time"
)

// AtIST is pure. The cases below pin its behaviour at the day boundary
// (was today's HH:MM ahead of `now`? if so fire today, else tomorrow)
// and across timezones (the function reads `now` in any zone but always
// schedules in IST).
func TestAtIST(t *testing.T) {
	// Helper: build a UTC time. We want the test stable regardless of
	// the developer's machine timezone.
	utc := func(year int, month time.Month, day, hour, min int) time.Time {
		return time.Date(year, month, day, hour, min, 0, 0, time.UTC)
	}

	cases := []struct {
		name     string
		fireH    int
		fireM    int
		now      time.Time
		wantHour int   // hour-of-day in IST that we expect NextFire to land on
		wantMin  int
		wantDate int   // day-of-month in IST. Lets us verify "today" vs "tomorrow".
		wantMon  time.Month
	}{
		// "today's 09:00 IST is still ahead" — IST is UTC+5:30, so 02:00 UTC
		// is 07:30 IST. NextFire(09:00) is later today.
		{
			name:     "before today's fire — same-day",
			fireH:    9, fireM: 0,
			now:      utc(2026, 5, 9, 2, 0), // 02:00 UTC = 07:30 IST same day
			wantHour: 9, wantMin: 0,
			wantDate: 9, wantMon: time.May,
		},
		// "today's 09:00 IST has passed" — fire tomorrow.
		// 06:00 UTC = 11:30 IST. NextFire(09:00) jumps to tomorrow 09:00 IST.
		{
			name:     "after today's fire — tomorrow",
			fireH:    9, fireM: 0,
			now:      utc(2026, 5, 9, 6, 0),
			wantHour: 9, wantMin: 0,
			wantDate: 10, wantMon: time.May,
		},
		// 03:00 IST job, called just before the fire (00:00 UTC = 05:30 IST
		// PREVIOUS day). Hmm that's actually a tricky case — UTC midnight is
		// already next-day in IST. We want the function to fire today (in IST).
		// Stick with a clean case: 21:00 UTC = 02:30 IST next day, so the
		// next 03:00 IST is 30 min away — same IST date.
		{
			name:     "03:00 IST job — early morning IST",
			fireH:    3, fireM: 0,
			now:      utc(2026, 5, 9, 21, 0), // 21:00 UTC May 9 = 02:30 IST May 10
			wantHour: 3, wantMin: 0,
			wantDate: 10, wantMon: time.May,
		},
		// Across month boundary: now is 23:30 IST on the last day of May.
		// 23:30 IST May 31 = 18:00 UTC May 31. 09:00 fire next is June 1.
		{
			name:     "month boundary",
			fireH:    9, fireM: 0,
			now:      utc(2026, 5, 31, 18, 0),
			wantHour: 9, wantMin: 0,
			wantDate: 1, wantMon: time.June,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next := AtIST(c.fireH, c.fireM)(c.now)
			istNext := next.In(IST)
			if istNext.Hour() != c.wantHour || istNext.Minute() != c.wantMin {
				t.Errorf("AtIST(%d:%d)(%s) IST hour:min = %d:%d, want %d:%d",
					c.fireH, c.fireM, c.now, istNext.Hour(), istNext.Minute(), c.wantHour, c.wantMin)
			}
			if istNext.Day() != c.wantDate || istNext.Month() != c.wantMon {
				t.Errorf("AtIST(%d:%d)(%s) IST date = %s %d, want %s %d",
					c.fireH, c.fireM, c.now,
					istNext.Month(), istNext.Day(), c.wantMon, c.wantDate)
			}
			// NextFire must always be in the future relative to now —
			// never returns a past timestamp (that would tight-loop the
			// runner; defensive `wait < 0 → 1s` would mask the bug).
			if !next.After(c.now) {
				t.Errorf("AtIST result %s is not strictly after now %s", next, c.now)
			}
		})
	}
}

// IST itself isn't worth a unit test (it's a package-level Location), but
// we can spot-check the offset to catch a packaging mishap (e.g. a build
// without /etc/zoneinfo data falling through to the FixedZone branch).
func TestISTOffset(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, IST)
	_, offset := now.Zone()
	wantSeconds := int((5*time.Hour + 30*time.Minute).Seconds())
	if offset != wantSeconds {
		t.Errorf("IST offset = %ds, want %ds", offset, wantSeconds)
	}
}
