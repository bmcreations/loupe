package httpapi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

func openTestTrace(t *testing.T) (*trace.Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tr, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr, path
}

func gateRecords(t *testing.T, path, gate string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range readTraceLines(t, path) {
		if r["kind"] == "bitrate" && r["gate"] == gate {
			out = append(out, r)
		}
	}
	return out
}

// A delay or loss cut, and a sharpen and its skip, put their inputs in the
// trace record next to the gate.
func TestBitrateRecordCarriesTheDecisionsInputs(t *testing.T) {
	tr, path := openTestTrace(t)
	m := NewManager(target.Options{})
	m.SetTrace(tr)
	m.recordBitrate("sim:x", session.BitrateReport{Gate: session.GateLoss, Current: 1_900_000, From: 4_000_000, To: 1_900_000,
		Detail: map[string]any{"deliveredFrac": 0.42, "events": 2.0, "runMs": int64(2000)}})
	m.recordBitrate("sim:x", session.BitrateReport{Gate: session.GateSharpenSkipped, Current: 1_900_000,
		Detail: map[string]any{"why": "awaitKey"}})
	m.recordBitrate("sim:x", session.BitrateReport{Gate: session.GateUndercarried, Current: 250_000, From: 250_000, To: 250_000, Live: true, MaxSize: 720})

	loss := gateRecords(t, path, "loss")
	if len(loss) != 1 || loss[0]["deliveredFrac"] != 0.42 || loss[0]["events"] != 2.0 || loss[0]["from"] != 4_000_000.0 {
		t.Fatalf("loss records %v", loss)
	}
	if sk := gateRecords(t, path, "sharpenSkipped"); len(sk) != 1 || sk[0]["why"] != "awaitKey" {
		t.Fatalf("skip records %v", sk)
	}
	if u := gateRecords(t, path, "undercarried"); len(u) != 1 || u[0]["sizeOnly"] != true || u[0]["maxSize"] != 720.0 {
		t.Fatalf("size-only record %v", u)
	}
}

func TestOpeningRecordsWhyItWasOrWasNotAResume(t *testing.T) {
	tr, path := openTestTrace(t)
	m := NewManager(target.Options{BitRate: 4_000_000})
	m.SetTrace(tr)
	m.SetAdaptiveBitRate(true)
	tgt := &optsTarget{opens: make(chan target.Options, 4)}
	m.Register(tgt)

	open := func(p string) {
		t.Helper()
		s, err := m.AcquireFrom(context.Background(), "stub", p)
		if err != nil {
			t.Fatal(err)
		}
		<-tgt.opens
		_ = s.Close()
		m.mu.Lock()
		delete(m.sessions, "stub")
		m.mu.Unlock()
	}
	phone := pathKey("quic", "203.0.113.9:4000")
	open(phone)
	open("")
	m.paths.put(phone, 9_000_000)
	open(phone)
	m.paths.put(phone, 800_000)
	open(phone)

	skipped := gateRecords(t, path, "resumeSkipped")
	var why []any
	for _, r := range skipped {
		why = append(why, r["why"])
	}
	if len(why) != 3 || why[0] != "nothingRemembered" || why[1] != "noPath" || why[2] != "notBelowCeiling" {
		t.Fatalf("resumeSkipped reasons %v", why)
	}
	if res := gateRecords(t, path, "resume"); len(res) != 1 || res[0]["current"] != 800_000.0 || res[0]["remembered"] != 800_000.0 {
		t.Fatalf("resume records %v", res)
	}
}

func TestALeavingViewerSaysWhetherItsRateWasSaved(t *testing.T) {
	tr, path := openTestTrace(t)
	orig := pathStableFor
	t.Cleanup(func() { pathStableFor = orig })

	m := NewManager(target.Options{BitRate: 4_000_000})
	m.Register(&stubTarget{})
	s := &Server{mgr: m, trace: tr}
	leave := func(stableFor time.Duration) {
		pathStableFor = stableFor
		p := pathKey("ws", "203.0.113.9:1")
		sess, err := m.AcquireFrom(t.Context(), "stub", p)
		if err != nil {
			t.Fatal(err)
		}
		conn := &stalledConn{fakeConn: fakeConn{name: "stalled"}, closed: make(chan struct{})}
		done := make(chan struct{})
		go func() { s.serveViewerFrom("stub", sess, conn, p); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = conn.Close()
			t.Fatal("serveViewerFrom did not return")
		}
	}
	leave(0)
	leave(time.Hour)
	if got := gateRecords(t, path, "resumeSaved"); len(got) != 1 || got[0]["rate"] != 4_000_000.0 {
		t.Fatalf("resumeSaved %v", got)
	}
	if got := gateRecords(t, path, "resumeNotSaved"); len(got) != 1 || got[0]["why"] != "rateNotHeld" {
		t.Fatalf("resumeNotSaved %v", got)
	}
}
