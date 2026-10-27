package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// The pacer steered on a signal that travelled over the link it was throttling.
// In the 07:36 trace that signal froze for three seconds at a time -- eight
// such runs in 96 rows, each bracketing a congestion event -- and the worst of
// them held `pacerKeep` at 1.00 while `shed` climbed 270 to 315.
func TestPickRTTPrefersTheTransportMeasurement(t *testing.T) {
	// A fresh QUIC sample wins even when the phone's report is recent, because
	// only one of the two is measured beneath the queue.
	rtt, src := pickRTT(180*time.Millisecond, 40, 100*time.Millisecond)
	if src != "quic" || rtt != 180*time.Millisecond {
		t.Errorf("got %v from %q, want 180ms from quic", rtt, src)
	}
}

func TestPickRTTFallsBackToAFreshClientReport(t *testing.T) {
	// No QUIC sample: WebSocket, or the window before the first ACK.
	rtt, src := pickRTT(0, 40, 100*time.Millisecond)
	if src != "client" || rtt != 40*time.Millisecond {
		t.Errorf("got %v from %q, want 40ms from client", rtt, src)
	}
}

func TestPickRTTDiscardsAStaleClientReport(t *testing.T) {
	// This is the 07:36:43-45 sample: 111.3ms, three seconds old, repeated into
	// the controller once a second while the link drowned. Zero means "no
	// signal", which holds the send fraction steady; the old behaviour read it
	// as "the path is fine" and opened up.
	rtt, src := pickRTT(0, 111.3, 3*time.Second)
	if rtt != 0 || src != "stale" {
		t.Errorf("got %v from %q, want 0 from stale", rtt, src)
	}
}

// Both thresholds assumed a fast path, which is trap 5 one layer down. On the
// 233ms baseline 5G measured here, half of the 250ms write budget is gone to
// propagation before anything is wrong: twelve rows in the trace report
// writeMaxMs within 8ms of 250.0 with "payload: deadline exceeded", against an
// average write of 27ms.
func TestFrameDeadlineScalesWithThePath(t *testing.T) {
	fast := frameDeadlineFor(10 * time.Millisecond)
	slow := frameDeadlineFor(233 * time.Millisecond)

	if fast < frameDeadline {
		t.Errorf("deadline %v on a fast path is below the floor %v", fast, frameDeadline)
	}
	if slow <= fast {
		t.Errorf("deadline is %v on a 233ms path and %v on a 10ms one; a slow path needs more, not the same", slow, fast)
	}
	// A frame on a 233ms path has spent roughly 115ms in flight before anything
	// has gone wrong, so the budget has to cover that on top of the floor.
	if slow < frameDeadline+100*time.Millisecond {
		t.Errorf("deadline %v on a 233ms path leaves under 100ms for propagation", slow)
	}
	// And a wild baseline must not let one frame hold the transport open.
	if got := frameDeadlineFor(30 * time.Second); got != frameDeadlineCap {
		t.Errorf("deadline %v on a 30s baseline, want the cap %v", got, frameDeadlineCap)
	}
}

// A frame abandoned at its deadline is not a delivery and not a disconnect.
// Returning nil for it was the seventh instance of this project's
// unmeasured-branch shape: the viewer credited the frame to goodput, which is
// the one direct measurement of what the link carries, and cleared its keyframe
// wait on a keyframe the client never received.
//
// The viewer loop has no harness, so the contract is tested here instead.
func TestAnAbandonedFrameIsNeitherDeliveredNorFatal(t *testing.T) {
	cases := []struct {
		name             string
		err              error
		delivered, fatal bool
	}{
		{"delivered", nil, true, false},
		{"abandoned", errFrameAbandoned, false, false},
		{"wrapped abandoned", fmt.Errorf("frame 12: %w", errFrameAbandoned), false, false},
		{"connection gone", errors.New("stream closed"), false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			delivered, fatal := frameDelivered(c.err)
			if delivered != c.delivered || fatal != c.fatal {
				t.Errorf("frameDelivered(%v) = %v, %v; want %v, %v",
					c.err, delivered, fatal, c.delivered, c.fatal)
			}
		})
	}
}

