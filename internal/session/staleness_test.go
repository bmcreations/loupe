package session

import (
	"testing"
	"time"
)

// A fixed staleness threshold assumes a fast path. On 5G with a 233ms baseline
// the pacer can be reporting a perfectly healthy link while every frame on it
// trips a 150ms staleness check, and the two disagree about the same path.
func TestStalenessScalesWithThePath(t *testing.T) {
	fast := StalenessFor(10 * time.Millisecond)
	slow := StalenessFor(233 * time.Millisecond)

	if fast < MaxStaleness {
		t.Errorf("staleness %v on a fast path is below the floor %v", fast, MaxStaleness)
	}
	if slow <= fast {
		t.Errorf("staleness is %v on a 233ms path and %v on a 10ms one", slow, fast)
	}
	if got := StalenessFor(30 * time.Second); got != stalenessCap {
		t.Errorf("staleness %v on a 30s baseline, want the cap %v", got, stalenessCap)
	}
	// A negative baseline cannot happen from Pacer.Base, but a threshold that
	// goes below its own floor would shed everything, so it is pinned.
	if got := StalenessFor(-time.Second); got != MaxStaleness {
		t.Errorf("staleness %v on a negative baseline, want the floor %v", got, MaxStaleness)
	}
}

// Pacer rejection, staleness, a keyframe wait and a frame abandoned in flight
// are four different faults.
// Collapsing them into one counter is what made trap 19 invisible, and the same
// shape made the 07:36 trace unreadable: a `shed` of 315 says nothing about
// whether the controller was working or the link had failed.
func TestShedBreakdownSumsToTheTotal(t *testing.T) {
	var s Session

	s.NoteShed(ShedPacer)
	s.NoteShed(ShedPacer)
	s.NoteShed(ShedStale)
	s.NoteShed(ShedAwaitKey)
	s.NoteShed(ShedAwaitKey)
	s.NoteShed(ShedAwaitKey)
	s.NoteShed(ShedAbandon)
	s.NoteShed(ShedAbandon)
	s.NoteShed(ShedAbandon)
	s.NoteShed(ShedAbandon)

	got := s.ShedBreakdown()
	want := ShedCounts{Pacer: 2, Stale: 1, AwaitKey: 3, Abandon: 4}
	if got != want {
		t.Errorf("breakdown = %+v; want %+v", got, want)
	}
	sum := got.Pacer + got.Stale + got.AwaitKey + got.Abandon
	if total := s.ShedCount(); total != sum {
		t.Errorf("ShedCount %d != breakdown sum %d; the four reasons must account for every shed frame", total, sum)
	}
}

// ClientStats is session state, so a viewer that joins is handed whatever the
// previous viewer last reported. The 07:36:31 reconnect was handed 1400ms from
// a connection that had already failed. The timestamp is what lets the caller
// tell that reading from a current one.
func TestClientRTTCarriesItsAge(t *testing.T) {
	var s Session

	if _, at := s.ClientRTTAt(); !at.IsZero() {
		t.Error("an unreported rtt has a timestamp; a caller cannot tell it was never measured")
	}

	before := time.Now()
	s.RecordClientStats(map[string]any{"rttMs": float64(42)})
	ms, at := s.ClientRTTAt()
	if ms != 42 {
		t.Errorf("rtt = %v, want 42", ms)
	}
	if at.Before(before) {
		t.Errorf("timestamp %v predates the report at %v", at, before)
	}

	// A report without a usable rtt must not refresh the timestamp, or a client
	// that has stopped measuring looks like one that is still reporting.
	s.RecordClientStats(map[string]any{"fps": float64(2)})
	if _, again := s.ClientRTTAt(); !again.Equal(at) {
		t.Error("a report carrying no rtt refreshed the timestamp, hiding a stale measurement")
	}
}
