package session

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// rotStream is a stream whose dimensions change under the session, the way a
// real one's do when the device turns over.
type rotStream struct {
	mu     sync.RWMutex
	meta   target.Meta
	frames chan target.Frame
}

func (r *rotStream) Meta() target.Meta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.meta
}

func (r *rotStream) rotate(w, h int) {
	r.mu.Lock()
	r.meta.Width, r.meta.Height = w, h
	r.mu.Unlock()
}

func (r *rotStream) Frames() <-chan target.Frame { return r.frames }
func (r *rotStream) Touch(target.Touch) error    { return nil }
func (r *rotStream) Scroll(target.Scroll) error  { return nil }
func (r *rotStream) Key(target.Key) error        { return nil }
func (r *rotStream) RequestKeyframe() error      { return nil }
func (r *rotStream) Err() error                  { return nil }
func (r *rotStream) Close() error                { return nil }

func initSizeFrom(t *testing.T, msg OutMsg) (int, int) {
	t.Helper()
	if !msg.Control || len(msg.Data) == 0 || msg.Data[0] != MsgInit {
		t.Fatalf("not an init message: control=%v", msg.Control)
	}
	var p InitPayload
	if err := json.Unmarshal(msg.Data[1:], &p); err != nil {
		t.Fatalf("decode init: %v", err)
	}
	return p.CodedWidth, p.CodedHeight
}

// The dimensions are emulator-5554's own, measured either side of a rotation
// with cmd/rotprobe: 480x1080 portrait, 1080x480 landscape.
//
// The client has no other way to learn this. scrcpy sends its codec meta once
// at stream start and never resends it, and the stream does not end, so
// without a fresh init the canvas keeps the old shape for the rest of the
// session.
func TestARotationRepublishesInitToEveryViewer(t *testing.T) {
	rs := &rotStream{meta: target.Meta{Width: 480, Height: 1080}}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  rs,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
		init:    InitPayload{CodedWidth: 480, CodedHeight: 1080},
	}

	v := &viewer{id: 1, out: make(chan OutMsg, 4)}
	s.viewers[v.id] = v

	rs.rotate(1080, 480)
	s.noteSize(rs.Meta())

	select {
	case msg := <-v.out:
		w, h := initSizeFrom(t, msg)
		if w != 1080 || h != 480 {
			t.Fatalf("init reported %dx%d, want 1080x480", w, h)
		}
	default:
		t.Fatal("no init was sent, so the viewer keeps a 480x1080 canvas for a 1080x480 picture")
	}

	if s.init.CodedWidth != 1080 || s.init.CodedHeight != 480 {
		t.Fatalf("cached init is %dx%d, want 1080x480: a later joiner would get the stale size",
			s.init.CodedWidth, s.init.CodedHeight)
	}
}

// A parameter set arrives on every reconnect and whenever the encoder resends
// its parameter sets, which on an idle panel is many times a minute. Init is
// unsheddable and makes the client reconfigure its decoder, so republishing it
// on a size that did not change would rebuild the decoder for nothing.
func TestAParameterSetAtTheSameSizeRepublishesNothing(t *testing.T) {
	rs := &rotStream{meta: target.Meta{Width: 480, Height: 1080}}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  rs,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
		init:    InitPayload{CodedWidth: 480, CodedHeight: 1080},
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 4)}
	s.viewers[v.id] = v

	for i := 0; i < 5; i++ {
		s.noteSize(rs.Meta())
	}

	if len(v.out) != 0 {
		t.Fatalf("%d init messages sent for a stream that never changed shape", len(v.out))
	}
}

// Meta is read from a live stream, and a stream that has ended or has not yet
// read its header reports zero. Publishing that would set the canvas to 0x0,
// which is a black screen rather than a visible failure.
func TestAZeroSizedMetaIsIgnored(t *testing.T) {
	s := &Session{
		ID:      "test",
		key:     "test",
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
		init:    InitPayload{CodedWidth: 480, CodedHeight: 1080},
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 4)}
	s.viewers[v.id] = v

	s.noteSize(target.Meta{})
	s.noteSize(target.Meta{Width: 1080})

	if len(v.out) != 0 {
		t.Fatal("a zero dimension was published")
	}
	if s.init.CodedWidth != 480 || s.init.CodedHeight != 1080 {
		t.Fatalf("cached init was overwritten with %dx%d", s.init.CodedWidth, s.init.CodedHeight)
	}
}

