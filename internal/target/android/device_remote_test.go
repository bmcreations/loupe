package android

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"

	"github.com/bmcreations/loupe/internal/scrcpy"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// fakeSecond stands in for a second scrcpy session.
type fakeSecond struct {
	launch remoteLaunch
	frames chan scrcpy.Frame

	mu     sync.Mutex
	ctl    [][]byte
	closed bool
}

func (f *fakeSecond) Frames() <-chan scrcpy.Frame { return f.frames }

func (f *fakeSecond) SendControl(msg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctl = append(f.ctl, append([]byte(nil), msg...))
	return nil
}

func (f *fakeSecond) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.frames)
	}
	return nil
}

func (f *fakeSecond) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeSecond) controls() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.ctl...)
}

func (f *fakeSecond) emit(fr scrcpy.Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.frames <- fr
	}
}

// fakeLaunches records every launch the track makes.
type fakeLaunches struct {
	mu       sync.Mutex
	sessions []*fakeSecond
	fail     bool
	tries    int
}

func (l *fakeLaunches) launch(_ context.Context, rl remoteLaunch) (target.RemoteSession, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tries++
	if l.fail {
		return nil, errors.New("no device")
	}
	s := &fakeSecond{launch: rl, frames: make(chan scrcpy.Frame, 64)}
	l.sessions = append(l.sessions, s)
	return s, nil
}

func (l *fakeLaunches) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sessions)
}

func (l *fakeLaunches) attempts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tries
}

func (l *fakeLaunches) setFail(fail bool) {
	l.mu.Lock()
	l.fail = fail
	l.mu.Unlock()
}

func (l *fakeLaunches) session(t *testing.T, i int) *fakeSecond {
	t.Helper()
	eventually(t, "launch", func() bool { return l.count() > i })
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessions[i]
}

// eventually waits on goroutines the track starts: launches and readers.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *adbRemote) locked(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn()
}

func (r *adbRemote) launchIdle() bool {
	idle := false
	r.locked(func() { idle = !r.launching })
	return idle
}

func (r *adbRemote) isCur(s *fakeSecond) bool {
	ok := false
	r.locked(func() { ok = r.cur != nil && r.cur.sess == s })
	return ok
}

func (r *adbRemote) isNext(s *fakeSecond) bool {
	ok := false
	r.locked(func() { ok = r.next != nil && r.next.sess == s })
	return ok
}

func recvFrame(t *testing.T, ch <-chan target.Frame) target.Frame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("remote track closed")
		}
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame on the remote track")
	}
	return target.Frame{}
}

func noFrame(t *testing.T, ch <-chan target.Frame) {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("remote track closed")
		}
		t.Fatalf("unexpected frame %+v", f)
	case <-time.After(20 * time.Millisecond):
	}
}

var (
	cfgData   = []byte{0, 0, 0, 1, 0x67}
	keyData   = []byte{0, 0, 0, 1, 0x65}
	deltaData = []byte{0, 0, 0, 1, 0x41}
)

func cfgFrame(at time.Time) scrcpy.Frame {
	return scrcpy.Frame{Config: true, Data: cfgData, Captured: at}
}

func keyFrame(pts time.Duration, at time.Time) scrcpy.Frame {
	return scrcpy.Frame{PTS: pts, Key: true, Data: keyData, Captured: at}
}

func deltaFrame(pts time.Duration, at time.Time) scrcpy.Frame {
	return scrcpy.Frame{PTS: pts, Data: deltaData, Captured: at}
}

// running opens the gate and brings the first session up to its keyframe.
func running(t *testing.T, r *adbRemote, l *fakeLaunches, clk *fakeClock) *fakeSecond {
	t.Helper()
	r.gate(true)
	r.step(clk.Now())
	s := l.session(t, 0)
	eventually(t, "the first session to start", func() bool { return r.isNext(s) })
	s.emit(cfgFrame(clk.Now()))
	s.emit(keyFrame(0, clk.Now()))
	if f := recvFrame(t, r.out); !f.Config {
		t.Fatalf("first frame %+v, want the parameter set", f)
	}
	if f := recvFrame(t, r.out); !f.Key {
		t.Fatalf("second frame %+v, want the keyframe", f)
	}
	return s
}

