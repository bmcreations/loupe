package httpapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// remoteStream is a flowStream with a remote track, recording the keyframe
// requests each encoder gets.
type remoteStream struct {
	flowStream
	remote chan target.Frame
	mu     sync.Mutex
	mainKeys, remoteKeys,
	gateOps int
}

func (s *remoteStream) Remote() <-chan target.Frame { return s.remote }
func (s *remoteStream) RemoteGate(bool) error       { s.mu.Lock(); s.gateOps++; s.mu.Unlock(); return nil }
func (s *remoteStream) RemoteKeyframe() error       { s.mu.Lock(); s.remoteKeys++; s.mu.Unlock(); return nil }
func (s *remoteStream) RemoteCap(int) error         { return nil }
func (s *remoteStream) RemoteMaxSize(int) error     { return nil }
func (s *remoteStream) RequestKeyframe() error      { s.mu.Lock(); s.mainKeys++; s.mu.Unlock(); return nil }
func (s *remoteStream) keys() (main, remote int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mainKeys, s.remoteKeys
}

type remoteTarget struct {
	flowTarget
	st *remoteStream
}

func (t *remoteTarget) Open(context.Context, target.Options) (target.Stream, error) {
	return t.st, nil
}

// startRemoteViewer opens a session over a stream with a remote track and
// attaches one remote viewer through the real write loop.
func startRemoteViewer(t *testing.T, conn Conn) (*session.Session, *remoteStream, chan target.Frame) {
	t.Helper()
	st := &remoteStream{flowStream: flowStream{frames: make(chan target.Frame, 8)}, remote: make(chan target.Frame, 16)}
	m := NewManager(target.Options{})
	m.Register(&remoteTarget{st: st})
	s := &Server{mgr: m}
	sess, err := m.Acquire(t.Context(), "flow")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	done := make(chan struct{})
	go func() {
		s.serveViewerWith("flow", sess, conn, session.ViewerOptions{Acks: true})
		close(done)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		sess.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("serveViewer still running 3s after the session closed")
		}
	})
	waitUntil(t, "viewer attach", func() bool { return sess.ViewerTrack(1) == "remote" })
	return sess, st, st.remote
}

// A remote-track viewer is sent a delta that the main track's rules would shed
// as stale: its oldest unacked frame is 600 ms old on a 100 ms path, and the
// frame sat 300 ms in its queue behind the keyframe write. The pacer is no
// gate either.
func TestRemoteViewerWriteLoopDoesNotShedStaleOrPaced(t *testing.T) {
	conn := &staleConn{ackConn: newAckConn(), gate: make(chan struct{})}
	sess, _, remote := startRemoteViewer(t, conn)
	flow, _ := sess.ViewerFlow(1)
	remote <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}}
	waitUntil(t, "config sent", func() bool { return len(conn.sentFrames()) >= 1 })
	remote <- target.Frame{Key: true, PTS: time.Millisecond, Data: []byte{0, 0, 0, 1, 0x65, 1}}
	for i := 2; i < 8; i++ {
		remote <- target.Frame{PTS: time.Duration(i) * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, byte(i)}}
	}
	time.Sleep(300 * time.Millisecond)
	now := time.Now()
	ackingViewer(flow, now, 50*time.Millisecond, 600*time.Millisecond)
	close(conn.gate)
	defer func() {
		if t.Failed() {
			for _, f := range conn.sentFrames() {
				t.Logf("sent key=%v cfg=%v pts=%d", f.Key, f.Config, f.PTSMicros)
			}
			t.Logf("shed %+v wait %d stats %v", sess.ShedBreakdown(), sess.WaitCount(), sess.Stats())
		}
	}()
	waitUntil(t, "all deltas sent", func() bool { return len(conn.sentFrames()) == 8 })
	b := sess.ShedBreakdown()
	if b.Stale != 0 || b.Pacer != 0 || b.AwaitKey != 0 || b.Abandon != 0 {
		t.Fatalf("remote viewer shed %+v", b)
	}
}

// abandonConn resets the delta frame with pts abandonPTS.
type abandonConn struct {
	*ackConn
	abandonPTS uint64
}

func (c *abandonConn) SendFrame(f session.OutMsg, d time.Time) error {
	if f.PTSMicros == c.abandonPTS {
		return errFrameAbandoned
	}
	return c.ackConn.SendFrame(f, d)
}

// A lost frame is the one thing that breaks a remote viewer's chain: it asks
// the remote encoder for a keyframe, not the main one, waits for it, and
// decodes again from it.
func TestRemoteViewerAbandonAsksRemoteKeyframeAndWaits(t *testing.T) {
	conn := &abandonConn{ackConn: newAckConn(), abandonPTS: 3000}
	sess, st, remote := startRemoteViewer(t, conn)
	waitUntil(t, "join key", func() bool { _, r := st.keys(); return r == 1 })

	remote <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}}
	remote <- target.Frame{Key: true, PTS: time.Millisecond, Data: []byte{0, 0, 0, 1, 0x65, 1}}
	remote <- target.Frame{PTS: 2 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, 2}}
	remote <- target.Frame{PTS: 3 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, 3}} // abandoned
	remote <- target.Frame{PTS: 4 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, 4}} // waits for key
	waitUntil(t, "abandon seen", func() bool {
		b := sess.ShedBreakdown()
		return b.Abandon == 1 && b.AwaitKey == 1
	})
	waitUntil(t, "remote key asked", func() bool { _, r := st.keys(); return r == 2 })
	if m, _ := st.keys(); m != 0 {
		t.Fatalf("main encoder asked for %d keyframes", m)
	}
	remote <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}}
	remote <- target.Frame{Key: true, PTS: 5 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x65, 5}}
	remote <- target.Frame{PTS: 6 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, 6}}
	waitUntil(t, "resumed", func() bool {
		for _, f := range conn.sentFrames() {
			if f.PTSMicros == 6000 {
				return true
			}
		}
		return false
	})
	if b := sess.ShedBreakdown(); b.Stale != 0 || b.Pacer != 0 {
		t.Fatalf("shed %+v", b)
	}
}
