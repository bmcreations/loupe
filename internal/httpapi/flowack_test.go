package httpapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
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

func traceTick(t *testing.T, flow session.FlowStats, local bool) (map[string]any, map[string]any) {
	t.Helper()
	attach := time.Now()
	tr := newHealthTracker(attach, func() time.Time { return attach.Add(time.Second) })
	sess := fakeStatsSource{media: 60, frameCount: 60}
	body, rec, err := runStatsTick("t", sess, tr, session.NewPacer(), fakeConn{name: "ws"}, "client", flow, local)
	if err != nil {
		t.Fatalf("runStatsTick: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return rec, msg
}

var flowTraceKeys = []string{"acking", "queueMs", "baseMs", "backlog", "oldestMs", "ackBps"}

func TestTraceRecordCarriesFlowNumbersForAckingViewer(t *testing.T) {
	flow := session.FlowStats{Acking: true, Backlog: 4, OldestMs: 91.26, QueueMs: 37.04, BaseMs: 52.5, DeliveredBps: 1_234_567.8}
	rec, msg := traceTick(t, flow, true)
	for _, k := range flowTraceKeys {
		if _, ok := rec[k]; !ok {
			t.Errorf("trace record has no %q", k)
		}
	}
	if rec["acking"] != true || rec["backlog"] != 4 || rec["queueMs"] != 37.0 || rec["baseMs"] != 52.5 || rec["oldestMs"] != 91.3 || rec["ackBps"] != int64(1_234_567) {
		t.Errorf("unexpected flow fields: %v", rec)
	}
	if rec["local"] != true {
		t.Errorf("local = %v, want true", rec["local"])
	}
	if msg["queueMs"] != 37.0 || msg["backlog"] != 4.0 {
		t.Errorf("MsgStats queueMs %v backlog %v, want 37 and 4", msg["queueMs"], msg["backlog"])
	}
}

func TestTraceRecordOmitsFlowNumbersForViewerThatNeverAcks(t *testing.T) {
	rec, msg := traceTick(t, session.FlowStats{Backlog: 9, OldestMs: 500}, false)
	for _, k := range flowTraceKeys {
		if _, ok := rec[k]; ok {
			t.Errorf("trace record has %q for a viewer that never acked", k)
		}
	}
	for _, k := range []string{"queueMs", "backlog"} {
		if _, ok := msg[k]; ok {
			t.Errorf("MsgStats has %q for a viewer that never acked", k)
		}
	}
	if v, ok := rec["local"]; !ok || v != false {
		t.Errorf("local = %v (present %v), want false", v, ok)
	}
}

// staleConn is an ackConn on a 100 ms path whose first keyframe write blocks
// until released, so deltas queued meanwhile age in the viewer's queue by a
// known amount, as they do behind a large keyframe write on a phone.
type staleConn struct {
	*ackConn
	gate chan struct{}
}

func (c *staleConn) PathRTT() time.Duration { return 100 * time.Millisecond }
func (c *staleConn) SendFrame(f session.OutMsg, d time.Time) error {
	if f.Key {
		<-c.gate
	}
	return c.ackConn.SendFrame(f, d)
}

// runStaleCase attaches a viewer, lets prep shape its FlowEst, then queues one
// delta behind a blocked keyframe for queuedFor and releases the write. It
// returns the number of delta frames the connection was sent, the session and
// the viewer's id.
func runStaleCase(t *testing.T, queuedFor time.Duration, prep func(flow *session.FlowEst, now time.Time)) (deltasSent int, sess *session.Session) {
	t.Helper()
	ft := &flowTarget{frames: make(chan target.Frame, 8)}
	m := NewManager(target.Options{})
	m.Register(ft)
	s := &Server{mgr: m}
	sess, err := m.Acquire(t.Context(), "flow")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	conn := &staleConn{ackConn: newAckConn(), gate: make(chan struct{})}
	done := make(chan struct{})
	go func() { s.serveViewer("flow", sess, conn); close(done) }()
	t.Cleanup(func() {
		_ = conn.Close()
		sess.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("serveViewer still running 3s after the session closed")
		}
	})
	var flow *session.FlowEst
	waitUntil(t, "viewer attach", func() bool { flow, _ = sess.ViewerFlow(1); return flow != nil })

	// The config frame resets the in-flight list, so the flow is shaped after
	// it, and after the wait so the ages prep sets are the ones the write loop
	// sees.
	ft.frames <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}}
	waitUntil(t, "config sent", func() bool { return len(conn.sentFrames()) == 1 })
	ft.frames <- target.Frame{Key: true, PTS: 1 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x65, 1}}
	ft.frames <- target.Frame{PTS: 2 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, 2}}
	time.Sleep(queuedFor)
	prep(flow, time.Now())
	close(conn.gate)
	waitUntil(t, "delta handled", func() bool {
		return len(conn.sentFrames()) >= 3 || sess.ShedBreakdown().Stale > 0
	})
	for _, f := range conn.sentFrames() {
		if !f.Key && !f.Config {
			deltasSent++
		}
	}
	return deltasSent, sess
}

