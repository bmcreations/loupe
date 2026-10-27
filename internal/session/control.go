package session

import (
	"fmt"
	"time"
)

// This file puts the Arbiter behind a Session: viewer input feeds it,
// /api/input asks it, and every change it reports goes to viewers as
// MsgControl and to the control observer for the trace.
//
// ctlMu is taken before s.mu and never inside it. The MsgControl fan-out
// runs under ctlMu, so two changes racing cannot deliver their states out of
// order, and a joiner cannot be handed a state older than one broadcast after
// it.

// SetHumanHold sets how long a viewer's last input holds the target against
// agents. Call it before the first viewer joins, like SetClientBuffer.
func (s *Session) SetHumanHold(d time.Duration) {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	s.arb = NewArbiter(d)
}

// SetControlObserver installs the hook that records each control change. It
// is called outside every Session lock, but on the goroutine that caused the
// change, which may be a viewer's read loop, so it should not block long.
func (s *Session) SetControlObserver(fn func(ControlChange)) {
	s.ctlMu.Lock()
	s.onControl = fn
	s.ctlMu.Unlock()
}

// HasViewer reports whether id is attached to this session now.
func (s *Session) HasViewer(id uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.viewers[id]
	return ok
}

// HumanInput records input a viewer sent outside the stream, which is its own
// /api/input. It reports false, recording nothing, when the viewer is not
// attached.
func (s *Session) HumanInput(viewer uint64) bool {
	if !s.HasViewer(viewer) {
		return false
	}
	s.control(func(a *Arbiter, now time.Time) []ControlChange { return a.Human(viewer, now) })
	return true
}

// ViewerTakeOver holds the target for a viewer that asked for it with
// MsgTakeOver, sending nothing to the device. See Arbiter.TakeOver.
func (s *Session) ViewerTakeOver(viewer uint64) error {
	if !s.HasViewer(viewer) {
		return fmt.Errorf("take over: viewer %d is not attached", viewer)
	}
	s.control(func(a *Arbiter, now time.Time) []ControlChange { return a.TakeOver(viewer, now) })
	return nil
}

// AgentInput asks whether an agent may act now. When it may not, retryAfter
// is how long the hold has left and holder is the viewer holding it.
func (s *Session) AgentInput(name string) (ok bool, retryAfter time.Duration, holder uint64) {
	s.control(func(a *Arbiter, now time.Time) []ControlChange {
		var changes []ControlChange
		ok, retryAfter, changes = a.Agent(name, now)
		holder = a.State().Holder
		return changes
	})
	return ok, retryAfter, holder
}

// AgentActed records that an agent's action reached the device, which is
// what shows it driving. AgentInput alone does not: the action can still fail.
func (s *Session) AgentActed(name string) {
	s.control(func(a *Arbiter, now time.Time) []ControlChange { return a.AgentActed(name, now) })
}

// AgentFailed records that an allowed agent's action did not reach the
// device, which ends a "waiting" banner without showing it driving.
func (s *Session) AgentFailed() {
	s.control(func(a *Arbiter, now time.Time) []ControlChange { return a.AgentFailed(now) })
}

// ControlState reports who holds the target and what the agent is doing.
func (s *Session) ControlState() ControlState {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	return s.arbiter().State()
}

func (s *Session) noteTouch(viewer uint64, e TouchEvent) {
	s.control(func(a *Arbiter, now time.Time) []ControlChange {
		return a.Touch(viewer, e.Action, e.PointerID, now)
	})
}

func (s *Session) noteHuman(viewer uint64) {
	s.control(func(a *Arbiter, now time.Time) []ControlChange { return a.Human(viewer, now) })
}

func (s *Session) noteViewerLeft(viewer uint64) {
	s.control(func(a *Arbiter, now time.Time) []ControlChange { return a.ViewerLeft(viewer, now) })
}

func (s *Session) controlTick() {
	s.control(func(a *Arbiter, now time.Time) []ControlChange { return a.Tick(now) })
}

// sendControlTo queues the current state to one viewer, which is how a
// joiner learns it.
func (s *Session) sendControlTo(id uint64) {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	msg := OutMsg{Data: EncodeControl(s.arbiter().State()), Control: true, QueuedAt: time.Now()}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Checked under s.mu: RemoveViewer deletes before it closes the channel.
	if v, ok := s.viewers[id]; ok {
		select {
		case v.out <- msg:
		default:
		}
	}
}

// stopControl stops the timer for good and records what the session's end
// closed. No viewer is left to send MsgControl to.
func (s *Session) stopControl() {
	s.ctlMu.Lock()
	s.ctlClosed = true
	if s.ctlTimer != nil {
		s.ctlTimer.Stop()
	}
	changes := s.arbiter().End(time.Now())
	observe := s.onControl
	s.ctlMu.Unlock()
	if observe != nil {
		for _, c := range changes {
			observe(c)
		}
	}
}

// arbiter returns the session's arbiter, building one with the default hold
// for a Session built directly rather than through the manager. ctlMu must be
// held.
func (s *Session) arbiter() *Arbiter {
	if s.arb == nil {
		s.arb = NewArbiter(DefaultHumanHold)
	}
	return s.arb
}

// control runs fn against the arbiter, sends viewers the new state if it
// changed, re-arms the timer for the next change time makes on its own, and
// hands each change to the observer.
func (s *Session) control(fn func(a *Arbiter, now time.Time) []ControlChange) {
	s.ctlMu.Lock()
	a := s.arbiter()
	now := time.Now()
	changes := fn(a, now)
	if st := a.State(); st != s.ctlSent {
		s.ctlSent = st
		msg := OutMsg{Data: EncodeControl(st), Control: true, QueuedAt: now}
		s.mu.RLock()
		for _, v := range s.viewers {
			select {
			case v.out <- msg:
			default:
				s.eventsDropped.Add(1)
			}
		}
		s.mu.RUnlock()
	}
	if !s.ctlClosed {
		if d := a.Deadline(); !d.IsZero() {
			wait := d.Sub(now)
			if s.ctlTimer == nil {
				s.ctlTimer = time.AfterFunc(wait, s.controlTick)
			} else {
				s.ctlTimer.Reset(wait)
			}
		}
	}
	observe := s.onControl
	s.ctlMu.Unlock()

	if observe != nil {
		for _, c := range changes {
			observe(c)
		}
	}
}
