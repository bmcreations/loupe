package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// newTestSession builds a Session directly, the way every other test in this
// package does (see keyStream in broadcast_test.go), over a 1080x2400 stream
// -- large enough that the gesture and scroll normalization tests below are
// not tripping the "no real size yet" 0x0 guard in GestureAssembler.finish and
// viewerPoint. It is not built through Open, so it starts none of pump's
// goroutines; nothing here needs a stream that actually produces frames.
func newTestSession(t *testing.T) *Session {
	t.Helper()
	return &Session{
		ID:      "test",
		key:     "test",
		stream:  &keyStream{frames: make(chan target.Frame)},
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
		init:    InitPayload{CodedWidth: 1080, CodedHeight: 2400},
	}
}

// nextControl drains out until it finds a control message of the wanted type,
// failing after one second. AddViewer queues an init message first, so a test
// that wants the event right after it has to skip past that.
func nextControl(t *testing.T, out <-chan OutMsg, want byte) OutMsg {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case msg := <-out:
			if len(msg.Data) > 0 && msg.Data[0] == want {
				return msg
			}
		case <-deadline:
			t.Fatalf("no message of type %#x within 1s", want)
		}
	}
}

func TestEventsAreNumberedFromOne(t *testing.T) {
	r := newEventRing(4)
	a := r.add(Event{Kind: "tap"})
	b := r.add(Event{Kind: "tap"})
	if a.Seq != 1 || b.Seq != 2 {
		t.Fatalf("seq = %d, %d; want 1, 2", a.Seq, b.Seq)
	}
}

func TestTheRingKeepsOnlyTheNewest(t *testing.T) {
	r := newEventRing(3)
	for i := 0; i < 5; i++ {
		r.add(Event{Kind: "tap"})
	}
	got := r.after(0)
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 {
		t.Fatalf("after(0) = %+v; want seqs 3..5", got)
	}
}

func TestAfterReturnsOnlyLaterEvents(t *testing.T) {
	r := newEventRing(8)
	for i := 0; i < 5; i++ {
		r.add(Event{Kind: "tap"})
	}
	got := r.after(3)
	if len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("after(3) = %+v; want seqs 4, 5", got)
	}
}

func TestEncodeEventIsTypeByteThenJSON(t *testing.T) {
	b := EncodeEvent(Event{Seq: 7, Kind: "tap", Source: "viewer", Viewer: 2,
		From: &Point{0.5, 0.25}, Ms: 90, T: 1790301234567})
	if b[0] != MsgEvent {
		t.Fatalf("type byte = %#x; want %#x", b[0], MsgEvent)
	}
	var back Event
	if err := json.Unmarshal(b[1:], &back); err != nil {
		t.Fatal(err)
	}
	if back.Seq != 7 || back.From.X != 0.5 {
		t.Fatalf("round trip = %+v", back)
	}
}

// A rotate event back to portrait carries Rotation 0, and that is a real
// value distinct from "no rotation on this event" -- a tap has no Rotation
// at all. omitempty on a plain int cannot tell those apart, so Rotation is a
// pointer and this pins that a genuine zero still appears on the wire.
func TestARotationOfZeroStillEncodes(t *testing.T) {
	zero := 0
	b := EncodeEvent(Event{Seq: 1, Kind: "rotate", Source: "agent", Rotation: &zero})
	if !strings.Contains(string(b[1:]), `"rotation":0`) {
		t.Fatalf("body = %s; want it to contain \"rotation\":0", b[1:])
	}
}

// TestARedactedEventEncodesRedactedAndTextLen pins the wire shape of a
// redacted text event: "redacted":true and "textLen":7 must both be on the
// wire byte for byte, not just present after a round trip through the Go
// struct (which would pass even with the wrong JSON tag on either field).
func TestARedactedEventEncodesRedactedAndTextLen(t *testing.T) {
	b := EncodeEvent(Event{Seq: 1, Kind: "text", Source: "agent",
		Text: "•••", Redacted: true, TextLen: 7})
	body := string(b[1:])
	if !strings.Contains(body, `"redacted":true`) {
		t.Fatalf("body = %s; want it to contain \"redacted\":true", body)
	}
	if !strings.Contains(body, `"textLen":7`) {
		t.Fatalf("body = %s; want it to contain \"textLen\":7", body)
	}
}

