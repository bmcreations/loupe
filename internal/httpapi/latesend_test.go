package httpapi

import (
	"testing"
	"time"
)

func TestLateGateSendsStaleFramesLateOnlyOnStreamTransports(t *testing.T) {
	now := time.Unix(1000, 0)
	cases := []struct {
		name    string
		canLate bool
		wait    time.Duration
		want    bool
	}{
		{"quic, a keyframe-sized backlog", true, 300 * time.Millisecond, true},
		{"quic, at the wait bound", true, lateWaitMax, true},
		{"quic, past the wait bound", true, lateWaitMax + time.Millisecond, false},
		{"websocket sheds as before", false, 200 * time.Millisecond, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := lateGate{canLate: tc.canLate}
			if got := g.sendLate(now, tc.wait); got != tc.want {
				t.Fatalf("sendLate = %v, want %v", got, tc.want)
			}
		})
	}
}

// 18 frames queue behind a 300ms keyframe write; all are stale and all are
// consecutive, and they must all go. A run that lasts staleRunMax is a link
// that is not draining, and ends in a shed.
func TestLateGateShedsOnlyASustainedRun(t *testing.T) {
	g := lateGate{canLate: true}
	now := time.Unix(1000, 0)
	for i := 0; i < 18; i++ {
		if !g.sendLate(now.Add(time.Duration(i)*5*time.Millisecond), 300*time.Millisecond) {
			t.Fatalf("frame %d of a draining backlog was shed", i)
		}
	}
	if g.sendLate(now.Add(staleRunMax), 300*time.Millisecond) {
		t.Fatal("a run of stale frames lasting staleRunMax was still sent late")
	}
	// The shed ended the run, so the next stale frame starts a new one.
	if !g.sendLate(now.Add(staleRunMax+time.Millisecond), 300*time.Millisecond) {
		t.Error("the run was not reset by the shed")
	}
}

func TestLateGateRunEndsWhenAFrameGoesOutInTime(t *testing.T) {
	g := lateGate{canLate: true}
	now := time.Unix(1000, 0)
	g.sendLate(now, 200*time.Millisecond)
	g.fresh()
	if !g.sendLate(now.Add(staleRunMax+time.Second), 200*time.Millisecond) {
		t.Fatal("an old run survived a fresh frame")
	}
}

func TestLateGateSeesAFrameQueuedBehindAKeyWrite(t *testing.T) {
	g := lateGate{canLate: true}
	now := time.Unix(1000, 0)
	if g.overlappedKey(now) {
		t.Error("overlap reported before any key write")
	}
	g.noteKeyWritten(now)
	cases := []struct {
		name     string
		queuedAt time.Time
		want     bool
	}{
		{"queued during the write", now.Add(-100 * time.Millisecond), true},
		{"queued after it ended", now.Add(time.Millisecond), false},
	}
	for _, tc := range cases {
		if got := g.overlappedKey(tc.queuedAt); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSendTraceReportsPerSecondMaximaAndCumulativeCounts(t *testing.T) {
	var tr sendTrace
	tr.noteWrite(false, 20*time.Millisecond)
	tr.noteWrite(true, 310*time.Millisecond)
	tr.noteWrite(false, 40*time.Millisecond)
	tr.sentLate, tr.staleOverKey = 3, 2
	rec := tr.take()
	if rec["writeMaxMs"] != 310.0 || rec["keyWriteMaxMs"] != 310.0 || rec["sentLate"] != uint64(3) || rec["staleOverKey"] != uint64(2) {
		t.Fatalf("row %v", rec)
	}
	tr.noteWrite(false, 5*time.Millisecond)
	rec = tr.take()
	if rec["writeMaxMs"] != 5.0 || rec["keyWriteMaxMs"] != 0.0 || rec["sentLate"] != uint64(3) {
		t.Fatalf("next second %v: maxima must reset, counts must not", rec)
	}
}