// testMain is the main session the tests' remote tracks sit beside: the
// Seeker's.
var testMain = remoteLaunch{MaxSize: 1440, MaxFPS: 60, BitRate: 4_000_000}

func newTestRemote(l *fakeLaunches, clk *fakeClock, mainKeys *int) *adbRemote {
	var mu sync.Mutex
	return newADBRemoteAt(l.launch, testMain, func() error {
		mu.Lock()
		*mainKeys++
		mu.Unlock()
		return nil
	}, clk.Now)
}

// pickAt is one pick, failing the test when it does not return want.
func pickAt(t *testing.T, p *rungPicker, at time.Time, bps, carrying, want int, why string) {
	t.Helper()
	if got := p.pick(at, bps, carrying); got != want {
		t.Fatalf("%s: rung %d, want %d", why, got, want)
	}
}

func TestRungPickerStartsAtLaunchRungAndRaisesWhileItCarries(t *testing.T) {
	p := newRungPicker(adbRemoteRungs)
	at := time.Unix(0, 0)
	pickAt(t, p, at, 0, -1, adbRemoteLaunchRung, "no cap yet")
	// Nothing raises on a cap from before the launch rung carries the track.
	pickAt(t, p, at, 3_000_000, -1, 1, "first ask")
	pickAt(t, p, at.Add(5*time.Second), 3_000_000, -1, 1, "still launching")

	c1 := at.Add(5 * time.Second)
	pickAt(t, p, c1, 3_000_000, 1, 1, "the hold starts")
	pickAt(t, p, c1.Add(remoteRaiseHold-time.Millisecond), 3_000_000, 1, 1, "before the hold")
	pickAt(t, p, c1.Add(remoteRaiseHold), 3_000_000, 1, 2, "after the hold, one rung up")
	// The next raise waits for that rung to carry the track.
	pickAt(t, p, c1.Add(3*remoteRaiseHold), 3_000_000, 1, 2, "rung 2 not carrying yet")
	c2 := c1.Add(3 * remoteRaiseHold)
	pickAt(t, p, c2, 3_000_000, 2, 2, "rung 2 carries")
	pickAt(t, p, c2.Add(remoteRaiseHold), 3_000_000, 2, 3, "after rung 2's hold")

	// Past the regret window a cut goes straight to what the cap selects.
	c3 := c2.Add(remoteRaiseHold + remoteRaiseRegret)
	pickAt(t, p, c3, 3_000_000, 3, 3, "rung 3 carries")
	pickAt(t, p, c3.Add(time.Millisecond), 500_000, 3, 0, "a plain cut")
}

func TestRungPickerDipRestartsTheHold(t *testing.T) {
	p := newRungPicker(adbRemoteRungs)
	at := time.Unix(0, 0)
	pickAt(t, p, at, 1_300_000, 1, 1, "the hold starts")
	pickAt(t, p, at.Add(1500*time.Millisecond), 1_000_000, 1, 1, "a dip")
	pickAt(t, p, at.Add(1800*time.Millisecond), 1_300_000, 1, 1, "the hold restarts")
	pickAt(t, p, at.Add(3*time.Second), 1_300_000, 1, 1, "the hold counted from before the dip")
	pickAt(t, p, at.Add(1800*time.Millisecond+remoteRaiseHold), 1_300_000, 1, 2, "a full hold after the dip")
}

// The e2e5 run on the Seeker at 1 Mbps: a raise into 1.5 Mbps flooded the
// link and its backlog drove the cap to the floor. 720px runs at about the
// link's rate there and could not drain it; 480px drained it in two seconds.
func TestRungPickerRegretCutFollowsTheCapToTheFloor(t *testing.T) {
	p := newRungPicker(adbRemoteRungs)
	at := time.Unix(0, 0)
	pickAt(t, p, at, 1_300_000, 1, 1, "the hold starts")
	raised := at.Add(remoteRaiseHold)
	pickAt(t, p, raised, 1_300_000, 1, 2, "raised")
	c2 := raised.Add(600 * time.Millisecond)
	pickAt(t, p, c2, 1_300_000, 2, 2, "rung 2 carries")
	pickAt(t, p, c2.Add(time.Second), 671_666, 2, 1, "the cap falls")
	pickAt(t, p, c2.Add(1200*time.Millisecond), 300_000, 2, 0, "and the cut follows it to the floor while rung 1 starts")
	c0 := c2.Add(1800 * time.Millisecond)
	pickAt(t, p, c0, 300_000, 0, 0, "the floor carries")
	up := c0.Add(2 * time.Second)
	pickAt(t, p, up, 875_001, 0, 0, "the cap recovers")
	pickAt(t, p, up.Add(remoteRaiseHold), 875_001, 0, 1, "back on 720px after rung 1's own hold, which the regret did not charge")
}