// TestANonRedactedEventOmitsRedactedAndTextLen is the other half: an
// ordinary text event must not carry redacted or textLen at all, which is
// what pins omitempty on both fields rather than a hardcoded false/0.
func TestANonRedactedEventOmitsRedactedAndTextLen(t *testing.T) {
	b := EncodeEvent(Event{Seq: 1, Kind: "text", Source: "agent", Text: "hello"})
	body := string(b[1:])
	if strings.Contains(body, `"redacted"`) {
		t.Fatalf("body = %s; want no \"redacted\" key on a non-redacted event", body)
	}
	if strings.Contains(body, `"textLen"`) {
		t.Fatalf("body = %s; want no \"textLen\" key on a non-redacted event", body)
	}
}

// EncodeEvent must never send a MsgEvent whose body silently failed to
// marshal. json.Marshal errors on NaN/Inf, and Point owns the only float
// fields on Event, so guarding them there is what keeps the type byte and
// the body in agreement.
func TestNonFiniteCoordinatesEncodeAsZero(t *testing.T) {
	b := EncodeEvent(Event{Seq: 1, Kind: "tap", Source: "viewer",
		From: &Point{math.NaN(), math.Inf(1)}})
	var back Event
	if err := json.Unmarshal(b[1:], &back); err != nil {
		t.Fatalf("body did not decode: %v (body = %s)", err, b[1:])
	}
	if back.From.X != 0 || back.From.Y != 0 {
		t.Fatalf("From = %+v; want {0, 0}", back.From)
	}
}

