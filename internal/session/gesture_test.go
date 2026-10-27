package session

import (
	"testing"
	"time"
)

const (
	actDown, actUp, actMove, actCancel = 0, 1, 2, 3
)

type step = struct {
	a        uint8
	id       uint64
	x, y, ms int32
}

// feedGesture feeds a run of steps against a fixed stream size and returns
// the last completed gesture, if any. Tests that need the size to change
// mid-gesture (a rotation) call g.Touch directly instead.
func feedGesture(g *GestureAssembler, w, h int32, t0 time.Time, steps ...step) *Event {
	var got *Event
	for _, s := range steps {
		if e := g.Touch(TouchEvent{Action: s.a, PointerID: s.id, X: s.x, Y: s.y}, int(w), int(h),
			t0.Add(time.Duration(s.ms)*time.Millisecond)); e != nil {
			got = e
		}
	}
	return got
}

func TestAShortStillTouchIsATap(t *testing.T) {
	g := NewGestureAssembler()
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0), step{actDown, 1, 540, 1200, 0}, step{actUp, 1, 543, 1202, 90})
	if e == nil || e.Kind != "tap" || e.Ms != 90 {
		t.Fatalf("got %+v; want a 90ms tap", e)
	}
	if e.From.X != 0.5 || e.From.Y != 0.5 {
		t.Fatalf("from = %+v; want [0.5 0.5]", e.From)
	}
}

func TestAHeldStillTouchIsALongPress(t *testing.T) {
	g := NewGestureAssembler()
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0), step{actDown, 1, 540, 1200, 0}, step{actUp, 1, 540, 1200, 650})
	if e == nil || e.Kind != "longPress" {
		t.Fatalf("got %+v; want longPress", e)
	}
}

// longPressMin is pinned exactly at its boundary: 499ms is still a tap, and
// 500ms -- the constant's own value -- is already a longPress. A fencepost
// error in either direction (> instead of >=, or the constant off by a
// millisecond) shows up as one of these two flipping.
func TestATouchHeldExactly499msIsATap(t *testing.T) {
	g := NewGestureAssembler()
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0), step{actDown, 1, 540, 1200, 0}, step{actUp, 1, 540, 1200, 499})
	if e == nil || e.Kind != "tap" {
		t.Fatalf("got %+v; want tap at 499ms", e)
	}
}

func TestATouchHeldExactly500msIsALongPress(t *testing.T) {
	g := NewGestureAssembler()
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0), step{actDown, 1, 540, 1200, 0}, step{actUp, 1, 540, 1200, 500})
	if e == nil || e.Kind != "longPress" {
		t.Fatalf("got %+v; want longPress at exactly 500ms", e)
	}
}

func TestMovementPastTheSlopIsASwipe(t *testing.T) {
	g := NewGestureAssembler()
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0),
		step{actDown, 1, 540, 1800, 0}, step{actMove, 1, 540, 1200, 80}, step{actUp, 1, 540, 600, 180})
	if e == nil || e.Kind != "swipe" || e.To.Y != 0.25 || e.Ms != 180 {
		t.Fatalf("got %+v; want a 180ms swipe ending at y 0.25", e)
	}
}

func TestJustUnderTheSlopIsStillATap(t *testing.T) {
	// 2% of the 1080 short side is 21.6px.
	g := NewGestureAssembler()
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0), step{actDown, 1, 500, 1200, 0}, step{actUp, 1, 521, 1200, 60})
	if e == nil || e.Kind != "tap" {
		t.Fatalf("got %+v; want tap at 21px", e)
	}
}

// TestAHorizontalMoveOnTheShortSideIsASwipe pins three things a purely
// vertical slop test cannot: the slop fraction itself (0.02, not something
// looser), that the slop is measured against the *short* side of the stream
// (1080, not the long side 2400) and not whichever side the move happens to
// run along, and that a horizontal move counts at all -- a track() that only
// looked at dy would never see this one.
func TestAHorizontalMoveOnTheShortSideIsASwipe(t *testing.T) {
	g := NewGestureAssembler()
	// 22px horizontal on a 1080x2400 stream: past 0.02*1080 = 21.6px, so a
	// swipe. Against the long side (0.02*2400 = 48px) or a looser 0.05
	// fraction (0.05*1080 = 54px) it would read as a tap instead.
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0), step{actDown, 1, 540, 1200, 0}, step{actUp, 1, 562, 1200, 90})
	if e == nil || e.Kind != "swipe" {
		t.Fatalf("got %+v; want swipe on a 22px horizontal move (short side 1080, slop 21.6px)", e)
	}
}

func TestASecondPointerMakesItMultiTouch(t *testing.T) {
	cases := []struct {
		name  string
		steps []step
	}{
		{"the second pointer down lifts first", []step{
			{actDown, 1, 400, 1200, 0}, {actDown, 2, 700, 1200, 20},
			{actUp, 2, 800, 1200, 200}, {actUp, 1, 300, 1200, 210},
		}},
		{"the first pointer down lifts first", []step{
			{actDown, 1, 400, 1200, 0}, {actDown, 2, 700, 1200, 20},
			{actUp, 1, 300, 1200, 200}, {actUp, 2, 800, 1200, 210},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := NewGestureAssembler()
			e := feedGesture(g, 1080, 2400, time.Unix(0, 0), c.steps...)
			if e == nil || e.Kind != "multiTouch" || e.Pointers != 2 {
				t.Fatalf("got %+v; want multiTouch with 2 pointers", e)
			}
		})
	}
}