// TestAKeyframeGetsLongerThanAPictureItReplaces pins the exemption the shed
// gate already states. Parameter sets and keyframes are never shed, and trap 35
// is what happened when the write deadline did not honour the same rule: at a
// 380ms baseline every frame got 440ms, and a keyframe at the 250 kbps floor
// needed roughly twice that.
// observedKeyframeWrite is the worst keyframe write measured on a link shaped
// to 400 kbps with a 300ms queue -- the conditions of trap 35, where the old
// 440ms deadline abandoned every keyframe. Both deadline tests assert against
// this rather than against each other, so changing a constant cannot move the
// expectation with it.
const observedKeyframeWrite = 1187 * time.Millisecond

func TestAKeyframeGetsLongerThanAPictureItReplaces(t *testing.T) {
	const base = 380 * time.Millisecond

	picture := deadlineFor(false, false, base)
	key := deadlineFor(true, false, base)
	cfg := deadlineFor(false, true, base)

	if key <= picture {
		t.Errorf("keyframe deadline %v does not exceed the picture deadline %v", key, picture)
	}
	if cfg != key {
		t.Errorf("parameter set got %v, want the keyframe deadline %v", cfg, key)
	}
	// Assert against the measurement, not against the constants. Comparing a
	// deadline to a multiple of another deadline moves both sides together when
	// a constant changes, which is how a 250ms floor passed this test while
	// reinstating trap 35.
	if key < observedKeyframeWrite {
		t.Errorf("keyframe deadline %v is under the measured worst write %v", key, observedKeyframeWrite)
	}
	if picture >= observedKeyframeWrite {
		t.Errorf("picture deadline %v already covers %v, so this test proves nothing", picture, observedKeyframeWrite)
	}
}

// TestTheKeyframeDeadlineScalesAndIsStillBounded is the trap 5 direction: a
// constant assumes a fast path. It is bounded in the other direction because a
// frame that holds the transport indefinitely is its own fault.
func TestTheKeyframeDeadlineScalesAndIsStillBounded(t *testing.T) {
	fast := keyframeDeadlineFor(10 * time.Millisecond)
	slow := keyframeDeadlineFor(380 * time.Millisecond)

	if fast < observedKeyframeWrite {
		t.Errorf("deadline %v on a fast path is under the measured worst write %v", fast, observedKeyframeWrite)
	}
	if slow <= fast {
		t.Errorf("deadline %v on a 380ms baseline does not exceed %v on a 10ms one", slow, fast)
	}
	if got := keyframeDeadlineFor(-time.Second); got != keyframeDeadline {
		t.Errorf("deadline %v on a negative baseline, want the floor %v", got, keyframeDeadline)
	}

	// The other side, and an absolute bound for the same reason as above. 5s is
	// maxPlausibleTransitMs in web/jitter.js: at or past it, a keyframe this
	// transport delivered is one the client's own guard would discard as
	// implausible, which trap 31 relies on not happening.
	const clientPlausibilityGuard = 5 * time.Second
	if got := keyframeDeadlineFor(time.Hour); got >= clientPlausibilityGuard {
		t.Errorf("deadline %v on a one-hour baseline reaches the client guard %v", got, clientPlausibilityGuard)
	}

	// The multiplier, not just the floor. A keyframe is an order of magnitude
	// larger than a picture, so it pays the path cost over many more round
	// trips and its margin has to widen as the path slows. Scaling both on the
	// same term leaves that margin constant, which passes every assertion above
	// and still starves the frame the decoder is waiting for on a slow link.
	near := keyframeDeadlineFor(20*time.Millisecond) - frameDeadlineFor(20*time.Millisecond)
	far := keyframeDeadlineFor(400*time.Millisecond) - frameDeadlineFor(400*time.Millisecond)
	if far <= near {
		t.Errorf("keyframe margin %v on a 400ms path does not exceed %v on a 20ms one", far, near)
	}
}

// No report has ever arrived: reportAge must read above clientRTTMaxAge so
// session.Health treats it as stale rather than as a healthy, silent 0/0.
func TestHealthInputFromClientReportWithNoReportReadsAsStale(t *testing.T) {
	now := time.Now()
	rttMs, delivered, age := healthInputFromClientReport(nil, time.Time{}, now, now)
	if rttMs != 0 || delivered != 0 {
		t.Errorf("got rttMs %v delivered %v, want 0, 0", rttMs, delivered)
	}
	if age <= clientRTTMaxAge {
		t.Errorf("age %v does not exceed clientRTTMaxAge %v; a never-reported session would read as fresh", age, clientRTTMaxAge)
	}
}

