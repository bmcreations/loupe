package httpapi

import (
	"encoding/json"
	"time"

	"github.com/bmcreations/loupe/internal/session"
)

// Safari never raises a QUIC connection's flow-control limits (trap 52,
// WebKit bug 319818): it grants 16 MiB of stream data and, by that report,
// about 7600 streams, and a connection that reaches either delivers nothing
// more. The server cannot raise a window the peer owns, so it counts what it
// has spent against both and asks the client to move to a fresh connection
// before either runs out.
//
// The counting is here and not in the client because a frame reset at its
// deadline spends the peer's window and never reaches the client: the run
// that froze at 14.27MB of delivered bytes had five of them.
const (
	// rotateBytes leaves 4 MiB for the client to open a second connection and
	// receive a keyframe on it, which is about 10s at synthetic's 3.3 Mbps.
	rotateBytes = 12 << 20
	// rotateStreams leaves about 1600 frames, 27s at 60fps.
	rotateStreams = 6000

	// exhaustedBytes and exhaustedStreams are close enough to the limits that
	// a connection past them and not delivering is stuck for good.
	exhaustedBytes   = 16<<20 - 512<<10
	exhaustedStreams = 7400
	// exhaustedStall is how long such a connection must go without
	// delivering a frame before it is closed. The count gates the close, not
	// the stall alone: a stall by itself is what a bad cellular link looks
	// like, and closing on that is trap 21's reconnect.
	exhaustedStall = 2 * time.Second

	// Safari can also stop granting stream credit well short of either
	// limit. On the phone on 12 October the room to open streams shrank from
	// 84 to 8 over 18s and then no grant came for 85s, at 8.46MB and 1182
	// streams, while its stats kept arriving on the control stream at an rtt
	// of 9ms. Every frame after that waited for credit until its deadline. A
	// fresh connection starts with a fresh allowance, so a connection that
	// has had no credit for starveRotate is asked to move, and one that
	// still has none at starveClose is closed. On the same phone on an
	// internet path that afternoon this happened every 12-24s, and grants
	// trickled for 4-5s before stopping, so the clock starts at the first
	// wait and a second of it is enough: at home the room never ran out.
	starveRotate = 1 * time.Second
	starveClose  = 6 * time.Second
	// controlAlive is how recently the client must have sent something on
	// the control stream for a credit stall to count. The client reports
	// once a second. Without that a stall is a link that has gone quiet,
	// and closing on it is trap 21's reconnect.
	controlAlive = 2500 * time.Millisecond
)

// wtStreamPreamble is what each unidirectional stream spends before the frame
// header: the WebTransport stream type 0x54, a two-byte varint, and the
// session ID, a varint of one to eight bytes. Four is enough for any session
// ID under 2^14, and overcounting only moves the rotation earlier.
const wtStreamPreamble = 4

// peerSpend is what a connection has spent of the peer's window, and how it
// has fared since.
type peerSpend struct {
	Bytes, Streams uint64
	// SinceSent is how long since a frame was last delivered.
	SinceSent time.Duration
	// Starved is how long the peer has refused a new stream for want of
	// credit, or zero while streams open.
	Starved time.Duration
	// SinceRecv is how long since the client last sent on the control stream.
	SinceRecv time.Duration
}

// starved reports whether the peer has stopped granting stream credit while
// the client is still talking, for at least d.
func (p peerSpend) starved(d time.Duration) bool {
	return p.Starved >= d && p.SinceRecv < controlAlive
}

// peerWindow is implemented by a connection that spends a peer's
// flow-control window the browser may never refill. Only wtConn does.
type peerWindow interface {
	PeerWindow() peerSpend
	// CloseExhausted closes the connection with wtPeerWindowExhausted.
	CloseExhausted() error
}

type peerWindowAction int

const (
	peerWindowOK peerWindowAction = iota
	peerWindowRotate
	peerWindowClose
)

// decidePeerWindow says what to do with a connection's counts, and why.
// asked reports whether this connection has already been sent MsgRotate,
// which is sent once.
func decidePeerWindow(p peerSpend, asked bool) (peerWindowAction, string) {
	if (p.Bytes >= exhaustedBytes || p.Streams >= exhaustedStreams) && p.SinceSent >= exhaustedStall {
		return peerWindowClose, "window"
	}
	if p.starved(starveClose) {
		return peerWindowClose, "credit"
	}
	if asked {
		return peerWindowOK, ""
	}
	if p.Bytes >= rotateBytes || p.Streams >= rotateStreams {
		return peerWindowRotate, "window"
	}
	if p.starved(starveRotate) {
		return peerWindowRotate, "credit"
	}
	return peerWindowOK, ""
}

// peerWindowWatch is how often watchPeerWindow reads the counts.
const peerWindowWatch = 500 * time.Millisecond

// watchPeerWindow closes a connection that has spent the peer's window and
// stopped delivering, until done closes. It runs apart from the viewer's
// writer because the writer is what stops: once the window is gone, a write
// on the control stream blocks with no deadline, and the writer never reaches
// another check. On the phone on 11 October the stats records stopped at
// 16.66MB and nothing closed the session until the page was hidden 33s later.
// Closing the session fails that blocked write, so the writer ends too.
func (s *Server) watchPeerWindow(pw peerWindow, targetID string, viewerID uint64, done <-chan struct{}) {
	t := time.NewTicker(peerWindowWatch)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		p := pw.PeerWindow()
		action, reason := decidePeerWindow(p, true)
		if action != peerWindowClose {
			continue
		}
		s.trace.Record(map[string]any{"kind": "wt", "action": "exhausted", "target": targetID,
			"viewer": viewerID, "reason": reason, "bytes": p.Bytes, "streams": p.Streams,
			"sinceSentMs": p.SinceSent.Milliseconds(), "starvedMs": p.Starved.Milliseconds()})
		_ = pw.CloseExhausted()
		return
	}
}

// encodeRotate builds MsgRotate.
func encodeRotate(bytes, streams uint64) []byte {
	body, _ := json.Marshal(map[string]uint64{"bytes": bytes, "streams": streams})
	return append([]byte{session.MsgRotate}, body...)
}
