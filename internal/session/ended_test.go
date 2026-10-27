package session

import "testing"

func closing(s *Session) bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return s.closing
}

func TestEndTellsEveryViewerWhyBeforeClosing(t *testing.T) {
	s := newTestSession(t)
	_, outA := s.AddViewer()
	_, outB := s.AddViewer()
	outs := []<-chan OutMsg{outA, outB}
	// No pump in this fixture, and Close waits on done.
	close(s.done)
	if closing(s) {
		t.Fatal("fixture starts closed")
	}
	if err := s.End("shutDown"); err != nil {
		t.Fatal(err)
	}
	for i, out := range outs {
		msg := nextControl(t, out, MsgEnded)
		if got := string(msg.Data[1:]); got != `{"reason":"shutDown"}` {
			t.Errorf("viewer %d got %q", i, got)
		}
	}
	if !closing(s) {
		t.Error("End did not close the session")
	}
	if got := s.EndReason(); got != "shutDown" {
		t.Errorf("EndReason = %q", got)
	}
}

func TestEndReasonIsEmptyForAnyOtherClose(t *testing.T) {
	s := newTestSession(t)
	close(s.done)
	_ = s.Close()
	if got := s.EndReason(); got != "" {
		t.Errorf("EndReason = %q, want empty", got)
	}
}

// A viewer whose queue is full still hears why: End gives up the oldest
// message to make room.
func TestEndReachesAViewerWithAFullQueue(t *testing.T) {
	s := newTestSession(t)
	s.queueLen = 3
	_, out := s.AddViewer()
	for len(out) < cap(out) {
		s.sendClipboard("x")
	}
	close(s.done)
	if err := s.End("deleted"); err != nil {
		t.Fatal(err)
	}
	nextControl(t, out, MsgEnded)
}
