package httpapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

func postInputAs(s *Server, targetID, body string, hdr map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/input?target="+targetID, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	s.handleInput(w, r)
	return w
}

// heldSession opens dev1 with one viewer that has just touched it.
func heldSession(t *testing.T) (*Server, *session.Session, uint64) {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400})
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	id, _ := sess.AddViewer()
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 0, PointerID: 1})
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 1, PointerID: 1})
	return &Server{mgr: m}, sess, id
}

// The value agents hardcode.
func TestErrCodeHumanHeldIsStable(t *testing.T) {
	if ErrCodeHumanHeld != "humanHeld" {
		t.Fatalf("ErrCodeHumanHeld = %q; agents key on \"humanHeld\"", ErrCodeHumanHeld)
	}
}

func TestAnAgentIsRefusedWhileAViewerHoldsTheTarget(t *testing.T) {
	s, sess, _ := heldSession(t)
	rec := postInputAs(s, "dev1", `{"action":"tap","x":1,"y":1}`, map[string]string{HeaderAgent: "claude"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != ErrCodeHumanHeld || body.RetryAfterMs <= 0 {
		t.Fatalf("body = %+v; want humanHeld with retryAfterMs", body)
	}
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Errorf("Retry-After = %q; want whole seconds >= 1", rec.Header().Get("Retry-After"))
	}
	for _, e := range sess.EventsAfter(0) {
		if e.Source == "agent" {
			t.Errorf("a refused action published %+v", e)
		}
	}
	if st := sess.ControlState(); st.Agent != session.AgentWaiting || st.AgentName != "claude" {
		t.Errorf("control state = %+v; want claude waiting", st)
	}
}

// A malformed request is still a 400, not a 409: arbitration runs after
// validation.
func TestArbitrationRunsAfterValidation(t *testing.T) {
	s, _, _ := heldSession(t)
	if rec := postInput(s, "dev1", `{"action":"tap"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400: %s", rec.Code, rec.Body.String())
	}
}

// The viewer's own /api/input -- an iOS button, a rotation -- is the human's:
// it goes through, is published as that viewer's, and keeps the hold.
func TestAViewersOwnInputPassesAndIsPublishedAsTheirs(t *testing.T) {
	s, sess, id := heldSession(t)
	before := sess.EventsAfter(0)
	var after uint64
	if len(before) > 0 {
		after = before[len(before)-1].Seq
	}
	rec := postInputAs(s, "dev1", `{"action":"tap","x":1,"y":1}`,
		map[string]string{HeaderViewer: strconv.FormatUint(id, 10)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200: %s", rec.Code, rec.Body.String())
	}
	ev := sess.EventsAfter(after)
	if len(ev) != 1 || ev[0].Source != "viewer" || ev[0].Viewer != id {
		t.Fatalf("events = %+v; want one from viewer %d", ev, id)
	}
	if ok, _, _ := sess.AgentInput(""); ok {
		t.Error("an agent was let through right after the viewer's input")
	}
}

// A viewer id that is not attached is an agent's request, not an error.
func TestAStaleViewerIDIsAnAgent(t *testing.T) {
	s, _, id := heldSession(t)
	rec := postInputAs(s, "dev1", `{"action":"tap","x":1,"y":1}`,
		map[string]string{HeaderViewer: strconv.FormatUint(id+100, 10)})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409: %s", rec.Code, rec.Body.String())
	}
}

// With nobody at a viewer, an agent drives as it always has, and the
// refusal and the success are both traced with the caller.
func TestAnAgentDrivesAFreeTargetAndTheTraceNamesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400})
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	s := &Server{mgr: m, trace: tw}
	sess.AddViewer()

	if rec := postInputAs(s, "dev1", `{"action":"tap","x":1,"y":1}`, map[string]string{HeaderAgent: "claude"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if st := sess.ControlState(); st.Agent != session.AgentDriving {
		t.Errorf("control state = %+v; want driving", st)
	}
	tw.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		_ = json.Unmarshal(sc.Bytes(), &rec)
		if rec["kind"] == "agent" && rec["action"] == "tap" {
			found = true
			if rec["caller"] != "agent" || rec["agent"] != "claude" {
				t.Errorf("trace record %v; want caller agent, agent claude", rec)
			}
		}
	}
	if !found {
		t.Fatal("no kind:agent record for the tap")
	}
}

// An agent is shown driving only once an action reached the device. A 501
// from a target that cannot do the action, or a 502 from one that tried and
// failed, left the banner saying "driving" while nothing had moved.
func TestAFailedAgentActionDoesNotShowItDriving(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400, tapErr: errors.New("adb: device offline")})
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	s := &Server{mgr: m}
	sess.AddViewer()

	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"action":"pinch","x":1,"y":1,"scale":2}`, http.StatusNotImplemented},
		{`{"action":"tap","x":1,"y":1}`, http.StatusBadGateway},
	} {
		rec := postInputAs(s, "dev1", tc.body, map[string]string{HeaderAgent: "claude"})
		if rec.Code != tc.want {
			t.Fatalf("%s: status = %d, want %d: %s", tc.body, rec.Code, tc.want, rec.Body.String())
		}
		if st := sess.ControlState(); st.Agent != session.AgentIdle {
			t.Errorf("%s: control state = %+v; want idle", tc.body, st)
		}
	}
}