// ackingViewer makes flow acking with its last ack lastAckAgo ago and its
// oldest unacked frame oldestMs old. The unacked entry is recorded before the
// keyframe is written, so it stays the oldest.
func ackingViewer(flow *session.FlowEst, now time.Time, lastAckAgo, oldest time.Duration) {
	at := now.Add(-lastAckAgo)
	flow.Sent(800, 100, at.Add(-60*time.Millisecond))
	flow.Ack(800, at)
	flow.Sent(900, 100, now.Add(-oldest))
}

func TestStaleUsesAckAgeWhenAcking(t *testing.T) {
	// 300 ms in the queue would be shed by the QueuedAt rule (~200 ms here);
	// the oldest unacked frame is only 150 ms old, under rtt+120 = 220.
	sent, sess := runStaleCase(t, 300*time.Millisecond, func(f *session.FlowEst, now time.Time) {
		ackingViewer(f, now, 50*time.Millisecond, 150*time.Millisecond)
	})
	if sent != 1 || sess.ShedBreakdown().Stale != 0 {
		t.Fatalf("deltas sent %d, shed stale %d, want 1 and 0", sent, sess.ShedBreakdown().Stale)
	}
}

func TestStaleAckAgeExceeded(t *testing.T) {
	sent, sess := runStaleCase(t, 50*time.Millisecond, func(f *session.FlowEst, now time.Time) {
		ackingViewer(f, now, 50*time.Millisecond, 400*time.Millisecond)
	})
	if sent != 0 || sess.ShedBreakdown().Stale != 1 {
		t.Fatalf("deltas sent %d, shed stale %d, want 0 and 1", sent, sess.ShedBreakdown().Stale)
	}
}

func TestStaleFallsBackWhenAcksStop(t *testing.T) {
	// Acked 5 s ago (hidden tab): the old rule applies and 300 ms in the
	// queue is stale even though the oldest unacked frame is young.
	sent, sess := runStaleCase(t, 300*time.Millisecond, func(f *session.FlowEst, now time.Time) {
		ackingViewer(f, now, 5*time.Second, 10*time.Millisecond)
	})
	if sent != 0 || sess.ShedBreakdown().Stale != 1 {
		t.Fatalf("deltas sent %d, shed stale %d, want 0 and 1", sent, sess.ShedBreakdown().Stale)
	}
}

func TestStaleBackstop(t *testing.T) {
	sent, sess := runStaleCase(t, 1100*time.Millisecond, func(f *session.FlowEst, now time.Time) {
		ackingViewer(f, now, 50*time.Millisecond, 10*time.Millisecond)
	})
	if sent != 0 || sess.ShedBreakdown().Stale != 1 {
		t.Fatalf("deltas sent %d, shed stale %d, want 0 and 1", sent, sess.ShedBreakdown().Stale)
	}
}

// TestFrameStaleRules pins the decision itself, including the needKey half
// that the loop applies to any true result.
func TestFrameStaleRules(t *testing.T) {
	rtt, base := 100*time.Millisecond, 100*time.Millisecond
	acking := func(oldest float64, ago time.Duration) session.FlowStats {
		return session.FlowStats{Acking: true, OldestMs: oldest, LastAckAgo: ago}
	}
	for _, c := range []struct {
		name   string
		flow   session.FlowStats
		queued time.Duration
		rtt    time.Duration
		stale  bool
		rule   string
	}{
		{"ack young oldest", acking(219, 0), 300 * time.Millisecond, rtt, false, "ack"},
		{"ack old oldest", acking(221, 0), 0, rtt, true, "ack"},
		{"ack no rtt uses base", acking(221, 0), 0, 0, true, "ack"},
		{"backstop", acking(0, 0), 1001 * time.Millisecond, rtt, true, "ack"},
		{"quiet viewer queued rule", acking(0, 4001*time.Millisecond), 300 * time.Millisecond, rtt, true, "queued"},
		{"quiet viewer fresh frame", acking(900, 4001*time.Millisecond), 10 * time.Millisecond, rtt, false, "queued"},
		{"never acks", session.FlowStats{OldestMs: 900}, 10 * time.Millisecond, rtt, false, "queued"},
	} {
		stale, rule := frameStale(c.flow, c.queued, c.rtt, base)
		if stale != c.stale || rule != c.rule {
			t.Errorf("%s: stale %v rule %q, want %v %q", c.name, stale, rule, c.stale, c.rule)
		}
	}
}

func TestTraceRecordNamesStaleRule(t *testing.T) {
	rec, _ := traceTick(t, session.FlowStats{Acking: true}, false)
	if rec["staleRule"] != "ack" {
		t.Errorf("staleRule = %v, want ack", rec["staleRule"])
	}
	rec, _ = traceTick(t, session.FlowStats{Acking: true, LastAckAgo: 5 * time.Second}, false)
	if rec["staleRule"] != "queued" {
		t.Errorf("staleRule = %v, want queued after 5 s of silence", rec["staleRule"])
	}
	rec, _ = traceTick(t, session.FlowStats{}, false)
	if _, ok := rec["staleRule"]; ok {
		t.Error("staleRule present for a viewer that never acks")
	}
}
