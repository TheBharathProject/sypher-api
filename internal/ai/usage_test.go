package ai

import (
	"testing"
	"time"
)

func TestPeriodWindowMonthBoundaries(t *testing.T) {
	// Mid-month
	now := time.Date(2026, 5, 17, 12, 30, 0, 0, time.UTC)
	start, end := PeriodWindow(now)
	if !start.Equal(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start: got %v", start)
	}
	if !end.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end: got %v", end)
	}
	// Year boundary (December → January)
	now = time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)
	start, end = PeriodWindow(now)
	if !start.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dec start: got %v", start)
	}
	if !end.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dec end: got %v", end)
	}
}

func TestErrUsageExceededIsSentinel(t *testing.T) {
	// Compile-time check that the sentinel is comparable with errors.Is.
	if ErrUsageExceeded == nil {
		t.Fatal("ErrUsageExceeded must not be nil")
	}
}
