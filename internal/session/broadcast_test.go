package session

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// keyStream records keyframe requests and does nothing else.
type keyStream struct {
	requests atomic.Int64
	frames   chan target.Frame
}

func (k *keyStream) Meta() target.Meta           { return target.Meta{} }
func (k *keyStream) Frames() <-chan target.Frame { return k.frames }
func (k *keyStream) Touch(target.Touch) error    { return nil }
func (k *keyStream) Scroll(target.Scroll) error  { return nil }
func (k *keyStream) Key(target.Key) error        { return nil }
func (k *keyStream) RequestKeyframe() error      { k.requests.Add(1); return nil }
func (k *keyStream) Err() error                  { return nil }
func (k *keyStream) Close() error                { return nil }

// A viewer whose channel fills is marked for resync and will then discard every
// inter-frame until a keyframe arrives. scrcpy emits no keyframes on a timer,
// so unless the fan-out asks for one the viewer never recovers: the device kept
// streaming while framesOut, framesDrop and framesShed all sat frozen for the
// rest of the session.
func TestBroadcastAsksForAKeyframeWhenAViewerFallsBehind(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
	}

	v := &viewer{id: 1, out: make(chan OutMsg, 2)}
	s.viewers[v.id] = v

	// Fill the channel, then overflow it.
	for i := 0; i < 8; i++ {
		s.broadcast(OutMsg{})
	}

	if !v.needsKey {
		t.Fatal("viewer should be marked for resync after its channel overflowed")
	}
	if got := s.DropCount(); got == 0 {
		t.Fatal("overflow should have been counted as a drop")
	}

	// The request runs off the producer goroutine, so give it a moment.
	deadline := time.Now().Add(time.Second)
	for s.stream.(*keyStream).requests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ks.requests.Load(); got == 0 {
		t.Fatal("falling behind must request a keyframe, or the viewer waits forever")
	}

	// Sustained starvation must not mean one control message per frame.
	for i := 0; i < 200; i++ {
		s.broadcast(OutMsg{})
	}
	if got := ks.requests.Load(); got > 2 {
		t.Errorf("keyframe requests not rate limited: got %d for 208 frames", got)
	}
}

// While a viewer is latched every frame takes a branch that used to increment
// nothing, which in the trace is indistinguishable from a device that has gone
// quiet. framesWait is what separates them.
func TestSuppressedFramesAreCounted(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 1), needsKey: true}
	s.viewers[v.id] = v

	for i := 0; i < 5; i++ {
		s.broadcast(OutMsg{})
	}

	if got := s.WaitCount(); got != 5 {
		t.Fatalf("suppressed frames not counted: got %d, want 5", got)
	}
	if got := s.framesOut.Load(); got != 0 {
		t.Fatalf("framesOut should stay flat while latched, got %d", got)
	}

	// A keyframe clears the latch and traffic resumes.
	s.broadcast(OutMsg{Key: true})
	if v.needsKey {
		t.Fatal("a keyframe must clear the resync flag")
	}
}

// A joining viewer starts latched, because the frame it lands mid-way through
// is almost never a keyframe and decoding from the middle of a GOP produces
// garbage. Nothing in the fan-out asked for one, though, and scrcpy emits no
// keyframes on a timer: a viewer that joined a quiet stream sat at framesOut 0
// for the life of the session, with awaitKey frozen rather than climbing.
func TestJoiningAsksForAKeyframe(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
	}

	s.AddViewer()

	deadline := time.Now().Add(time.Second)
	for ks.requests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ks.requests.Load(); got == 0 {
		t.Fatal("joining must request a keyframe, or the viewer waits for one that never comes")
	}
}

// The parameter sets replayed to a joiner are the stream's first frame, and
// they kept the QueuedAt of that moment. A viewer's write deadline runs from
// QueuedAt, so on WebTransport the replay went out already expired and was
// reset: a phone that reconnected to a running simulator got keyframes and no
// SPS, and failed every decode ("Decoder failure", haveConfig false) until
// the session ended. Android hid it, because each keyframe request restarts
// scrcpy's encoder and that sends fresh parameter sets; simcap does not.
func TestJoinerReplayOfParameterSetsIsFresh(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
		lastCfg: &OutMsg{Config: true, QueuedAt: time.Now().Add(-5 * time.Minute)},
	}

	_, out := s.AddViewer()
	<-out // init
	cfg := <-out
	if !cfg.Config {
		t.Fatalf("second message to a joiner is not the parameter sets: %+v", cfg)
	}
	if age := time.Since(cfg.QueuedAt); age > time.Second {
		t.Errorf("replayed parameter sets are %v old; their write deadline has already passed", age)
	}
}

