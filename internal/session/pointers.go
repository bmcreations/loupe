package session

import (
	"sort"
	"sync"
)

// downPointers is every pointer a viewer has put down and not lifted, with
// the last position it was sent at.
//
// A viewer that goes away mid-gesture never sends the up. Android then holds
// that pointer down for good and rejects every injected DOWN after it as
// "pointers already down" -- this server's, scrcpy's and `adb shell input`'s
// alike -- while `input` still exits 0 (docs/internals/traps.md, trap 50). On the
// Acme One two pointers were stuck that way until something lifted them, so
// whatever leaves lifts what it left down.
type downPointers struct {
	mu sync.Mutex
	by map[uint64]map[uint64]TouchEvent // viewer -> pointer -> last event
}

// note records e from viewer.
func (d *downPointers) note(viewer uint64, e TouchEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch e.Action {
	case 0, 2: // down, move
		if d.by == nil {
			d.by = map[uint64]map[uint64]TouchEvent{}
		}
		ps := d.by[viewer]
		if ps == nil {
			ps = map[uint64]TouchEvent{}
			d.by[viewer] = ps
		}
		if _, down := ps[e.PointerID]; e.Action == 2 && !down {
			return // a move with no down is not a finger on the screen
		}
		ps[e.PointerID] = e
	case 1, 3: // up, cancel
		delete(d.by[viewer], e.PointerID)
		if len(d.by[viewer]) == 0 {
			delete(d.by, viewer)
		}
	}
}

// take removes and returns an up for each pointer viewer still has down, at
// the position it was last sent, in pointer order.
func (d *downPointers) take(viewer uint64) []TouchEvent {
	d.mu.Lock()
	ps := d.by[viewer]
	delete(d.by, viewer)
	d.mu.Unlock()
	ups := make([]TouchEvent, 0, len(ps))
	for _, e := range ps {
		e.Action = 1
		e.Pressure = 0
		ups = append(ups, e)
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].PointerID < ups[j].PointerID })
	return ups
}

// viewers lists every viewer with a pointer down.
func (d *downPointers) viewers() []uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]uint64, 0, len(d.by))
	for id := range d.by {
		ids = append(ids, id)
	}
	return ids
}

// liftPointers sends an up for every pointer viewer left down. A failure is
// not returned: the viewer is already gone, and the next session's first
// touch meets the same device either way.
func (s *Session) liftPointers(viewer uint64) {
	ups := s.pointers.take(viewer)
	for _, e := range ups {
		_ = s.Touch(e)
	}
	if len(ups) > 0 {
		s.traceLifted(viewer, len(ups))
	}
}
