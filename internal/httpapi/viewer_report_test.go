package httpapi

import (
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// The Seeker at 22:01 on 2026-11-24: Handset on the Mac it is plugged into
// (loopback, ~0.5ms) and Handset on a second Mac over a DERP relay (~140ms),
// both on one session. Each viewer's stats tick has to judge its own link:
// the loopback viewer was reading "rtt 139ms over a 1ms base" off the relayed
// viewer's report.
func TestEachViewerIsJudgedOnItsOwnReport(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400})
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	attach := time.Now().Add(-time.Second)
	local, _ := sess.AddViewer()
	remote, _ := sess.AddViewer()
	sess.RecordClientStats(local, map[string]any{"rttMs": 0.5, "decFps": 60.0})
	sess.RecordClientStats(remote, map[string]any{"rttMs": 140.0, "decFps": 60.0})

	for _, c := range []struct {
		name string
		id   uint64
		base time.Duration
		rtt  float64
	}{
		{"local", local, 500 * time.Microsecond, 0.5},
		{"remote", remote, 100 * time.Millisecond, 140},
	} {
		pacer := session.NewPacer()
		pacer.Observe(c.base)
		tr := newHealthTracker(attach, time.Now)
		counters := viewerCounters{Session: sess, id: c.id}
		_, rec, err := runStatsTick("dev1", counters, tr, pacer, fakeConn{name: "ws"}, "client", session.FlowStats{}, c.name == "local")
		if err != nil {
			t.Fatalf("%s: runStatsTick: %v", c.name, err)
		}
		cs, _ := rec["client"].(map[string]any)
		if cs["rttMs"] != c.rtt {
			t.Errorf("%s viewer's trace row carries client %v, want its own rttMs %v", c.name, cs, c.rtt)
		}
		if reason, _ := rec["healthReason"].(string); strings.Contains(reason, "rtt") {
			t.Errorf("%s viewer judged on another viewer's rtt: %q", c.name, reason)
		}

		ms, _ := sess.ViewerClientRTTAt(c.id)
		if ms != c.rtt {
			t.Errorf("%s viewer's pacer would read rtt %v, want its own %v", c.name, ms, c.rtt)
		}
	}
}