// Eight frames was 133ms at 60fps, and the 09:53 5G trace showed write stalls
// of 280-297ms against it: one stall overflowed the channel, dropped a frame
// and latched the viewer, and 340 of that session's 390 shed frames were the
// keyframe wait that followed. The constant also meant different things at
// different rates -- 800ms on trap 20's idle 10Hz panel -- which is why it
// looked adequate on a desk.
func TestViewerQueueHoldsASecondOfFrames(t *testing.T) {
	// A 280ms stall is the one the trace showed. The queue must ride it out.
	if got := viewerQueueLen(60); got < 60 {
		t.Errorf("viewerQueueLen(60) = %d, want >= 60 so a 280ms stall does not drop", got)
	}
	// Zero means the target's own default, which is 60fps.
	if got := viewerQueueLen(0); got < 60 {
		t.Errorf("viewerQueueLen(0) = %d, want the 60fps depth, not a floor", got)
	}
	// A low rate must not produce a queue so shallow it drops on any hiccup.
	if got := viewerQueueLen(10); got < minViewerQueue {
		t.Errorf("viewerQueueLen(10) = %d, want at least %d", got, minViewerQueue)
	}
	// And a wild rate must not let one viewer pin unbounded memory.
	if got := viewerQueueLen(100000); got > maxViewerQueue {
		t.Errorf("viewerQueueLen(100000) = %d, want at most %d", got, maxViewerQueue)
	}
}

// The limiter used to DROP a request that arrived inside its window, not delay
// it. For the repeats under sustained starvation that is fine. For the first
// ask after a viewer latches it is not: that request is the only thing that
// ends the blackout, and an unrelated earlier ask could swallow it entirely,
// leaving the viewer waiting on a keyframe nobody had asked for.
func TestALatchingViewerAskIsNeverDropped(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 2)}
	s.viewers[v.id] = v

	// An unrelated request has just gone out, so the limiter is closed.
	s.askKeyframe()
	waitRequests(t, ks, 1)

	// Now the viewer latches, well inside the limiter's window. The ask must
	// still reach the encoder -- delayed to the edge floor, never discarded.
	for i := 0; i < 8; i++ {
		s.broadcast(OutMsg{})
	}
	if !v.needsKey {
		t.Fatal("viewer should be latched after its channel overflowed")
	}
	waitRequests(t, ks, 2)
}

// Coalescing is what bounds the cost, not the shortened floor. A latched
// viewer reaches the drop branch on nearly every frame, and that is the
// once-per-frame control message the old limiter existed to prevent: holding
// at most one request outstanding has to do the same job.
func TestSustainedStarvationStillRateLimits(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 2)}
	s.viewers[v.id] = v

	// Keyframes keep clearing the latch, so every overflow after one is a fresh
	// edge -- the worst case for a limiter that keys on the transition.
	for i := 0; i < 300; i++ {
		s.broadcast(OutMsg{Key: i%3 == 0})
	}
	// Well past the edge floor, so every request this loop scheduled has had
	// time to fire. Reading at the floor itself races them and reports a low
	// count whether they coalesced or not.
	time.Sleep(4 * keyframeEdgeInterval)
	if got := ks.requests.Load(); got > 4 {
		t.Errorf("keyframe requests not rate limited: got %d for 300 frames", got)
	}
}

func waitRequests(t *testing.T, ks *keyStream, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for ks.requests.Load() < want && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := ks.requests.Load(); got < want {
		t.Fatalf("keyframe requests: got %d, want at least %d", got, want)
	}
}

// The ceiling is a server setting but only the client can act on it, so it
// rides the init message. A viewer that never learns it silently keeps the
// client's compiled-in default, which is the bug this guards.
func TestInitCarriesTheClientBufferCeiling(t *testing.T) {
	s := &Session{
		ID:       "s",
		key:      "test",
		stream:   &keyStream{frames: make(chan target.Frame)},
		viewers:  make(map[uint64]*viewer),
		done:     make(chan struct{}),
		queueLen: 8,
	}
	s.SetClientBuffer(275)

	_, out := s.AddViewer()
	select {
	case msg := <-out:
		var got InitPayload
		if len(msg.Data) < 1 || msg.Data[0] != MsgInit {
			t.Fatalf("first message is not init: %v", msg.Data)
		}
		if err := json.Unmarshal(msg.Data[1:], &got); err != nil {
			t.Fatalf("init is not InitPayload JSON: %v", err)
		}
		if got.MaxBufferMs != 275 {
			t.Errorf("init maxBufferMs = %d, want 275", got.MaxBufferMs)
		}
	case <-time.After(time.Second):
		t.Fatal("no init message")
	}
}

