package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// swapStream is a stream whose frames a test writes by hand and whose close is
// observable, which is what every relaunch assertion turns on.
type swapStream struct {
	frames chan target.Frame
	once   sync.Once
	closed atomic.Bool
	keyReq atomic.Int64
}

func newSwapStream() *swapStream {
	return &swapStream{frames: make(chan target.Frame, 8)}
}

func (s *swapStream) Meta() target.Meta           { return target.Meta{Width: 100, Height: 200} }
func (s *swapStream) Frames() <-chan target.Frame { return s.frames }
func (s *swapStream) Touch(target.Touch) error    { return nil }
func (s *swapStream) Scroll(target.Scroll) error  { return nil }
func (s *swapStream) Key(target.Key) error        { return nil }
func (s *swapStream) RequestKeyframe() error      { s.keyReq.Add(1); return nil }
func (s *swapStream) Err() error                  { return nil }

func (s *swapStream) Close() error {
	s.once.Do(func() {
		s.closed.Store(true)
		close(s.frames)
	})
	return nil
}

// swapTarget hands out a prepared stream per Open, so a test controls exactly
// what a relaunch gets and can make one fail.
type swapTarget struct {
	kind target.Kind

	// gate holds Open until a test releases it, and entered fires when Open is
	// inside that wait. Together they let a test place another event in the
	// middle of a relaunch.
	gate      chan struct{}
	entered   chan struct{}
	enterOnce sync.Once

	mu      sync.Mutex
	queue   []*swapStream
	err     error
	opened  []target.Options
	handles []*swapStream
}

func (t *swapTarget) ID() string { return "swap" }
func (t *swapTarget) Kind() target.Kind {
	if t.kind == "" {
		return target.KindDevice
	}
	return t.kind
}
func (t *swapTarget) Label() string { return "swap" }

func (t *swapTarget) Open(_ context.Context, opts target.Options) (target.Stream, error) {
	if t.gate != nil {
		t.enterOnce.Do(func() {
			if t.entered != nil {
				close(t.entered)
			}
		})
		<-t.gate
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return nil, t.err
	}
	t.opened = append(t.opened, opts)
	s := t.queue[0]
	t.queue = t.queue[1:]
	t.handles = append(t.handles, s)
	return s, nil
}

func (t *swapTarget) openedOpts() []target.Options {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]target.Options(nil), t.opened...)
}

func (t *swapTarget) fail(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.err = err
}