func TestRungPickerRegretDoublesOnlyTheFailedRungsHold(t *testing.T) {
	p := newRungPicker(adbRemoteRungs)
	at := time.Unix(0, 0)
	pickAt(t, p, at, 1_300_000, 1, 1, "the hold starts")
	pickAt(t, p, at.Add(remoteRaiseHold), 1_300_000, 1, 2, "raised")
	c2 := at.Add(remoteRaiseHold + 600*time.Millisecond)
	pickAt(t, p, c2.Add(time.Second), 1_000_000, 2, 1, "regret cut")
	c1 := c2.Add(1600 * time.Millisecond)
	pickAt(t, p, c1, 1_300_000, 1, 1, "the cap recovers")
	pickAt(t, p, c1.Add(remoteRaiseHold), 1_300_000, 1, 1, "the hold into rung 2 doubled")
	pickAt(t, p, c1.Add(2*remoteRaiseHold), 1_300_000, 1, 2, "after the doubled hold")

	// The hold into rung 1, which carried, is unchanged.
	c2 = c1.Add(2*remoteRaiseHold + 600*time.Millisecond)
	cut := c2.Add(remoteRaiseRegret)
	pickAt(t, p, cut, 400_000, 2, 0, "a plain cut, past the regret window")
	c0 := cut.Add(600 * time.Millisecond)
	pickAt(t, p, c0, 700_000, 0, 0, "the hold starts")
	pickAt(t, p, c0.Add(remoteRaiseHold), 700_000, 0, 1, "rung 1's hold was not charged")
}

func TestRungPickerCutBeforeARaiseCarriesChargesNothing(t *testing.T) {
	p := newRungPicker(adbRemoteRungs)
	at := time.Unix(0, 0)
	pickAt(t, p, at, 1_300_000, 1, 1, "the hold starts")
	raised := at.Add(remoteRaiseHold)
	pickAt(t, p, raised, 1_300_000, 1, 2, "raised")
	pickAt(t, p, raised.Add(100*time.Millisecond), 1_000_000, 1, 1, "cut before rung 2 carried")
	pickAt(t, p, raised.Add(200*time.Millisecond), 1_300_000, 1, 1, "the hold starts again")
	pickAt(t, p, raised.Add(200*time.Millisecond+remoteRaiseHold), 1_300_000, 1, 2, "the hold into rung 2 was not doubled")
}