// Every viewer is sent every event, its own included, so the id in init is
// the only way a client can tell its own tap from another viewer's. Two
// viewers must each be told their own id, not the first joiner's.
func TestInitTellsEachViewerItsOwnID(t *testing.T) {
	s := &Session{
		ID:       "s",
		key:      "test",
		stream:   &keyStream{frames: make(chan target.Frame)},
		viewers:  make(map[uint64]*viewer),
		done:     make(chan struct{}),
		queueLen: 8,
	}
	for i := 0; i < 2; i++ {
		id, out := s.AddViewer()
		msg := <-out
		var got InitPayload
		if err := json.Unmarshal(msg.Data[1:], &got); err != nil {
			t.Fatalf("init is not InitPayload JSON: %v", err)
		}
		if got.Viewer != id {
			t.Errorf("viewer %d was told it is viewer %d", id, got.Viewer)
		}
	}
}

// waitForRequest blocks until the stream has been asked for a keyframe at least
// n times, because the ask runs off the producer goroutine behind a timer.
func waitForRequest(t *testing.T, ks *keyStream, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for ks.requests.Load() < n && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := ks.requests.Load(); got < n {
		t.Fatalf("wanted at least %d keyframe requests, got %d", n, got)
	}
}

// A keyframe skips the resync branch by design -- it is the frame that ends the
// wait -- so it reaches the select, and a full channel drops it. Every drop then
// asked for another one. The encoder answers, the answer lands on a channel that
// is still full, and the loop closes: a stalled viewer turns the entire stream
// into keyframes, which are the largest frames the encoder can emit, on the one
// link already proven too slow to keep up.
//
// In the trace it reads as drop climbing with awaitKey pinned at 0, which by
// trap 19's own discriminator says the device has gone quiet. It had not:
// emulator-5554 ended a 121s session with drop 746, framesOut 737, awaitKey 0.
func TestAStalledViewerDoesNotTurnTheStreamIntoKeyframes(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 4)}
	s.viewers[v.id] = v

	// Latch it: fill the channel, then overflow.
	for i := 0; i < 8; i++ {
		s.broadcast(OutMsg{})
	}
	waitForRequest(t, ks, 1)
	asked := ks.requests.Load()

	// The device answers, repeatedly, while the viewer reads nothing. This is
	// the run the old code could not leave.
	for i := 0; i < 40; i++ {
		s.broadcast(OutMsg{Key: true})
		s.broadcast(OutMsg{})
		time.Sleep(3 * time.Millisecond)
	}

	if got := ks.requests.Load() - asked; got > 1 {
		t.Errorf("asked for %d more keyframes while the viewer had nowhere to put one", got)
	}
}

// The other direction: once the viewer drains, the ask has to come back, or the
// fix above would trade a keyframe flood for trap 22's permanent blank screen.
func TestADrainedViewerIsAskedForAKeyframeAgain(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 4)}
	s.viewers[v.id] = v

	for i := 0; i < 8; i++ {
		s.broadcast(OutMsg{})
	}
	waitForRequest(t, ks, 1)
	asked := ks.requests.Load()

	// The viewer catches up.
	for len(v.out) > 0 {
		<-v.out
	}

	// An inter-frame is still useless to it, but there is room for an answer now.
	for i := 0; i < 10; i++ {
		s.broadcast(OutMsg{})
		time.Sleep(10 * time.Millisecond)
	}
	waitForRequest(t, ks, asked+1)
}

// A viewer that drains at exactly the rate it fills sits permanently near-full,
// which is the common case on a link the pacer is already working on. One free
// slot is enough for a keyframe to land and unlatch, and then the inter-frames
// behind it re-latch immediately -- the same loop one frame further on, and the
// encoder still spends the link on keyframes. Room has to mean room for what
// follows.
func TestANearlyFullViewerIsNotAskedForAKeyframe(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 4)}
	s.viewers[v.id] = v

	for i := 0; i < 8; i++ {
		s.broadcast(OutMsg{})
	}
	waitForRequest(t, ks, 1)
	asked := ks.requests.Load()

	// Hold it one short of full and keep it there. Inter-frames cannot refill a
	// latched viewer -- they are what the latch suppresses -- so the refill is a
	// parameter set, which reaches the channel without ending the wait.
	for i := 0; i < 40; i++ {
		<-v.out
		if got := len(v.out); got != cap(v.out)-1 {
			t.Fatalf("meant to hold the channel one short of full, got %d of %d", got, cap(v.out))
		}
		s.broadcast(OutMsg{})
		s.broadcast(OutMsg{Config: true})
		time.Sleep(3 * time.Millisecond)
	}

	if got := ks.requests.Load() - asked; got > 0 {
		t.Errorf("asked for %d keyframes with one slot free, which is not room for one", got)
	}
}
