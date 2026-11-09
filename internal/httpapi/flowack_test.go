package httpapi

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

func TestAnsweredInlineIncludesFrameAck(t *testing.T) {
	if !answeredInline(session.MsgFrameAck) {
		t.Fatal("a frame ack waits behind queued touches, so its time reads as link queueing")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:5000": true, "[::1]:5000": true, "127.0.0.1": true,
		"192.168.1.20:5000": false, "100.101.2.3:443": false, "[fe80::1]:1": false, "": false, "not-an-addr": false,
	} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func frameAckPayload(pts uint64) []byte {
	b := make([]byte, 9)
	b[0] = session.MsgFrameAck
	binary.BigEndian.PutUint64(b[1:], pts)
	return b
}

// flowTarget is a target whose frames the test pushes by hand.
type flowTarget struct{ frames chan target.Frame }

func (f *flowTarget) ID() string        { return "flow" }
func (f *flowTarget) Kind() target.Kind { return target.KindSynthetic }
func (f *flowTarget) Label() string     { return "flow" }
func (f *flowTarget) Open(context.Context, target.Options) (target.Stream, error) {
	return &flowStream{frames: f.frames}, nil
}

type flowStream struct {
	frames chan target.Frame
	once   sync.Once
}

func (s *flowStream) Meta() target.Meta           { return target.Meta{Width: 100, Height: 100} }
func (s *flowStream) Frames() <-chan target.Frame { return s.frames }
func (s *flowStream) Touch(target.Touch) error    { return target.ErrNoControl }
func (s *flowStream) Scroll(target.Scroll) error  { return target.ErrNoControl }
func (s *flowStream) Key(target.Key) error        { return target.ErrNoControl }
func (s *flowStream) RequestKeyframe() error      { return nil }
func (s *flowStream) Err() error                  { return nil }
func (s *flowStream) Close() error                { s.once.Do(func() { close(s.frames) }); return nil }

// ackConn records the frames it is sent and hands the viewer whatever the test
// queues for Recv, as the read loop would receive it.
type ackConn struct {
	fakeConn
	mu     sync.Mutex
	frames []session.OutMsg
	recv   chan []byte
	closed chan struct{}
	once   sync.Once
}

func newAckConn() *ackConn {
	return &ackConn{fakeConn: fakeConn{name: "ack"}, recv: make(chan []byte, 8), closed: make(chan struct{})}
}
func (c *ackConn) SendFrame(f session.OutMsg, _ time.Time) error {
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
	return nil
}
func (c *ackConn) sentFrames() []session.OutMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]session.OutMsg(nil), c.frames...)
}
func (c *ackConn) Recv() ([]byte, error) {
	select {
	case b := <-c.recv:
		return b, nil
	case <-c.closed:
		return nil, io.EOF
	}
}
func (c *ackConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestViewerAckMeasuresQueue drives frames through the real write loop and an
// ack through the real read loop (so through answeredInline and dispatch), and
// checks the ack is matched to the frame the writer recorded: the pts in the
// frame the connection was sent is the pts the client would ack.
func TestViewerAckMeasuresQueue(t *testing.T) {
	ft := &flowTarget{frames: make(chan target.Frame, 8)}
	m := NewManager(target.Options{})
	m.Register(ft)
	s := &Server{mgr: m}
	sess, err := m.Acquire(t.Context(), "flow")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	conn := newAckConn()
	done := make(chan struct{})
	go func() { s.serveViewer("flow", sess, conn); close(done) }()
	defer func() {
		// The writer ends with the session, not with the connection.
		_ = conn.Close()
		sess.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("serveViewer still running 3s after the session closed")
		}
	}()

	// Viewer ids count from 1 and this is the only viewer.
	const viewerID = 1
	var flow *session.FlowEst
	waitUntil(t, "viewer attach", func() bool { flow, _ = sess.ViewerFlow(viewerID); return flow != nil })

	ft.frames <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}}
	ft.frames <- target.Frame{Key: true, PTS: 1 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x65, 1}}
	ft.frames <- target.Frame{PTS: 2 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, 2}}
	waitUntil(t, "three frames sent", func() bool { return len(conn.sentFrames()) == 3 })

	sent := conn.sentFrames()
	if got := flow.Snapshot(time.Now()); got.Backlog != 2 || got.Acking {
		t.Fatalf("before any ack: backlog %d acking %v, want 2 false (config frames are not recorded)", got.Backlog, got.Acking)
	}
	// The /ws envelope's pts and OutMsg.PTSMicros, which /wt puts in its own
	// header, are the same number: the one the client acks.
	if env := binary.BigEndian.Uint64(sent[1].Data[2:]); env != sent[1].PTSMicros || env != 1000 {
		t.Fatalf("envelope pts %d, OutMsg pts %d, want both 1000", env, sent[1].PTSMicros)
	}

	conn.recv <- frameAckPayload(sent[1].PTSMicros)
	waitUntil(t, "ack applied", func() bool { return flow.Snapshot(time.Now()).Backlog == 1 })
	if got := flow.Snapshot(time.Now()); !got.Acking {
		t.Fatal("viewer should be acking after its first ack")
	}

	// An ack for a pts never sent is ignored, not read as acking everything.
	conn.recv <- frameAckPayload(99999)
	time.Sleep(20 * time.Millisecond)
	if got := flow.Snapshot(time.Now()); got.Backlog != 1 {
		t.Fatalf("unknown ack changed backlog to %d, want 1", got.Backlog)
	}

	// A new config frame (encoder rebuilt) clears what is in flight.
	ft.frames <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}}
	waitUntil(t, "config reset", func() bool { return flow.Snapshot(time.Now()).Backlog == 0 })
}

func TestDispatchFrameAckIgnoresShortBody(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	sess, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	id, out := sess.AddViewer()
	<-out
	s := &Server{mgr: m}
	if err := s.dispatch(nil, sess, id, []byte{session.MsgFrameAck, 1, 2}); err != nil {
		t.Fatalf("short ack returned %v, want nil", err)
	}
	if err := s.dispatch(nil, sess, 9999, frameAckPayload(1)); err != nil {
		t.Fatalf("ack for a gone viewer returned %v, want nil", err)
	}
}