func TestRemoteRungsForTopWithTheMainSession(t *testing.T) {
	cases := []struct {
		main remoteLaunch
		top  remoteRung
		n    int
	}{
		{testMain, remoteRung{MaxSize: 1440, MaxFPS: 60, BitRate: 4_000_000, From: 3_200_000}, 5},
		{remoteLaunch{MaxSize: 1080, MaxFPS: 60, BitRate: 4_000_000}, remoteRung{MaxSize: 1080, MaxFPS: 60, BitRate: 4_000_000, From: 3_200_000}, 5},
		// A rung over the main session's size or rate is clamped to it.
		{remoteLaunch{MaxSize: 1000, MaxFPS: 30, BitRate: 2_000_000}, remoteRung{MaxSize: 1000, MaxFPS: 30, BitRate: 2_000_000, From: 1_600_000}, 4},
		// From stays reachable from the rung below.
		{remoteLaunch{MaxSize: 1440, MaxFPS: 60, BitRate: 8_000_000}, remoteRung{MaxSize: 1440, MaxFPS: 60, BitRate: 8_000_000, From: 4_800_000}, 5},
		// No bitrate asked for: the table as it is.
		{remoteLaunch{}, adbRemoteRungs[len(adbRemoteRungs)-1], len(adbRemoteRungs)},
		// A main session under the bottom rung still gets one.
		{remoteLaunch{MaxSize: 400, BitRate: 200_000}, remoteRung{MaxSize: 400, MaxFPS: 30, BitRate: 300_000}, 1},
	}
	for _, c := range cases {
		rungs := adbRemoteRungsFor(c.main)
		if len(rungs) != c.n || rungs[len(rungs)-1] != c.top {
			t.Errorf("main %+v: %d rungs topped by %+v, want %d topped by %+v", c.main, len(rungs), rungs[len(rungs)-1], c.n, c.top)
		}
		for i, g := range rungs {
			if c.main.MaxSize > 0 && g.MaxSize > c.main.MaxSize {
				t.Errorf("main %+v: rung %d is %dpx, over the main session", c.main, i, g.MaxSize)
			}
			if g.From > g.BitRate {
				t.Errorf("main %+v: rung %d is selected from %d, over its own rate %d: rate control raises only while the encoder fills half the cap", c.main, i, g.From, g.BitRate)
			}
			// Rate control raises while the encoder fills half its cap, so a
			// rung selected from over twice the rung below's rate is out of
			// reach. The bottom rung sits at exactly twice: it ran 0.36-0.47
			// Mbps for its 300k on the Seeker.
			if i > 0 && (g.From <= rungs[i-1].From || g.From > 2*rungs[i-1].BitRate) {
				t.Errorf("main %+v: rung %d From %d does not rise, or is out of reach of rung %d's %d bps", c.main, i, g.From, i-1, rungs[i-1].BitRate)
			}
		}
		lo, start, hi := remoteRateRange(rungs)
		if lo > rungs[0].BitRate || hi < rungs[len(rungs)-1].From {
			t.Errorf("main %+v: rate range %d-%d does not reach every rung", c.main, lo, hi)
		}
		if p := newRungPicker(rungs); p.rungFor(start) != p.cur {
			t.Errorf("main %+v: rate control starts at %d, which selects rung %d, not the launch rung %d", c.main, start, p.rungFor(start), p.cur)
		}
	}
}

func TestADBRemoteGateStartsOneSessionAndStopsAfterLinger(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{}
	var mainKeys int
	r := newTestRemote(l, clk, &mainKeys)
	defer r.close()

	r.step(clk.Now())
	if l.attempts() != 0 {
		t.Fatal("launched with the gate held")
	}
	s := running(t, r, l, clk)
	if s.launch != (remoteLaunch{MaxSize: 720, MaxFPS: 30, BitRate: 750_000}) {
		t.Fatalf("first launch %+v, want the launch rung", s.launch)
	}
	eventually(t, "the switch to the first session", func() bool { return r.isCur(s) })
	for range 5 {
		r.gate(true)
		r.step(clk.Add(100 * time.Millisecond))
	}
	if n := l.attempts(); n != 1 {
		t.Fatalf("%d launches with the gate open, want 1", n)
	}

	r.gate(false)
	r.step(clk.Add(remoteLinger - time.Second))
	if s.isClosed() {
		t.Fatal("stopped before the linger")
	}
	r.step(clk.Add(2 * time.Second))
	if !s.isClosed() {
		t.Fatal("still running after the linger")
	}
	select {
	case _, ok := <-r.out:
		if !ok {
			t.Fatal("Remote() closed with the second session")
		}
	default:
	}

	// A viewer that comes back gets a new session, at the launch rung.
	r.setCap(3_000_000)
	r.gate(true)
	r.step(clk.Add(time.Second))
	s2 := l.session(t, 1)
	if s2.launch.BitRate != 750_000 {
		t.Fatalf("relaunch at %d bps, want the launch rung", s2.launch.BitRate)
	}
	if mainKeys != 0 {
		t.Fatalf("the main session was reset %d times", mainKeys)
	}
}

func TestADBRemoteGateReopenedInLingerKeepsSession(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{}
	var mainKeys int
	r := newTestRemote(l, clk, &mainKeys)
	defer r.close()
	s := running(t, r, l, clk)
	r.gate(false)
	r.step(clk.Add(2 * time.Second))
	r.gate(true)
	r.step(clk.Add(10 * time.Second))
	if s.isClosed() || l.attempts() != 1 {
		t.Fatalf("closed %v after %d launches, want the first session kept", s.isClosed(), l.attempts())
	}
}

