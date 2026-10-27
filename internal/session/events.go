package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"sync"
)

// Point is a position as a fraction of the screen, [0,1] on each axis for
// From/To. Event.Scroll reuses the same type for a scroll delta, which is
// unbounded and can be negative -- it is not a [0,1] fraction, just the same
// two-float shape, so MarshalJSON never clamps.
//
// Fractions, not pixels, because the two sources of events live in different
// spaces: a viewer's touch is in stream coordinates, which MaxSize rescales,
// and an agent's is in device pixels. A fraction is the one space both map
// into without knowing about the other, and it survives a rotation's change
// of size.
type Point struct{ X, Y float64 }

// NormPoint reports (x, y) as a fraction of a w x h screen, or nil if the
// size is not known. A nil point is the deliberate answer for an unknown
// size: dividing by zero would produce a NaN or Inf, and guessing a size
// would put a wrong coordinate in a log meant to be trusted.
//
// The fraction is not clamped. outside reports whether it fell outside [0,1]
// on either axis -- a coordinate past the edge of the screen -- and each
// caller decides what that means: a viewer's scroll reports it as is, while
// the agent path clamps and, for a tap, attaches an Err.
func NormPoint(x, y, w, h int) (pt *Point, outside bool) {
	if w <= 0 || h <= 0 {
		return nil, false
	}
	fx := float64(x) / float64(w)
	fy := float64(y) / float64(h)
	return &Point{X: fx, Y: fy}, fx < 0 || fx > 1 || fy < 0 || fy > 1
}

func (p Point) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]float64{round3(finite(p.X)), round3(finite(p.Y))})
}

// finite maps a non-finite float to 0. It exists so a NaN or Inf coordinate
// -- which json.Marshal refuses outright -- cannot make EncodeEvent produce
// a MsgEvent whose body silently failed to marshal; see EncodeEvent.
func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func (p *Point) UnmarshalJSON(b []byte) error {
	var a [2]float64
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	p.X, p.Y = a[0], a[1]
	return nil
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// Event is one completed gesture, from a viewer or from /api/input.
type Event struct {
	Seq    uint64 `json:"seq"`
	T      int64  `json:"t"`      // unix milliseconds
	Source string `json:"source"` // "viewer" or "agent"
	Viewer uint64 `json:"viewer,omitempty"`
	// Agent is the X-Loupe-Agent name an agent sent, so a viewer can label
	// the cursor it draws for that agent's input. Empty for a viewer and for
	// an agent that sent no name.
	Agent string `json:"agent,omitempty"`
	// Kind is tap, longPress, swipe, pinch, multiTouch, scroll, key, text,
	// button or rotate.
	Kind     string `json:"kind"`
	From     *Point `json:"from,omitempty"`
	To       *Point `json:"to,omitempty"`
	Ms       int    `json:"ms,omitempty"`
	Pointers int    `json:"pointers,omitempty"`
	// Scale is a pinch's zoom: above 1 spreads, below 1 closes.
	Scale   float64 `json:"scale,omitempty"`
	Keycode int     `json:"keycode,omitempty"`
	Button  string  `json:"button,omitempty"`
	Text    string  `json:"text,omitempty"`
	// Rotation is a pointer because 0 (back to portrait) is a real, distinct
	// value from "this event carries no rotation" -- a plain int with
	// omitempty cannot tell those two apart, and a rotate event landing back
	// on portrait would encode with no rotation field at all.
	Rotation *int   `json:"rotation,omitempty"`
	Scroll   *Point `json:"scroll,omitempty"` // h, v delta, not a [0,1] fraction
	// Redacted marks a text event whose Text was withheld because some
	// focused node in the view hierarchy reported password="true" -- a dump
	// can carry more than one focused node, and any of them saying so is
	// enough -- or because that could not be positively ruled out (no
	// Inspector, a dump error, a dump timeout, or no focused node at all --
	// see publishAgentEvent). Text carries a fixed marker in that case, never
	// the characters.
	Redacted bool `json:"redacted,omitempty"`
	// TextLen is the rune count of the text actually typed, present only
	// alongside Redacted so a viewer can still see how much was entered
	// without seeing what it was.
	TextLen int    `json:"textLen,omitempty"`
	Err     string `json:"err,omitempty"`
}

// eventRingSize is how many events a session keeps for replay. A client that
// sees a gap in seq, or joins late, fetches what it missed with
// GET /api/events?after=; past this many events since the last fetch it has
// to fall back to whatever the live stream shows it from here.
const eventRingSize = 256

// eventRing keeps the newest events so a viewer that missed one -- control
// sends are non-blocking, so a busy viewer does -- or joined late can ask for
// what it has not seen.
type eventRing struct {
	mu   sync.Mutex
	buf  []Event
	next uint64
}

func newEventRing(n int) *eventRing {
	if n < 1 {
		// A ring with no capacity would panic the first time add appends
		// past cap(buf) == 0, so it has to hold at least the one event it
		// is about to receive.
		n = 1
	}
	return &eventRing{buf: make([]Event, 0, n), next: 1}
}

func (r *eventRing) add(e Event) Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.Seq = r.next
	r.next++
	if len(r.buf) == cap(r.buf) {
		copy(r.buf, r.buf[1:])
		r.buf = r.buf[:len(r.buf)-1]
	}
	r.buf = append(r.buf, e)
	return e
}

