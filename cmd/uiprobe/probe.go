package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/bmcreations/loupe/internal/session"
)

// emulatorTargetRe is what an emulator target id looks like: "emulator-"
// followed by one or more digits and nothing else. Anchored on both ends, so
// neither a prefix match nor a substring match can be satisfied by anything
// this does not fully consume.
var emulatorTargetRe = regexp.MustCompile(`^emulator-\d+$`)

// targetAllowed reports whether id is safe for this probe to drive without
// -target-ok. The probe sends real touches, keys and button presses, so this
// is what stops a copy-pasted command line from landing on a device someone
// is using -- see the warning in CLAUDE.md about physical devices and trap 42's rotation probes,
// which name the same concern for a sibling tool.
//
// A full-match regexp, not strings.HasPrefix: "xemulator-5554" must not pass,
// and neither may "emulator-" with nothing after the dash, nor a query-string
// fragment smuggled in after a real-looking prefix such as
// "emulator-5554&target=ACME00000000001" -- a HasPrefix check accepts all
// three. A target id is never attacker-controlled here -- it comes from the
// operator's own command line -- but the guard exists to catch a fat-fingered
// copy of a real device id, and anything looser than a full match leaves a
// gap the fat finger can fall through.
func targetAllowed(id string) bool {
	return id == "synthetic" || emulatorTargetRe.MatchString(id)
}

// guardDecision is targetAllowed plus the -target-ok override, pulled out so
// run's wiring -- "refuse unless allowed, unless overridden" -- is itself a
// unit under test rather than only reachable through run's full dial-and-check
// path. ok=false is guard's cue to print a FAIL and never open a connection;
// detail is empty when ok is true, since nothing needs saying.
func guardDecision(id string, targetOK bool) (ok bool, detail string) {
	if id == "" {
		return false, `missing ?target= on the /wt URL`
	}
	if targetAllowed(id) || targetOK {
		return true, ""
	}
	return false, fmt.Sprintf(
		"target %q is not \"synthetic\" or \"emulator-<digits>\"; pass -target-ok to override", id)
}

// checkResult is one line of probe output.
type checkResult struct {
	name   string
	status string // "PASS", "FAIL" or "SKIP"
	detail string
}

func (r checkResult) String() string {
	return fmt.Sprintf("%s %s: %s", r.status, r.name, r.detail)
}

func pass(name, detail string) checkResult { return checkResult{name, "PASS", detail} }
func fail(name, detail string) checkResult { return checkResult{name, "FAIL", detail} }
func skip(name, detail string) checkResult { return checkResult{name, "SKIP", detail} }

// encodeTouch builds a MsgTouch payload exactly as docs/protocol.md's Touch
// table lays it out and testdata/protocol/touch.hex pins:
// type(1) action(1) pointerId(4) x(4) y(4) pressure(2), all big-endian.
// x and y are stream coordinates -- the space Meta.Width/Height describes,
// which is codedWidth/codedHeight from Init -- not device pixels.
func encodeTouch(action uint8, pointerID uint32, x, y int32, pressure uint16) []byte {
	b := make([]byte, 1+1+4+4+4+2)
	b[0] = session.MsgTouch
	b[1] = action
	binary.BigEndian.PutUint32(b[2:], pointerID)
	binary.BigEndian.PutUint32(b[6:], uint32(x))
	binary.BigEndian.PutUint32(b[10:], uint32(y))
	binary.BigEndian.PutUint16(b[14:], pressure)
	return b
}

// frameControl prepends the u32 big-endian length prefix every WebTransport
// control message carries in both directions
// (internal/httpapi/webtransport.go's wtFrameControl,
// testdata/protocol/wt-control-hello.hex).
func frameControl(b []byte) []byte {
	framed := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(framed[:4], uint32(len(b)))
	copy(framed[4:], b)
	return framed
}

// decodeEvent strips MsgEvent's leading type byte and decodes the JSON body.
// It errors on any other message type so a caller cannot accidentally match
// a stats or init frame against event criteria.
func decodeEvent(frame []byte) (session.Event, error) {
	var e session.Event
	if len(frame) == 0 {
		return e, fmt.Errorf("empty control frame")
	}
	if frame[0] != session.MsgEvent {
		return e, fmt.Errorf("not a MsgEvent frame: type 0x%02x", frame[0])
	}
	if err := json.Unmarshal(frame[1:], &e); err != nil {
		return e, fmt.Errorf("decode event: %w", err)
	}
	return e, nil
}

// coordTol is the coordinate-matching tolerance the plan specifies: 0.01 of
// the [0,1] fraction Event.From/To carry. See docs/protocol.md's Event
// table -- From/To are always a fraction of the screen, never pixels,
// regardless of which space the touch that produced them was sent in, so no
// normalization by codedWidth/codedHeight is needed on the receiving side.
const coordTol = 0.01

