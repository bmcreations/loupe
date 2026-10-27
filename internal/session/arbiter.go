package session

import (
	"encoding/json"
	"time"
)

// Agent states, as MsgControl and the trace report them.
const (
	AgentIdle    = "idle"
	AgentDriving = "driving"
	AgentWaiting = "waiting"
)

// agentShown is how long an agent's last /api/input keeps it reported as
// driving or waiting. An agent's requests come seconds apart with nothing
// held between them, so without a window the banner would flash once per
// request.
const agentShown = 10 * time.Second

// stuckPointer is how long a pointer held down with no move stops counting
// as held. A lost up event would otherwise hold the device against every
// agent for the life of the connection.
const stuckPointer = 30 * time.Second

// DefaultHumanHold is how long a viewer's last input holds a target against
// agents when nothing configures it.
const DefaultHumanHold = 5 * time.Second

// ControlState is the body of MsgControl: who holds the target and what the
// most recent agent is doing.
type ControlState struct {
	Held bool `json:"held"`
	// Holder is the viewer id holding the target, 0 when it is free.
	Holder    uint64 `json:"holder,omitempty"`
	Agent     string `json:"agent"`
	AgentName string `json:"agentName,omitempty"`
}

// ControlChange is one transition, for the trace. State is "held" or "free"
// for a change of hold, and empty for a change of agent state.
type ControlChange struct {
	State     string
	Viewer    uint64
	HeldMs    int64
	Agent     string
	AgentName string
}

// EncodeControl builds a MsgControl: the type byte, then the state as JSON.
func EncodeControl(st ControlState) []byte {
	body, err := json.Marshal(st)
	if err != nil {
		// ControlState has no unmarshalable fields.
		body = []byte(`{"held":false,"agent":"idle"}`)
	}
	return append([]byte{MsgControl}, body...)
}

type pointerKey struct {
	viewer, pointer uint64
}

// Arbiter decides whether an agent's /api/input may act. A person wins: any
// viewer input holds the target, for as long as a finger is down and then for
// the hold window after the last input, and an agent is refused while it is
// held. Two viewers are not arbitrated against each other; the most recent to
// touch is the holder.
//
// It does no I/O and starts nothing. Every method takes the time and returns
// what changed, so Session decides what to send and record, and a test steps
// the clock. It is not safe for concurrent use; Session guards it.
type Arbiter struct {
	hold time.Duration

	holder    uint64
	heldSince time.Time
	lastHuman time.Time
	// down is each pointer a viewer has down, and when it last moved.
	down map[pointerKey]time.Time

	agent     string
	agentName string
	lastAgent time.Time
}

// NewArbiter returns a free arbiter whose hold outlasts a viewer's last input
// by hold.
func NewArbiter(hold time.Duration) *Arbiter {
	if hold < 0 {
		hold = 0
	}
	return &Arbiter{hold: hold, down: map[pointerKey]time.Time{}, agent: AgentIdle}
}

// Touch records a viewer's pointer event. action is a target.TouchAction.
func (a *Arbiter) Touch(viewer uint64, action uint8, pointer uint64, now time.Time) []ControlChange {
	changes := a.Tick(now)
	k := pointerKey{viewer, pointer}
	switch action {
	case 0, 2: // down, move
		a.down[k] = now
	default: // up, cancel
		delete(a.down, k)
	}
	return append(changes, a.take(viewer, now)...)
}

// Human records any other input from a viewer: a key, text, a scroll, or its
// own /api/input.
func (a *Arbiter) Human(viewer uint64, now time.Time) []ControlChange {
	changes := a.Tick(now)
	return append(changes, a.take(viewer, now)...)
}

// TakeOver is a viewer asking for the target from the banner rather than by
// touching the device. It holds as Human does, and an agent shown driving is
// shown waiting at once: its next action will be refused, and leaving it
// "driving" until then would keep offering a take over that already happened.
func (a *Arbiter) TakeOver(viewer uint64, now time.Time) []ControlChange {
	changes := a.Human(viewer, now)
	if a.agent == AgentDriving {
		changes = append(changes, a.agentIs(AgentWaiting, a.agentName, now)...)
	}
	return changes
}

func (a *Arbiter) take(viewer uint64, now time.Time) []ControlChange {
	a.lastHuman = now
	if a.holder == viewer {
		return nil
	}
	if a.holder == 0 {
		a.heldSince = now
	}
	a.holder = viewer
	return []ControlChange{{State: "held", Viewer: viewer}}
}

