package session

import (
	"testing"
	"time"
)

// feed runs the controller at 100 ms ticks for d, with the encoder producing
// prodBps and v as the worst viewer, and returns the cap after each tick.
type rateRun struct {
	r     *remoteRate
	now   time.Time
	bytes uint64
}

func newRateRun() *rateRun {
	return &rateRun{r: newRemoteRate(), now: time.Unix(1000, 0)}
}

func (x *rateRun) run(d time.Duration, v rateViewer, prodBps float64) (caps []int, reasons []string) {
	for end := x.now.Add(d); x.now.Before(end); {
		x.now = x.now.Add(100 * time.Millisecond)
		x.bytes += uint64(prodBps / 8 / 10)
		c, why := x.r.Tick(x.now, []rateViewer{v}, x.bytes)
		caps = append(caps, c)
		reasons = append(reasons, why)
	}
	return
}

func minInt(xs []int) int {
	m := xs[0]
	for _, x := range xs {
		if x < m {
			m = x
		}
	}
	return m
}

// The 11:59 cellular trace: cap 4 Mbps, the phone receiving 2-2.8 Mbps, the
// queue growing from 375 ms to 2 s. The cap must fall below 2.5 Mbps within 2 s
// of congestion and never below the floor.
func TestRemoteRateFollowsCellularCollapse(t *testing.T) {
	x := newRateRun()
	good := rateViewer{QueueMs: 5, OldestMs: 100, BaseMs: 120, DeliveredBps: 3_500_000}
	x.run(2*time.Second, good, 3_800_000)
	if got := x.r.Cap(); got < DefaultRemoteCapBps {
		t.Fatalf("clean link lowered the cap to %d", got)
	}
	// Slow start has pushed the cap up; pin it to the traced 4 Mbps.
	x.r.cap = 4_000_000

	bad := []rateViewer{
		{QueueMs: 375, OldestMs: 500, BaseMs: 120, DeliveredBps: 2_800_000},
		{QueueMs: 900, OldestMs: 1000, BaseMs: 120, DeliveredBps: 2_400_000},
		{QueueMs: 1500, OldestMs: 1400, BaseMs: 120, DeliveredBps: 2_000_000},
		{QueueMs: 2000, OldestMs: 1500, BaseMs: 120, DeliveredBps: 2_000_000},
	}
	var caps []int
	reasons := map[string]bool{}
	for _, v := range bad {
		c, why := x.run(500*time.Millisecond, v, 2_500_000)
		caps = append(caps, c...)
		for _, w := range why {
			reasons[w] = true
		}
	}
	// caps covers 2 s of congestion; the cap is under 2.5 Mbps by the end.
	if last := caps[len(caps)-1]; last >= 2_500_000 {
		t.Errorf("cap after 2 s of congestion = %d, want < 2.5 Mbps", last)
	}
	if m := minInt(caps); m < remoteCapMin {
		t.Errorf("cap fell to %d, below the floor", m)
	}
	if !reasons[rateCongested] {
		t.Errorf("no congested decision: %v", reasons)
	}
}

// The first cut after the evidence's first congested sample already lands
// near delivery.
func TestRemoteRateCutsTowardDelivery(t *testing.T) {
	x := newRateRun()
	x.r.cap, x.r.slowStart = 4_000_000, false
	caps, why := x.run(300*time.Millisecond, rateViewer{QueueMs: 375, OldestMs: 500, BaseMs: 120, DeliveredBps: 2_400_000}, 3_000_000)
	if caps[0] != 2_040_000 || why[0] != rateCongested {
		t.Errorf("first cut = %d %s, want 2040000 congested", caps[0], why[0])
	}
}

// Congestion with a tiny delivery rate (one big frame on a quiet screen) cuts
// by 15% but does not chase the delivery rate down.
func TestRemoteRateIgnoresDeliveryOfIdleLink(t *testing.T) {
	x := newRateRun()
	x.r.slowStart = false
	caps, _ := x.run(100*time.Millisecond, rateViewer{QueueMs: 80, OldestMs: 200, BaseMs: 50, DeliveredBps: 100_000}, 100_000)
	if caps[0] != 3_400_000 {
		t.Errorf("cap = %d, want 3400000", caps[0])
	}
}

