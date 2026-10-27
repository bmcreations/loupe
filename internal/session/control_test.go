package session

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func controlSession() *Session {
	return &Session{
		ID:      "test",
		key:     "test",
		stream:  &rotStream{},
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
		init:    InitPayload{CodedWidth: 480, CodedHeight: 1080},
	}
}

// nextState returns the next MsgControl state on ch, skipping everything else.
func nextState(t *testing.T, ch <-chan OutMsg) ControlState {
	t.Helper()
	m := nextControl(t, ch, MsgControl)
	var st ControlState
	if err := json.Unmarshal(m.Data[1:], &st); err != nil {
		t.Fatalf("MsgControl body: %v", err)
	}
	return st
}

func TestAJoinerIsToldTheControlStateAfterInit(t *testing.T) {
	s := controlSession()
	_, ch := s.AddViewer()
	first := <-ch
	if first.Data[0] != MsgInit {
		t.Fatalf("first message 0x%02x, want init", first.Data[0])
	}
	if st := nextState(t, ch); st.Held || st.Agent != AgentIdle {
		t.Errorf("joiner's state = %+v, want free and idle", st)
	}
}

func TestAViewerTouchHoldsAndEveryViewerHears(t *testing.T) {
	s := controlSession()
	a, cha := s.AddViewer()
	_, chb := s.AddViewer()
	nextState(t, cha)
	nextState(t, chb)

	_ = s.ViewerTouch(a, TouchEvent{Action: 0, PointerID: 1, X: 10, Y: 10})
	for _, ch := range []<-chan OutMsg{cha, chb} {
		if st := nextState(t, ch); !st.Held || st.Holder != a {
			t.Errorf("state = %+v, want held by %d", st, a)
		}
	}
	ok, retry, holder := s.AgentInput("claude")
	if ok || holder != a || retry <= 0 {
		t.Errorf("AgentInput = %v, %v, %d; want refused by %d", ok, retry, holder, a)
	}
	if st := nextState(t, chb); st.Agent != AgentWaiting || st.AgentName != "claude" {
		t.Errorf("state = %+v, want claude waiting", st)
	}
}

func TestTheHoldLapsesWithNoFurtherInput(t *testing.T) {
	s := controlSession()
	s.SetHumanHold(40 * time.Millisecond)
	var mu sync.Mutex
	var changes []ControlChange
	s.SetControlObserver(func(c ControlChange) {
		mu.Lock()
		changes = append(changes, c)
		mu.Unlock()
	})
	id, ch := s.AddViewer()
	nextState(t, ch)
	_ = s.ViewerKey(id, KeyEvent{Action: 1, Keycode: 29})
	if st := nextState(t, ch); !st.Held {
		t.Fatalf("state = %+v, want held", st)
	}
	if st := nextState(t, ch); st.Held {
		t.Fatalf("state = %+v, want free once the window lapsed", st)
	}
	if ok, _, _ := s.AgentInput(""); !ok {
		t.Error("agent refused after the hold lapsed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(changes) < 2 || changes[0].State != "held" || changes[1].State != "free" {
		t.Errorf("observed %+v, want held then free", changes)
	}
}

func TestTheHolderLeavingFreesTheTarget(t *testing.T) {
	s := controlSession()
	a, _ := s.AddViewer()
	_, chb := s.AddViewer()
	nextState(t, chb)
	_ = s.ViewerTouch(a, TouchEvent{Action: 0, PointerID: 1})
	nextState(t, chb)
	s.RemoveViewer(a)
	if st := nextState(t, chb); st.Held {
		t.Errorf("state = %+v after the holder left, want free", st)
	}
}

func TestHumanInputNeedsAnAttachedViewer(t *testing.T) {
	s := controlSession()
	if s.HumanInput(7) {
		t.Fatal("HumanInput accepted a viewer that is not attached")
	}
	id, _ := s.AddViewer()
	if !s.HumanInput(id) {
		t.Fatal("HumanInput refused an attached viewer")
	}
	if ok, _, _ := s.AgentInput(""); ok {
		t.Error("agent allowed after a viewer's /api/input")
	}
}

func TestTheSessionEndingRecordsTheAgentIdle(t *testing.T) {
	s := controlSession()
	var changes []ControlChange
	s.SetControlObserver(func(c ControlChange) { changes = append(changes, c) })
	s.AgentActed("claude")
	s.stopControl()
	if n := len(changes); n != 2 || changes[1].Agent != AgentIdle || changes[1].AgentName != "claude" {
		t.Fatalf("observed %+v, want driving then idle/claude", changes)
	}
}

func TestAViewerTakesOverFromADrivingAgent(t *testing.T) {
	s := controlSession()
	id, ch := s.AddViewer()
	nextState(t, ch)
	s.AgentActed("claude")
	nextState(t, ch)

	if err := s.ViewerTakeOver(id); err != nil {
		t.Fatalf("ViewerTakeOver: %v", err)
	}
	if st := nextState(t, ch); !st.Held || st.Holder != id || st.Agent != AgentWaiting {
		t.Errorf("state = %+v, want held by %d with claude waiting", st, id)
	}
	if err := s.ViewerTakeOver(id + 100); err == nil {
		t.Error("ViewerTakeOver accepted a viewer that is not attached")
	}
}