// openSwap builds a live session over the first prepared stream.
func openSwap(t *testing.T, streams ...*swapStream) (*Session, *swapTarget) {
	t.Helper()
	tgt := &swapTarget{queue: streams}
	s, err := Open(context.Background(), "test", tgt, target.Options{
		MaxFPS:  60,
		BitRate: 3_000_000,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, tgt
}

// waitFor polls until cond holds, which is how every assertion against pump is
// made: it runs on its own goroutine and there is no handshake to join.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// drainPTS reads frame messages off a viewer channel, returning their
// presentation timestamps. Control messages carry no picture and are skipped.
func drainPTS(ch <-chan OutMsg, n int) []uint64 {
	var out []uint64
	for len(out) < n {
		msg, ok := <-ch
		if !ok {
			return out
		}
		if msg.Control {
			continue
		}
		out = append(out, msg.PTSMicros)
	}
	return out
}

// A relaunch is the only way to change the bitrate -- scrcpy has no opcode for
// it -- so the stream has to be replaced underneath a session that stays up.
// Tearing the session down instead would make every change depend on an iOS
// reconnect, which trap 21 showed is the fragile path.
func TestRelaunchSwapsTheStreamWithoutEndingTheSession(t *testing.T) {
	first, second := newSwapStream(), newSwapStream()
	s, tgt := openSwap(t, first, second)
	_, out := s.AddViewer()

	first.frames <- target.Frame{PTS: 10 * time.Millisecond, Key: true, Data: []byte{1}, Captured: time.Now()}
	if got := drainPTS(out, 1); len(got) != 1 {
		t.Fatalf("first stream frame never arrived: %v", got)
	}

	if err := s.Relaunch(1_000_000); err != nil {
		t.Fatalf("relaunch: %v", err)
	}

	if !first.closed.Load() {
		t.Error("the replaced stream must be closed, or its encoder keeps running on the device")
	}
	select {
	case <-s.Done():
		t.Fatal("a relaunch ended the session; the whole point is that viewers stay connected")
	default:
	}

	if got := s.CurrentBitRate(); got != 1_000_000 {
		t.Errorf("CurrentBitRate = %d, want 1000000", got)
	}
	opts := tgt.openedOpts()
	if len(opts) != 2 || opts[1].BitRate != 1_000_000 {
		t.Fatalf("relaunch opened with %+v, want a second open at 1000000 bps", opts)
	}
	if opts[1].MaxFPS != 60 {
		t.Errorf("relaunch changed MaxFPS to %d; only the bitrate should move", opts[1].MaxFPS)
	}

	// Frames from the new encoder must reach the viewer, which is the thing
	// that distinguishes a swap from a session that merely failed to notice
	// its stream had gone.
	second.frames <- target.Frame{PTS: 5 * time.Millisecond, Key: true, Data: []byte{2}, Captured: time.Now()}
	if got := drainPTS(out, 1); len(got) != 1 {
		t.Fatal("no frame from the new stream reached the viewer")
	}
}

// The ambiguity a swap introduces: a stream ending used to mean the session
// was over, and now it can also mean it was replaced. Getting this wrong in
// this direction leaves a dead session that never cleans up.
func TestSessionEndsWhenTheStreamDiesOnItsOwn(t *testing.T) {
	first := newSwapStream()
	s, _ := openSwap(t, first)
	_ = first.Close()

	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stream ended with no replacement; the session must end with it")
	}
}

// scrcpy restarts every session's PTS at zero. The client anchors its
// presentation clock on the first PTS it sees and re-anchors only on overrun,
// so a timeline that jumps backwards leaves every later frame reading as
// already due -- the jitter buffer silently degrades to draw-on-arrival and
// nothing reports it.
func TestRelaunchKeepsPresentationTimestampsMonotonic(t *testing.T) {
	first, second := newSwapStream(), newSwapStream()
	s, _ := openSwap(t, first, second)
	_, out := s.AddViewer()

	now := time.Now()
	first.frames <- target.Frame{PTS: 90 * time.Second, Key: true, Data: []byte{1}, Captured: now}
	before := drainPTS(out, 1)
	if len(before) != 1 {
		t.Fatal("no frame from the first stream")
	}

	if err := s.Relaunch(1_000_000); err != nil {
		t.Fatalf("relaunch: %v", err)
	}

	// The new encoder's timeline starts over, as scrcpy's does.
	second.frames <- target.Frame{PTS: 0, Key: true, Data: []byte{2}, Captured: now.Add(400 * time.Millisecond)}
	second.frames <- target.Frame{PTS: 16 * time.Millisecond, Data: []byte{3}, Captured: now.Add(416 * time.Millisecond)}

	after := drainPTS(out, 2)
	if len(after) != 2 {
		t.Fatalf("only %d frames after the relaunch", len(after))
	}
	if after[0] <= before[0] {
		t.Fatalf("PTS went backwards across the relaunch: %d then %d", before[0], after[0])
	}
	if after[1] <= after[0] {
		t.Fatalf("PTS not monotonic within the new stream: %d then %d", after[0], after[1])
	}
	// The jump should track the wall-clock gap, not a fixed nudge: the client's
	// clock stays valid only if the timeline advances by as much as real time
	// did while the encoder was restarting.
	if gap := after[0] - before[0]; gap < 350_000 || gap > 450_000 {
		t.Errorf("PTS advanced %dus across a 400ms restart; want roughly the wall-clock gap", gap)
	}
}

// A relaunch that cannot open must leave the session exactly as it was. The
// controller fires on a link that is already failing, which is when an adb
// call is most likely to time out, and losing the session there would turn a
// degraded picture into no picture.
func TestFailedRelaunchLeavesTheSessionRunning(t *testing.T) {
	first := newSwapStream()
	s, tgt := openSwap(t, first)
	tgt.fail(errors.New("adb: device offline"))
	if err := s.Relaunch(1_000_000); err == nil {
		t.Fatal("relaunch should report the open failure")
	}

	if first.closed.Load() {
		t.Error("a failed relaunch closed the working stream")
	}
	select {
	case <-s.Done():
		t.Fatal("a failed relaunch ended the session")
	default:
	}
	if got := s.CurrentBitRate(); got != 3_000_000 {
		t.Errorf("CurrentBitRate = %d after a failed relaunch, want the original 3000000", got)
	}
}

// A viewer's queue still holds frames from the old encoder when the new one's
// parameter sets arrive. Handing it the inter-frames in between means decoding
// against a GOP that no longer exists.
func TestRelaunchPutsViewersBackIntoTheKeyframeWait(t *testing.T) {
	first, second := newSwapStream(), newSwapStream()
	s, _ := openSwap(t, first, second)
	id, out := s.AddViewer()
	first.frames <- target.Frame{PTS: time.Millisecond, Key: true, Data: []byte{1}, Captured: time.Now()}
	drainPTS(out, 1)

	// The exclusive lock, not the read lock: broadcast clears needsKey while
	// holding mu for reading, which is safe only because pump is the single
	// writer. Another reader under RLock would race it.
	waitFor(t, "the keyframe to clear the join wait", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.viewers[id].needsKey
	})

	if err := s.Relaunch(1_000_000); err != nil {
		t.Fatalf("relaunch: %v", err)
	}

	s.mu.Lock()
	needsKey := s.viewers[id].needsKey
	resolved := s.codecResolved
	s.mu.Unlock()
	if !needsKey {
		t.Error("viewer was not put back into the keyframe wait after the swap")
	}
	if resolved {
		t.Error("codec must be re-derived: H.264 level follows bitrate, so a relaunch can change the codec string")
	}
}

