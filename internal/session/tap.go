package session

import (
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// A Tap receives every frame the session's encoder produced, after the PTS
// offset and before broadcast. It is not a viewer: it is not paced, not shed,
// not counted by the bitrate controller and not part of arbitration. A
// recorder needs every frame, and a viewer's loop exists to drop them.
type Tap struct {
	// C carries the frames. Data is shared with the broadcast path and must
	// not be modified. It is never closed; stop reading on Dropped or after
	// Remove.
	C <-chan target.Frame
	// Dropped is closed the first time a frame would not fit. After that the
	// tap receives nothing more, so its file has no silent hole.
	Dropped <-chan struct{}

	ch      chan target.Frame
	dropped chan struct{}
	s       *Session
	gone    bool // guarded by s.mu
	full    bool // written only with s.mu held, by the one sender
}

// AddTap registers a tap holding up to n frames. If the session has seen a
// config, it is the tap's first frame.
func (s *Session) AddTap(n int) *Tap {
	if n < 1 {
		n = 1
	}
	t := &Tap{ch: make(chan target.Frame, n), dropped: make(chan struct{}), s: s}
	t.C, t.Dropped = t.ch, t.dropped
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastCfgRaw.Data != nil {
		t.send(s.lastCfgRaw)
	}
	s.taps = append(s.taps, t)
	return t
}

// NewTap is test support for packages that consume a tap: it returns a tap on
// a session with no stream, and send, which hands it a frame the way pump
// does. A config sent through it is remembered, as on a live session.
func NewTap(n int) (*Tap, func(target.Frame)) {
	s := &Session{}
	return s.AddTap(n), s.fanTaps
}

// Remove unregisters the tap. It is safe to call more than once and from any
// goroutine.
func (t *Tap) Remove() {
	s := t.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.gone {
		return
	}
	t.gone = true
	for i, x := range s.taps {
		if x == t {
			s.taps = append(s.taps[:i:i], s.taps[i+1:]...)
			break
		}
	}
}

// send never blocks. Callers hold s.mu, and pump is the only concurrent
// sender, so full needs no lock of its own.
func (t *Tap) send(f target.Frame) {
	if t.full {
		return
	}
	select {
	case t.ch <- f:
	default:
		t.full = true
		close(t.dropped)
	}
}

// fanTaps hands one frame to every tap. A config is also remembered for later
// taps, under the write lock so a tap added meanwhile sees it once, not twice.
func (s *Session) fanTaps(f target.Frame) {
	if f.Config {
		s.mu.Lock()
		s.lastCfgRaw = f
		for _, t := range s.taps {
			t.send(f)
		}
		s.mu.Unlock()
		return
	}
	s.mu.RLock()
	for _, t := range s.taps {
		t.send(f)
	}
	s.mu.RUnlock()
}

// Hold keeps the session open with no viewers attached until release is
// called. Manager.Release and the idle reaper count holds with viewers.
// release is idempotent.
func (s *Session) Hold() (release func()) {
	s.holds.Add(1)
	var once sync.Once
	return func() { once.Do(func() { s.holds.Add(-1) }) }
}

// Holds reports how many holds are outstanding.
func (s *Session) Holds() int { return int(s.holds.Load()) }

// BitrateChange is one committed encoder bitrate change.
type BitrateChange struct {
	At       time.Time
	From, To int
}

// BitrateChanges returns the session's changes, oldest first. It keeps the
// last 256.
func (s *Session) BitrateChanges() []BitrateChange {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]BitrateChange(nil), s.bitrateLog...)
}