// clientStats is session scoped, not viewer scoped: a report recorded before
// this viewer attached belongs to whichever connection was there first, and
// must read exactly like no report at all -- not as a fresh, healthy 0ms rtt
// (finding 4). This is what keeps a joining viewer from being judged against
// another connection's leftover numbers while its own pacer Base() is still
// 0.
func TestHealthInputFromClientReportBeforeAttachReadsAsStale(t *testing.T) {
	attachedAt := time.Now()
	reportedBeforeAttach := attachedAt.Add(-time.Second)
	now := attachedAt.Add(time.Millisecond)

	cs := map[string]any{"rttMs": 900.0, "decFps": 60.0}
	rttMs, delivered, age := healthInputFromClientReport(cs, reportedBeforeAttach, attachedAt, now)
	if rttMs != 0 || delivered != 0 {
		t.Errorf("got rttMs %v delivered %v, want 0, 0 for a pre-attach report", rttMs, delivered)
	}
	if age <= clientRTTMaxAge {
		t.Errorf("age %v does not exceed clientRTTMaxAge %v; a pre-attach report would otherwise read as fresh", age, clientRTTMaxAge)
	}
}

// The mirror of the above: a report recorded at or after attach is this
// viewer's own and must be read normally.
func TestHealthInputFromClientReportAtAttachIsRead(t *testing.T) {
	attachedAt := time.Now()
	now := attachedAt.Add(time.Millisecond)

	cs := map[string]any{"rttMs": 12.0, "decFps": 55.0}
	rttMs, delivered, age := healthInputFromClientReport(cs, attachedAt, attachedAt, now)
	if rttMs != 12 || delivered != 55 {
		t.Errorf("got rttMs %v delivered %v, want 12, 55 for a report recorded exactly at attach", rttMs, delivered)
	}
	if age != time.Millisecond {
		t.Errorf("age = %v, want exactly 1ms (now - at, not time.Since)", age)
	}
}

// statsMsgBody is what a viewer's transport actually sends every second.
// Both fields the reviewer named -- "health" and "healthReason" -- must be
// present, and a stale verdict must not repeat old fps/rtt numbers.
func TestStatsMsgBodyCarriesHealthAndReason(t *testing.T) {
	tick := healthTick{ShedRate: 3, RttMs: 44, Delivered: 58}
	body := statsMsgBody(tick, "degraded", "shed 3/s", 900, 55.5, 1.0, "webtransport")

	if body["health"] != "degraded" {
		t.Errorf("body[\"health\"] = %v, want \"degraded\"", body["health"])
	}
	if body["healthReason"] != "shed 3/s" {
		t.Errorf("body[\"healthReason\"] = %v, want \"shed 3/s\"", body["healthReason"])
	}
	if body["fps"] != 58 {
		t.Errorf("body[\"fps\"] = %v, want 58", body["fps"])
	}
	if body["rttMs"] != float64(44) {
		t.Errorf("body[\"rttMs\"] = %v, want 44", body["rttMs"])
	}
}

// A stale verdict omits fps and rttMs entirely (finding 6) rather than
// repeating whatever the last live report happened to say.
func TestStatsMsgBodyOmitsFpsAndRttWhenStale(t *testing.T) {
	tick := healthTick{RttMs: 999, Delivered: 999}
	body := statsMsgBody(tick, "stale", "no report", 0, 0, 1.0, "websocket")

	if _, ok := body["fps"]; ok {
		t.Errorf("body has \"fps\" = %v while stale, want it omitted", body["fps"])
	}
	if _, ok := body["rttMs"]; ok {
		t.Errorf("body has \"rttMs\" = %v while stale, want it omitted", body["rttMs"])
	}
}

// Delivered is read from the client's own decFps, and nothing else. A report
// that also carries other numeric fields -- kbps, bufferMs, decodeQueue --
// must not leak into it, which is the wiring-level form of trap 35: the only
// thing this function will ever call "delivered" is what web/app.js's
// sendClientStats calls decFps, never a byte count the server measured
// itself.
func TestHealthInputFromClientReportReadsOnlyDecFps(t *testing.T) {
	at := time.Now()
	cs := map[string]any{
		"rttMs":     42.5,
		"decFps":    37.0,
		"kbps":      float64(900),
		"fps":       float64(60),
		"bufferMs":  float64(120),
		"transport": "webtransport",
	}
	rttMs, delivered, age := healthInputFromClientReport(cs, at, at.Add(-time.Second), time.Now())
	if rttMs != 42.5 {
		t.Errorf("rttMs = %v, want 42.5", rttMs)
	}
	if delivered != 37 {
		t.Errorf("delivered = %v, want 37 (decFps), not kbps/fps/bufferMs", delivered)
	}
	if age < 0 || age > time.Second {
		t.Errorf("age = %v, want ~0 for a report recorded just now", age)
	}
}