// Close and Relaunch both decide under streamMu, so whichever loses the race
// must not leave an encoder running that nothing will read.
func TestRelaunchIsRefusedOnAClosedSession(t *testing.T) {
	first, spare := newSwapStream(), newSwapStream()
	s, _ := openSwap(t, first, spare)
	_ = s.Close()

	if err := s.Relaunch(1_000_000); err == nil {
		t.Fatal("relaunch on a closed session should fail")
	}
	if spare.closed.Load() {
		t.Error("the refused relaunch should not have opened a stream at all")
	}
}

// Synthetic passes Options.BitRate to ffmpeg, so the controller runs on it and
// a change relaunches the encoder at the new rate, as it does for a device.
func TestSyntheticTargetsAdaptByRelaunch(t *testing.T) {
	first, second := newSwapStream(), newSwapStream()
	tgt := &swapTarget{kind: target.KindSynthetic, queue: []*swapStream{first, second}}
	s, err := Open(context.Background(), "syn", tgt, target.Options{MaxFPS: 60, BitRate: 3_000_000})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if !s.AdaptBitRate() {
		t.Fatal("the bitrate controller refused a synthetic target")
	}
	live, err := s.changeBitRate(1_000_000)
	if err != nil {
		t.Fatalf("changeBitRate: %v", err)
	}
	if live {
		t.Error("a synthetic stream reported a live change; it has to relaunch")
	}
	opts := tgt.openedOpts()
	if len(opts) != 2 || opts[1].BitRate != 1_000_000 {
		t.Fatalf("relaunch opened with %+v, want a second open at 1000000 bps", opts)
	}
	if got := s.CurrentBitRate(); got != 1_000_000 {
		t.Fatalf("CurrentBitRate = %d, want 1000000", got)
	}
}

// The controller reads only shedding it can act on. A keyframe wait is the
// consequence of an earlier shed, and one dropped frame at 60fps produces
// sixteen of them, so counting them would let a single drop argue for a
// bitrate change sixteen times over. The policy excludes them; this checks the
// call site does too.
func TestSamplerExcludesKeyframeWaitsFromShedding(t *testing.T) {
	first := newSwapStream()
	s, _ := openSwap(t, first)
	id, _ := s.AddViewer()
	s.mu.Lock()
	s.viewers[id].joined = time.Now().Add(-time.Hour)
	s.mu.Unlock()

	s.NoteShed(ShedPacer)
	s.NoteShed(ShedStale)
	for i := 0; i < 20; i++ {
		s.NoteShed(ShedAwaitKey)
	}

	sample, ok := newBitrateSampler().next(s, time.Now())
	if !ok {
		t.Fatal("a viewer attached for the whole interval should produce a sample")
	}
	if sample.Shed != 2 {
		t.Errorf("Shed = %d, want 2: pacer and staleness only", sample.Shed)
	}
}

