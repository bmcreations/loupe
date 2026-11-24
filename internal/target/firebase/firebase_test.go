package firebase

import (
	"testing"
	"time"
)

func TestResolveClockRollsPastMidnight(t *testing.T) {
	now := time.Date(2026, 11, 10, 23, 50, 0, 0, time.Local)
	if got, want := ResolveClock("12:05 AM", now), time.Date(2026, 11, 11, 0, 5, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, want := ResolveClock("12:30 PM", time.Date(2026, 11, 10, 12, 20, 0, 0, time.Local)), time.Date(2026, 11, 10, 12, 30, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if !ResolveClock("soon", now).IsZero() {
		t.Error("unreadable clock should be zero")
	}
}
