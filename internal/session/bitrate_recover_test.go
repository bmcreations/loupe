package session

import (
	"testing"
	"time"
)

var (
	healthyLink = BitrateSample{Frames: 60, Bytes: 100_000, RTT: 30 * time.Millisecond, BaseRTT: 25 * time.Millisecond}
	slowLink    = BitrateSample{Frames: 60, Shed: 15, Bytes: 31_250, RTT: 400 * time.Millisecond, BaseRTT: 25 * time.Millisecond}
	// The cascade: the link is fine again but the viewer still awaits a key.
	awaitOnly = BitrateSample{Frames: 60, AwaitKey: 58, Bytes: 5_000, RTT: 30 * time.Millisecond, BaseRTT: 25 * time.Millisecond}
)

// Keyframe waits on a link whose current rtt is back near baseline are the
// backlog draining, not a reason to cut the encoder.
func TestAwaitKeyOnAHealthyLinkIsNotADownSignal(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	got, _ := feed(a, time.Now(), 40, awaitOnly)
	if got != 0 {
		t.Fatalf("await-only window on a healthy link changed the bitrate to %d", got)
	}
}

// With no rtt reading the starved gate still fires, as it did before.
func TestAwaitKeyWithoutRTTStillStarves(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	s := awaitOnly
	s.RTT, s.BaseRTT = 0, 0
	got, _ := feed(a, time.Now(), 12, s)
	if got == 0 {
		t.Fatal("starved link with unknown rtt was left alone")
	}
}

// Down on a slow link, then a short healthy stretch brings it back well
// before upWindow, even when the change cap was already spent.
func TestFastRecoveryStepsUpWithoutWaitingForUpWindow(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	a.changes = maxAdaptiveChanges // cap spent by earlier episodes
	a.current = kbps(500)
	now := time.Now()
	// Anchor the window.
	a.Observe(now, healthyLink)
	got, end := feed(a, now, int(recoverWindow/time.Second)+2, healthyLink)
	if got != kbps(750) {
		t.Fatalf("fast recovery chose %d bps, want 750000", got)
	}
	if end.Sub(now) >= upWindow {
		t.Fatal("test ran past upWindow; it proves nothing")
	}
	if a.changes != maxAdaptiveChanges {
		t.Fatalf("recovery step counted against the cap: %d", a.changes)
	}
}

// One bad interval restarts the healthy stretch.
func TestFastRecoveryNeedsAnUnbrokenStretch(t *testing.T) {
	a := NewLiveBitrateAdapter(kbps(3000))
	a.current = kbps(500)
	now := time.Now()
	a.Observe(now, healthyLink)
	got, now := feed(a, now, 4, healthyLink)
	if got != 0 {
		t.Fatalf("stepped up after only 4s healthy: %d", got)
	}
	bad := healthyLink
	bad.RTT = 300 * time.Millisecond // smoothed rtt could still read fine; current does not
	_, now = feed(a, now, 1, bad)
	got, _ = feed(a, now, int(liveRecoverWindow/time.Second)-1, healthyLink)
	if got != 0 {
		t.Fatalf("stretch not restarted by a slow interval: %d", got)
	}
	got, _ = feed(a, now.Add(4*time.Second), 3, healthyLink)
	if got == 0 {
		t.Fatal("no step after a full healthy stretch")
	}
}

// A step down soon after a fast step up switches fast recovery off, so a link
// that cannot hold the rate does not hunt.
func TestFastRecoveryBacksOffAfterAWrongStep(t *testing.T) {
	a := NewLiveBitrateAdapter(kbps(3000))
	a.current = kbps(500)
	now := time.Now()
	a.Observe(now, healthyLink)
	got, now := feed(a, now, int(liveRecoverWindow/time.Second)+1, healthyLink)
	if got == 0 {
		t.Fatal("expected a recovery step")
	}
	got, now = feed(a, now, 12, slowLink)
	if got == 0 {
		t.Fatal("expected the slow link to bring the rate down again")
	}
	got, _ = feed(a, now, 10, healthyLink) // under liveUpWindow, so only fast recovery could fire
	if got != 0 {
		t.Fatalf("fast recovery fired again inside recoverHold: %d", got)
	}
}
