package session

import (
	"testing"
	"time"
)

// testPacer returns a pacer whose clock is advanced by hand, so slot rotation
// can be exercised without sleeping through it.
func testPacer() (*Pacer, func(time.Duration)) {
	now := time.Unix(0, 0)
	p := NewPacer()
	p.now = func() time.Time { return now }
	return p, func(d time.Duration) { now = now.Add(d) }
}

// observeFor feeds one sample per 100ms of simulated time for d, which is the
// shape the viewer loop produces: many samples spread across wall clock.
func observeFor(p *Pacer, advance func(time.Duration), d, rtt time.Duration) {
	for elapsed := time.Duration(0); elapsed < d; elapsed += 100 * time.Millisecond {
		p.Observe(rtt)
		advance(100 * time.Millisecond)
	}
}

// admitRate runs n frames through the pacer and returns the fraction admitted.
func admitRate(p *Pacer, n int) float64 {
	sent := 0
	for i := 0; i < n; i++ {
		if p.Admit() {
			sent++
		}
	}
	return float64(sent) / float64(n)
}

func TestPacerOpenWhenLatencyIsFine(t *testing.T) {
	p := NewPacer()
	for i := 0; i < 100; i++ {
		p.Observe(50 * time.Millisecond)
	}
	if got := p.Keep(); got != 1 {
		t.Errorf("keep = %v on a healthy link, want 1", got)
	}
	if rate := admitRate(p, 1000); rate < 0.99 {
		t.Errorf("admitted %.2f of frames on a healthy link, want ~1", rate)
	}
}

func TestPacerBacksOffUnderLatency(t *testing.T) {
	p := NewPacer()
	p.Observe(20 * time.Millisecond) // a fast baseline
	// The condition that defeated the socket-level check: seconds of latency
	// while the local socket reports healthy.
	for i := 0; i < 60; i++ {
		p.Observe(3 * time.Second)
	}
	if p.Keep() > 0.3 {
		t.Errorf("keep = %v after sustained 3s latency, expected heavy backoff", p.Keep())
	}
	if rate := admitRate(p, 1000); rate > 0.35 {
		t.Errorf("still admitting %.2f of frames under a 3s backlog", rate)
	}
}

func TestPacerBacksOffFasterWhenWorse(t *testing.T) {
	mild, severe := NewPacer(), NewPacer()
	mild.Observe(20 * time.Millisecond)
	severe.Observe(20 * time.Millisecond)
	for i := 0; i < 20; i++ {
		mild.Observe(200 * time.Millisecond)
		severe.Observe(5 * time.Second)
	}
	if severe.Keep() >= mild.Keep() {
		t.Errorf("severe backlog kept %v, mild kept %v; backoff should scale with excess",
			severe.Keep(), mild.Keep())
	}
}

func TestPacerRecoversSlowly(t *testing.T) {
	p := NewPacer()
	p.Observe(20 * time.Millisecond)
	for i := 0; i < 60; i++ {
		p.Observe(3 * time.Second)
	}
	collapsed := p.Keep()

	// One good sample must not undo the backoff; that is what re-fills the
	// buffers that caused the backlog.
	p.Observe(50 * time.Millisecond)
	if p.Keep() > collapsed+recoverStep*1.5 {
		t.Errorf("recovered too fast: %v -> %v on a single good sample", collapsed, p.Keep())
	}

	for i := 0; i < 200; i++ {
		p.Observe(50 * time.Millisecond)
	}
	if p.Keep() != 1 {
		t.Errorf("keep = %v after sustained recovery, want 1", p.Keep())
	}
}

func TestPacerHoldsSteadyInTheHysteresisBand(t *testing.T) {
	p := NewPacer()
	// Establish a near-zero baseline, then congest.
	p.Observe(10 * time.Millisecond)
	for i := 0; i < 40; i++ {
		p.Observe(3 * time.Second)
	}
	before := p.Keep()
	// Between recoverExcess and targetExcess the controller should do nothing,
	// or it oscillates between flooding and starving.
	for i := 0; i < 50; i++ {
		p.Observe(10*time.Millisecond + 100*time.Millisecond)
	}
	if p.Keep() != before {
		t.Errorf("keep moved from %v to %v inside the hysteresis band", before, p.Keep())
	}
}

// This is the failure seen in production: a relayed path whose baseline round
// trip already exceeds any fixed threshold. With absolute targets the pacer
// read the path's own propagation delay as congestion it had caused, clamped to
// its floor, and starved the stream to a couple of frames per second while
// reporting that latency was under control.
func TestPacerDoesNotCollapseOnASlowButHealthyPath(t *testing.T) {
	p := NewPacer()
	// A relay with a 260ms baseline and no queuing of our making.
	for i := 0; i < 400; i++ {
		p.Observe(260 * time.Millisecond)
	}
	if p.Keep() != 1 {
		t.Errorf("keep = %v on a high-latency but uncongested path, want 1", p.Keep())
	}
	if rate := admitRate(p, 1000); rate < 0.99 {
		t.Errorf("admitted only %.2f of frames on an uncongested relay", rate)
	}
}

