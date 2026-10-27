package httpapi

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// recordingConn logs each control message, CloseWithReason and Close, in
// order. Its first SendControl blocks on gate, if there is one, so a test can
// queue messages behind a stuck writer.
type recordingConn struct {
	fakeConn
	gate   chan struct{}
	mu     sync.Mutex
	events []string
	closed chan struct{}
	once   sync.Once
	first  sync.Once
}

func (c *recordingConn) log(s string) {
	c.mu.Lock()
	c.events = append(c.events, s)
	c.mu.Unlock()
}

func (c *recordingConn) SendControl(b []byte) error {
	if c.gate != nil {
		c.first.Do(func() { <-c.gate })
	}
	c.log(fmt.Sprintf("ctl:%#x", b[0]))
	return nil
}
func (c *recordingConn) Recv() ([]byte, error) {
	<-c.closed
	return nil, io.EOF
}
func (c *recordingConn) CloseWithReason(code uint32, msg string) {
	c.log(fmt.Sprintf("reason:%d:%s", code, msg))
}
func (c *recordingConn) Close() error {
	c.once.Do(func() {
		c.log("close")
		close(c.closed)
	})
	return nil
}

func (c *recordingConn) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

// endedFixture serves one viewer whose writer is stuck on its first control
// message, queues a burst behind it, ends the session, and then lets the
// writer go. It returns what the conn saw once serveViewer has returned.
func endedFixture(t *testing.T, reason string) []string {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	sess, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	conn := &recordingConn{fakeConn: fakeConn{name: "rec"}, gate: make(chan struct{}), closed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		s.serveViewer("stub", sess, conn)
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for sess.ViewerCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("viewer never attached")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The writer is (or soon will be) blocked on the init message. Queue a
	// burst of control messages behind it, so that when it wakes the queue
	// holds many and Done is already closed.
	for i := 0; i < 10; i++ {
		sess.PublishEvent(session.Event{Kind: "tap"})
	}
	if err := sess.End(reason); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session not done")
	}
	close(conn.gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveViewer still running 3s after End")
	}
	return conn.snapshot()
}

// The writer must flush queued control messages once the session is done: its
// select chooses at random between a ready queue and Done, so without the
// flush MsgEnded behind a burst is usually lost.
func TestEndedReachesTheViewerBeforeItsConnectionCloses(t *testing.T) {
	ev := endedFixture(t, "deleted")
	end := -1
	for i, e := range ev {
		if e == "ctl:0xa" {
			end = i
		}
	}
	if end < 0 {
		t.Fatalf("MsgEnded never written: %v", ev)
	}
	for i, e := range ev {
		if e == "close" && i < end {
			t.Fatalf("close before MsgEnded: %v", ev)
		}
	}
	if ev[len(ev)-1] != "close" {
		t.Errorf("close not last: %v", ev)
	}
}

// A transport that cannot count on MsgEnded arriving is closed with the
// reason, code 3.
func TestEndedClosesTheTransportWithItsReason(t *testing.T) {
	if wtEnded != 3 {
		t.Fatalf("wtEnded = %d, want 3", wtEnded)
	}
	for _, reason := range []string{"shutDown", "deleted"} {
		ev := endedFixture(t, reason)
		want := fmt.Sprintf("reason:%d:%s", wtEnded, reason)
		n := len(ev)
		if n < 2 || ev[n-2] != want || ev[n-1] != "close" {
			t.Errorf("%s: events = %v, want %q then close at the end", reason, ev, want)
		}
	}
}