func TestADBRemoteKeyframeResetsOnlyTheSecondSession(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{}
	var mainKeys int
	r := newTestRemote(l, clk, &mainKeys)
	defer r.close()

	// Nothing running: a launch leads with a keyframe anyway.
	r.keyframe()
	if mainKeys != 0 {
		t.Fatal("an ask with nothing running reset the main session")
	}

	s := running(t, r, l, clk)
	eventually(t, "the switch", func() bool { return r.isCur(s) })
	reset := []byte{17}
	r.keyframe()
	eventually(t, "the reset", func() bool { return len(s.controls()) == 1 })
	if c := s.controls()[0]; !bytes.Equal(c, reset) {
		t.Fatalf("control % x, want RESET_VIDEO % x", c, reset)
	}

	// Further asks while it is outstanding are the same ask.
	r.keyframe()
	r.keyframe()
	time.Sleep(10 * time.Millisecond)
	if n := len(s.controls()); n != 1 {
		t.Fatalf("%d resets for one outstanding ask", n)
	}

	// A keyframe answers it, and the next ask resets again.
	s.emit(cfgFrame(clk.Now()))
	s.emit(keyFrame(time.Second, clk.Now()))
	recvFrame(t, r.out)
	recvFrame(t, r.out)
	r.keyframe()
	eventually(t, "the second reset", func() bool { return len(s.controls()) == 2 })

	// An unanswered ask expires.
	clk.Add(remoteKeyWait)
	r.keyframe()
	eventually(t, "the third reset", func() bool { return len(s.controls()) == 3 })
	if mainKeys != 0 {
		t.Fatalf("the main session was reset %d times", mainKeys)
	}
}

func TestADBRemoteRungChangeIsMakeBeforeBreak(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{}
	var mainKeys int
	r := newTestRemote(l, clk, &mainKeys)
	defer r.close()
	old := running(t, r, l, clk)
	eventually(t, "the switch", func() bool { return r.isCur(old) })
	old.emit(deltaFrame(100*time.Millisecond, clk.Now()))
	if f := recvFrame(t, r.out); f.PTS != 100*time.Millisecond {
		t.Fatalf("PTS %v", f.PTS)
	}

	r.setCap(1_300_000)
	r.step(clk.Add(time.Second))
	r.step(clk.Add(remoteRaiseHold))
	nu := l.session(t, 1)
	if nu.launch != (remoteLaunch{MaxSize: 1080, MaxFPS: 30, BitRate: 1_500_000}) {
		t.Fatalf("relaunch %+v, want one rung up", nu.launch)
	}
	eventually(t, "the replacement", func() bool { return r.isNext(nu) })

	// The old session carries the track until the new one has a picture.
	old.emit(deltaFrame(200*time.Millisecond, clk.Now()))
	if f := recvFrame(t, r.out); f.PTS != 200*time.Millisecond || old.isClosed() {
		t.Fatalf("PTS %v closed %v: the old session stopped before the new one started", f.PTS, old.isClosed())
	}
	nu.emit(deltaFrame(time.Hour, clk.Now())) // nothing before its parameter set
	at := clk.Add(50 * time.Millisecond)
	nu.emit(cfgFrame(at))
	nu.emit(keyFrame(0, at))
	if f := recvFrame(t, r.out); !f.Config {
		t.Fatalf("frame %+v, want the new parameter set", f)
	}
	if !r.isCur(nu) {
		t.Fatal("the replacement is not carrying the track after its parameter set")
	}
	k := recvFrame(t, r.out)
	if !k.Key || k.PTS <= 200*time.Millisecond {
		t.Fatalf("new keyframe %+v: PTS must continue the track's timeline", k)
	}
	eventually(t, "the old session to close", old.isClosed)
	old.emit(deltaFrame(300*time.Millisecond, clk.Now()))
	nu.emit(deltaFrame(33*time.Millisecond, at.Add(33*time.Millisecond)))
	if f := recvFrame(t, r.out); f.PTS != k.PTS+33*time.Millisecond {
		t.Fatalf("PTS %v, want %v", f.PTS, k.PTS+33*time.Millisecond)
	}
	if n := l.attempts(); n != 2 {
		t.Fatalf("%d launches, want 2", n)
	}
}

