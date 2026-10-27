package session

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

const (
	down = 0
	up   = 1
	move = 2
)

func TestAgentActsOnAFreeTarget(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	ok, retry, changes := a.Agent("claude", at(0))
	if !ok || retry != 0 {
		t.Fatalf("Agent on a free target = %v, %v; want true, 0", ok, retry)
	}
	// Being allowed is not acting: the action may still fail.
	if len(changes) != 0 || a.State().Agent != AgentIdle {
		t.Fatalf("allowed: changes = %+v, state %+v; want nothing yet", changes, a.State())
	}
	changes = a.AgentActed("claude", at(300))
	if len(changes) != 1 || changes[0].Agent != AgentDriving || changes[0].AgentName != "claude" {
		t.Fatalf("changes = %+v, want one driving/claude", changes)
	}
	if st := a.State(); st.Held || st.Agent != AgentDriving || st.AgentName != "claude" {
		t.Fatalf("State = %+v", st)
	}
	if c := a.AgentActed("claude", at(600)); len(c) != 0 {
		t.Errorf("acting again reported %+v", c)
	}
}

func TestHumanInputHoldsForTheWindow(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	changes := a.Human(3, at(0))
	if len(changes) != 1 || changes[0].State != "held" || changes[0].Viewer != 3 {
		t.Fatalf("changes = %+v, want held by 3", changes)
	}
	ok, retry, changes := a.Agent("", at(1800))
	if ok {
		t.Fatal("agent allowed inside the hold window")
	}
	if retry != 3200*time.Millisecond {
		t.Errorf("retryAfter = %v, want 3.2s", retry)
	}
	if len(changes) != 1 || changes[0].Agent != AgentWaiting {
		t.Errorf("changes = %+v, want waiting", changes)
	}
	if ok, _, _ := a.Agent("", at(5000)); !ok {
		t.Fatal("agent refused once the window ran out")
	}
}

func TestAFingerDownHoldsPastTheWindow(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.Touch(1, down, 0, at(0))
	ok, retry, _ := a.Agent("", at(20_000))
	if ok {
		t.Fatal("agent allowed while a finger is down")
	}
	if retry != 5*time.Second {
		t.Errorf("retryAfter with a finger down = %v, want the whole window", retry)
	}
	a.Touch(1, up, 0, at(21_000))
	if ok, _, _ := a.Agent("", at(25_999)); ok {
		t.Fatal("agent allowed before the window after the lift")
	}
	if ok, _, _ := a.Agent("", at(26_000)); !ok {
		t.Fatal("agent refused after the window after the lift")
	}
}

func TestAStuckFingerStopsHolding(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.Touch(1, down, 0, at(0))
	if d := a.Deadline(); !d.Equal(at(30_000)) {
		t.Fatalf("Deadline = %v, want the stuck-pointer cap", d)
	}
	if ok, _, _ := a.Agent("", at(29_999)); ok {
		t.Fatal("agent allowed before the stuck cap")
	}
	if ok, _, _ := a.Agent("", at(30_000)); !ok {
		t.Fatal("a finger with no move for 30s still holds")
	}
}

func TestAMovingFingerIsNotStuck(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.Touch(1, down, 0, at(0))
	a.Touch(1, move, 0, at(29_000))
	if ok, _, _ := a.Agent("", at(40_000)); ok {
		t.Fatal("a finger that moved 11s ago was treated as stuck")
	}
}

func TestTickFreesAndReportsHowLong(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.Human(2, at(0))
	a.Human(2, at(3000))
	if d := a.Deadline(); !d.Equal(at(8000)) {
		t.Fatalf("Deadline = %v, want 8s", d)
	}
	if c := a.Tick(at(7999)); len(c) != 0 {
		t.Fatalf("Tick before the deadline = %+v", c)
	}
	c := a.Tick(at(8000))
	if len(c) != 1 || c[0].State != "free" || c[0].Viewer != 2 || c[0].HeldMs != 8000 {
		t.Fatalf("Tick at the deadline = %+v, want free by 2 after 8000ms", c)
	}
	if !a.Deadline().IsZero() {
		t.Errorf("Deadline after freeing = %v, want zero", a.Deadline())
	}
}

func TestASecondViewerTakesTheHold(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.Human(1, at(0))
	c := a.Human(2, at(1000))
	if len(c) != 1 || c[0].State != "held" || c[0].Viewer != 2 {
		t.Fatalf("changes = %+v, want held by 2", c)
	}
	if st := a.State(); st.Holder != 2 {
		t.Errorf("holder = %d, want 2", st.Holder)
	}
	if c := a.Human(2, at(1500)); len(c) != 0 {
		t.Errorf("repeat input from the holder reported %+v", c)
	}
}

func TestTheHolderLeavingFreesAtOnce(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.Touch(4, down, 0, at(0))
	if c := a.ViewerLeft(9, at(100)); len(c) != 0 {
		t.Fatalf("another viewer leaving reported %+v", c)
	}
	c := a.ViewerLeft(4, at(200))
	if len(c) != 1 || c[0].State != "free" || c[0].Viewer != 4 {
		t.Fatalf("changes = %+v, want free by 4", c)
	}
	if ok, _, _ := a.Agent("", at(300)); !ok {
		t.Fatal("agent refused after the holder left")
	}
}