// A frame reset in flight is the link failing to carry what it was offered,
// which is what Shed measures -- staleness decided a moment earlier. Leaving it
// out understates the shed fraction by exactly the frames that cost the most
// link time, since a frame only reaches the deadline by occupying it.
func TestSamplerCountsAbandonedFramesAsShedding(t *testing.T) {
	first := newSwapStream()
	s, _ := openSwap(t, first)
	id, _ := s.AddViewer()
	s.mu.Lock()
	s.viewers[id].joined = time.Now().Add(-time.Hour)
	s.mu.Unlock()

	s.NoteShed(ShedPacer)
	s.NoteShed(ShedStale)
	s.NoteShed(ShedAbandon)
	s.NoteShed(ShedAbandon)

	sample, ok := newBitrateSampler().next(s, time.Now())
	if !ok {
		t.Fatal("a viewer attached for the whole interval should produce a sample")
	}
	if sample.Shed != 4 {
		t.Errorf("Shed = %d, want 4: pacer, staleness and both abandoned frames", sample.Shed)
	}
}

// Bytes delivered is the only direct measurement of what the link carries, and
// the encoder is shared, so with two viewers what it has to fit inside is the
// slower of them. Summing would read two half-speed links as one fast one.
func TestSamplerReportsTheSlowestFullIntervalViewer(t *testing.T) {
	first := newSwapStream()
	s, _ := openSwap(t, first)
	slow, _ := s.AddViewer()
	fast, _ := s.AddViewer()
	s.mu.Lock()
	s.viewers[slow].joined = time.Now().Add(-time.Hour)
	s.viewers[fast].joined = time.Now().Add(-time.Hour)
	s.mu.Unlock()

	s.NoteSent(slow, 1000)
	s.NoteSent(fast, 9000)

	sample, ok := newBitrateSampler().next(s, time.Now())
	if !ok {
		t.Fatal("expected a sample")
	}
	if sample.Bytes != 1000 {
		t.Errorf("Bytes = %d, want 1000 from the slower viewer", sample.Bytes)
	}
}

// A viewer that joined part way through carried part of an interval's bytes.
// Counting it reads a fresh connection as a slow link, which would argue for a
// bitrate cut every time someone reloads the page.
func TestSamplerIgnoresAViewerThatJoinedMidInterval(t *testing.T) {
	first := newSwapStream()
	s, _ := openSwap(t, first)
	joiner, _ := s.AddViewer()
	s.NoteSent(joiner, 10)

	if _, ok := newBitrateSampler().next(s, time.Now()); ok {
		t.Fatal("a viewer younger than the interval must not be measured")
	}

	s.mu.Lock()
	s.viewers[joiner].joined = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	sample, ok := newBitrateSampler().next(s, time.Now())
	if !ok || sample.Bytes != 10 {
		t.Fatalf("sample = %+v ok=%v, want 10 bytes once the viewer is old enough", sample, ok)
	}
}

// A relaunch takes hundreds of milliseconds, which is long enough for the
// session to end while one is in flight. Committing the result then would
// install an encoder that pump has already stopped reading and that nothing
// will ever close: on a device that is a live scrcpy server with a held
// display, for the lifetime of the loupe process.
func TestRelaunchDoesNotInstallAStreamNobodyWillRead(t *testing.T) {
	first, second := newSwapStream(), newSwapStream()
	s, tgt := openSwap(t, first, second)

	tgt.gate = make(chan struct{})
	tgt.entered = make(chan struct{})

	done := make(chan error, 1)
	go func() { done <- s.Relaunch(1_000_000) }()
	<-tgt.entered

	// The stream dies on its own while the replacement is still opening, so
	// pump ends the session.
	_ = first.Close()
	waitFor(t, "the session to end", func() bool {
		select {
		case <-s.done:
			return true
		default:
			return false
		}
	})

	close(tgt.gate)
	err := <-done
	if !errors.Is(err, errSessionOver) {
		t.Fatalf("relaunch into an ended session returned %v; want errSessionOver", err)
	}
	if !second.closed.Load() {
		t.Error("the replacement stream was opened and left running after the session ended")
	}
}

