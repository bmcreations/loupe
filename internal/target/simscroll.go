package target

import (
	"math"
	"slices"
	"time"
)

// A wheel or trackpad scroll on an iOS simulator or phone is a held finger.
// iOS has no scroll-wheel event the HID route takes, so Scroll drags one
// finger from where the pointer was when the burst began and lifts it once
// the deltas stop:
//
//   - The finger goes down only after the burst has moved past
//     wheelSlopFrac, so a nudge of the wheel never becomes a tap on whatever
//     is under the pointer.
//   - A trackpad's momentum deltas keep moving it, slowing as they slow, so
//     it lifts at almost no speed and iOS does not fling a second time.
//   - A wheel notch is one jump; the finger then rests until it lifts, so
//     a notch scrolls a step and stops.
//   - Reaching the screen's edge lifts the finger and puts it down again at
//     the burst's start, the way a thumb takes a new grip.
//   - A real touch lifts it first (Touch).
//
// A viewer's delta is a notch's worth per unit: 120 points of a precise
// delta, as the Mac and web viewers send it (handset VideoView.scrollWheel).
const (
	// wheelUnitFrac is how far one unit drags, as a fraction of the
	// stream's height: 120 points of an iPhone shown about 850 points tall.
	wheelUnitFrac = 0.14
	// wheelSlopFrac is how far a burst moves before the finger lands, about
	// 20 points on a phone, past UIKit's tap slop.
	wheelSlopFrac = 0.02
	// wheelEdgeFrac keeps the finger this far inside the screen.
	wheelEdgeFrac = 0.02
	// wheelIdle is how long after the last delta the finger lifts. A
	// trackpad sends one every 8 to 16ms while it or its momentum moves.
	wheelIdle = 120 * time.Millisecond
	// wheelFingerID is the pointer ID the finger carries in fingers. A
	// viewer's pointer IDs are small.
	wheelFingerID = math.MaxUint64
	// wheelDefaultHeight stands in for a stream with no picture size yet.
	wheelDefaultHeight = 1440
)

// wheelFinger is a scroll burst's state, under simStream.touchMu.
type wheelFinger struct {
	burst  bool  // a delta came within wheelIdle
	down   bool  // the finger is on the screen
	ax, ay int32 // where the burst began, in stream pixels
	dx, dy float64
	timer  *time.Timer
	gen    uint64 // which arming of timer is current
}

// Scroll implements Stream by dragging a finger; see wheelFinger. A scroll
// while a viewer's finger is down is dropped.
func (st *simStream) Scroll(sc Scroll) error {
	if st.in == nil {
		return ErrNoControl
	}
	st.touchMu.Lock()
	defer st.touchMu.Unlock()
	w := &st.wheel
	if !w.down && len(st.fingers) > 0 {
		return nil
	}
	m := st.Meta()
	width, height := float64(m.Width), float64(m.Height)
	if width <= 0 || height <= 0 {
		width, height = wheelDefaultHeight*9/19.5, wheelDefaultHeight
	}
	if !w.burst {
		*w = wheelFinger{burst: true, ax: sc.X, ay: sc.Y, timer: w.timer, gen: w.gen}
	}
	unit := wheelUnitFrac * height
	w.dx += float64(sc.HScroll) * unit
	w.dy += float64(sc.VScroll) * unit
	st.armWheelLocked()

	if !w.down && math.Hypot(w.dx, w.dy) < wheelSlopFrac*height {
		return nil
	}
	x, y := float64(w.ax)+w.dx, float64(w.ay)+w.dy
	edge := wheelEdgeFrac * height
	if x < edge || x > width-edge || y < edge || y > height-edge {
		// Out of room: lift at the edge and start over from the anchor
		// with what is left of this delta.
		cx, cy := clamp(x, edge, width-edge), clamp(y, edge, height-edge)
		var err error
		if w.down {
			err = st.moveWheelLocked(cx, cy, "up")
		}
		w.dx, w.dy = x-cx, y-cy
		w.down = false
		if err != nil {
			return err
		}
		x, y = float64(w.ax)+w.dx, float64(w.ay)+w.dy
		x, y = clamp(x, edge, width-edge), clamp(y, edge, height-edge)
	}
	if !w.down {
		st.fingers = append(st.fingers, finger{wheelFingerID, w.ax, w.ay})
		w.down = true
		if err := st.sendFingers("down"); err != nil {
			st.dropWheelLocked()
			return err
		}
	}
	return st.moveWheelLocked(x, y, "move")
}

// moveWheelLocked puts the finger at x, y and sends phase. "up" also takes
// it out of fingers.
func (st *simStream) moveWheelLocked(x, y float64, phase string) error {
	i := slices.IndexFunc(st.fingers, func(f finger) bool { return f.id == wheelFingerID })
	if i < 0 {
		return nil
	}
	st.fingers[i].x, st.fingers[i].y = int32(math.Round(x)), int32(math.Round(y))
	err := st.sendFingers(phase)
	if phase == "up" {
		st.dropWheelLocked()
	}
	return err
}

// dropWheelLocked forgets the finger without sending anything.
func (st *simStream) dropWheelLocked() {
	st.fingers = slices.DeleteFunc(st.fingers, func(f finger) bool { return f.id == wheelFingerID })
	st.wheel.down = false
}

// liftWheelLocked ends the burst, lifting the finger where it is.
func (st *simStream) liftWheelLocked() error {
	w := &st.wheel
	if !w.burst {
		return nil
	}
	w.burst = false
	w.gen++
	if w.timer != nil {
		w.timer.Stop()
	}
	if !w.down {
		return nil
	}
	i := slices.IndexFunc(st.fingers, func(f finger) bool { return f.id == wheelFingerID })
	if i < 0 {
		w.down = false
		return nil
	}
	f := st.fingers[i]
	return st.moveWheelLocked(float64(f.x), float64(f.y), "up")
}

// armWheelLocked (re)starts the idle timer. A firing from an earlier arming
// sees a newer gen and does nothing.
func (st *simStream) armWheelLocked() {
	w := &st.wheel
	w.gen++
	gen := w.gen
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(wheelIdle, func() {
		st.touchMu.Lock()
		defer st.touchMu.Unlock()
		if st.wheel.gen == gen {
			_ = st.liftWheelLocked()
		}
	})
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
