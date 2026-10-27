package session

import (
	"sync"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// touchStream records every touch it is sent.
type touchStream struct {
	rotStream
	mu      sync.Mutex
	touches []target.Touch
}

func (s *touchStream) Touch(t target.Touch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touches = append(s.touches, t)
	return nil
}

func (s *touchStream) sent() []target.Touch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]target.Touch(nil), s.touches...)
}

func touchSession() (*Session, *touchStream) {
	st := &touchStream{}
	s := controlSession()
	s.stream = st
	return s, st
}

func lifts(ts []target.Touch) map[uint64]target.Touch {
	out := map[uint64]target.Touch{}
	for _, t := range ts {
		if t.Action == target.TouchUp {
			out[t.PointerID] = t
		}
	}
	return out
}

// A viewer that goes away with fingers down would leave them down on the
// device, and Android then rejects every injected touch after it, from this
// server or from adb (docs/traps.md, trap 50).
func TestAViewerLeavingMidTouchLiftsItsFingers(t *testing.T) {
	s, st := touchSession()
	id, _ := s.AddViewer()

	_ = s.ViewerTouch(id, TouchEvent{Action: 0, PointerID: 0, X: 10, Y: 20})
	_ = s.ViewerTouch(id, TouchEvent{Action: 0, PointerID: 1, X: 30, Y: 40})
	_ = s.ViewerTouch(id, TouchEvent{Action: 2, PointerID: 1, X: 35, Y: 45})
	before := len(st.sent())

	s.RemoveViewer(id)

	up := lifts(st.sent()[before:])
	if len(up) != 2 {
		t.Fatalf("lifts after leaving = %+v, want pointers 0 and 1", up)
	}
	if p := up[0]; p.X != 10 || p.Y != 20 {
		t.Errorf("pointer 0 lifted at %d,%d, want where it was, 10,20", p.X, p.Y)
	}
	if p := up[1]; p.X != 35 || p.Y != 45 {
		t.Errorf("pointer 1 lifted at %d,%d, want its last move, 35,45", p.X, p.Y)
	}
}

func TestAViewerLeavingWithNothingDownSendsNothing(t *testing.T) {
	s, st := touchSession()
	id, _ := s.AddViewer()

	_ = s.ViewerTouch(id, TouchEvent{Action: 0, PointerID: 0, X: 10, Y: 20})
	_ = s.ViewerTouch(id, TouchEvent{Action: 1, PointerID: 0, X: 10, Y: 20})
	_ = s.ViewerTouch(id, TouchEvent{Action: 0, PointerID: 1, X: 10, Y: 20})
	_ = s.ViewerTouch(id, TouchEvent{Action: 3, PointerID: 1, X: 10, Y: 20})
	before := len(st.sent())

	s.RemoveViewer(id)

	if extra := st.sent()[before:]; len(extra) != 0 {
		t.Errorf("sent %+v after a clean leave, want nothing", extra)
	}
}

// Only the leaving viewer's fingers are lifted: another viewer's finger that
// is still down is a gesture in progress.
func TestALeavingViewerLiftsOnlyItsOwnFingers(t *testing.T) {
	s, st := touchSession()
	a, _ := s.AddViewer()
	b, _ := s.AddViewer()

	_ = s.ViewerTouch(a, TouchEvent{Action: 0, PointerID: 0, X: 1, Y: 1})
	_ = s.ViewerTouch(b, TouchEvent{Action: 0, PointerID: 7, X: 2, Y: 2})
	before := len(st.sent())

	s.RemoveViewer(a)

	up := lifts(st.sent()[before:])
	if _, ok := up[7]; ok || len(up) != 1 {
		t.Errorf("lifts = %+v, want only viewer a's pointer 0", up)
	}
}

func TestClosingTheSessionLiftsFingersStillDown(t *testing.T) {
	s, st := touchSession()
	close(s.done) // no pump in this fixture; Close waits on done
	id, _ := s.AddViewer()
	_ = s.ViewerTouch(id, TouchEvent{Action: 0, PointerID: 0, X: 5, Y: 6})

	_ = s.Close()

	if up := lifts(st.sent()); len(up) != 1 {
		t.Errorf("lifts on close = %+v, want pointer 0", up)
	}
}
