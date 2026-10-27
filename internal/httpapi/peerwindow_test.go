package httpapi

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

func TestDecidePeerWindow(t *testing.T) {
	alive := time.Second
	cases := []struct {
		name  string
		p     peerSpend
		asked bool
		want  peerWindowAction
	}{
		{"fresh", peerSpend{Bytes: 1 << 20, Streams: 100}, false, peerWindowOK},
		{"bytes reach rotate", peerSpend{Bytes: rotateBytes, Streams: 100}, false, peerWindowRotate},
		{"streams reach rotate", peerSpend{Bytes: 1 << 20, Streams: rotateStreams}, false, peerWindowRotate},
		{"rotate is asked once", peerSpend{Bytes: rotateBytes, Streams: 100}, true, peerWindowOK},
		// A stall alone is a bad link, not an exhausted window (trap 21).
		{"stall below the limit", peerSpend{Bytes: 4 << 20, Streams: 100, SinceSent: time.Minute, SinceRecv: time.Minute}, true, peerWindowOK},
		// Past the limit but still delivering: the client may yet move.
		{"exhausted and delivering", peerSpend{Bytes: exhaustedBytes, Streams: 100, SinceSent: time.Second}, true, peerWindowOK},
		{"exhausted bytes and stalled", peerSpend{Bytes: exhaustedBytes, Streams: 100, SinceSent: exhaustedStall}, true, peerWindowClose},
		{"exhausted streams and stalled", peerSpend{Bytes: 1 << 20, Streams: exhaustedStreams, SinceSent: exhaustedStall}, true, peerWindowClose},
		{"close wins over a first rotate", peerSpend{Bytes: exhaustedBytes, Streams: 100, SinceSent: exhaustedStall}, false, peerWindowClose},
		// 12 October: no credit for 85s at 8.46MB, client still talking.
		{"brief credit wait", peerSpend{Bytes: 8 << 20, Streams: 1182, Starved: starveRotate / 2, SinceRecv: alive}, false, peerWindowOK},
		{"starved, client alive", peerSpend{Bytes: 8 << 20, Streams: 1182, Starved: starveRotate, SinceRecv: alive}, false, peerWindowRotate},
		{"starved, already asked", peerSpend{Bytes: 8 << 20, Streams: 1182, Starved: starveRotate, SinceRecv: alive}, true, peerWindowOK},
		{"starved long, client alive", peerSpend{Bytes: 8 << 20, Streams: 1182, Starved: starveClose, SinceRecv: alive}, true, peerWindowClose},
		{"starved long, never asked", peerSpend{Bytes: 8 << 20, Streams: 1182, Starved: starveClose, SinceRecv: alive}, false, peerWindowClose},
		// The client has gone quiet too: a link, not a credit stall.
		{"starved, client quiet", peerSpend{Bytes: 8 << 20, Streams: 1182, Starved: time.Minute, SinceRecv: controlAlive}, false, peerWindowOK},
	}
	for _, c := range cases {
		if got, _ := decidePeerWindow(c.p, c.asked); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// The thresholds have to leave room in the order they are crossed, or the
// backstop would close a connection the client was never asked to leave.
func TestPeerWindowThresholdsAreOrdered(t *testing.T) {
	if !(rotateBytes < exhaustedBytes && exhaustedBytes < 16<<20) {
		t.Errorf("bytes: rotate %d, exhausted %d, limit %d", rotateBytes, exhaustedBytes, 16<<20)
	}
	if !(rotateStreams < exhaustedStreams) {
		t.Errorf("streams: rotate %d, exhausted %d", rotateStreams, exhaustedStreams)
	}
	if !(starveRotate < starveClose) {
		t.Errorf("credit: rotate %v, close %v", starveRotate, starveClose)
	}
}

// windowConn reports a fixed spend and records what the viewer loop does
// with it.
type windowConn struct {
	stalledConn
	mu        sync.Mutex
	spend     peerSpend
	controls  [][]byte
	exhausted int
	// blockControl makes SendControl block until the connection closes,
	// which is what a write on Safari's control stream does once the
	// connection's window is spent.
	blockControl bool
}

// Sends fail once the connection is closed, as a real one's do; the writer
// stops on that, not on Recv ending.
func (c *windowConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}
func (c *windowConn) SendFrame(session.OutMsg, time.Time) error {
	if c.isClosed() {
		return errors.New("closed")
	}
	return nil
}
func (c *windowConn) Keepalive() error {
	if c.isClosed() {
		return errors.New("closed")
	}
	return nil
}
func (c *windowConn) SendControl(b []byte) error {
	if c.blockControl {
		<-c.closed
	}
	if c.isClosed() {
		return errors.New("closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.controls = append(c.controls, append([]byte(nil), b...))
	return nil
}
func (c *windowConn) PeerWindow() peerSpend {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spend
}
func (c *windowConn) CloseExhausted() error {
	c.mu.Lock()
	c.exhausted++
	c.mu.Unlock()
	return c.Close()
}

func (c *windowConn) rotates() []map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]uint64
	for _, b := range c.controls {
		if len(b) > 0 && b[0] == session.MsgRotate {
			var m map[string]uint64
			_ = json.Unmarshal(b[1:], &m)
			out = append(out, m)
		}
	}
	return out
}

func serveWindowConn(t *testing.T, c *windowConn) chan struct{} {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	sess, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	done := make(chan struct{})
	go func() {
		s.serveViewer("stub", sess, c)
		close(done)
	}()
	return done
}

func TestViewerAsksForRotationOnceNearTheLimit(t *testing.T) {
	c := &windowConn{
		stalledConn: stalledConn{fakeConn: fakeConn{name: "wt"}, closed: make(chan struct{})},
		spend:       peerSpend{Bytes: rotateBytes + 1, Streams: 10},
	}
	done := serveWindowConn(t, c)
	defer func() { _ = c.Close(); <-done }()

	// The check runs on each stats tick, once a second; wait for two.
	time.Sleep(2500 * time.Millisecond)
	r := c.rotates()
	if len(r) != 1 {
		t.Fatalf("got %d MsgRotate, want 1", len(r))
	}
	if r[0]["bytes"] != rotateBytes+1 || r[0]["streams"] != 10 {
		t.Errorf("MsgRotate body %v", r[0])
	}
}

func TestViewerClosesAnExhaustedConnection(t *testing.T) {
	c := &windowConn{
		stalledConn: stalledConn{fakeConn: fakeConn{name: "wt"}, closed: make(chan struct{})},
		spend:       peerSpend{Bytes: exhaustedBytes, SinceSent: exhaustedStall},
	}
	done := serveWindowConn(t, c)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = c.Close()
		t.Fatal("serveViewer still running 3s after the window was exhausted")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exhausted != 1 {
		t.Errorf("CloseExhausted called %d times, want 1", c.exhausted)
	}
}

// The writer blocks on its first control write, init or stats, and never
// checks again; the close has to come from somewhere else.
func TestViewerClosesAnExhaustedConnectionWhoseWriterIsStuck(t *testing.T) {
	c := &windowConn{
		stalledConn:  stalledConn{fakeConn: fakeConn{name: "wt"}, closed: make(chan struct{})},
		spend:        peerSpend{Bytes: exhaustedBytes},
		blockControl: true,
	}
	done := serveWindowConn(t, c)
	// Delivering until now: the stall starts here.
	time.Sleep(peerWindowWatch)
	c.mu.Lock()
	c.spend.SinceSent = exhaustedStall
	c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = c.Close()
		<-done
		t.Fatal("serveViewer still running 3s after the window was exhausted")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exhausted != 1 {
		t.Errorf("CloseExhausted called %d times, want 1", c.exhausted)
	}
}

// 12 October: credit stopped at 8.46MB with the client still talking. The
// viewer asks it to move first, and closes once the stall outlasts the move.
func TestViewerRotatesThenClosesACreditStarvedConnection(t *testing.T) {
	c := &windowConn{
		stalledConn: stalledConn{fakeConn: fakeConn{name: "wt"}, closed: make(chan struct{})},
		spend:       peerSpend{Bytes: 8 << 20, Streams: 1182, Starved: starveRotate, SinceRecv: time.Second},
	}
	done := serveWindowConn(t, c)
	time.Sleep(1500 * time.Millisecond)
	if r := c.rotates(); len(r) != 1 {
		t.Fatalf("got %d MsgRotate, want 1", len(r))
	}
	c.mu.Lock()
	c.spend.Starved = starveClose
	c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = c.Close()
		<-done
		t.Fatal("serveViewer still running 3s after credit stopped for good")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exhausted != 1 {
		t.Errorf("CloseExhausted called %d times, want 1", c.exhausted)
	}
}
