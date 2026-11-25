package session

import "testing"

// The relayed-link rig on 2026-11-25: viewer A on loopback missed none of
// 7,138 frames and read "bad: shed N/s" for 39 seconds, because every one of
// the session's 1,955 sheds was viewer B's. Each viewer's count holds only the
// frames shed for it, and the session totals still hold all of them.
func TestShedsAreCountedPerViewer(t *testing.T) {
	s := newTestSession(t)
	direct, _ := s.AddViewer()
	relayed, _ := s.AddViewer()

	for range 2 {
		s.NoteShedFor(relayed, ShedPacer)
		s.NoteShedFor(relayed, ShedStale)
	}
	for range 26 {
		s.NoteShedFor(relayed, ShedAwaitKey)
	}

	if n := s.ViewerShedCount(direct); n != 0 {
		t.Errorf("direct viewer's sheds = %d, want 0: none were its own", n)
	}
	if n := s.ViewerShedCount(relayed); n != 30 {
		t.Errorf("relayed viewer's sheds = %d, want its own 30", n)
	}
	if n := s.ShedCount(); n != 30 {
		t.Errorf("session sheds = %d, want 30", n)
	}
	if b := s.ShedBreakdown(); b.Pacer+b.Stale+b.AwaitKey+b.Abandon != s.ShedCount() {
		t.Errorf("breakdown %+v does not sum to the session's %d", b, s.ShedCount())
	}

	// A viewer that has gone has no count, not the session's, and a shed noted
	// after it left still counts for the session without bringing it back.
	s.RemoveViewer(relayed)
	if n := s.ViewerShedCount(relayed); n != 0 {
		t.Errorf("removed viewer's sheds = %d, want 0", n)
	}
	s.NoteShedFor(relayed, ShedStale)
	if n := s.ViewerShedCount(relayed); n != 0 {
		t.Errorf("removed viewer's sheds after a late shed = %d, want 0", n)
	}
	if n := s.ShedCount(); n != 31 {
		t.Errorf("session sheds = %d, want 31", n)
	}
	if n := s.ViewerShedCount(direct); n != 0 {
		t.Errorf("direct viewer's sheds = %d after the relayed viewer left, want 0", n)
	}

	// A viewer that joins later starts at zero, not at the session's total.
	late, _ := s.AddViewer()
	if n := s.ViewerShedCount(late); n != 0 {
		t.Errorf("late viewer's sheds = %d, want 0", n)
	}
}