func TestPacerShedsOnQueuingAboveASlowBaseline(t *testing.T) {
	p := NewPacer()
	for i := 0; i < 50; i++ {
		p.Observe(260 * time.Millisecond) // establish the baseline
	}
	// Now add a real backlog on top of that baseline.
	for i := 0; i < 60; i++ {
		p.Observe(2 * time.Second)
	}
	if p.Keep() > 0.4 {
		t.Errorf("keep = %v with 1.7s of queuing above baseline, expected backoff", p.Keep())
	}
}

func TestPacerRecoversOnASlowPath(t *testing.T) {
	p := NewPacer()
	for i := 0; i < 50; i++ {
		p.Observe(260 * time.Millisecond)
	}
	for i := 0; i < 60; i++ {
		p.Observe(2 * time.Second)
	}
	collapsed := p.Keep()
	// Backlog clears; latency returns to the path's own floor. The controller
	// must reopen, which the absolute-threshold version could never do.
	for i := 0; i < 300; i++ {
		p.Observe(265 * time.Millisecond)
	}
	if p.Keep() <= collapsed {
		t.Errorf("keep stuck at %v after the backlog cleared on a slow path", p.Keep())
	}
	if p.Keep() != 1 {
		t.Errorf("keep = %v after sustained recovery, want 1", p.Keep())
	}
}

func TestPacerLearnsAWorseBaseline(t *testing.T) {
	p, advance := testPacer()
	observeFor(p, advance, 2*time.Second, 40*time.Millisecond)
	// The path genuinely degrades and stays there. The baseline must follow,
	// or the controller chases a floor it can no longer reach and starves.
	observeFor(p, advance, baseSlot*baseSlots+time.Second, 400*time.Millisecond)
	if p.Base() < 300*time.Millisecond {
		t.Errorf("base = %v, did not adopt the worse path", p.Base())
	}
	observeFor(p, advance, 3*time.Second, 400*time.Millisecond)
	if p.Keep() != 1 {
		t.Errorf("keep = %v; a stably slow path is not congestion", p.Keep())
	}
}

// A phone that moves from wifi onto cellular keeps the same session. The
// baseline it learned on wifi describes a path that no longer exists, and while
// it survives, every cellular sample reads as this server's own queuing.
//
// The 07:36 trace is this exact shape: base pinned at 7ms from wifi, client rtt
// 500-2000ms on 5G, `keep` clamped at its 0.1 floor for thirteen seconds, with
// no route back because recovery wanted rtt under 67ms. The old baseline was
// counted in samples, not seconds, so at the 29fps that link managed it had not
// expired by the time the viewer gave up and reconnected.
func TestPacerRelearnsTheBaselineAcrossAHandover(t *testing.T) {
	p, advance := testPacer()
	observeFor(p, advance, 3*time.Second, 8*time.Millisecond)
	if p.Base() > 10*time.Millisecond {
		t.Fatalf("base = %v before the handover, want the wifi floor", p.Base())
	}

	// Onto cellular: a real floor of 230ms, no queuing above it.
	observeFor(p, advance, baseSlot*baseSlots+time.Second, 230*time.Millisecond)

	if p.Base() < 200*time.Millisecond {
		t.Errorf("base = %v, still describing the path the phone left", p.Base())
	}
	observeFor(p, advance, 3*time.Second, 230*time.Millisecond)
	if p.Keep() != 1 {
		t.Errorf("keep = %v; a 230ms path with no queuing is healthy, not congested", p.Keep())
	}
}

// The mirror of the handover case: a burst of queuing must not be mistaken for
// a new floor, or the pacer stops seeing the congestion it is there to relieve.
func TestPacerBaselineSurvivesABurstOfQueuing(t *testing.T) {
	p, advance := testPacer()
	observeFor(p, advance, 3*time.Second, 40*time.Millisecond)
	observeFor(p, advance, 2*time.Second, 900*time.Millisecond)

	if p.Base() > 100*time.Millisecond {
		t.Errorf("base = %v; two seconds of queuing is not a new path", p.Base())
	}
	if p.Keep() > 0.5 {
		t.Errorf("keep = %v; 900ms over a 40ms floor should have shed hard", p.Keep())
	}
}

func TestPacerNeverFullyStarves(t *testing.T) {
	p := NewPacer()
	p.Observe(20 * time.Millisecond)
	for i := 0; i < 5000; i++ {
		p.Observe(30 * time.Second)
	}
	if p.Keep() < minKeep {
		t.Errorf("keep = %v, below the floor %v", p.Keep(), minKeep)
	}
	if rate := admitRate(p, 1000); rate < 0.05 {
		t.Errorf("admitted %.3f of frames; the stream should never go fully silent", rate)
	}
}

func TestPacerIgnoresMissingMeasurement(t *testing.T) {
	p := NewPacer()
	p.Observe(20 * time.Millisecond)
	for i := 0; i < 30; i++ {
		p.Observe(3 * time.Second)
	}
	before := p.Keep()
	p.Observe(0) // client has not reported yet
	if p.Keep() != before {
		t.Errorf("a missing measurement changed keep from %v to %v", before, p.Keep())
	}
}
