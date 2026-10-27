package httpapi

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// encodeTouchPayload builds a MsgTouch body in the wire layout DecodeTouch
// expects: action(1) pointerId(4) x(4) y(4) pressure(2). Only the fields this
// test cares about are given real values; pressure is left at zero.
func encodeTouchPayload(action uint8, pointerID uint64, x, y int32) []byte {
	b := make([]byte, 15)
	b[0] = action
	binary.BigEndian.PutUint32(b[1:], uint32(pointerID))
	binary.BigEndian.PutUint32(b[5:], uint32(x))
	binary.BigEndian.PutUint32(b[9:], uint32(y))
	return b
}

// TestDispatchAttributesATouchToItsViewer is the wiring Task 3 adds one layer
// above session.ViewerTouch's own tests: a real client payload, decoded and
// routed by dispatch, has to reach the session as that specific viewer's
// gesture. It is here rather than in the session package because dispatch,
// the thing under test, lives in httpapi.
//
// It fails if dispatch reverts to calling sess.Touch(e) directly instead of
// sess.ViewerTouch(viewerID, e): Touch forwards to the device and returns,
// with no gesture assembler and no viewer attribution, so nothing would ever
// land in the ring and EventsAfter(0) would come back empty.
func TestDispatchAttributesATouchToItsViewer(t *testing.T) {
	m := NewManager(target.Options{MaxFPS: 60})
	m.Register(&stubTarget{})

	sess, err := m.Acquire(context.Background(), "stub")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	viewerID, out := sess.AddViewer()
	<-out // init

	srv := &Server{mgr: m}

	// dispatch reads its own type byte off the front of payload (see
	// server.go: msgType, body := payload[0], payload[1:]), so it is called
	// the way viewer.go's read loop calls it: with the type byte included.
	//
	// stubStream.Touch returns target.ErrNoControl -- dispatch's own doc
	// comment says an input error must never be fatal to the session, and
	// ViewerTouch still assembles and publishes the gesture regardless of
	// what the device-forwarding half returned, so that error is expected
	// here and not a test failure.
	down := append([]byte{session.MsgTouch}, encodeTouchPayload(0, 1, 540, 1200)...)
	if err := srv.dispatch(nil, sess, viewerID, down); err != target.ErrNoControl {
		t.Fatalf("dispatch(down): got %v, want %v", err, target.ErrNoControl)
	}
	up := append([]byte{session.MsgTouch}, encodeTouchPayload(1, 1, 540, 1200)...)
	if err := srv.dispatch(nil, sess, viewerID, up); err != target.ErrNoControl {
		t.Fatalf("dispatch(up): got %v, want %v", err, target.ErrNoControl)
	}

	got := sess.EventsAfter(0)
	if len(got) != 1 {
		t.Fatalf("EventsAfter(0) = %+v; want exactly one tap", got)
	}
	if got[0].Kind != "tap" || got[0].Viewer != viewerID {
		t.Fatalf("event = %+v; want a tap from viewer %d", got[0], viewerID)
	}
}
