package target

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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

func (l *fakeLaunches) launch(_ context.Context, rl remoteLaunch) (remoteSession, error) {
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

func recvFrame(t *testing.T, ch <-chan Frame) Frame {
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
	return Frame{}
}

func noFrame(t *testing.T, ch <-chan Frame) {
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

func newTestRemote(l *fakeLaunches, clk *fakeClock, mainKeys *int) *adbRemote {
	var mu sync.Mutex
	return newADBRemoteAt(l.launch, 1440, func() error {
		mu.Lock()
		*mainKeys++
		mu.Unlock()
		return nil
	}, clk.Now)
}

func TestRungPickerCutsAtOnceAndRaisesAfterHold(t *testing.T) {
	p := newRungPicker(adbRemoteRungs)
	at := time.Unix(0, 0)
	if got := p.pick(at, 3_000_000); got != 0 {
		t.Fatalf("first ask raised at once to rung %d", got)
	}
	if got := p.pick(at.Add(remoteRaiseHold-time.Millisecond), 3_000_000); got != 0 {
		t.Fatalf("raised to %d before the hold", got)
	}
	if got := p.pick(at.Add(remoteRaiseHold), 3_000_000); got != 1 {
		t.Fatalf("rung %d after the hold, want one step up to 1", got)
	}
	if got := p.pick(at.Add(2*remoteRaiseHold), 3_000_000); got != 2 {
		t.Fatalf("rung %d after a second hold, want 2", got)
	}
	if got := p.pick(at.Add(3*remoteRaiseHold), 3_000_000); got != 3 {
		t.Fatalf("rung %d after a third hold, want the top, 3", got)
	}
	if got := p.pick(at.Add(3*remoteRaiseHold+time.Millisecond), 500_000); got != 0 {
		t.Fatalf("cut to %d, want straight to 0", got)
	}
	// A cap that dips under the next rung restarts its hold.
	p = newRungPicker(adbRemoteRungs)
	p.pick(at, 1_300_000)
	p.pick(at.Add(2*time.Second), 500_000)
	p.pick(at.Add(2500*time.Millisecond), 1_300_000)
	if got := p.pick(at.Add(4*time.Second), 1_300_000); got != 0 {
		t.Fatalf("rung %d: the hold did not restart after the dip", got)
	}
}

func TestRungPickerHoldDoublesWhenARaiseIsCut(t *testing.T) {
	p := newRungPicker(adbRemoteRungs)
	at := time.Unix(0, 0)
	p.pick(at, 1_300_000)
	at = at.Add(remoteRaiseHold)
	if got := p.pick(at, 1_300_000); got != 1 {
		t.Fatalf("rung %d, want 1", got)
	}
	at = at.Add(time.Second)
	p.pick(at, 500_000) // the link could not take it
	p.pick(at, 1_300_000)
	if got := p.pick(at.Add(remoteRaiseHold), 1_300_000); got != 0 {
		t.Fatalf("raised again after %v, want the hold doubled", remoteRaiseHold)
	}
	if got := p.pick(at.Add(2*remoteRaiseHold), 1_300_000); got != 1 {
		t.Fatalf("rung %d after the doubled hold, want 1", got)
	}
}

func TestRungsSitUnderTheirRatesAndInTheRange(t *testing.T) {
	for i, g := range adbRemoteRungs {
		if g.From > g.BitRate {
			t.Errorf("rung %d is selected from %d, over its own rate %d: rate control raises only while the encoder fills half the cap", i, g.From, g.BitRate)
		}
		if i > 0 && g.From <= adbRemoteRungs[i-1].From {
			t.Errorf("rung %d From %d does not rise", i, g.From)
		}
	}
	if adbRemoteRateMin > adbRemoteRungs[0].BitRate || adbRemoteRateMax < adbRemoteRungs[len(adbRemoteRungs)-1].From {
		t.Errorf("rate range %d-%d does not reach every rung", adbRemoteRateMin, adbRemoteRateMax)
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
	if s.launch != (remoteLaunch{MaxSize: 480, MaxFPS: 30, BitRate: 300_000}) {
		t.Fatalf("first launch %+v, want the bottom rung", s.launch)
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

	// A viewer that comes back gets a new session, at the bottom rung.
	r.setCap(3_000_000)
	r.gate(true)
	r.step(clk.Add(time.Second))
	s2 := l.session(t, 1)
	if s2.launch.BitRate != 300_000 {
		t.Fatalf("relaunch at %d bps, want the bottom rung", s2.launch.BitRate)
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
	if nu.launch != (remoteLaunch{MaxSize: 720, MaxFPS: 30, BitRate: 750_000}) {
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
	r := newADBRemoteAt(l.launch, 1440, func() error {
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
	r.offerMain(Frame{Config: true, Data: cfgData, Captured: clk.Now()})
	r.offerMain(Frame{PTS: time.Second, Data: deltaData, Captured: clk.Now()})
	r.offerMain(Frame{PTS: 2 * time.Second, Key: true, Data: keyData, Captured: clk.Now()})
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
	r := newADBRemoteAt(l.launch, 400, nil, clk.Now)
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
	r.offerMain(Frame{Key: true, Data: keyData}) // must not panic
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
	var s Stream = &adbStream{}
	if _, ok := s.(RemoteStream); !ok {
		t.Fatal("adbStream is not a RemoteStream")
	}
	st, ok := s.(SteppedRemoteStream)
	if !ok {
		t.Fatal("adbStream is not a SteppedRemoteStream")
	}
	if lo, start, hi := st.RemoteRateRange(); lo != adbRemoteRateMin || start != adbRemoteRateStart || hi != adbRemoteRateMax {
		t.Fatalf("range %d %d %d", lo, start, hi)
	}
}

func TestADBRemoteWanted(t *testing.T) {
	for _, c := range []struct {
		env  string
		kind Kind
		want bool
	}{
		{"", KindDevice, true},
		{"", KindEmulator, false},
		{"on", KindEmulator, true},
		{"off", KindDevice, false},
	} {
		t.Setenv("LOUPE_REMOTE_TRACK", c.env)
		if got := adbRemoteWanted(c.kind); got != c.want {
			t.Errorf("LOUPE_REMOTE_TRACK=%q %s: %v, want %v", c.env, c.kind, got, c.want)
		}
	}
}
