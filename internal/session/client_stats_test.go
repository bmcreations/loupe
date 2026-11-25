package session

import "testing"

// Two Handset viewers on one Seeker: the Mac's own app on loopback, measuring
// about 0.5ms, and a second Mac over a DERP relay, measuring about 140ms. With
// one session-wide report each viewer was judged on whichever landed last, so
// the loopback viewer read "rtt 139ms over a 1ms base" and the relayed
// viewer's pacer took 0.3ms as its base against an ack base near 100ms.
func TestClientStatsAreKeptPerViewer(t *testing.T) {
	s := newTestSession(t)
	local, _ := s.AddViewer()
	remote, _ := s.AddViewer()

	s.RecordClientStats(local, map[string]any{"rttMs": 0.5, "decFps": float64(60)})
	s.RecordClientStats(remote, map[string]any{"rttMs": float64(140), "decFps": float64(30)})

	for _, c := range []struct {
		name string
		id   uint64
		rtt  float64
	}{{"local", local, 0.5}, {"remote", remote, 140}} {
		cs, at := s.ViewerClientStatsAt(c.id)
		if at.IsZero() || cs["rttMs"] != c.rtt {
			t.Errorf("%s viewer's report = %v at %v, want its own rttMs %v", c.name, cs, at, c.rtt)
		}
		ms, rttAt := s.ViewerClientRTTAt(c.id)
		if ms != c.rtt || rttAt.IsZero() {
			t.Errorf("%s viewer's rtt = %v at %v, want its own %v", c.name, ms, rttAt, c.rtt)
		}
	}

	// A viewer that has not reported has no report, not someone else's.
	quiet, _ := s.AddViewer()
	if cs, at := s.ViewerClientStatsAt(quiet); cs != nil || !at.IsZero() {
		t.Errorf("unreported viewer was handed %v at %v", cs, at)
	}
	if ms, at := s.ViewerClientRTTAt(quiet); ms != 0 || !at.IsZero() {
		t.Errorf("unreported viewer was handed rtt %v at %v", ms, at)
	}

	// A report for a viewer that has gone is dropped, and the session-wide
	// summary still shows the newest report from anyone.
	s.RemoveViewer(remote)
	s.RecordClientStats(remote, map[string]any{"rttMs": float64(999)})
	if cs, _ := s.ViewerClientStatsAt(remote); cs != nil {
		t.Errorf("removed viewer still has a report: %v", cs)
	}
	if cs := s.ClientStats(); cs["rttMs"] != float64(140) {
		t.Errorf("session summary = %v, want the newest live report (rttMs 140)", cs)
	}
}
