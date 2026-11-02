package session

import (
	"testing"
	"time"
)

// The 5G trace row: undercarried 4M -> 1.08M with awaitMs 5999 of activeMs
// 10999. A 6s keyframe wait carried a trickle; the 5 seconds outside it
// carried ~1.83 Mbps (228_750 B/s). Goodput over the whole busy time read the
// wait as the link. rttOKFor is 0 here (no rtt in the samples), so the 3s
// healthy grace does not discount the wait either.
func TestUndercarriedAimsAtTheRateOutsideAKeyframeWait(t *testing.T) {
	cases := []struct {
		name  string
		build func() *BitrateAdapter
	}{
		{"relaunch", func() *BitrateAdapter { return NewBitrateAdapter(4_000_000) }},
		{"live", func() *BitrateAdapter { return NewLiveBitrateAdapter(4_000_000) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.build()
			now := time.Now()
			a.Observe(now, BitrateSample{})
			var next int
			var rep BitrateReport
			for i := 0; i < 10; i++ {
				now = now.Add(time.Second)
				s := BitrateSample{Frames: 60, Shed: 0, Bytes: 228_750}
				if i < 6 {
					s = BitrateSample{Frames: 60, AwaitKey: 40, Bytes: 5_000}
				}
				next, rep = a.Observe(now, s)
			}
			if rep.Gate != GateUndercarried {
				t.Fatalf("gate %q, want undercarried (awaitMs %d activeMs %d)", rep.Gate, rep.AwaitMs, rep.ActiveMs)
			}
			if rep.CarriedBps < 1_700_000 || rep.CarriedBps > 1_950_000 {
				t.Errorf("carried %d bps, want ~1.83 Mbps", rep.CarriedBps)
			}
			if next < 1_700_000 {
				t.Errorf("cut to %d, want the rate outside the wait (the whole-window figure is %d)", next, rep.GoodputBps)
			}
			if rep.GoodputBps >= rep.CarriedBps {
				t.Errorf("goodput %d should read under carried %d here", rep.GoodputBps, rep.CarriedBps)
			}
		})
	}
}

func TestCarriedIsInconclusiveWithTooLittleOutsideAWait(t *testing.T) {
	cases := []struct {
		name     string
		clearSec int
		want     bool
	}{
		{"none", 0, false},
		{"under minBusyTime", 1, false},
		{"exactly minBusyTime", 2, true},
		{"plenty", 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewLiveBitrateAdapter(4_000_000)
			now := time.Now()
			a.Observe(now, BitrateSample{})
			for i := 0; i < 8; i++ {
				now = now.Add(time.Second)
				s := BitrateSample{Frames: 60, AwaitKey: 40, Bytes: 5_000}
				if i < tc.clearSec {
					s = BitrateSample{Frames: 60, Bytes: 100_000}
				}
				a.Observe(now, s)
			}
			if got := a.carried() > 0; got != tc.want {
				t.Fatalf("carried %d, conclusive=%v want %v", a.carried(), got, tc.want)
			}
		})
	}
}