func TestACancelledGestureEmitsNothing(t *testing.T) {
	g := NewGestureAssembler()
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0),
		step{actDown, 1, 400, 1200, 0}, step{actDown, 2, 700, 1200, 20},
		step{actCancel, 2, 700, 1200, 40}, step{actUp, 1, 400, 1200, 60})
	if e != nil {
		t.Fatalf("got %+v; want nothing after a cancel", e)
	}
}

// TestTheSizeAtTheDownIsTheOneUsed is the point of Task 2's fix: the caller
// passes the *live* size on every call (a real rotation mid-gesture would
// hand the up a swapped w/h), and the assembler must still classify against
// the size it locked at the down. A small move that reads as a tiny fraction
// of the down's 1080x2400 reads as a large fraction of the swapped 2400x1080
// if the size is not locked, which would flip this from tap to swipe.
func TestTheSizeAtTheDownIsTheOneUsed(t *testing.T) {
	g := NewGestureAssembler()
	g.Touch(TouchEvent{Action: actDown, PointerID: 1, X: 540, Y: 1200}, 1080, 2400, time.Unix(0, 0))
	// A rotation lands mid-gesture: the up carries the new, rotated size.
	e := g.Touch(TouchEvent{Action: actUp, PointerID: 1, X: 550, Y: 1210}, 2400, 1080, time.Unix(0, 50e6))
	if e == nil || e.Kind != "tap" || e.To != nil {
		t.Fatalf("got %+v; want a tap locked to the down's size, with no To", e)
	}
	if e.From.X != 0.5 || e.From.Y != 0.5 {
		t.Fatalf("from = %+v; want [0.5 0.5]", e.From)
	}
}

// TestAStaleOpenGestureIsDiscardedOnTheNextDown covers a lost up: a single
// missed pointerup inside a live viewer's stream (a dropped or reordered
// control message) never sends the up that would close pointer 1's gesture.
// Without a timeout that latches forever -- every later down joins the stuck
// gesture as a second pointer, and len(down) can never return to zero
// because pointer 1's up will never arrive. A down that arrives after this
// much idle time must discard the old gesture and start clean, rather than
// turning pointer 2's ordinary tap into a multiTouch with a ghost. The
// second down's timestamp is a literal, not staleGesture-relative, so this
// test still fails if staleGesture is mutated to something absurd like an
// hour.
func TestAStaleOpenGestureIsDiscardedOnTheNextDown(t *testing.T) {
	g := NewGestureAssembler()
	t0 := time.Unix(0, 0)
	g.Touch(TouchEvent{Action: actDown, PointerID: 1, X: 540, Y: 1200}, 1080, 2400, t0) // its up never comes

	e := g.Touch(TouchEvent{Action: actDown, PointerID: 2, X: 200, Y: 200}, 1080, 2400, t0.Add(6*time.Second))
	if e != nil {
		t.Fatalf("got %+v; want nothing from a down alone", e)
	}
	e = g.Touch(TouchEvent{Action: actUp, PointerID: 2, X: 200, Y: 200}, 1080, 2400, t0.Add(6*time.Second+50*time.Millisecond))
	if e == nil || e.Kind != "tap" || e.Pointers != 0 {
		t.Fatalf("got %+v; want a plain tap from pointer 2 alone, not a multiTouch with the stuck pointer 1", e)
	}
}

// TestAZeroSizedStreamYieldsNoEvent covers a gesture that completes before
// the session has ever learned a real stream size (or after MaxSize/rotation
// briefly reports zero). norm() leaves the position nil rather than dividing
// by zero, and finish() must emit nothing rather than a positionless or
// fabricated [0,0] tap.
func TestAZeroSizedStreamYieldsNoEvent(t *testing.T) {
	g := NewGestureAssembler()
	e := feedGesture(g, 0, 0, time.Unix(0, 0), step{actDown, 1, 0, 0, 0}, step{actUp, 1, 0, 0, 50})
	if e != nil {
		t.Fatalf("got %+v; want nothing for a 0x0 stream", e)
	}
}

// TestAnUpForAPointerNeverDownIsIgnored covers a stray up for a pointer ID
// this assembler never saw a down for -- a duplicate or reordered control
// message. It must not perturb the real gesture's movement tracking (which
// would misclassify a tap as a swipe off the stray pointer's coordinates) or
// end the gesture early.
func TestAnUpForAPointerNeverDownIsIgnored(t *testing.T) {
	g := NewGestureAssembler()
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0),
		step{actDown, 1, 540, 1200, 0},
		step{actUp, 2, 900, 1200, 10}, // unknown pointer, must be a no-op
		step{actUp, 1, 540, 1200, 90})
	if e == nil || e.Kind != "tap" {
		t.Fatalf("got %+v; want a tap unaffected by the unknown pointer's up", e)
	}
}

// TestACancelForAPointerNeverDownIsIgnored covers a stray cancel for a
// pointer ID this assembler never saw a down for -- the same duplicate or
// reordered control message as the stray-up case. It must not discard a
// gesture it has nothing to do with.
func TestACancelForAPointerNeverDownIsIgnored(t *testing.T) {
	g := NewGestureAssembler()
	e := feedGesture(g, 1080, 2400, time.Unix(0, 0),
		step{actDown, 1, 540, 1200, 0},
		step{actCancel, 2, 900, 1200, 10}, // unknown pointer, must be a no-op
		step{actUp, 1, 540, 1200, 90})
	if e == nil || e.Kind != "tap" {
		t.Fatalf("got %+v; want a tap not discarded by the unknown pointer's cancel", e)
	}
}