func closeEnough(a, b float64) bool {
	return math.Abs(a-b) <= coordTol
}

// gestureWant describes what a sent gesture should produce as an Event.
type gestureWant struct {
	source string // "viewer" or "agent"
	kind   string
	from   [2]float64
	// to is checked only when hasTo is true -- a tap has no To.
	hasTo bool
	to    [2]float64
}

// matchesGesture reports whether e is the event a sent gesture should have
// produced, within coordTol on every coordinate checked. e.Err must be empty:
// ViewerTouch publishes the gesture event whether or not the target accepted
// it (session.go: "Forwarding comes first and is not conditional on the
// assembler"), so a target that rejects input -- target.Synthetic's Touch
// always returns ErrNoControl, giving Err
// "target: this target does not accept input" -- still produces a
// From/To-matching event. Without this check that event reads as a PASS for
// input that never reached anything. matchesGestureIgnoringErr is the same
// match minus this one condition, for the callers that need to tell "never
// arrived" apart from "arrived but was rejected".
func matchesGesture(e session.Event, want gestureWant) bool {
	return e.Err == "" && matchesGestureIgnoringErr(e, want)
}

func matchesGestureIgnoringErr(e session.Event, want gestureWant) bool {
	if e.Source != want.source || e.Kind != want.kind {
		return false
	}
	if e.From == nil || !closeEnough(e.From.X, want.from[0]) || !closeEnough(e.From.Y, want.from[1]) {
		return false
	}
	if want.hasTo {
		if e.To == nil || !closeEnough(e.To.X, want.to[0]) || !closeEnough(e.To.Y, want.to[1]) {
			return false
		}
	}
	return true
}

// gestureOutcome classifies what waitEvent returned for a sent tap or swipe
// into a checkResult. e/ok are exactly waitEvent's own return values, from a
// predicate that matches on source/kind/coordinates alone
// (matchesGestureIgnoringErr) -- Err is decided here, not in the predicate,
// so a rejected gesture is classified rather than waited past.
//
// The classification is gated on control, the target's own init.Control:
// ViewerTouch publishes the gesture event whether or not the target accepted
// it ("Forwarding comes first and is not conditional on the assembler",
// session.go), so an event carrying Err is expected and unremarkable for a
// target that already reported it accepts no input at all (control=false,
// e.g. target.Synthetic) -- that is what run's own control=false gate exists
// to predict, and this reports it as SKIP. A target reporting control=true
// promised to accept input; coming back with Err instead means it failed to
// do the one thing it claimed to support (a real target.Actuator error), and
// that must FAIL rather than being waved through as a SKIP -- an emulator
// whose control socket write fails must not exit 0.
func gestureOutcome(name string, control bool, timeout time.Duration, e session.Event, ok bool) checkResult {
	if !ok {
		return fail(name, fmt.Sprintf("no matching viewer/%s event within %v", name, timeout))
	}
	if e.Err != "" {
		detail := fmt.Sprintf("seq=%d arrived but was rejected by the target: %s", e.Seq, e.Err)
		if !control {
			return skip(name, detail)
		}
		return fail(name, detail)
	}
	if e.To != nil {
		return pass(name, fmt.Sprintf("seq=%d from=%v to=%v", e.Seq, *e.From, *e.To))
	}
	return pass(name, fmt.Sprintf("seq=%d from=%v", e.Seq, *e.From))
}

// matchesButton reports whether e is the button event a POST /api/input
// {"action":"button"} should have produced. As with matchesGesture, a button
// press the target rejected still publishes (agent.go's publishAgentEvent
// sets Err on actionErr != nil), so this excludes it the same way.
func matchesButton(e session.Event, source, button string) bool {
	return e.Err == "" && e.Source == source && e.Kind == "button" && e.Button == button
}

// seqStrictlyIncreasing reports whether seqs, in the order events arrived on
// the wire, is strictly increasing -- session.eventRing.add assigns Seq
// under one lock in arrival order, so a gap or a repeat here means either a
// dropped/reordered control send or a session restart mid-probe, either of
// which is worth failing loudly on rather than silently tolerating.
func seqStrictlyIncreasing(seqs []uint64) bool {
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			return false
		}
	}
	return true
}

// missingFromPage returns the seqs in seen that do not appear in page's
// events, preserving seen's order. Used for check 6: every seq the probe
// observed live must be replayable from GET /api/events?after=0.
func missingFromPage(seen []uint64, page session.EventPage) []uint64 {
	have := make(map[uint64]bool, len(page.Events))
	for _, e := range page.Events {
		have[e.Seq] = true
	}
	var missing []uint64
	for _, s := range seen {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	return missing
}