// A still screen produces next to nothing, so the cap stays where it is.
func TestRemoteRateIdleScreenDoesNotRaise(t *testing.T) {
	x := newRateRun()
	clean := rateViewer{QueueMs: 2, OldestMs: 30, BaseMs: 40, DeliveredBps: 50_000}
	caps, why := x.run(20*time.Second, clean, 50_000)
	if m := minInt(caps); m != DefaultRemoteCapBps || caps[len(caps)-1] != DefaultRemoteCapBps {
		t.Errorf("idle screen moved the cap: %d..%d", m, caps[len(caps)-1])
	}
	if why[len(why)-1] != rateIdle {
		t.Errorf("reason = %s, want idle", why[len(why)-1])
	}
}

// A busy, clean link ramps to the ceiling; after a cut it recovers to 90% of
// the ceiling, then creeps.
func TestRemoteRateCleanLinkRampsUp(t *testing.T) {
	x := newRateRun()
	clean := rateViewer{QueueMs: 2, OldestMs: 40, BaseMs: 40, DeliveredBps: 6_000_000}
	// Content fills whatever the cap is.
	for i := 0; i < 100; i++ {
		x.run(100*time.Millisecond, clean, float64(x.r.Cap()))
	}
	if got := x.r.Cap(); got != remoteCapMax {
		t.Fatalf("clean busy link: cap %d, want %d", got, remoteCapMax)
	}

	// Congestion at 3 Mbps delivery sets a ceiling and cuts.
	x.run(300*time.Millisecond, rateViewer{QueueMs: 100, OldestMs: 300, BaseMs: 40, DeliveredBps: 5_000_000}, 8_000_000)
	cut := x.r.Cap()
	if cut >= remoteCapMax {
		t.Fatalf("no cut: %d", cut)
	}
	sawRecover := false
	for i := 0; i < 100; i++ {
		_, why := x.run(100*time.Millisecond, clean, float64(x.r.Cap()))
		sawRecover = sawRecover || why[0] == rateRecover
	}
	if !sawRecover || x.r.Cap() <= cut {
		t.Errorf("cap did not recover: %d -> %d", cut, x.r.Cap())
	}
	if x.r.Cap() < int(5_000_000*0.9) {
		t.Errorf("cap %d below 90%% of the ceiling", x.r.Cap())
	}
}

// With no viewer reading acks the controller changes nothing.
func TestRemoteRateNoViewersIdle(t *testing.T) {
	r := newRemoteRate()
	if c, why := r.Tick(time.Unix(1, 0), nil, 0); c != DefaultRemoteCapBps || why != rateIdle {
		t.Errorf("got %d %s", c, why)
	}
}

// The worst viewer is the congested one with the least delivery.
func TestWorstRateViewer(t *testing.T) {
	ok := rateViewer{QueueMs: 5, OldestMs: 50, BaseMs: 40, DeliveredBps: 9e6}
	slow := rateViewer{QueueMs: 400, OldestMs: 600, BaseMs: 40, DeliveredBps: 2e6}
	slower := rateViewer{QueueMs: 100, OldestMs: 300, BaseMs: 40, DeliveredBps: 1e6}
	if got := worstRateViewer([]rateViewer{ok, slow, slower}); got != slower {
		t.Errorf("got %+v", got)
	}
}

// The session sends 0x84 only when the cap changes, and only 0x84 to the
// remote encoder.
func TestSessionSendsCapOnChangeOnly(t *testing.T) {
	s, fr := remoteSession(t)
	id, _ := s.AddViewerWith(ViewerOptions{Acks: true})
	waitOps(t, fr, "key")
	s.mu.RLock()
	v := s.viewers[id]
	s.mu.RUnlock()
	now := time.Now()
	step := func() { now = now.Add(100 * time.Millisecond); s.remoteGateTick(now, 100*time.Millisecond) }

	// Steady and quiet: no cap op.
	for i := 0; i < 5; i++ {
		v.flow.Sent(uint64(i+1), 1000, now)
		v.flow.Ack(uint64(i+1), now.Add(10*time.Millisecond))
		step()
	}
	for _, op := range fr.opsNow() {
		if op == "cap" {
			t.Fatalf("cap sent on a quiet link: %v", fr.opsNow())
		}
	}
	// A 500 ms ack with a 10 ms base: congested, the cap is cut once.
	v.flow.Sent(100, 1000, now)
	v.flow.Ack(100, now.Add(500*time.Millisecond))
	for i := 0; i < 3; i++ {
		step()
	}
	n := 0
	for _, op := range fr.opsNow() {
		if op == "cap" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("cap ops = %d, want 1: %v", n, fr.opsNow())
	}
	if got := s.remote.capBps.Load(); got >= DefaultRemoteCapBps {
		t.Errorf("capBps = %d", got)
	}
}