// A malformed or missing field (wrong JSON type, or simply absent) reads as
// zero rather than panicking a type assertion. json.Unmarshal into
// map[string]any always produces float64 for a JSON number, so this is a
// defensive floor, not an expected client shape.
func TestHealthInputFromClientReportToleratesMissingFields(t *testing.T) {
	now := time.Now()
	rttMs, delivered, _ := healthInputFromClientReport(map[string]any{}, now, now.Add(-time.Second), now)
	if rttMs != 0 || delivered != 0 {
		t.Errorf("got rttMs %v delivered %v, want 0, 0 for an empty report", rttMs, delivered)
	}
}

// fakeStatsSource is a minimal statsSource for pinning runStatsTick's own
// wiring (M20/M21/M22 from the round-3 review) independent of a live
// session pump or transport.
type fakeStatsSource struct {
	media, shed uint64
	cs          map[string]any
	csAt        time.Time
	frameCount  uint64
	viewers     int
}

func (f fakeStatsSource) MediaCount() uint64                         { return f.media }
func (f fakeStatsSource) ShedCount() uint64                          { return f.shed }
func (f fakeStatsSource) ClientStatsAt() (map[string]any, time.Time) { return f.cs, f.csAt }
func (f fakeStatsSource) FrameCount() uint64                         { return f.frameCount }
func (f fakeStatsSource) ShedBreakdown() session.ShedCounts          { return session.ShedCounts{} }
func (f fakeStatsSource) DropCount() uint64                          { return 0 }
func (f fakeStatsSource) WaitCount() uint64                          { return 0 }
func (f fakeStatsSource) KeyCount() uint64                           { return 0 }
func (f fakeStatsSource) Stats() map[string]any                      { return map[string]any{"framesOut": uint64(0)} }
func (f fakeStatsSource) ViewerCount() int                           { return f.viewers }
func (f fakeStatsSource) ClientStats() map[string]any                { return f.cs }

type fakeConn struct{ name string }

func (c fakeConn) SendFrame(f session.OutMsg, deadline time.Time) error { return nil }
func (c fakeConn) SendControl(b []byte) error                           { return nil }
func (c fakeConn) Recv() ([]byte, error)                                { return nil, nil }
func (c fakeConn) Keepalive() error                                     { return nil }
func (c fakeConn) Name() string                                         { return c.name }
func (c fakeConn) Close() error                                         { return nil }