func TestRoundingKeepsThreePlaces(t *testing.T) {
	b, err := json.Marshal(Point{0.74321, 1.0 / 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "[0.743,0.333]"; got != want {
		t.Fatalf("marshaled = %s; want %s", got, want)
	}
}

// A ring with no requested capacity still has to hold at least the event
// just added, or add would grow buf past cap(buf) == 0 and panic on the
// first call.
func TestARingOfZeroCapacityStillHoldsOneEvent(t *testing.T) {
	r := newEventRing(0)
	e := r.add(Event{Kind: "tap"})
	if e.Seq != 1 {
		t.Fatalf("seq = %d; want 1", e.Seq)
	}
	if got := r.after(0); len(got) != 1 {
		t.Fatalf("after(0) = %+v; want 1 event", got)
	}
}

// TestAViewersTapReachesEveryViewer is the end-to-end path Task 3 adds:
// ViewerTouch forwards to the device (proven elsewhere) and, on the gesture it
// completes, publishes a MsgEvent to every attached viewer and keeps it in the
// ring.
func TestAViewersTapReachesEveryViewer(t *testing.T) {
	s := newTestSession(t)
	a, outA := s.AddViewer()
	_, outB := s.AddViewer()

	s.ViewerTouch(a, TouchEvent{Action: 0, PointerID: 1, X: 540, Y: 1200})
	s.ViewerTouch(a, TouchEvent{Action: 1, PointerID: 1, X: 540, Y: 1200})

	for name, out := range map[string]<-chan OutMsg{"a": outA, "b": outB} {
		msg := nextControl(t, out, MsgEvent)
		if !bytes.Contains(msg.Data, []byte(`"kind":"tap"`)) {
			t.Fatalf("viewer %s got %s", name, msg.Data)
		}
	}
	if got := s.EventsAfter(0); len(got) != 1 || got[0].Viewer != a {
		t.Fatalf("ring = %+v; want one tap from viewer %d", got, a)
	}
}

// TestAnAgentEventIsPublishedWithItsSource covers the other producer:
// /api/input calls PublishEvent directly, with no viewer and no gesture
// assembler involved, and the event still lands in the ring with a timestamp.
func TestAnAgentEventIsPublishedWithItsSource(t *testing.T) {
	s := newTestSession(t)
	s.PublishEvent(Event{Source: "agent", Kind: "button", Button: "home"})
	if got := s.EventsAfter(0); len(got) != 1 || got[0].Source != "agent" || got[0].T == 0 {
		t.Fatalf("ring = %+v", got)
	}
}

// TestAKeyUpIsPublishedAsAButtonEvent covers ViewerKey: a key-down alone
// publishes nothing (a held key repeats, and the down is not the complete
// gesture), and the up carries the classification. Keycode 4 names a device
// button (target.ButtonForKeycode) so it publishes kind:"button" with the
// name, not the raw code -- see TestAnUnnamedKeyUpStaysAKeyEvent for the
// keycode that has no name.
func TestAKeyUpIsPublishedAsAButtonEvent(t *testing.T) {
	s := newTestSession(t)
	id, out := s.AddViewer()

	if err := s.ViewerKey(id, KeyEvent{Action: 0, Keycode: 4}); err != nil {
		t.Fatal(err)
	}
	if got := s.EventsAfter(0); len(got) != 0 {
		t.Fatalf("key-down alone published %+v; want nothing", got)
	}

	if err := s.ViewerKey(id, KeyEvent{Action: 1, Keycode: 4}); err != nil {
		t.Fatal(err)
	}
	msg := nextControl(t, out, MsgEvent)
	if !bytes.Contains(msg.Data, []byte(`"kind":"button"`)) || !bytes.Contains(msg.Data, []byte(`"button":"back"`)) {
		t.Fatalf("got %s; want a button event named back", msg.Data)
	}
}

// TestAnUnnamedKeyUpStaysAKeyEvent covers the other side of the
// classification: keycode 29 (KEYCODE_A) names no button, so it still
// publishes kind:"key" with the raw keycode.
func TestAnUnnamedKeyUpStaysAKeyEvent(t *testing.T) {
	s := newTestSession(t)
	id, out := s.AddViewer()

	if err := s.ViewerKey(id, KeyEvent{Action: 1, Keycode: 29}); err != nil {
		t.Fatal(err)
	}
	msg := nextControl(t, out, MsgEvent)
	if !bytes.Contains(msg.Data, []byte(`"kind":"key"`)) || !bytes.Contains(msg.Data, []byte(`"keycode":29`)) {
		t.Fatalf("got %s; want a key event with keycode 29", msg.Data)
	}
	if bytes.Contains(msg.Data, []byte(`"button"`)) {
		t.Fatalf("got %s; unnamed keycode must not carry a button field", msg.Data)
	}
}

// TestScrollCoalescesWithinTheWindow pins the coalescing scrollPublishInterval
// exists for: a wheel's burst of small deltas has to collapse into one event
// with the accumulated total, not one event per tick -- and the tail of a
// burst that goes quiet has to flush on its own, not wait for an unrelated
// scroll to trigger it. Kills three mutations: setting the window to an hour
// (the trailing flush would then never fire), removing the accumulator reset
// on flush (the trailing event would double-count), and changing the
// accumulator's += to = (the trailing event would report only the last delta,
// 3, instead of the sum, 5).
func TestScrollCoalescesWithinTheWindow(t *testing.T) {
	s := newTestSession(t)
	id, out := s.AddViewer()
	<-out // init

	if err := s.ViewerScroll(id, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 1}); err != nil {
		t.Fatal(err)
	}
	msg := nextControl(t, out, MsgEvent)
	var leading Event
	if err := json.Unmarshal(msg.Data[1:], &leading); err != nil {
		t.Fatal(err)
	}
	if leading.Kind != "scroll" || leading.Scroll == nil || leading.Scroll.Y != 1 {
		t.Fatalf("leading event = %+v; want a scroll event with delta 1", leading)
	}

	// Two more scrolls inside the window must not publish anything of their
	// own; they add to the accumulator the trailing flush will report.
	if err := s.ViewerScroll(id, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.ViewerScroll(id, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 3}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-out:
		t.Fatalf("got a second event inside the coalescing window: %s", msg.Data)
	case <-time.After(50 * time.Millisecond):
	}

	// Past the window, the burst's tail -- 2 + 3 -- flushes on its own, with
	// nothing further arriving to trigger it.
	msg = nextControl(t, out, MsgEvent)
	var trailing Event
	if err := json.Unmarshal(msg.Data[1:], &trailing); err != nil {
		t.Fatal(err)
	}
	if trailing.Kind != "scroll" || trailing.Scroll == nil || trailing.Scroll.Y != 5 {
		t.Fatalf("trailing event = %+v; want a scroll event with accumulated delta 5", trailing)
	}
}

// TestRemoveViewerFlushesAnOpenScrollBurst pins RemoveViewer's own flush,
// separately from the trailing timer TestScrollCoalescesWithinTheWindow
// covers: viewer A scrolls into an open burst and disconnects well inside the
// coalescing window, and viewer B -- still attached -- must see A's tail
// immediately, attributed to A, rather than not at all (RemoveViewer never
// waits for the timer) or after a 250ms delay (a flush that only cancelled
// the timer without also publishing would still lose the tail).
func TestRemoveViewerFlushesAnOpenScrollBurst(t *testing.T) {
	s := newTestSession(t)
	idA, outA := s.AddViewer()
	<-outA // init
	_, outB := s.AddViewer()
	<-outB // init

	if err := s.ViewerScroll(idA, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 1}); err != nil {
		t.Fatal(err)
	}
	nextControl(t, outA, MsgEvent) // A's own leading edge
	nextControl(t, outB, MsgEvent) // and B's copy of it

	if err := s.ViewerScroll(idA, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.ViewerScroll(idA, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 3}); err != nil {
		t.Fatal(err)
	}

	// Well before scrollPublishInterval, so a message arriving here can only
	// be RemoveViewer's own flush, not the trailing timer.
	s.RemoveViewer(idA)

	select {
	case msg, ok := <-outB:
		if !ok {
			t.Fatal("B's channel closed instead of delivering A's flushed scroll")
		}
		if len(msg.Data) == 0 || msg.Data[0] != MsgEvent {
			t.Fatalf("got message type %#x; want MsgEvent", msg.Data[0])
		}
		var e Event
		if err := json.Unmarshal(msg.Data[1:], &e); err != nil {
			t.Fatal(err)
		}
		if e.Viewer != idA {
			t.Fatalf("event.Viewer = %d; want %d (A)", e.Viewer, idA)
		}
		if e.Kind != "scroll" || e.Scroll == nil || e.Scroll.Y != 5 {
			t.Fatalf("flushed event = %+v; want a scroll event with accumulated delta 5", e)
		}
	case <-time.After(scrollPublishInterval / 2):
		t.Fatal("RemoveViewer did not flush A's open scroll burst to B")
	}
}