func (r *eventRing) after(seq uint64) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Event{}
	for _, e := range r.buf {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out
}

// page returns oldest, latest and the events after 'after' as one consistent
// read. All three come from the buffer under a single lock acquisition, so a
// concurrent add cannot land between reading oldest/latest and reading the
// events and leave the two disagreeing -- which is exactly the failure a
// caller uses oldest/latest to detect in the first place. Both are 0 when the
// ring is empty.
func (r *eventRing) page(after uint64) (oldest, latest uint64, events []Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) > 0 {
		oldest = r.buf[0].Seq
		latest = r.buf[len(r.buf)-1].Seq
	}
	events = make([]Event, 0, len(r.buf))
	for _, e := range r.buf {
		if e.Seq > after {
			events = append(events, e)
		}
	}
	return oldest, latest, events
}

// newSessionKey returns a fresh per-Session identity: 8 random bytes,
// hex-encoded. It has nothing to do with the target id (Session.ID, shared
// across restarts) or seq (which restarts at 1 on every Open) -- it is the
// one thing that changes when a session restarts, which is what lets a
// poller reading GET /api/events tell "caught up" apart from "reading a
// session that is gone." See EventPage.
func newSessionKey() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// As fatal as the same failure in NewToken: a poller cannot detect a
		// restart without a key that actually changes.
		panic("loupe: no entropy for a session key: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// EventPage is what a caller asking for events gets back. Session, Oldest and
// Latest exist because seq alone cannot tell a poller two things a plain
// slice of events cannot express:
//
//   - a changed Session, or after > Latest, means the session it was reading
//     has restarted -- seq starts over at 1, so a stale `after` against the
//     new session can read as "caught up" when it is actually far behind --
//     and it should refetch from after=0;
//   - Oldest > after+1 means the ring has wrapped past what it asked for, and
//     those events are gone for good.
type EventPage struct {
	Session string  `json:"session"`
	Oldest  uint64  `json:"oldest"`
	Latest  uint64  `json:"latest"`
	Events  []Event `json:"events"`
}

// EncodeEvent builds a MsgEvent: the type byte, then the event as JSON.
func EncodeEvent(e Event) []byte {
	// The marshal error is discarded on purpose, not ignored by accident:
	// Point.MarshalJSON maps NaN/Inf to 0 via finite, which is the only way
	// json.Marshal can fail on an Event, so nothing here can actually error.
	body, _ := json.Marshal(e)
	return append([]byte{MsgEvent}, body...)
}