// The pump must call noteSize off the config frame, not just at stream start.
// This is the wiring, which the unit tests above do not cover: without it
// noteSize is correct and never runs.
func TestThePumpRepublishesInitWhenAConfigFrameChangesTheSize(t *testing.T) {
	rs := &rotStream{
		meta:   target.Meta{Width: 480, Height: 1080},
		frames: make(chan target.Frame, 4),
	}
	s := &Session{
		ID:       "test",
		key:      "test",
		stream:   rs,
		viewers:  map[uint64]*viewer{},
		done:     make(chan struct{}),
		queueLen: 16,
		init:     InitPayload{CodedWidth: 480, CodedHeight: 1080},
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 16)}
	s.viewers[v.id] = v

	go s.pump()

	// The stream reads the new size off the parameter set before forwarding
	// it, which is the ordering adbStream.pump guarantees.
	rs.rotate(1080, 480)
	rs.frames <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}, Captured: time.Now()}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		w, h := s.init.CodedWidth, s.init.CodedHeight
		s.mu.RUnlock()
		if w == 1080 && h == 480 {
			close(rs.frames)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(rs.frames)
	t.Fatal("the pump never republished the size after a config frame")
}

// The pump counts a config frame in both FrameCount and ConfigCount, never
// only the first. internal/httpapi's healthTracker subtracts ConfigCount
// from FrameCount to get media-only produced frames (finding 5: a resent
// parameter set is never decoded into a picture, so it must not read as an
// undelivered frame) -- but healthTracker's own tests drive that arithmetic
// against a fake counters struct, which proves nothing about whether the
// real pump increments s.configIn at all. This is the wiring one layer
// below the fake: if `s.configIn.Add(1)` were ever dropped from the pump,
// every healthTracker unit test would stay green while ConfigCount() read 0
// against every real session.
func TestThePumpCountsConfigFramesSeparatelyFromMediaFrames(t *testing.T) {
	rs := &rotStream{
		meta:   target.Meta{Width: 480, Height: 1080},
		frames: make(chan target.Frame, 4),
	}
	s := &Session{
		ID:       "test",
		key:      "test",
		stream:   rs,
		viewers:  map[uint64]*viewer{},
		done:     make(chan struct{}),
		queueLen: 16,
	}
	v := &viewer{id: 1, out: make(chan OutMsg, 16)}
	s.viewers[v.id] = v

	go s.pump()

	rs.frames <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}, Captured: time.Now()}
	rs.frames <- target.Frame{Data: []byte{0, 0, 0, 1, 0x41}, Captured: time.Now()}
	rs.frames <- target.Frame{Data: []byte{0, 0, 0, 1, 0x41}, Captured: time.Now()}

	deadline := time.Now().Add(2 * time.Second)
	// Wait on MediaCount as well: the pump adds to framesIn before it adds
	// to mediaIn, so FrameCount reaching 3 does not mean the third frame
	// has been counted as media yet.
	for time.Now().Before(deadline) && (s.FrameCount() < 3 || s.MediaCount() < 2) {
		time.Sleep(5 * time.Millisecond)
	}
	close(rs.frames)

	if got := s.FrameCount(); got != 3 {
		t.Fatalf("FrameCount() = %d, want 3 (1 config frame + 2 media frames)", got)
	}
	if got := s.ConfigCount(); got != 1 {
		t.Fatalf("ConfigCount() = %d, want 1: a mutation dropping s.configIn.Add(1) from the pump reads 0 here", got)
	}
	if got := s.MediaCount(); got != 2 {
		t.Fatalf("MediaCount() = %d, want 2: the health verdict's produced count must exclude the config frame", got)
	}
}