func TestADBRemoteFallsBackToMainAfterFailedLaunches(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{fail: true}
	var mainKeys int
	var mu sync.Mutex
	r := newADBRemoteAt(l.launch, testMain, func() error {
		mu.Lock()
		mainKeys++
		mu.Unlock()
		return nil
	}, clk.Now)
	defer r.close()
	keys := func() int { mu.Lock(); defer mu.Unlock(); return mainKeys }

	r.gate(true)
	for i := 1; i <= remoteFailLimit; i++ {
		r.step(clk.Add(remoteBackoffMax))
		eventually(t, "a failed launch", func() bool { return l.attempts() == i && r.launchIdle() })
	}
	fallback := false
	r.locked(func() { fallback = r.fallback })
	if !fallback {
		t.Fatalf("no fallback after %d failed launches", remoteFailLimit)
	}
	eventually(t, "a keyframe ask to the main session", func() bool { return keys() == 1 })

	// The main session's frames now carry the track, from a keyframe, with
	// its parameter set ahead of it.
	r.offerMain(target.Frame{Config: true, Data: cfgData, Captured: clk.Now()})
	r.offerMain(target.Frame{PTS: time.Second, Data: deltaData, Captured: clk.Now()})
	r.offerMain(target.Frame{PTS: 2 * time.Second, Key: true, Data: keyData, Captured: clk.Now()})
	if f := recvFrame(t, r.out); !f.Config {
		t.Fatalf("frame %+v, want the parameter set", f)
	}
	if f := recvFrame(t, r.out); !f.Key {
		t.Fatalf("frame %+v, want the keyframe, not the delta before it", f)
	}
	r.keyframe()
	eventually(t, "a keyframe ask to the main session", func() bool { return keys() == 2 })

	// It stays that way for the stream.
	l.setFail(false)
	r.step(clk.Add(time.Minute))
	time.Sleep(10 * time.Millisecond)
	if n := l.attempts(); n != remoteFailLimit {
		t.Fatalf("%d launches after the fallback", n)
	}
}

func TestADBRemoteFailedRelaunchKeepsRunningSession(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{}
	var mainKeys int
	r := newTestRemote(l, clk, &mainKeys)
	defer r.close()
	s := running(t, r, l, clk)
	eventually(t, "the switch", func() bool { return r.isCur(s) })

	l.setFail(true)
	r.setCap(1_300_000)
	r.step(clk.Add(time.Second))
	for i := 2; i <= remoteFailLimit+2; i++ {
		r.step(clk.Add(remoteBackoffMax + remoteRaiseHold))
		eventually(t, "a failed relaunch", func() bool { return l.attempts() == i && r.launchIdle() })
	}
	fallback := false
	r.locked(func() { fallback = r.fallback })
	if fallback || !r.isCur(s) || s.isClosed() {
		t.Fatalf("fallback %v cur %v closed %v: a failed relaunch dropped a working session", fallback, r.isCur(s), s.isClosed())
	}
	s.emit(deltaFrame(time.Second, clk.Now()))
	recvFrame(t, r.out)
	if mainKeys != 0 {
		t.Fatalf("the main session was reset %d times", mainKeys)
	}
}

func TestADBRemoteSessionThatSendsNothingIsReplaced(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{}
	var mainKeys int
	r := newTestRemote(l, clk, &mainKeys)
	defer r.close()
	r.gate(true)
	r.step(clk.Now())
	s := l.session(t, 0)
	eventually(t, "the launch", func() bool { return r.isNext(s) })
	r.step(clk.Add(remoteFirstFrame + time.Millisecond))
	if !s.isClosed() {
		t.Fatal("a session that never sent a parameter set was kept")
	}
	r.step(clk.Add(remoteBackoffMax))
	l.session(t, 1)
}