// TestATrailingBurstThatSumsToZeroStillFlushes pins scrollPending as distinct
// from the accumulator being nonzero. The leading delta of a burst publishes
// immediately and is never added to the accumulator -- only what follows it
// is -- so a burst whose *non-leading* deltas cancel, +2 then -2, leaves
// accH == 0 && accV == 0 even though a real scroll out and back happened.
// Checking the sum instead of a separate pending flag would read that as an
// untouched accumulator and drop the trailing flush entirely.
func TestATrailingBurstThatSumsToZeroStillFlushes(t *testing.T) {
	s := newTestSession(t)
	id, out := s.AddViewer()
	<-out // init

	if err := s.ViewerScroll(id, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 1}); err != nil {
		t.Fatal(err)
	}
	nextControl(t, out, MsgEvent) // leading edge, delta 1, not accumulated

	if err := s.ViewerScroll(id, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.ViewerScroll(id, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: -2}); err != nil {
		t.Fatal(err)
	}

	msg := nextControl(t, out, MsgEvent)
	var trailing Event
	if err := json.Unmarshal(msg.Data[1:], &trailing); err != nil {
		t.Fatal(err)
	}
	if trailing.Kind != "scroll" || trailing.Scroll == nil || trailing.Scroll.Y != 0 {
		t.Fatalf("trailing event = %+v; want a scroll event reporting the accumulated delta 0, not a dropped flush", trailing)
	}
}