// Two relaunches in flight at once would both open an encoder, and the second
// commit would overwrite the first's stream without closing it. The controller
// is serial, so this guard only matters for a second caller -- a bitrate
// written through /api/config while adaptation is running is the obvious one.
func TestRelaunchRefusesASecondOneInFlight(t *testing.T) {
	first, second, third := newSwapStream(), newSwapStream(), newSwapStream()
	s, tgt := openSwap(t, first, second, third)

	tgt.gate = make(chan struct{})
	tgt.entered = make(chan struct{})

	done := make(chan error, 1)
	go func() { done <- s.Relaunch(1_000_000) }()
	<-tgt.entered

	race := make(chan error, 1)
	go func() { race <- s.Relaunch(500_000) }()

	select {
	case err := <-race:
		if !errors.Is(err, errRelaunchInFlight) {
			t.Fatalf("second relaunch returned %v; want errRelaunchInFlight", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a second relaunch reached the target while the first was still opening")
	}

	close(tgt.gate)
	if err := <-done; err != nil {
		t.Fatalf("first relaunch: %v", err)
	}
	if third.closed.Load() || len(tgt.openedOpts()) != 2 {
		t.Errorf("target opened %d times; the refused relaunch should not have reached it", len(tgt.openedOpts()))
	}
}

// keysIn is what separates a keyframe flood from a slow link: both move
// framesIn and framesDrop the same way, and only this says the encoder is
// being asked for IDRs faster than the GOP would emit them. It exists because
// its absence made a 121s emulator session unreadable.
func TestKeyframesAreCountedSeparately(t *testing.T) {
	st := newSwapStream()
	s, _ := openSwap(t, st)

	for _, key := range []bool{true, false, false, true, false} {
		st.frames <- target.Frame{Data: []byte{0, 0, 0, 1, 0x65}, Key: key, Captured: time.Now()}
	}
	waitFor(t, "frames to reach pump", func() bool { return s.FrameCount() == 5 })

	if got := s.KeyCount(); got != 2 {
		t.Errorf("keysIn = %d, want 2 of the 5 frames", got)
	}
}

// The controller must report every interval it evaluates, not only the ones it
// acts on. A run that reaches the decision and declines has to be
// distinguishable from one that never gets there, and on a healthy link the
// declining path is the only one that ever executes -- so if it reports
// nothing, the controller is invisible for the entire time it is working
// correctly. That is how it went unobserved through four sessions of real
// congestion.
func TestTheControllerReportsIntervalsItDoesNotActOn(t *testing.T) {
	s, _ := openSwap(t, newSwapStream())
	id, _ := s.AddViewer()
	s.mu.Lock()
	s.viewers[id].joined = time.Now().Add(-time.Hour)
	s.mu.Unlock()

	var mu sync.Mutex
	var got []BitrateReport
	s.SetBitrateObserver(func(rep BitrateReport) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, rep)
	})
	if !s.AdaptBitRate() {
		t.Fatal("the controller refused to start on a device target")
	}

	// An idle session acts on nothing, which is precisely the case that used
	// to be silent.
	waitFor(t, "the controller to report an interval it did not act on", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 1
	})

	mu.Lock()
	defer mu.Unlock()
	for _, rep := range got {
		if rep.Gate == "" {
			t.Fatalf("an interval was reported with no gate: %+v", rep)
		}
		if rep.To != 0 {
			t.Fatalf("an idle session was adapted to %d bps", rep.To)
		}
	}
}

// AdaptBitRate's return value is the difference between a controller that ran
// and found nothing to do and one that never started. Discarding it at the
// call site is how the second stayed invisible, so the refusal has to be
// reported rather than assumed.
func TestRefusingToStartIsDistinguishableFromFindingNothingToDo(t *testing.T) {
	// A simulator whose stream cannot change bitrate live is the one target
	// the controller refuses.
	first := newSwapStream()
	tgt := &swapTarget{kind: target.KindSimulator, queue: []*swapStream{first}}
	s, err := Open(context.Background(), "sim", tgt, target.Options{MaxFPS: 60, BitRate: 3_000_000})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	reported := make(chan BitrateReport, 4)
	s.SetBitrateObserver(func(rep BitrateReport) { reported <- rep })

	if s.AdaptBitRate() {
		t.Fatal("the controller started on a simulator with no live bitrate")
	}
	// A refusal must be silent on this channel: nothing ran, so there is no
	// interval to report. The caller is the one that has to record it, which
	// is why AdaptBitRate returns at all.
	select {
	case rep := <-reported:
		t.Fatalf("a refused controller reported an interval: %+v", rep)
	case <-time.After(3 * bitrateSampleInterval):
	}
}
