package session

import (
	"context"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// The 12:15:50 cut in the 5G trace. About ten seconds of a static screen
// (2 frames a second, 160 kbps; the window is judged before ten seconds of it
// would be sparse enough to reset), then a burst: a few frames missed their
// deadline, the abandon started a keyframe wait, and the next seconds shed
// frames awaiting it. The undercarried gate read goodput 535 kbps over the
// whole window -- what the still screen needed -- and cut 4 Mbps to it.
// Nothing in the window measured the link, so nothing may be cut.
func TestABurstAfterAnIdleScreenDoesNotCutToTheIdleGoodput(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	start := time.Now()
	a.Observe(start, BitrateSample{})
	rtt := func(s BitrateSample) BitrateSample {
		s.RTT, s.BaseRTT = 250*time.Millisecond, 100*time.Millisecond // inside a spike
		return s
	}
	idle := rtt(BitrateSample{Frames: 2, Bytes: 20_000})
	burst := rtt(BitrateSample{Frames: 60, Shed: 3, AwaitKey: 30, Bytes: 15_000})

	var rep BitrateReport
	for i := 1; i <= 11; i++ {
		s := idle
		if i > 8 {
			s = burst
		}
		var next int
		next, rep = a.Observe(start.Add(time.Duration(i)*time.Second), s)
		if next != 0 {
			t.Fatalf("second %d: %s cut the encoder to %d (goodput %d over %dms busy of %dms active)",
				i, rep.Gate, next, rep.GoodputBps, rep.BusyMs, rep.ActiveMs)
		}
	}
	if rep.GoodputBps != 0 {
		t.Errorf("goodput %d reported for a window that was mostly idle; want 0 (not measured)", rep.GoodputBps)
	}
}

// The same cut must also stay out of the shed gate, which aims at goodput too.
func TestAShedBurstAfterAnIdleScreenIsDeclined(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	start := time.Now()
	a.Observe(start, BitrateSample{})
	idled := false
	for i := 1; i <= 11; i++ {
		s := BitrateSample{Frames: 2, Bytes: 20_000}
		if i > 8 {
			s = BitrateSample{Frames: 60, Shed: 30, Bytes: 15_000}
		}
		next, rep := a.Observe(start.Add(time.Duration(i)*time.Second), s)
		if next != 0 {
			t.Fatalf("second %d: %s cut to %d", i, rep.Gate, next)
		}
		idled = idled || rep.Gate == GateIdle
	}
	if !idled {
		t.Error("the window was never declined as idle")
	}
}

// A window that really was busy still measures the link: moving content for the
// whole window, 10 fps up, carrying a fifth of what was asked.
func TestABusyWindowStillAimsAtGoodput(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	got, _ := feed(a, time.Now(), 12, BitrateSample{Frames: 30, Shed: 12, Bytes: 100_000})
	if got == 0 || got > 1_000_000 {
		t.Fatalf("busy shedding window moved bitrate to %d, want a cut to about 800 kbps", got)
	}
}

// A keyframe wait that follows our own abandon is not a capacity signal. The
// abandon is a shed, so healthyFor is zero for the whole wait; the rtt being
// back near baseline is what says the link has recovered.
func TestAKeyframeWaitAfterAnAbandonOnANormalRTTIsNotASignal(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	s := BitrateSample{Frames: 60, Shed: 2, AwaitKey: 40, Bytes: 5_000,
		RTT: 110 * time.Millisecond, BaseRTT: 100 * time.Millisecond}
	got, _ := feed(a, time.Now(), 30, s)
	if got != 0 {
		t.Fatalf("keyframe waits on a normal rtt cut the bitrate to %d", got)
	}
}

func TestFPSIsLoweredBeforeSize(t *testing.T) {
	const w, h = 664, 1440
	cases := []struct {
		name     string
		bps, cur int
		want     int
	}{
		{"full rate keeps 60", 4_500_000, 60, 60},
		{"2.1 Mbps at 664x1440 halves the rate", 2_100_000, 60, 30},
		{"1 Mbps goes to 15", 1_000_000, 60, 15},
		{"the floor holds at 15", 250_000, 15, 15},
		{"a rate that only just fits 60 does not step up", 4_300_000, 30, 30},
		{"a rate with margin steps back up", 6_500_000, 30, 60},
	}
	for _, c := range cases {
		if got := fpsForBitRate(c.bps, 60, c.cur, w, h); got != c.want {
			t.Errorf("%s: fpsForBitRate(%d, cur %d) = %d, want %d", c.name, c.bps, c.cur, got, c.want)
		}
	}
	if got := fpsForBitRate(100, 30, 30, w, h); got != 15 {
		t.Errorf("30fps session at the floor = %d, want 15", got)
	}
	if got := fpsForBitRate(100, 10, 10, w, h); got != 10 {
		t.Errorf("a session opened below the floor was raised to %d", got)
	}
}

// A simulator cannot lower its frame rate, so the size ladder stops at heldEdge
// instead of going to 248x544.
func TestASimulatorHoldsItsSizeAtTheFloor(t *testing.T) {
	st := &resizeStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	tgt := &liveTarget{swapTarget: swapTarget{kind: target.KindSimulator}, stream: st}
	s, err := Open(context.Background(), "hold", tgt, target.Options{MaxFPS: 60, BitRate: 4_000_000})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 664, 1440
	s.mu.Unlock()
	if edge, err := s.resizeForBitRate(535_000); err != nil || edge != heldEdge {
		t.Fatalf("resize at 535 kbps = %d, %v; want the held %d, not the 540 rung", edge, err, heldEdge)
	}
}

// A relaunch cut asks the new encoder for fewer frames, not a smaller picture.
func TestARelaunchCutLowersTheCaptureRate(t *testing.T) {
	first, second := newSwapStream(), newSwapStream()
	s, tgt := openSwap(t, first, second)
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 664, 1440
	s.mu.Unlock()
	s.streamMu.Lock()
	s.opts.MaxFPS = 60
	s.openFPS = 60
	s.streamMu.Unlock()
	if _, err := s.changeBitRate(1_000_000); err != nil {
		t.Fatalf("changeBitRate: %v", err)
	}
	tgt.mu.Lock()
	defer tgt.mu.Unlock()
	if n := len(tgt.opened); n == 0 || tgt.opened[n-1].MaxFPS != 15 {
		t.Fatalf("relaunched with %+v, want MaxFPS 15", tgt.opened)
	}
}