// TestPumpTeardownFlushesAnOpenScrollBurst covers pump's own defer, which
// flushes every attached viewer's open scroll burst before their channels
// close -- the same reason RemoveViewer and the trailing timer do, for the
// case neither of those runs: the stream ends (the device disconnects, the
// process shuts down) while a burst is still open and nobody ever calls
// RemoveViewer. Run through the real pump goroutine, not flushViewerScroll
// directly, since the bug this guards against is the flush happening after
// serveViewer's own RemoveViewer has already deleted the viewer, at which
// point RemoveViewer's lookup finds nothing and there is nothing left to
// flush.
func TestPumpTeardownFlushesAnOpenScrollBurst(t *testing.T) {
	ks := &keyStream{frames: make(chan target.Frame)}
	s := &Session{
		ID:       "test",
		key:      "test",
		stream:   ks,
		viewers:  map[uint64]*viewer{},
		done:     make(chan struct{}),
		queueLen: 16,
		init:     InitPayload{CodedWidth: 1080, CodedHeight: 2400},
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 4)}
	s.viewers[v.id] = v

	go s.pump()

	if err := s.ViewerScroll(1, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 1}); err != nil {
		t.Fatal(err)
	}
	nextControl(t, v.out, MsgEvent) // leading edge, delta 1

	if err := s.ViewerScroll(1, ScrollEvent{X: 540, Y: 1200, HScroll: 0, VScroll: 2}); err != nil {
		t.Fatal(err)
	}

	// Ends the stream well inside scrollPublishInterval, so a trailing event
	// arriving here can only be pump's teardown flush, not the timer.
	close(ks.frames)

	select {
	case msg, ok := <-v.out:
		if !ok {
			t.Fatal("viewer channel closed without flushing the open scroll burst")
		}
		var e Event
		if err := json.Unmarshal(msg.Data[1:], &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind != "scroll" || e.Scroll == nil || e.Scroll.Y != 2 {
			t.Fatalf("flushed event = %+v; want a scroll event with delta 2", e)
		}
	case <-time.After(scrollPublishInterval / 2):
		t.Fatal("pump teardown did not flush the open scroll burst before ending")
	}
}

// TestConcurrentPublishEventDeliversStrictlyIncreasingSeq pins why
// PublishEvent numbers an event and fans it out under one critical section
// rather than two: with the lock released between them, two publishers can
// win the numbering race in one order and the fan-out race in the other, so a
// viewer receives a higher seq before a lower one. A drain goroutine reads
// concurrently with several publishers so the fan-out's non-blocking send
// actually contends, which a serial send-then-read never would.
func TestConcurrentPublishEventDeliversStrictlyIncreasingSeq(t *testing.T) {
	s := newTestSession(t)
	id, out := s.AddViewer()
	<-out // init
	_ = id

	const goroutines = 8
	const perGoroutine = 50

	var mu sync.Mutex
	var received []uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case msg, ok := <-out:
				if !ok {
					return
				}
				if len(msg.Data) == 0 || msg.Data[0] != MsgEvent {
					continue
				}
				var e Event
				if err := json.Unmarshal(msg.Data[1:], &e); err != nil {
					t.Errorf("unmarshal: %v", err)
					continue
				}
				mu.Lock()
				received = append(received, e.Seq)
				mu.Unlock()
			case <-time.After(500 * time.Millisecond):
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				s.PublishEvent(Event{Kind: "tap"})
			}
		}()
	}
	wg.Wait()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatal("received no events")
	}
	for i := 1; i < len(received); i++ {
		if received[i] <= received[i-1] {
			t.Fatalf("seq went %d -> %d at index %d; want strictly increasing", received[i-1], received[i], i)
		}
	}
}

// TestConcurrentPublishesDeliverStrictlyIncreasingSeqToTheObserver is
// TestConcurrentPublishEventDeliversStrictlyIncreasingSeq's counterpart for
// the observer rather than the viewer fan-out -- PublishEvent's doc comment
// says numbering, fan-out and the observer call all happen under one
// critical section, and a viewer-only test cannot catch a mutation that
// moves just the observer call outside it. This one installs an observer
// instead of reading a viewer's channel, so it fails on its own if
// s.onEvent(e) is moved after s.mu.Unlock().
func TestConcurrentPublishesDeliverStrictlyIncreasingSeqToTheObserver(t *testing.T) {
	s := newTestSession(t)

	var mu sync.Mutex
	var seen []uint64
	s.SetEventObserver(func(e Event) {
		mu.Lock()
		seen = append(seen, e.Seq)
		mu.Unlock()
	})

	const goroutines = 8
	const perGoroutine = 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				s.PublishEvent(Event{Kind: "tap"})
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != goroutines*perGoroutine {
		t.Fatalf("observer saw %d events; want %d", len(seen), goroutines*perGoroutine)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("observer saw seq go %d -> %d at index %d; want strictly increasing", seen[i-1], seen[i], i)
		}
	}
}

