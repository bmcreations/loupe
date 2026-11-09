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
	if got := x.r.Cap(); got < remoteCapStart {
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
	x.r.cap, x.r.slowStart = 4_000_000, false
	caps, _ := x.run(100*time.Millisecond, rateViewer{QueueMs: 200, OldestMs: 400, BaseMs: 50, DeliveredBps: 100_000}, 100_000)
	if caps[0] != 3_400_000 {
		t.Errorf("cap = %d, want 3400000", caps[0])
	}
}

// The 12:09 cellular run, handset-ios on the remote track decoding 38-63 fps:
// per-second queue and oldest readings from jitter alone. None of them is
// congestion, so the cap must not fall.
func TestRemoteRateCellularJitterIsNotCongestion(t *testing.T) {
	x := newRateRun()
	x.r.cap, x.r.slowStart = 2_500_000, false
	jitter := []rateViewer{
		{QueueMs: 15.2, OldestMs: 90.1, BaseMs: 70.7, DeliveredBps: 1_695_000},
		{QueueMs: 17, OldestMs: 92.5, BaseMs: 70.7, DeliveredBps: 1_314_000},
		{QueueMs: 31, OldestMs: 75.8, BaseMs: 70.7, DeliveredBps: 1_524_000},
		{QueueMs: 65.9, OldestMs: 0, BaseMs: 70.7, DeliveredBps: 894_000},
		{QueueMs: 72, OldestMs: 106.7, BaseMs: 70.7, DeliveredBps: 1_419_000},
		{QueueMs: 58.4, OldestMs: 60.2, BaseMs: 60.1, DeliveredBps: 1_598_000},
	}
	for _, v := range jitter {
		caps, why := x.run(time.Second, v, 1_500_000)
		for i, w := range why {
			if w == rateCongested {
				t.Fatalf("%+v read as congestion, cap %d", v, caps[i])
			}
		}
	}
	if got := x.r.Cap(); got < 2_500_000 {
		t.Errorf("cap fell to %d on jitter", got)
	}
}

// The 12:25 cellular run: the phone receiving about 1 Mbps with frames
// queued 150-430 ms. The cap must go under what the link carries.
func TestRemoteRateFallsBelowAThinLink(t *testing.T) {
	x := newRateRun()
	x.r.cap, x.r.slowStart = 1_500_000, false
	thin := []rateViewer{
		{QueueMs: 181.9, OldestMs: 259.3, BaseMs: 71.8, DeliveredBps: 897_000},
		{QueueMs: 165.7, OldestMs: 104.4, BaseMs: 71.8, DeliveredBps: 1_304_000},
		{QueueMs: 217, OldestMs: 314.4, BaseMs: 71.8, DeliveredBps: 1_257_000},
		{QueueMs: 349.3, OldestMs: 0, BaseMs: 71.8, DeliveredBps: 1_084_000},
	}
	for _, v := range thin {
		x.run(time.Second, v, float64(x.r.Cap()))
	}
	if got := x.r.Cap(); got >= 1_000_000 {
		t.Errorf("cap = %d after 4 s queued on a ~1 Mbps link, want < 1 Mbps", got)
	}
	if got := x.r.Cap(); got < remoteCapMin {
		t.Errorf("cap %d below the floor", got)
	}
}

// A still screen produces next to nothing, so the cap stays where it is.
func TestRemoteRateIdleScreenDoesNotRaise(t *testing.T) {
	x := newRateRun()
	clean := rateViewer{QueueMs: 2, OldestMs: 30, BaseMs: 40, DeliveredBps: 50_000}
	caps, why := x.run(20*time.Second, clean, 50_000)
	if m := minInt(caps); m != remoteCapStart || caps[len(caps)-1] != remoteCapStart {
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
	if c, why := r.Tick(time.Unix(1, 0), nil, 0); c != remoteCapStart || why != rateIdle {
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

	caps := func() (n int) {
		for _, op := range fr.opsNow() {
			if op == "cap" {
				n++
			}
		}
		return
	}
	// The encoder starts at DefaultRemoteCapBps; the first tick moves it to
	// the controller's start, once.
	v.flow.Sent(1, 1000, now)
	v.flow.Ack(1, now.Add(10*time.Millisecond))
	step()
	if n := caps(); n != 1 {
		t.Fatalf("start cap ops = %d, want 1: %v", n, fr.opsNow())
	}
	// Steady and quiet: no further cap op.
	for i := 1; i < 5; i++ {
		v.flow.Sent(uint64(i+1), 1000, now)
		v.flow.Ack(uint64(i+1), now.Add(10*time.Millisecond))
		step()
	}
	if n := caps(); n != 1 {
		t.Fatalf("cap sent on a quiet link: %v", fr.opsNow())
	}
	// A 500 ms ack with a 10 ms base: congested, the cap is cut once.
	v.flow.Sent(100, 1000, now)
	v.flow.Ack(100, now.Add(500*time.Millisecond))
	for i := 0; i < 3; i++ {
		step()
	}
	if n := caps(); n != 2 {
		t.Fatalf("cap ops = %d, want 2 (start, cut): %v", n, fr.opsNow())
	}
	if got := s.remote.capBps.Load(); got >= remoteCapStart {
		t.Errorf("capBps = %d", got)
	}
}
