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

// The relayed-link rig on 2026-11-25 (synthetic, seed 1, 120s): viewer A on
// loopback missed 0 of 7,138 frames and still read "bad: shed N/s" for 39
// seconds, because every one of the session's 1,955 sheds was viewer B's, on
// a shaped link with 250-950ms spikes. A viewer's verdict and its shedRate
// have to count only the frames shed for it.
func TestEachViewerIsJudgedOnItsOwnSheds(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400})
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	attach := time.Now().Add(-time.Second)
	direct, _ := sess.AddViewer()
	relayed, _ := sess.AddViewer()
	sess.RecordClientStats(direct, map[string]any{"rttMs": 0.5, "decFps": 60.0})
	sess.RecordClientStats(relayed, map[string]any{"rttMs": 140.0, "decFps": 60.0})

	type viewerTick struct {
		name     string
		id       uint64
		pacer    *session.Pacer
		tr       *healthTracker
		shedRate uint64
		verdict  string
		reason   string
	}
	viewers := []*viewerTick{
		{name: "direct", id: direct, shedRate: 0, verdict: "good", reason: ""},
		{name: "relayed", id: relayed, shedRate: 30, verdict: "bad", reason: "shed 30/s"},
	}
	for _, v := range viewers {
		v.pacer = session.NewPacer()
		v.tr = newHealthTracker(attach, time.Now)
	}
	viewers[0].pacer.Observe(500 * time.Microsecond)
	viewers[1].pacer.Observe(100 * time.Millisecond)

	tick := func(v *viewerTick) map[string]any {
		t.Helper()
		counters := viewerCounters{Session: sess, id: v.id}
		_, rec, err := runStatsTick("dev1", counters, v.tr, v.pacer, fakeConn{name: "ws"}, "client", session.FlowStats{}, v.name == "direct")
		if err != nil {
			t.Fatalf("%s: runStatsTick: %v", v.name, err)
		}
		return rec
	}

	// The first tick seeds each tracker's diff base.
	for _, v := range viewers {
		tick(v)
	}

	// One second of the relayed viewer's link: the run's mix, mostly frames
	// held back awaiting a keyframe after a stale or paced one.
	for range 2 {
		sess.NoteShedFor(relayed, session.ShedPacer)
		sess.NoteShedFor(relayed, session.ShedStale)
	}
	for range 26 {
		sess.NoteShedFor(relayed, session.ShedAwaitKey)
	}

	for _, v := range viewers {
		rec := tick(v)
		if got, _ := rec["shedRate"].(uint64); got != v.shedRate {
			t.Errorf("%s viewer's shedRate = %d, want its own %d", v.name, got, v.shedRate)
		}
		if got, _ := rec["viewerShed"].(uint64); got != v.shedRate {
			t.Errorf("%s viewer's viewerShed = %d, want its own %d", v.name, got, v.shedRate)
		}
		if rec["health"] != v.verdict || rec["healthReason"] != v.reason {
			t.Errorf("%s viewer read %v %q, want %s %q", v.name, rec["health"], rec["healthReason"], v.verdict, v.reason)
		}
		// The cumulative count and its breakdown stay session wide: they
		// sum to each other in every row (trap 27).
		if rec["shed"] != uint64(30) || rec["shedAwaitKey"] != uint64(26) {
			t.Errorf("%s viewer's row has shed %v, shedAwaitKey %v, want the session's 30 and 26", v.name, rec["shed"], rec["shedAwaitKey"])
		}
	}
}