// Agent asks whether an agent may act now. While a viewer holds the target
// it answers false with how long the hold has left, and the agent is reported
// as waiting. Otherwise it answers true and reports nothing new: being allowed
// is not acting, and the action can still fail. AgentActed or AgentFailed
// settles it. An agent shown waiting stays waiting until then, so a handover
// is one change from waiting to driving; going idle in between made the
// viewer's banner vanish for a moment before it said "driving". name is what
// the agent calls itself, and may be empty.
func (a *Arbiter) Agent(name string, now time.Time) (ok bool, retryAfter time.Duration, changes []ControlChange) {
	changes = a.Tick(now)
	if a.holder == 0 {
		return true, 0, changes
	}
	return false, a.freeAt(now).Sub(now), append(changes, a.agentIs(AgentWaiting, name, now)...)
}

// AgentActed records an agent's action that reached the device, and reports
// the agent as driving.
func (a *Arbiter) AgentActed(name string, now time.Time) []ControlChange {
	return append(a.Tick(now), a.agentIs(AgentDriving, name, now)...)
}

// AgentFailed records that an allowed agent's action did not reach the
// device. An agent still shown waiting is no longer waiting on anyone, and
// goes idle; one shown driving stays so, since its last action that landed
// did reach the device.
func (a *Arbiter) AgentFailed(now time.Time) []ControlChange {
	changes := a.Tick(now)
	if a.agent == AgentWaiting {
		a.agent = AgentIdle
		changes = append(changes, ControlChange{Agent: AgentIdle, AgentName: a.agentName})
	}
	return changes
}

func (a *Arbiter) agentIs(state, name string, now time.Time) []ControlChange {
	var changes []ControlChange
	if state != a.agent || name != a.agentName {
		changes = []ControlChange{{Agent: state, AgentName: name}}
	}
	a.agent, a.agentName, a.lastAgent = state, name, now
	return changes
}

// ViewerLeft frees the target at once if the viewer leaving holds it, and
// forgets its pointers either way.
func (a *Arbiter) ViewerLeft(viewer uint64, now time.Time) []ControlChange {
	changes := a.Tick(now)
	for k := range a.down {
		if k.viewer == viewer {
			delete(a.down, k)
		}
	}
	if a.holder == viewer && viewer != 0 {
		changes = append(changes, a.free(now))
	}
	return changes
}

// Tick makes the changes time makes on its own: a hold whose window has run
// out with no finger down is freed, and an agent not heard from in
// agentShown goes idle.
func (a *Arbiter) Tick(now time.Time) []ControlChange {
	var changes []ControlChange
	if a.holder != 0 && !now.Before(a.freeAt(now)) {
		changes = append(changes, a.free(now))
	}
	if a.agent != AgentIdle && now.Sub(a.lastAgent) >= agentShown {
		a.agent = AgentIdle
		changes = append(changes, ControlChange{Agent: AgentIdle, AgentName: a.agentName})
	}
	return changes
}

// End closes whatever is still open when the session ends: a hold, and an
// agent still shown as driving or waiting. Without it the trace's last word
// on a session that ended mid-drive would be "driving", forever.
func (a *Arbiter) End(now time.Time) []ControlChange {
	var changes []ControlChange
	if a.holder != 0 {
		changes = append(changes, a.free(now))
	}
	if a.agent != AgentIdle {
		a.agent = AgentIdle
		changes = append(changes, ControlChange{Agent: AgentIdle, AgentName: a.agentName})
	}
	return changes
}

// Deadline is when Tick next has something to do, or zero if nothing will
// change without further input.
func (a *Arbiter) Deadline() time.Time {
	var d time.Time
	if a.holder != 0 {
		d = a.releaseAt()
	}
	if a.agent != AgentIdle {
		idle := a.lastAgent.Add(agentShown)
		if d.IsZero() || idle.Before(d) {
			d = idle
		}
	}
	return d
}

// State is what MsgControl reports.
func (a *Arbiter) State() ControlState {
	st := ControlState{Held: a.holder != 0, Holder: a.holder, Agent: a.agent}
	if a.agent != AgentIdle {
		st.AgentName = a.agentName
	}
	return st
}

// releaseAt is when the hold ends if nothing else arrives: the window after
// the last input, or later if a pointer is still down and not yet stuck.
func (a *Arbiter) releaseAt() time.Time {
	at := a.lastHuman.Add(a.hold)
	for _, t := range a.down {
		if s := t.Add(stuckPointer); s.After(at) {
			at = s
		}
	}
	return at
}

// freeAt is what an agent is told to wait for. While a live finger is down
// that is not knowable, so it is the whole window from now: the soonest the
// hold could end if the finger lifted this instant.
func (a *Arbiter) freeAt(now time.Time) time.Time {
	at := a.lastHuman.Add(a.hold)
	for _, t := range a.down {
		if now.Before(t.Add(stuckPointer)) {
			if w := now.Add(a.hold); w.After(at) {
				at = w
			}
			break
		}
	}
	return at
}

func (a *Arbiter) free(now time.Time) ControlChange {
	c := ControlChange{State: "free", Viewer: a.holder, HeldMs: now.Sub(a.heldSince).Milliseconds()}
	a.holder = 0
	a.down = map[pointerKey]time.Time{}
	return c
}