func TestTheAgentGoesIdle(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.AgentActed("claude", at(0))
	if d := a.Deadline(); !d.Equal(at(10_000)) {
		t.Fatalf("Deadline = %v, want 10s", d)
	}
	c := a.Tick(at(10_000))
	if len(c) != 1 || c[0].Agent != AgentIdle {
		t.Fatalf("Tick = %+v, want idle", c)
	}
	if st := a.State(); st.Agent != AgentIdle || st.AgentName != "" {
		t.Errorf("State = %+v, want idle with no name", st)
	}
}

// A refused agent let through goes from waiting straight to driving. It used
// to go idle when it was allowed and driving once its action landed, which
// the viewer saw as the banner vanishing for a moment before "driving".
func TestARefusalThenSuccessIsOneHandover(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.Human(1, at(0))
	a.Agent("bot", at(1000))
	_, _, c := a.Agent("bot", at(2000))
	if len(c) != 0 {
		t.Errorf("a second refusal reported %+v", c)
	}
	ok, _, c := a.Agent("bot", at(6000))
	if !ok {
		t.Fatal("refused after the window")
	}
	if len(c) != 1 || c[0].State != "free" {
		t.Errorf("changes = %+v, want free alone", c)
	}
	if st := a.State(); st.Agent != AgentWaiting {
		t.Errorf("allowed, not yet acted: State = %+v; want still waiting", st)
	}
	if c := a.AgentActed("bot", at(6200)); len(c) != 1 || c[0].Agent != AgentDriving {
		t.Errorf("AgentActed = %+v, want driving", c)
	}
}

// An agent allowed through after a refusal whose action then fails is no
// longer waiting. It used to stay "waiting for you" until agentShown after
// the refusal, on a target nobody held.
func TestAnAllowedAgentWhoseActionFailsIsNoLongerWaiting(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.Human(1, at(0))
	a.Agent("bot", at(1000))
	ok, _, _ := a.Agent("bot", at(6000))
	if !ok {
		t.Fatal("refused after the window")
	}
	c := a.AgentFailed(at(6100))
	if len(c) != 1 || c[0].Agent != AgentIdle {
		t.Errorf("AgentFailed = %+v, want idle", c)
	}
	if st := a.State(); st.Agent != AgentIdle || st.AgentName != "" {
		t.Errorf("allowed, action failed: State = %+v; want idle", st)
	}
	if d := a.Deadline(); !d.IsZero() {
		t.Errorf("Deadline = %v; want none", d)
	}
}

// A failed action does not undo an agent shown driving: its last action that
// landed did reach the device.
func TestAFailedActionLeavesAnAgentDriving(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.AgentActed("bot", at(0))
	if c := a.AgentFailed(at(1000)); len(c) != 0 {
		t.Errorf("AgentFailed while driving reported %+v", c)
	}
}

// Being allowed does not undo an agent shown driving: its last action did
// reach the device.
func TestAnAllowedAgentStaysDriving(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.AgentActed("bot", at(0))
	if _, _, c := a.Agent("bot", at(1000)); len(c) != 0 {
		t.Errorf("allowed while driving reported %+v", c)
	}
	if st := a.State(); st.Agent != AgentDriving {
		t.Errorf("State = %+v; want driving", st)
	}
}

func TestEncodeControl(t *testing.T) {
	got := string(EncodeControl(ControlState{Held: true, Holder: 3, Agent: AgentWaiting, AgentName: "claude"}))
	want := "\x08" + `{"held":true,"holder":3,"agent":"waiting","agentName":"claude"}`
	if got != want {
		t.Errorf("EncodeControl = %q, want %q", got, want)
	}
}

func TestEndReportsWhatWasLeftOpen(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.AgentActed("claude", at(0))
	a.Human(3, at(1000))
	changes := a.End(at(2000))
	if len(changes) != 2 || changes[0].State != "free" || changes[0].HeldMs != 1000 ||
		changes[1].Agent != AgentIdle || changes[1].AgentName != "claude" {
		t.Fatalf("End = %+v, want free after 1000ms, then idle/claude", changes)
	}
	if st := a.State(); st.Held || st.Agent != AgentIdle {
		t.Fatalf("State after End = %+v", st)
	}
	if changes := a.End(at(3000)); len(changes) != 0 {
		t.Fatalf("second End = %+v, want nothing", changes)
	}
}

func TestTakingOverHoldsAndSetsADrivingAgentWaiting(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	a.AgentActed("claude", at(0))
	changes := a.TakeOver(3, at(1000))
	if len(changes) != 2 || changes[0].State != "held" || changes[0].Viewer != 3 ||
		changes[1].Agent != AgentWaiting || changes[1].AgentName != "claude" {
		t.Fatalf("changes = %+v, want held by 3 then claude waiting", changes)
	}
	if ok, _, _ := a.Agent("claude", at(2000)); ok {
		t.Fatal("agent allowed after a take over")
	}
}

func TestTakingOverLeavesAnIdleAgentIdle(t *testing.T) {
	a := NewArbiter(5 * time.Second)
	changes := a.TakeOver(3, at(0))
	if len(changes) != 1 || changes[0].State != "held" {
		t.Fatalf("changes = %+v, want held only", changes)
	}
	if st := a.State(); st.Agent != AgentIdle {
		t.Errorf("agent = %q, want idle", st.Agent)
	}
}