// TestPublishEventDoesNotBlockOnAFullViewer pins why the fan-out to each
// viewer is a non-blocking select rather than a plain send: PublishEvent runs
// under s.mu, so a blocking send to one slow viewer would freeze every other
// viewer and publisher on the session. A viewer here is attached and never
// drained; publishing more events than its channel holds must still return
// promptly, with the overflow counted rather than delivered.
func TestPublishEventDoesNotBlockOnAFullViewer(t *testing.T) {
	s := newTestSession(t)
	_, out := s.AddViewer()
	depth := cap(out)

	const extra = 10
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < depth+extra; i++ {
			s.PublishEvent(Event{Kind: "tap"})
		}
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PublishEvent blocked against a full, undrained viewer")
	}

	// AddViewer queues init and the control state, which occupy two slots,
	// so the viewer's channel can only ever hold depth-2 published events
	// before every further one is dropped.
	want := uint64(depth + extra - (depth - 2))
	if got := s.EventsDroppedCount(); got != want {
		t.Fatalf("EventsDroppedCount() = %d; want %d", got, want)
	}
}

// TestEventRingPageOnAnEmptyRingReportsZero pins one end of oldest/latest:
// before anything has been added, both are 0 -- not the zero value of some
// other type, and not a panic on an empty buf.
func TestEventRingPageOnAnEmptyRingReportsZero(t *testing.T) {
	r := newEventRing(eventRingSize)
	oldest, latest, events := r.page(0)
	if oldest != 0 || latest != 0 {
		t.Fatalf("oldest=%d latest=%d; want 0, 0 on an empty ring", oldest, latest)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v; want none", events)
	}
}

// TestEventRingPageWithOneEventReportsItAsBothBounds pins the case right
// after the empty one: with a single event kept, oldest and latest are the
// same seq.
func TestEventRingPageWithOneEventReportsItAsBothBounds(t *testing.T) {
	r := newEventRing(eventRingSize)
	r.add(Event{Kind: "tap"})
	oldest, latest, events := r.page(0)
	if oldest != 1 || latest != 1 {
		t.Fatalf("oldest=%d latest=%d; want 1, 1 with one event kept", oldest, latest)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v; want the one event", events)
	}
}

// TestEventRingPageFullButNotWrappedReportsOneAndTheCount pins the ring right
// up to capacity, before it has wrapped: oldest is still 1, latest is the
// count added, and nothing has been evicted yet.
func TestEventRingPageFullButNotWrappedReportsOneAndTheCount(t *testing.T) {
	r := newEventRing(eventRingSize)
	for i := 0; i < eventRingSize; i++ {
		r.add(Event{Kind: "tap"})
	}
	oldest, latest, events := r.page(0)
	if oldest != 1 {
		t.Fatalf("oldest = %d; want 1, nothing should have been evicted yet", oldest)
	}
	if latest != eventRingSize {
		t.Fatalf("latest = %d; want %d", latest, eventRingSize)
	}
	if len(events) != eventRingSize {
		t.Fatalf("len(events) = %d; want %d", len(events), eventRingSize)
	}
}

// TestEventRingPageStaysConsistentUnderConcurrentAdd is what makes reading
// oldest/latest under the same lock as events load-bearing rather than
// tidiness. oldest and latest come from page's own snapshot of r.buf, so a
// concurrent add cannot land between that read and the events loop and leave
// them describing two different snapshots of the ring -- which is exactly
// what a caller uses oldest/latest to rule out in the first place (a caller
// that trusted a stale latest could read "caught up" against events it had
// not actually seen). Splitting page's single critical section into two --
// one for oldest/latest, one for the events loop -- reproduces the failure
// this guards against: `go test -race` does not flag it, because each
// individual field access is still lock-protected, but this test's own
// invariant does, deterministically, within a few thousand iterations.
func TestEventRingPageStaysConsistentUnderConcurrentAdd(t *testing.T) {
	r := newEventRing(eventRingSize)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.add(Event{Kind: "tap"})
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	for i := 0; i < 5000; i++ {
		oldest, latest, events := r.page(0)
		if len(events) == 0 {
			continue
		}
		if events[0].Seq != oldest {
			t.Fatalf("events[0].Seq=%d != oldest=%d; page's three return values disagree", events[0].Seq, oldest)
		}
		if events[len(events)-1].Seq != latest {
			t.Fatalf("events[last].Seq=%d != latest=%d; page's three return values disagree", events[len(events)-1].Seq, latest)
		}
	}
}