// TestRunStatsTickThreadsTheRealPacerBaseline pins M20: a mutation that calls
// healthTrack.Sample(sess, 0) instead of the pacer's own Base() silently
// turns off the rtt check for every viewer, because BaseMs == 0 makes
// session.Health skip it regardless of how bad the rtt actually is.
//
// The reported "baseMs" field in MsgStats is computed from the pacer
// directly and does not observe what was threaded into Sample, so this
// cannot be pinned by reading the body's baseMs back -- it has to watch the
// verdict, which is the only place a wrong BaseMs is observable. A report
// with a good delivery ratio and no shedding but a real rtt over 3x the
// baseline must read "bad" naming rtt; if the mutation lands, BaseMs==0
// short-circuits that check in session.Health and it reads "good" instead.
func TestRunStatsTickThreadsTheRealPacerBaseline(t *testing.T) {
	attach := time.Now()
	tr := newHealthTracker(attach, func() time.Time { return attach.Add(time.Second) })
	pacer := session.NewPacer()
	pacer.Observe(50 * time.Millisecond) // drives Base() to a known nonzero ~50ms

	sess := fakeStatsSource{
		media: 60, frameCount: 60,
		// Delivery and shed are perfect; only rtt is bad: 200ms over a ~50ms
		// base is well past badRttExcess (120ms).
		cs: map[string]any{"rttMs": 250.0, "decFps": 60.0}, csAt: attach.Add(900 * time.Millisecond),
	}
	body, _, err := runStatsTick("t", sess, tr, pacer, fakeConn{name: "ws"}, "client")
	if err != nil {
		t.Fatalf("runStatsTick: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pacer.Base() == 0 {
		t.Fatal("test setup: pacer.Base() is 0, cannot distinguish from the mutation")
	}
	if got["health"] != "bad" {
		t.Errorf("health = %v, want bad (rtt threaded from a real nonzero base)", got["health"])
	}
	if reason, _ := got["healthReason"].(string); !strings.Contains(reason, "rtt") {
		t.Errorf("healthReason = %q, want it to name rtt", reason)
	}
}

// TestRunStatsTickReportsTheComputedVerdict pins M21: a mutation that hands
// MsgStats a hardcoded "good" regardless of session.Health's actual verdict
// would hide every one of the health findings above from the viewer that
// most needs them. Rather than pin one specific string (which would break
// the moment a threshold is legitimately retuned), this asserts that the
// message body and the trace record -- built from the same computed verdict
// -- always agree, and separately that a case designed to read "bad" does.
func TestRunStatsTickReportsTheComputedVerdict(t *testing.T) {
	attach := time.Now()
	var now time.Time
	tr := newHealthTracker(attach, func() time.Time { return now })
	pacer := session.NewPacer()

	// Feed enough fresh reports of 0 delivered against 60 produced to clear
	// session.MinProducedForRatio and reach a real "bad" judgement, not the
	// too-few-frames "good" a single tick would read.
	var body []byte
	var rec map[string]any
	for i := 1; i <= 6; i++ {
		now = attach.Add(time.Duration(i) * time.Second)
		sess := fakeStatsSource{
			media: uint64(60 * i), frameCount: uint64(60 * i),
			cs: map[string]any{"rttMs": 12.0, "decFps": 0.0}, csAt: now.Add(-100 * time.Millisecond),
		}
		var err error
		body, rec, err = runStatsTick("t", sess, tr, pacer, fakeConn{name: "ws"}, "client")
		if err != nil {
			t.Fatalf("runStatsTick: %v", err)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["health"] != rec["health"] {
		t.Errorf("MsgStats health %q != trace health %q", got["health"], rec["health"])
	}
	if got["healthReason"] != rec["healthReason"] {
		t.Errorf("MsgStats healthReason %q != trace healthReason %q", got["healthReason"], rec["healthReason"])
	}
	if got["health"] == "good" {
		t.Errorf("sustained 0-of-60 delivery read %q, want a non-good verdict", got["health"])
	}
}

// TestRunStatsTickTraceCarriesHealth pins M22: the trace record must carry
// the same health verdict and reason as the message the viewer receives, not
// silently omit them.
func TestRunStatsTickTraceCarriesHealth(t *testing.T) {
	attach := time.Now()
	tr := newHealthTracker(attach, func() time.Time { return attach.Add(time.Second) })
	pacer := session.NewPacer()
	sess := fakeStatsSource{
		media: 60, frameCount: 60,
		cs: map[string]any{"rttMs": 12.0, "decFps": 60.0}, csAt: attach.Add(900 * time.Millisecond),
	}
	_, rec, err := runStatsTick("t", sess, tr, pacer, fakeConn{name: "ws"}, "client")
	if err != nil {
		t.Fatalf("runStatsTick: %v", err)
	}
	if _, ok := rec["health"]; !ok {
		t.Error("trace record has no \"health\" field")
	}
	if _, ok := rec["healthReason"]; !ok {
		t.Error("trace record has no \"healthReason\" field")
	}
}

// Health reads the round trip of the last second, not the display's running
// average: that average takes about four seconds to fall back after a burst,
// and the dot read degraded for all of them.
func TestHealthInputPrefersTheLastSecondsRtt(t *testing.T) {
	at := time.Unix(1000, 0)
	cs := map[string]any{"rttMs": 150.0, "rttNowMs": 60.0, "decFps": 60.0}
	rtt, _, _ := healthInputFromClientReport(cs, at, at, at)
	if rtt != 60 {
		t.Fatalf("rtt = %v, want the last second's 60", rtt)
	}
	delete(cs, "rttNowMs")
	if rtt, _, _ = healthInputFromClientReport(cs, at, at, at); rtt != 150 {
		t.Fatalf("without rttNowMs rtt = %v, want rttMs 150", rtt)
	}
}

// stalledConn fails every send and, like a phone still swiping, keeps Recv
// open until the connection is closed.
type stalledConn struct {
	fakeConn
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *stalledConn) SendFrame(session.OutMsg, time.Time) error { return errors.New("send failed") }
func (c *stalledConn) SendControl([]byte) error                  { return errors.New("send failed") }
func (c *stalledConn) Recv() ([]byte, error) {
	<-c.closed
	return nil, io.EOF
}
func (c *stalledConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// TestServeViewerEndsWhenSendingStops pins the 12:56:03 stall: a send error
// ended the writer while Recv stayed open, so the viewer stayed attached with
// no video and no stats until the page was backgrounded 21s later.
func TestServeViewerEndsWhenSendingStops(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	sess, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	conn := &stalledConn{fakeConn: fakeConn{name: "stalled"}, closed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		s.serveViewer("stub", sess, conn)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = conn.Close()
		t.Fatal("serveViewer still running 3s after every send failed")
	}
}

// resetConn is the 07:05 shape: the phone resets the control stream, so Recv
// fails first, and every send after that meets the connection the read loop
// closed.
type resetConn struct {
	fakeConn
	closed    chan struct{}
	closeOnce sync.Once
}

var (
	errControlReset = errors.New("stream canceled with error code 0")
	errClosedHere   = errors.New("session closed locally, code 0")
)

func (c *resetConn) Recv() ([]byte, error) { return nil, errControlReset }
func (c *resetConn) SendFrame(session.OutMsg, time.Time) error {
	return c.send()
}
func (c *resetConn) SendControl([]byte) error { return c.send() }
func (c *resetConn) send() error {
	select {
	case <-c.closed:
		return fmt.Errorf("open frame stream: %w", errClosedHere)
	default:
		return nil
	}
}
func (c *resetConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// The viewer-stop record names the side that stopped first. Ten records in
// the 28 September 07:05 trace blamed the writer for the close the read loop
// made after the phone reset its control stream.
func TestViewerStopNamesTheSideThatStoppedFirst(t *testing.T) {
	s, path := settingsServerWithTrace(t, &stubTarget{})
	sess, err := s.mgr.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	conn := &resetConn{fakeConn: fakeConn{name: "reset"}, closed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		s.serveViewer("stub", sess, conn)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveViewer still running 3s after Recv failed")
	}

	var stops []map[string]any
	for _, rec := range readTraceRecords(t, path) {
		if rec["kind"] == "viewer-stop" {
			stops = append(stops, rec)
		}
	}
	if len(stops) != 1 {
		t.Fatalf("got %d viewer-stop records, want 1: %v", len(stops), stops)
	}
	rec := stops[0]
	if rec["side"] != "read" || rec["err"] != errControlReset.Error() {
		t.Errorf("side %v err %v, want the read error", rec["side"], rec["err"])
	}
	// The writer may or may not reach a send before serveViewer returns; when
	// it does, its error is kept beside the cause rather than in place of it.
	if w, ok := rec["writeErr"]; ok && w != "open frame stream: "+errClosedHere.Error() {
		t.Errorf("writeErr = %v", w)
	}
}

func TestViewerStopsKeepsTheFirstSide(t *testing.T) {
	sendErr := fmt.Errorf("open frame stream: %w", errors.New("boom"))
	cases := []struct {
		name     string
		note     func(v *viewerStops)
		side     string
		err      string
		otherKey string
	}{
		{"write then read", func(v *viewerStops) {
			v.note("write", sendErr)
			v.note("read", io.EOF)
		}, "write", "open frame stream: boom", "readErr"},
		{"read then write", func(v *viewerStops) {
			v.note("read", errControlReset)
			v.note("write", sendErr)
		}, "read", errControlReset.Error(), "writeErr"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v viewerStops
			c.note(&v)
			rec := v.record()
			if rec["side"] != c.side || rec["err"] != c.err {
				t.Errorf("side %v err %v", rec["side"], rec["err"])
			}
			if _, ok := rec[c.otherKey]; !ok {
				t.Errorf("no %s in %v", c.otherKey, rec)
			}
		})
	}

	// A writer that ends with its session has no error and stopped first;
	// the read error its close causes is not a reason to record anything.
	var v viewerStops
	v.note("write", nil)
	v.note("read", io.EOF)
	if rec := v.record(); rec != nil {
		t.Errorf("recorded %v for a session that ended", rec)
	}
}

func TestViewerStopRecordsTheInnermostType(t *testing.T) {
	var v viewerStops
	v.note("write", fmt.Errorf("open frame stream: %w", io.ErrClosedPipe))
	rec := v.record()
	if rec["errType"] != "*fmt.wrapError" || rec["innerType"] != "*errors.errorString" {
		t.Errorf("errType %v innerType %v", rec["errType"], rec["innerType"])
	}
}