func TestADBRemoteLaunchNoLargerThanMainOrMaxSize(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{}
	r := newADBRemoteAt(l.launch, remoteLaunch{MaxSize: 400}, nil, clk.Now)
	defer r.close()
	r.gate(true)
	r.step(clk.Now())
	if s := l.session(t, 0); s.launch.MaxSize != 400 {
		t.Fatalf("max size %d, want the main session's 400", s.launch.MaxSize)
	}
	eventually(t, "the launch", func() bool { return r.launchIdle() })
	r.setMaxSize(360)
	r.gate(false)
	r.step(clk.Add(remoteLinger + time.Second))
	r.gate(true)
	r.step(clk.Add(time.Second))
	if s := l.session(t, 1); s.launch.MaxSize != 360 {
		t.Fatalf("max size %d, want RemoteMaxSize's 360", s.launch.MaxSize)
	}
}

func TestADBRemoteFullTrackResumesAtKeyframe(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{}
	var mainKeys int
	r := newTestRemote(l, clk, &mainKeys)
	defer r.close()
	s := running(t, r, l, clk)
	eventually(t, "the switch", func() bool { return r.isCur(s) })
	for i := range remoteOutLen + 1 {
		s.emit(deltaFrame(time.Duration(i+1)*time.Millisecond, clk.Now()))
	}
	eventually(t, "a reset for the overflow", func() bool { return len(s.controls()) == 1 })
	for range remoteOutLen {
		recvFrame(t, r.out)
	}
	s.emit(deltaFrame(time.Second, clk.Now()))
	noFrame(t, r.out)
	s.emit(keyFrame(2*time.Second, clk.Now()))
	if f := recvFrame(t, r.out); !f.Key {
		t.Fatalf("frame %+v, want the keyframe", f)
	}
}

func TestADBRemoteCloseStopsSessionsAndTrack(t *testing.T) {
	clk := newFakeClock()
	l := &fakeLaunches{}
	var mainKeys int
	r := newTestRemote(l, clk, &mainKeys)
	s := running(t, r, l, clk)
	r.close()
	if !s.isClosed() {
		t.Fatal("close left the second session running")
	}
	for range r.out {
	}
	r.offerMain(target.Frame{Key: true, Data: keyData}) // must not panic
	r.close()
}

func TestADBStreamWithoutRemoteTrack(t *testing.T) {
	s := &adbStream{}
	if s.Remote() != nil {
		t.Fatal("Remote() should be nil")
	}
	if s.RemoteGate(true) == nil || s.RemoteKeyframe() == nil || s.RemoteCap(1) == nil || s.RemoteMaxSize(1) == nil {
		t.Fatal("expected errors")
	}
	if s.RemoteEncoderBps() != 0 {
		t.Fatal("encoder rate without a track")
	}
}

func TestADBStreamIsASteppedRemoteStream(t *testing.T) {
	var s target.Stream = &adbStream{}
	if _, ok := s.(target.RemoteStream); !ok {
		t.Fatal("adbStream is not a RemoteStream")
	}
	st, ok := s.(target.SteppedRemoteStream)
	if !ok {
		t.Fatal("adbStream is not a SteppedRemoteStream")
	}
	if lo, start, hi := st.RemoteRateRange(); lo != 300_000 || start != 750_000 || hi != 3_000_000 {
		t.Fatalf("range %d %d %d without a remote track, want the table's", lo, start, hi)
	}
	r := newADBRemoteAt(nil, testMain, nil, time.Now)
	defer r.close()
	s = &adbStream{remote: r}
	if lo, start, hi := s.(target.SteppedRemoteStream).RemoteRateRange(); lo != 300_000 || start != 750_000 || hi != 4_000_000 {
		t.Fatalf("range %d %d %d beside a 4 Mbps main session, want 300k 750k 4M", lo, start, hi)
	}
}

func TestADBRemoteWanted(t *testing.T) {
	for _, c := range []struct {
		env  string
		kind target.Kind
		want bool
	}{
		{"", target.KindDevice, true},
		{"", target.KindEmulator, false},
		{"on", target.KindEmulator, true},
		{"off", target.KindDevice, false},
	} {
		t.Setenv("LOUPE_REMOTE_TRACK", c.env)
		if got := adbRemoteWanted(c.kind); got != c.want {
			t.Errorf("LOUPE_REMOTE_TRACK=%q %s: %v, want %v", c.env, c.kind, got, c.want)
		}
	}
}