// TestEventRingPageWrappedReportsOldestPastOne pins the case this whole task
// exists for: past capacity, the ring has evicted its earliest events, so
// oldest has moved past 1 and latest keeps climbing with every add.
func TestEventRingPageWrappedReportsOldestPastOne(t *testing.T) {
	r := newEventRing(eventRingSize)
	extra := 50
	for i := 0; i < eventRingSize+extra; i++ {
		r.add(Event{Kind: "tap"})
	}
	oldest, latest, events := r.page(0)
	if oldest != uint64(extra+1) {
		t.Fatalf("oldest = %d; want %d, the ring should have evicted the first %d", oldest, extra+1, extra)
	}
	if latest != uint64(eventRingSize+extra) {
		t.Fatalf("latest = %d; want %d", latest, eventRingSize+extra)
	}
	if len(events) != eventRingSize {
		t.Fatalf("len(events) = %d; want %d, the ring's full capacity", len(events), eventRingSize)
	}
}

// textStream is a stream that takes text whole, recording what it was sent.
type textStream struct {
	keyStream
	typed []string
}

func (s *textStream) Text(text string) error {
	s.typed = append(s.typed, text)
	return nil
}

// TestViewerTextIsTypedAndPublishedRedacted pins both halves of ViewerText:
// the text reaches the stream whole, and the event carries its length and
// never the text. Publishing it would need the focused-field read that makes
// /api/input's text two seconds slow on Android.
func TestViewerTextIsTypedAndPublishedRedacted(t *testing.T) {
	s := newTestSession(t)
	ts := &textStream{keyStream: keyStream{frames: make(chan target.Frame)}}
	s.stream = ts
	id, out := s.AddViewer()

	if err := s.ViewerText(id, "héllo"); err != nil {
		t.Fatal(err)
	}
	if len(ts.typed) != 1 || ts.typed[0] != "héllo" {
		t.Fatalf("stream was sent %q; want one \"héllo\"", ts.typed)
	}
	msg := nextControl(t, out, MsgEvent)
	for _, want := range []string{`"kind":"text"`, `"redacted":true`, `"textLen":5`} {
		if !bytes.Contains(msg.Data, []byte(want)) {
			t.Fatalf("got %s; want %s", msg.Data, want)
		}
	}
	if bytes.Contains(msg.Data, []byte("llo")) {
		t.Fatalf("got %s; the event carries the typed text", msg.Data)
	}
}

// TestViewerTextOnAStreamThatCannotTypeFails covers a stream with no
// TextTyper: the viewer gets ErrNoControl rather than silence.
func TestViewerTextOnAStreamThatCannotTypeFails(t *testing.T) {
	s := newTestSession(t)
	id, _ := s.AddViewer()
	if err := s.ViewerText(id, "a"); !errors.Is(err, target.ErrNoControl) {
		t.Fatalf("err = %v; want ErrNoControl", err)
	}
}

func TestDecodeTextRejectsWhatItCannotType(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":    {},
		"invalid":  {0xff, 0xfe},
		"too long": bytes.Repeat([]byte("a"), MaxTextLen+1),
	} {
		if _, err := DecodeText(b); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v; want ErrMalformed", name, err)
		}
	}
	if got, err := DecodeText([]byte("ok")); err != nil || got != "ok" {
		t.Fatalf("DecodeText(ok) = %q, %v", got, err)
	}
}

// NormPoint is shared by the agent path, which clamps its result, and the
// viewer scroll path, which does not, so it must itself leave a point past
// the edge unclamped and say so through outside.
func TestNormPoint(t *testing.T) {
	cases := []struct {
		name        string
		x, y, w, h  int
		want        *Point
		wantOutside bool
	}{
		{"centre", 540, 1200, 1080, 2400, &Point{X: 0.5, Y: 0.5}, false},
		{"edges are inside", 1080, 0, 1080, 2400, &Point{X: 1, Y: 0}, false},
		{"past the right edge", 1620, 1200, 1080, 2400, &Point{X: 1.5, Y: 0.5}, true},
		{"above the top", 540, -240, 1080, 2400, &Point{X: 0.5, Y: -0.1}, true},
		{"unknown width", 540, 1200, 0, 2400, nil, false},
		{"unknown height", 540, 1200, 1080, -1, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, outside := NormPoint(c.x, c.y, c.w, c.h)
			if outside != c.wantOutside {
				t.Errorf("outside = %v; want %v", outside, c.wantOutside)
			}
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Errorf("point = %+v; want %+v", got, c.want)
			}
		})
	}
}
