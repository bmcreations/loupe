package httpapi

import (
	"errors"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/wsx"
)

// errFrameAbandoned reports a frame the transport started sending and then
// reset mid-flight because it outlived its deadline. The connection is fine;
// that one frame arrived nowhere.
//
// It exists because returning nil here was indistinguishable from delivery one
// layer up, and two decisions read that return value. The viewer credited the
// frame to goodput, which is the single direct measurement of what the link
// carries, and cleared its keyframe wait on a keyframe the client never got --
// then forwarded inter-frames the decoder had no anchor for. Across a 90-second
// run shaped to 400 kbps the trace read two frames sent per second while
// send.payloadErr rose by two per second and send.bytes advanced about 125
// bytes: nothing but parameter sets arrived, and goodput still reported
// 1.56 Mbps.
var errFrameAbandoned = errors.New("frame abandoned at its deadline")

// frameDelivered splits a SendFrame result into whether the frame arrived and
// whether the connection is over. Abandonment is neither: the frame is lost and
// the viewer keeps going.
//
// It is a function so the contract has a test. The viewer loop itself has no
// harness.
func frameDelivered(err error) (delivered, fatal bool) {
	switch {
	case err == nil:
		return true, false
	case errors.Is(err, errFrameAbandoned):
		return false, false
	default:
		return false, true
	}
}

// Conn is one viewer's connection, independent of how it is carried.
//
// The interface exists so the pacing, shedding and tracing logic is written
// once. Those decisions are about latency and the client's own measurements,
// not about framing, so a second transport should inherit them rather than
// reimplement them and drift.
type Conn interface {
	// SendFrame delivers one encoded access unit. deadline is when the frame
	// stops being worth showing; a transport that can abandon an in-flight
	// frame should do so rather than finish sending something already stale.
	//
	// A transport that abandons a frame must return errFrameAbandoned. Nothing
	// above this can tell a reset frame from a delivered one otherwise, and
	// both the goodput measurement and the keyframe wait depend on the
	// difference.
	SendFrame(f session.OutMsg, deadline time.Time) error
	// SendControl delivers a control message: init, stats or pong.
	SendControl(b []byte) error
	// Recv returns the next message from the client, blocking until one
	// arrives or the connection ends.
	Recv() ([]byte, error)
	// Keepalive nudges the connection so idle intermediaries do not reap it.
	Keepalive() error
	// Name identifies the transport in traces.
	Name() string
	Close() error
}

// wsConn carries a viewer over a WebSocket.
//
// TCP underneath means a frame handed to SendFrame is committed: it will be
// delivered, in order, however stale it becomes. Everything this transport can
// do about latency has to happen before the write, which is why the shedding
// upstream matters more here than on a transport that can cancel.
type wsConn struct{ c *wsx.Conn }

func (w wsConn) Name() string { return "websocket" }

func (w wsConn) SendFrame(f session.OutMsg, _ time.Time) error {
	return w.c.WriteBinary(f.Data)
}

func (w wsConn) SendControl(b []byte) error { return w.c.WriteBinary(b) }
func (w wsConn) Keepalive() error           { return w.c.WritePing() }
func (w wsConn) Close() error               { return w.c.Close() }

func (w wsConn) Recv() ([]byte, error) {
	for {
		op, payload, err := w.c.Read()
		if err != nil {
			return nil, err
		}
		if op == wsx.OpBinary && len(payload) > 0 {
			return payload, nil
		}
	}
}
