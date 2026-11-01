package session

import (
	"testing"
	"time"
)

// The 12:38 trace on 5G: the phone carried about 1 Mbps against a 4 Mbps
// encoder and the current round trip went 100 -> 507 -> 570ms. Every frame the
// link could not carry queued, and the windowed gates took ten seconds to act.
func queued(rtt time.Duration) BitrateSample {
	// Moving content at 60fps, a saturated 1 Mbps link delivering 125 kB/s.
	return BitrateSample{Frames: 60, Bytes: 125_000, RTT: rtt, BaseRTT: 100 * time.Millisecond}
}

func TestAClimbingRTTCutsTheBitrateWithinASecondOrTwo(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, queued(100*time.Millisecond))

	var got int
	var rep BitrateReport
	var at time.Duration
	for i, rtt := range []time.Duration{110, 300, 507, 570} {
		now = now.Add(time.Second)
		got, rep = a.Observe(now, queued(rtt*time.Millisecond))
		if got != 0 {
			at = time.Duration(i+1) * time.Second
			break
		}
	}
	if got == 0 {
		t.Fatal("no cut while the round trip sat at 5x baseline")
	}
	if at > 3*time.Second {
		t.Errorf("cut after %s, want within a couple of samples", at)
	}
	if rep.Gate != GateDelay {
		t.Errorf("gate %s, want %s", rep.Gate, GateDelay)
	}
	// 0.8 of the 1 Mbps the link carried while it queued.
	if got < 700_000 || got > 900_000 {
		t.Errorf("cut to %d, want about 800 kbps (0.8 of the carried 1 Mbps)", got)
	}
	if a.changes != 0 {
		t.Errorf("a delay cut counted against the change cap: %d", a.changes)
	}
}

func TestASingleRTTSpikeDoesNotCut(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, queued(100*time.Millisecond))
	for _, rtt := range []time.Duration{100, 400, 100, 100, 400, 100} {
		now = now.Add(time.Second)
		if got, _ := a.Observe(now, queued(rtt*time.Millisecond)); got != 0 {
			t.Fatalf("a one-sample spike to %dms cut to %d", rtt, got)
		}
	}
}

func TestAnIdleScreensRTTDoesNotCut(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, queued(100*time.Millisecond))
	idle := BitrateSample{Frames: 2, Bytes: 2_000, RTT: 500 * time.Millisecond, BaseRTT: 100 * time.Millisecond}
	if got, _ := feed(a, now, 8, idle); got != 0 {
		t.Fatalf("a slow round trip on a static screen cut to %d", got)
	}
}

func TestADelayCutWithoutAMeasurementDecreasesMultiplicatively(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, queued(100*time.Millisecond))
	// Every frame waiting for a keyframe: nothing measures the link.
	s := queued(500 * time.Millisecond)
	s.AwaitKey, s.Bytes = 60, 1_000
	got, _ := feed(a, now, 2, s)
	if got != 2_800_000 {
		t.Fatalf("blind cut to %d, want 0.7 of 4 Mbps", got)
	}
}

// The cut is outside the change cap, so a session that spent its changes on
// the way down still answers a queue.
func TestADelayCutIgnoresTheChangeCap(t *testing.T) {
	a := NewBitrateAdapter(4_000_000)
	a.changes = maxAdaptiveChanges
	now := time.Now()
	a.Observe(now, queued(100*time.Millisecond))
	if got, _ := feed(a, now, 3, queued(500*time.Millisecond)); got == 0 {
		t.Fatal("capped session did not answer a queue")
	}
}

// A relaunched encoder is not restarted over and over: after the settle window
// the round trip is judged again, and the per-period limit stops a flapping
// link.
func TestDelayCutsAreRateLimited(t *testing.T) {
	a := NewBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, queued(100*time.Millisecond))
	cuts := 0
	for i := 0; i < 120; i++ {
		now = now.Add(time.Second)
		if got, rep := a.Observe(now, queued(500*time.Millisecond)); got != 0 && rep.Gate == GateDelay {
			cuts++
			// Back to a high rate so the next cut is not floored away.
			a.current = 4_000_000
		}
	}
	if cuts != relaunchWindows.maxDelayCuts {
		t.Fatalf("%d delay cuts in two minutes, want the limit of %d", cuts, relaunchWindows.maxDelayCuts)
	}
}

func TestRecoveryAfterADelayCutNeedsDeliveryAsWellAsRTT(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, queued(100*time.Millisecond))
	got, now := feed(a, now, 3, queued(500*time.Millisecond))
	if got == 0 {
		t.Fatal("setup: no delay cut")
	}
	// RTT is back near base but the viewer still awaits a keyframe.
	waiting := healthyLink
	waiting.BaseRTT, waiting.RTT = 100*time.Millisecond, 110*time.Millisecond
	waiting.AwaitKey = 40
	if got, now = feed(a, now, 20, waiting); got != 0 {
		t.Fatalf("stepped up to %d while keyframe waits continued", got)
	}
	ok := waiting
	ok.AwaitKey = 0
	got, _ = feed(a, now, int(liveRecoverWindow/time.Second)+2, ok)
	if got == 0 {
		t.Fatal("no recovery once the round trip and delivery were healthy")
	}
	if a.afterDelay {
		t.Error("afterDelay stayed set after recovery")
	}
}

// A delay cut right after a fast step up is the same wrong guess a windowed
// step down is, and switches fast recovery off.
func TestADelayCutAfterAFastStepUpHoldsRecovery(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	a.current = 1_000_000
	now := time.Now()
	a.Observe(now, healthyLink)
	healthy := healthyLink
	healthy.BaseRTT, healthy.RTT = 100*time.Millisecond, 110*time.Millisecond
	got, now := feed(a, now, int(liveRecoverWindow/time.Second)+2, healthy)
	if got == 0 {
		t.Fatal("setup: no fast step up")
	}
	if got, _ = feed(a, now, 4, queued(500*time.Millisecond)); got == 0 {
		t.Fatal("no delay cut after the step up")
	}
	if !a.holdUntil.After(now) {
		t.Error("fast recovery not held after a delay cut that followed a fast step up")
	}
}
