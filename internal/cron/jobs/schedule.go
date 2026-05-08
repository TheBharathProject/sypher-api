// Package jobs holds the concrete cron job implementations.
//
// AtIST returns a NextFire callback that fires at hh:mm Asia/Kolkata
// (IST, UTC+5:30, no DST) every day. Why IST: the only person paying
// attention is in India, and "9 AM" only feels like 9 AM if you compute
// it in the user's timezone — UTC 09:00 lands at 14:30 IST which is mid-
// afternoon and useless as a "morning digest" trigger.
//
// If we ever ship for multiple timezones, this becomes "fire daily at the
// recipient's local 09:00", which is a per-user calculation, not a global
// schedule. Cross that bridge when international users show up.
package jobs

import "time"

// IST is Asia/Kolkata. We resolve once at package load — if /etc/zoneinfo
// is missing in the container, fall back to a fixed offset so the cron
// at least runs *somewhere* sensible.
var IST = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return loc
	}
	return time.FixedZone("IST", int((5*time.Hour + 30*time.Minute).Seconds()))
}()

// AtIST returns a NextFire that schedules for hour:minute IST every day.
// If today's hh:mm is still ahead, that's the next fire. Otherwise it's
// tomorrow's hh:mm. Pure function — no side effects, no shared state.
func AtIST(hour, minute int) func(now time.Time) time.Time {
	return func(now time.Time) time.Time {
		nowIST := now.In(IST)
		todayFire := time.Date(
			nowIST.Year(), nowIST.Month(), nowIST.Day(),
			hour, minute, 0, 0,
			IST,
		)
		if todayFire.After(nowIST) {
			return todayFire
		}
		return todayFire.Add(24 * time.Hour)
	}
}
