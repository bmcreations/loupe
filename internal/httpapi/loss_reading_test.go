package httpapi

import (
	"math"
	"testing"
	"time"
)

// The 13:23:26 second of the 5G trace: 58 produced, two stale sheds, 32
// keyframe-wait sheds, and the client decoded 23. What was sent whole is
// 58 - 34 = 24, and the wait frames are in neither the events nor the sent.
func TestLossReadingSeparatesEventsFromTheirKeyframeWait(t *testing.T) {
	base := time.Unix(5000, 0)
	tr := newHealthTracker(base, func() time.Time { return base })
	tr.sampleAt(base.Add(time.Second), fakeCounters{media: 1000, csAt: base}, 82)

	// Tick that the next report measures: 58 produced, 2 stale, 32 awaiting.
	tr.sampleAt(base.Add(2*time.Second), fakeCounters{media: 1058, shed: 34, stale: 2, await: 32, csAt: base}, 82)
	if _, ok := tr.TakeLoss(); ok {
		t.Fatal("a loss reading without a client report")
	}

	cs := map[string]any{"decFps": 23.0, "rttMs": 101.0}
	tr.sampleAt(base.Add(3*time.Second), fakeCounters{media: 1116, shed: 68, stale: 4, await: 64,
		cs: cs, csAt: base.Add(3 * time.Second)}, 82)
	r, ok := tr.TakeLoss()
	if !ok || !r.Valid {
		t.Fatal("no loss reading after a fresh client report")
	}
	if r.Produced != 58 || r.Delivered != 23 || math.Round(r.Sent) != 24 || math.Round(r.Events) != 2 || r.Abandons != 0 {
		t.Fatalf("reading %+v, want produced 58 delivered 23 sent 24 events 2 abandons 0", r)
	}
	if _, ok := tr.TakeLoss(); ok {
		t.Error("the same reading was handed out twice")
	}
}

func TestLossReadingCountsAnAbandonAsAnEventNotAsSent(t *testing.T) {
	base := time.Unix(6000, 0)
	tr := newHealthTracker(base, func() time.Time { return base })
	tr.sampleAt(base.Add(time.Second), fakeCounters{media: 0, csAt: base}, 82)
	tr.sampleAt(base.Add(2*time.Second), fakeCounters{media: 34, shed: 31, abandon: 1, await: 30, csAt: base}, 82)
	tr.sampleAt(base.Add(3*time.Second), fakeCounters{media: 68, shed: 62, abandon: 2, await: 60,
		cs: map[string]any{"decFps": 10.0}, csAt: base.Add(3 * time.Second)}, 82)
	r, _ := tr.TakeLoss()
	if math.Round(r.Sent) != 3 || math.Round(r.Events) != 1 || math.Round(r.Abandons) != 1 {
		t.Fatalf("reading %+v, want sent 3, one event, one abandon", r)
	}
}

// The 5G trace phone decoded nothing for its first 6s (decFps 0) while
// receiving 2.4 Mbps; the reading must not say the client decoded until a
// report with decoded frames has arrived, and must keep saying so after.
func TestLossReadingIsDecodedOnlyAfterTheFirstDecodedFrame(t *testing.T) {
	base := time.Unix(7000, 0)
	tr := newHealthTracker(base, func() time.Time { return base })
	tr.sampleAt(base.Add(time.Second), fakeCounters{media: 0, csAt: base}, 82)
	cases := []struct {
		name   string
		decFps float64
		want   bool
	}{
		{"nothing decoded", 0, false},
		{"still nothing", 0, false},
		{"first frames", 12, true},
		{"a later zero keeps it", 0, true},
	}
	media := uint64(0)
	for i, tc := range cases {
		media += 60
		at := base.Add(time.Duration(i+2) * time.Second)
		tr.sampleAt(at, fakeCounters{media: media, cs: map[string]any{"decFps": tc.decFps}, csAt: at}, 82)
		r, ok := tr.TakeLoss()
		if !ok || r.Decoded != tc.want {
			t.Errorf("%s: ok %v Decoded %v, want %v", tc.name, ok, r.Decoded, tc.want)
		}
	}
}