// An agent refused while a viewer held the target, then allowed once the
// viewer left, stays waiting until its action settles: driving if it lands,
// idle if it fails. It must not pass through idle on the way to driving, or
// the viewer's banner vanishes for a moment before it says "driving".
func TestARefusedAgentLetThroughSettlesOnItsAction(t *testing.T) {
	for _, tc := range []struct {
		body       string
		wantStatus int
		wantAgent  string
	}{
		{`{"action":"tap","x":1,"y":1}`, http.StatusOK, session.AgentDriving},
		{`{"action":"pinch","x":1,"y":1,"scale":2}`, http.StatusNotImplemented, session.AgentIdle},
	} {
		s, sess, id := heldSession(t)
		agent := map[string]string{HeaderAgent: "claude"}
		if rec := postInputAs(s, "dev1", tc.body, agent); rec.Code != http.StatusConflict {
			t.Fatalf("%s while held: status = %d; want 409", tc.body, rec.Code)
		}
		sess.RemoveViewer(id)
		if st := sess.ControlState(); st.Agent != session.AgentWaiting {
			t.Fatalf("after the viewer left: control state = %+v; want still waiting", st)
		}
		if rec := postInputAs(s, "dev1", tc.body, agent); rec.Code != tc.wantStatus {
			t.Fatalf("%s: status = %d; want %d: %s", tc.body, rec.Code, tc.wantStatus, rec.Body.String())
		}
		if st := sess.ControlState(); st.Agent != tc.wantAgent {
			t.Errorf("%s: control state = %+v; want %s", tc.body, st, tc.wantAgent)
		}
	}
}

// A move goes to the viewer as a MsgCursor while a person holds the
// target: it only draws, so the hold neither refuses it nor flips the
// banner, and it leaves the replay ring alone.
func TestAMovePassesTheHoldAndReachesViewersAsACursor(t *testing.T) {
	s, sess, _ := heldSession(t)
	_, out := sess.AddViewer()
	for len(out) > 0 {
		<-out
	}
	before := len(sess.EventsAfter(0))

	rec := postInputAs(s, "dev1", `{"action":"move","x":540,"y":600}`, map[string]string{HeaderAgent: "claude"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200: %s", rec.Code, rec.Body.String())
	}
	if got := len(sess.EventsAfter(0)); got != before {
		t.Errorf("ring went from %d to %d events; a move must not enter it", before, got)
	}
	if st := sess.ControlState(); st.Agent == session.AgentDriving || st.Agent == session.AgentWaiting {
		t.Errorf("control state = %+v; a move must not show the agent driving or waiting", st)
	}
	var msg session.OutMsg
	select {
	case msg = <-out:
	default:
		t.Fatal("the viewer got nothing")
	}
	if msg.Data[0] != session.MsgCursor {
		t.Fatalf("message type = %#x; want MsgCursor", msg.Data[0])
	}
	var e session.Event
	if err := json.Unmarshal(msg.Data[1:], &e); err != nil {
		t.Fatal(err)
	}
	if e.Kind != "move" || e.Agent != "claude" || e.Seq != 0 || e.From == nil || e.From.X != 0.5 || e.From.Y != 0.25 {
		t.Errorf("cursor = %+v from %+v; want claude's move at 0.5, 0.25 with no seq", e, e.From)
	}
}

func TestOnlyTheKeyboardSwitchAndAWaitAreExempt(t *testing.T) {
	for _, a := range InputActions {
		if got, want := arbitrated(a), a != "hardwareKeyboard" && a != "wait"; got != want {
			t.Errorf("arbitrated(%q) = %v; want %v", a, got, want)
		}
	}
}

func TestOnlyMoveSkipsTheHold(t *testing.T) {
	for _, a := range InputActions {
		if got, want := held(a), a != "hardwareKeyboard" && a != "move" && a != "wait"; got != want {
			t.Errorf("held(%q) = %v; want %v", a, got, want)
		}
	}
}

func TestAgentNameIsBoundedAndPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"  claude ":             "claude",
		"":                      "",
		"bad\nname":             "",
		"café":                  "",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
		strings.Repeat("a", 65): "",
	} {
		if got := agentName(in); got != want {
			t.Errorf("agentName(%q) = %q; want %q", in, got, want)
		}
	}
}
