package session

import (
	"math"
	"time"
)

const (
	tapSlopFrac  = 0.02
	longPressMin = 500 * time.Millisecond
	// staleGesture bounds how long an open gesture waits for a close that
	// never arrives. This is not about a viewer disappearing -- that goes
	// through RemoveViewer instead. It is a single missed pointerup inside a
	// live viewer's stream (a dropped or reordered control message) leaving
	// a pointer permanently "down": nothing else ever brings len(down) back
	// to zero, so without a timeout every later down on this assembler joins
	// the stuck gesture as one more pointer instead of starting its own. It
	// measures idle time, so a finger genuinely held still for this long
	// before a second finger joins is indistinguishable from a stuck one and
	// gets discarded too -- rare enough to treat as a logged edge case
	// rather than something to special-case here. A down that arrives after
	// this much idle time discards whatever was open and starts fresh.
	staleGesture = 5 * time.Second
)

// GestureAssembler turns one viewer's pointer stream into completed gestures.
// It belongs to exactly one goroutine -- the viewer's read loop is the only
// caller, and calls it synchronously as each touch arrives -- so it needs no
// locking. The stream size is not stored between calls either, for the same
// reason: Touch takes it as a parameter rather than through a Resize that a
// second goroutine could call concurrently with Touch.
type GestureAssembler struct {
	open     bool
	cancel   bool
	start    time.Time
	activity time.Time // when Touch last did anything with this gesture
	gw, gh   int       // stream size locked at the down; see Touch
	from     *Point    // nil while the stream size is unknown; see norm
	last     *Point
	maxMove  float64 // in pixels of the size locked at the down
	down     map[uint64]bool
	peak     int
}

func NewGestureAssembler() *GestureAssembler {
	return &GestureAssembler{down: map[uint64]bool{}}
}

// Touch feeds one event and returns the gesture it completed, if any. w and h
// are the stream's size *as of this call* -- the caller's job is to know the
// live size, not this type's -- but a gesture already open ignores them and
// keeps the size it locked on its own down, so a rotation landing mid-swipe
// cannot rescale a gesture already in flight (see
// TestTheSizeAtTheDownIsTheOneUsed).
func (g *GestureAssembler) Touch(e TouchEvent, w, h int, now time.Time) *Event {
	if e.Action == 0 && g.open && now.Sub(g.activity) >= staleGesture {
		g.open = false
		g.down = map[uint64]bool{}
	}
	g.activity = now
	switch e.Action {
	case 0: // down
		if !g.open {
			g.open, g.cancel = true, false
			g.start, g.gw, g.gh = now, w, h
			g.from = g.norm(e.X, e.Y)
			g.last, g.maxMove, g.peak = g.from, 0, 0
		}
		g.down[e.PointerID] = true
		if len(g.down) > g.peak {
			g.peak = len(g.down)
		}
	case 2: // move
		if g.open {
			g.track(e)
		}
	case 3: // cancel
		// A cancel for a pointer this assembler never saw a down for -- the
		// same duplicate or reordered control message as the stray-up case
		// below -- must not discard a gesture it has nothing to do with.
		if !g.open || !g.down[e.PointerID] {
			return nil
		}
		g.cancel = true
		delete(g.down, e.PointerID)
	case 1: // up
		// A pointer this assembler never saw a down for -- a duplicate or
		// reordered control message -- must not perturb the movement it is
		// tracking for the pointers that are actually part of the gesture.
		if !g.open || !g.down[e.PointerID] {
			return nil
		}
		g.track(e)
		delete(g.down, e.PointerID)
	}
	if !g.open || len(g.down) > 0 {
		return nil
	}
	g.open = false
	if g.cancel {
		return nil
	}
	return g.finish(now)
}

// norm is NormPoint against the size captured when the gesture opened: nil
// if that size was unknown, so a gesture with no coordinate space holds no
// position rather than a fabricated [0,0].
func (g *GestureAssembler) norm(x, y int32) *Point {
	p, _ := NormPoint(int(x), int(y), g.gw, g.gh)
	return p
}

func (g *GestureAssembler) track(e TouchEvent) {
	p := g.norm(e.X, e.Y)
	// gw and gh are fixed for the gesture, so p and from are nil together.
	if p == nil || g.from == nil {
		return
	}
	if g.peak == 1 {
		g.last = p
	}
	dx := (p.X - g.from.X) * float64(g.gw)
	dy := (p.Y - g.from.Y) * float64(g.gh)
	if d := math.Hypot(dx, dy); d > g.maxMove && g.peak == 1 {
		g.maxMove = d
	}
}

func (g *GestureAssembler) finish(now time.Time) *Event {
	// A gesture that opened and closed without ever learning a real stream
	// size (or during a resize's brief 0x0) has no coordinate space to
	// report a position in, and norm left from nil. Emit nothing rather
	// than an event with no position.
	if g.from == nil {
		return nil
	}
	ms := int(now.Sub(g.start) / time.Millisecond)
	e := &Event{Source: "viewer", Ms: ms, From: g.from}
	short := float64(min(g.gw, g.gh))
	switch {
	case g.peak > 1:
		e.Kind, e.Pointers = "multiTouch", g.peak
	case g.maxMove >= tapSlopFrac*short:
		e.Kind, e.To = "swipe", g.last
	case now.Sub(g.start) >= longPressMin:
		e.Kind = "longPress"
	default:
		e.Kind = "tap"
	}
	return e
}
