package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
)

// TestCheckTimeoutIsTwoSeconds pins the default healthTimeout run wires
// through: hermetic_test.go's passing-path tests rely on it being small
// enough to keep the suite fast, and its failing-path test
// (TestHealthFailsWithinTimeoutWhenNoStatsArrive) relies on it being this
// exact value rather than whatever a future edit leaves behind -- a
// mutation raising it to, say, 20s would still pass every check-level test
// here and only show up as a suite that got twenty seconds slower.
func TestCheckTimeoutIsTwoSeconds(t *testing.T) {
	if checkTimeout != 2*time.Second {
		t.Fatalf("checkTimeout = %v, want 2s", checkTimeout)
	}
}

// goldenBytes mirrors internal/session/golden_test.go's helper of the same
// name: a .hex fixture under testdata/protocol/ is one hex-encoded byte
// string per non-comment, non-blank line, concatenated in order. Reading the
// same fixtures a non-Go client would (docs/protocol.md says as much) is the
// point -- a hand-rolled byte literal here could drift from the wire without
// either side noticing.
func goldenBytes(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "protocol", name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	if !strings.HasSuffix(name, ".hex") {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return b
	}

	var out []byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("%s: bad hex line %q: %v", name, line, err)
		}
		out = append(out, b...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

func TestTargetAllowed(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"synthetic", true},
		{"emulator-5554", true},
		{"emulator-1", true},
		{"emulator-", false},                                // no digits after the dash
		{"emulator", false},                                 // no dash at all
		{"ACME00000000001", false},                          // the Acme One, by id
		{"", false},                                         // missing target
		{"xemulator-5554", false},                           // lookalike: contains "emulator-" but does not start with it
		{"not-an-emulator-5554", false},                     // contains "emulator-" mid-string
		{"EMULATOR-5554", false},                            // case matters; scrcpy/adb ids are lowercase
		{"emulator-5554extra", false},                       // trailing garbage after the digits
		{"emulator-5554&target=ACME00000000001", false},     // query-string injection attempt, decoded form
		{"emulator-5554%26target%3DACME00000000001", false}, // same attempt, percent-encoded literally in the id
	}
	for _, c := range cases {
		if got := targetAllowed(c.id); got != c.want {
			t.Errorf("targetAllowed(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// TestTargetAllowedRejectsContainsStyleGuard pins the full-match regexp
// choice directly: a guard loosened to strings.Contains, or to
// strings.HasPrefix, would let "xemulator-5554" and
// "emulator-5554&target=..." both pass, which is exactly the copy-pasted-id
// mistake the guard exists to catch.
func TestTargetAllowedRejectsContainsStyleGuard(t *testing.T) {
	for _, id := range []string{"xemulator-5554", "not-an-emulator-5554", "reemulator-1", "emulator-5554extra"} {
		if targetAllowed(id) {
			t.Errorf("targetAllowed(%q) = true, want false (contains \"emulator-\" but is not exactly an emulator id)", id)
		}
		if !strings.Contains(id, "emulator-") {
			t.Fatalf("test case %q no longer contains \"emulator-\"; fix the fixture", id)
		}
	}
}

func TestGuardDecision(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		targetOK bool
		wantOK   bool
	}{
		{"synthetic without override", "synthetic", false, true},
		{"synthetic with override", "synthetic", true, true},
		{"emulator without override", "emulator-5554", false, true},
		{"emulator with override", "emulator-5554", true, true},
		{"acme without override", "ACME00000000001", false, false},
		{"acme with override", "ACME00000000001", true, true},
		{"empty without override", "", false, false},
		{"empty with override", "", true, false}, // missing target is refused even with -target-ok
		{"lookalike without override", "xemulator-5554", false, false},
		{"lookalike with override", "xemulator-5554", true, true},
	}
	for _, c := range cases {
		ok, detail := guardDecision(c.id, c.targetOK)
		if ok != c.wantOK {
			t.Errorf("%s: guardDecision(%q, %v) ok = %v, want %v (detail=%q)", c.name, c.id, c.targetOK, ok, c.wantOK, detail)
		}
		if ok && detail != "" {
			t.Errorf("%s: guardDecision(%q, %v) ok=true but detail=%q, want empty", c.name, c.id, c.targetOK, detail)
		}
		if !ok && detail == "" {
			t.Errorf("%s: guardDecision(%q, %v) ok=false but detail is empty", c.name, c.id, c.targetOK)
		}
	}
}

func TestEncodeTouchMatchesGolden(t *testing.T) {
	want := goldenBytes(t, "touch.hex")
	// touch.hex: action=move(2) pointerId=100000 x=700000 y=1500000 pressure=0x1234
	got := encodeTouch(2, 100000, 700000, 1500000, 0x1234)
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeTouch mismatch:\n got  % x\n want % x", got, want)
	}
}

func TestFrameControlMatchesHelloGolden(t *testing.T) {
	want := goldenBytes(t, "wt-control-hello.hex")
	got := frameControl([]byte{session.MsgHello})
	if !bytes.Equal(got, want) {
		t.Fatalf("frameControl(hello) mismatch:\n got  % x\n want % x", got, want)
	}
}

func TestReadControlFrameRoundTripsHello(t *testing.T) {
	framed := goldenBytes(t, "wt-control-hello.hex")
	got, err := readControlFrame(bytes.NewReader(framed))
	if err != nil {
		t.Fatalf("readControlFrame: %v", err)
	}
	if !bytes.Equal(got, []byte{session.MsgHello}) {
		t.Fatalf("got % x, want %02x", got, session.MsgHello)
	}
}

func TestReadControlFrameRejectsOversizedLength(t *testing.T) {
	var buf bytes.Buffer
	// A length prefix past the 1MiB control limit must be rejected before
	// the read blocks trying to fill a huge buffer -- mirrors
	// internal/httpapi/webtransport.go's wtControlLimit check, which exists
	// so an attacker-chosen length cannot drive an unbounded allocation.
	buf.Write([]byte{0x7f, 0xff, 0xff, 0xff}) // ~2GB
	if _, err := readControlFrame(&buf); err == nil {
		t.Fatal("expected an error for an oversized control frame length")
	}
}

func TestDecodeEventMatchesGoldenTap(t *testing.T) {
	frame := goldenBytes(t, "event.hex")
	e, err := decodeEvent(frame)
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if e.Seq != 1 || e.Source != "viewer" || e.Viewer != 2 || e.Kind != "tap" {
		t.Fatalf("unexpected event: %+v", e)
	}
	if e.From == nil || e.From.X != 0.512 || e.From.Y != 0.734 {
		t.Fatalf("unexpected From: %+v", e.From)
	}
}

func TestDecodeEventMatchesGoldenButton(t *testing.T) {
	frame := goldenBytes(t, "event-button.hex")
	e, err := decodeEvent(frame)
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if e.Seq != 3 || e.Kind != "button" || e.Button != "home" || e.Source != "viewer" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

func TestDecodeEventMatchesGoldenRedactedText(t *testing.T) {
	frame := goldenBytes(t, "event-redacted.hex")
	e, err := decodeEvent(frame)
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if e.Seq != 2 || !e.Redacted || e.TextLen != 8 || e.Kind != "text" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

func TestDecodeEventRejectsWrongType(t *testing.T) {
	// event.hex's own type byte flipped to MsgInit -- decodeEvent must
	// refuse rather than silently unmarshal a MsgEvent's JSON body as
	// something else.
	frame := append([]byte{session.MsgInit}, goldenBytes(t, "event.hex")[1:]...)
	if _, err := decodeEvent(frame); err == nil {
		t.Fatal("expected an error for a non-MsgEvent frame")
	}
}

func TestMatchesGestureTap(t *testing.T) {
	frame := goldenBytes(t, "event.hex")
	e, err := decodeEvent(frame)
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	want := gestureWant{source: "viewer", kind: "tap", from: [2]float64{0.512, 0.734}}
	if !matchesGesture(e, want) {
		t.Fatalf("expected match: event=%+v want=%+v", e, want)
	}
}

func TestMatchesGestureToleranceBoundary(t *testing.T) {
	e := session.Event{Source: "viewer", Kind: "tap", From: &session.Point{X: 0.5, Y: 0.5}}
	cases := []struct {
		name string
		x, y float64
		want bool
	}{
		{"exact", 0.5, 0.5, true},
		{"within tolerance", 0.509, 0.5, true},
		{"just outside tolerance", 0.512, 0.5, false},
		{"negative direction within", 0.491, 0.5, true},
		{"negative direction outside", 0.488, 0.5, false},
	}
	for _, c := range cases {
		want := gestureWant{source: "viewer", kind: "tap", from: [2]float64{c.x, c.y}}
		if got := matchesGesture(e, want); got != c.want {
			t.Errorf("%s: matchesGesture(from=[0.5,0.5], want=[%v,%v]) = %v, want %v", c.name, c.x, c.y, got, c.want)
		}
	}
}

// TestCloseEnoughBoundaryIsInclusive pins the boundary exactly at coordTol,
// which is what kills a <= -> < mutation on closeEnough -- the nearby cases
// in TestMatchesGestureToleranceBoundary (0.509 within, 0.512 outside) leave
// a wide enough margin that a < mutant still passes them.
//
// a is 0 rather than 0.5 so the diff is exact float64 arithmetic rather than
// a rounded sum: 0.5+coordTol does not equal 0.51 bit-for-bit (0.01 has no
// exact float64 representation), but 0-coordTol is exactly -coordTol, so
// math.Abs(0-coordTol) is exactly coordTol with no rounding error to
// second-guess.
func TestCloseEnoughBoundaryIsInclusive(t *testing.T) {
	if !closeEnough(0, coordTol) {
		t.Error("closeEnough(0, coordTol) = false, want true (boundary is inclusive)")
	}
	if closeEnough(0, math.Nextafter(coordTol, math.Inf(1))) {
		t.Error("closeEnough(0, just past coordTol) = true, want false")
	}
}

func TestMatchesGestureRejectsWrongSourceOrKind(t *testing.T) {
	e := session.Event{Source: "viewer", Kind: "tap", From: &session.Point{X: 0.5, Y: 0.5}}
	if matchesGesture(e, gestureWant{source: "agent", kind: "tap", from: [2]float64{0.5, 0.5}}) {
		t.Error("matched despite wrong source")
	}
	if matchesGesture(e, gestureWant{source: "viewer", kind: "swipe", from: [2]float64{0.5, 0.5}}) {
		t.Error("matched despite wrong kind")
	}
}

// TestMatchesGestureRejectsWrongCoordinates covers the coordinate axes the
// tolerance-boundary tests above don't: a wrong From.Y (they only ever vary
// From.X against a fixed Y), and a wrong To on each axis independently. A
// matcher that only checked From.X and To.X -- or that compared To against
// From by mistake -- would pass every existing test here and still accept a
// swipe that landed nowhere near where it was sent.
func TestMatchesGestureRejectsWrongCoordinates(t *testing.T) {
	tap := session.Event{Source: "viewer", Kind: "tap", From: &session.Point{X: 0.5, Y: 0.5}}
	if matchesGesture(tap, gestureWant{source: "viewer", kind: "tap", from: [2]float64{0.5, 0.9}}) {
		t.Error("tap matched despite a From.Y far outside tolerance")
	}

	swipe := session.Event{
		Source: "viewer", Kind: "swipe",
		From: &session.Point{X: 0.5, Y: 0.75},
		To:   &session.Point{X: 0.5, Y: 0.25},
	}
	base := gestureWant{source: "viewer", kind: "swipe", from: [2]float64{0.5, 0.75}, hasTo: true, to: [2]float64{0.5, 0.25}}

	wrongToX := base
	wrongToX.to = [2]float64{0.9, 0.25}
	if matchesGesture(swipe, wrongToX) {
		t.Error("swipe matched despite a To.X far outside tolerance")
	}

	wrongToY := base
	wrongToY.to = [2]float64{0.5, 0.9}
	if matchesGesture(swipe, wrongToY) {
		t.Error("swipe matched despite a To.Y far outside tolerance")
	}

	// Sanity: base itself must still match, or the two cases above prove
	// nothing about the axis they claim to isolate.
	if !matchesGesture(swipe, base) {
		t.Fatal("swipe did not match its own exact coordinates")
	}
}

// TestGestureOutcomeIsGatedOnControl pins round 4's fix directly: a gesture
// event that arrived but carries Err must SKIP when the target's own init
// reported control=false (the documented, expected shape for a target that
// said up front it accepts no input, e.g. target.Synthetic), and must FAIL
// when init reported control=true -- a target that claimed to accept input
// and then failed to act on it, which run must not wave through as a SKIP.
// Mutation M17 (SKIP regardless of control) passes every other test in this
// package and is only caught by the control=true case here.
func TestGestureOutcomeIsGatedOnControl(t *testing.T) {
	rejected := session.Event{Seq: 7, Err: "target: this target does not accept input"}
	cases := []struct {
		name    string
		control bool
		e       session.Event
		ok      bool
		want    string
	}{
		{"no control, rejected -> skip", false, rejected, true, "SKIP"},
		{"control, rejected -> fail", true, rejected, true, "FAIL"},
		{"never arrived -> fail regardless of control", false, session.Event{}, false, "FAIL"},
		{"never arrived, control -> fail", true, session.Event{}, false, "FAIL"},
	}
	for _, c := range cases {
		got := gestureOutcome("tap", c.control, checkTimeout, c.e, c.ok)
		if got.status != c.want {
			t.Errorf("%s: gestureOutcome(control=%v, ok=%v) = %s (%s), want %s", c.name, c.control, c.ok, got.status, got.detail, c.want)
		}
	}
}

// TestGestureOutcomePassesOnCleanMatch pins the non-error path unchanged by
// round 4's control gating: an event that arrived with no Err PASSes
// regardless of control, and a swipe's detail carries To while a tap's does
// not.
func TestGestureOutcomePassesOnCleanMatch(t *testing.T) {
	tap := session.Event{Seq: 1, From: &session.Point{X: 0.5, Y: 0.5}}
	if got := gestureOutcome("tap", false, checkTimeout, tap, true); got.status != "PASS" {
		t.Fatalf("tap: got %s (%s), want PASS", got.status, got.detail)
	}
	swipe := session.Event{Seq: 2, From: &session.Point{X: 0.5, Y: 0.75}, To: &session.Point{X: 0.5, Y: 0.25}}
	got := gestureOutcome("swipe", true, checkTimeout, swipe, true)
	if got.status != "PASS" || !strings.Contains(got.detail, "to=") {
		t.Fatalf("swipe: got %s (%s), want PASS with a to= in the detail", got.status, got.detail)
	}
}

func TestMatchesGestureSwipeRequiresTo(t *testing.T) {
	// A tap event (no To) must not satisfy a swipe want that requires one --
	// the zero value of Point isn't a stand-in for "no To was sent".
	e := session.Event{Source: "viewer", Kind: "swipe", From: &session.Point{X: 0.5, Y: 0.75}}
	want := gestureWant{source: "viewer", kind: "swipe", from: [2]float64{0.5, 0.75}, hasTo: true, to: [2]float64{0.5, 0.25}}
	if matchesGesture(e, want) {
		t.Fatal("matched a swipe want against an event with no To")
	}
}

func TestMatchesButtonGoldenHome(t *testing.T) {
	frame := goldenBytes(t, "event-button.hex")
	e, err := decodeEvent(frame)
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if !matchesButton(e, "viewer", "home") {
		t.Fatalf("expected a match: %+v", e)
	}
	if matchesButton(e, "agent", "home") {
		t.Fatal("matched against the wrong source")
	}
	if matchesButton(e, "viewer", "back") {
		t.Fatal("matched against the wrong button")
	}
}

// TestMatchesButtonRejectsErrSet mirrors matchesGesture's own Err guard
// (TestMatchesGestureRejectsWrongCoordinates's sibling would be the wrong
// place for this: it is round 3's Err requirement, not a coordinate check).
// Without the guard, an agent-button POST that the target declined would
// still satisfy matchesButton, and checkAgentButton would report PASS on a
// button press that never reached the device.
func TestMatchesButtonRejectsErrSet(t *testing.T) {
	e := session.Event{Source: "agent", Kind: "button", Button: "home", Err: "target: this target does not accept input"}
	if matchesButton(e, "agent", "home") {
		t.Fatal("matched despite Err being set")
	}
}

func TestSeqStrictlyIncreasing(t *testing.T) {
	cases := []struct {
		name string
		seqs []uint64
		want bool
	}{
		{"empty", nil, true},
		{"single", []uint64{1}, true},
		{"increasing", []uint64{1, 2, 3}, true},
		{"gap is still increasing", []uint64{1, 5, 9}, true},
		{"repeat", []uint64{1, 2, 2, 3}, false},
		{"decreasing", []uint64{3, 2, 1}, false},
		{"restart mid-stream", []uint64{1, 2, 3, 1, 2}, false},
	}
	for _, c := range cases {
		if got := seqStrictlyIncreasing(c.seqs); got != c.want {
			t.Errorf("%s: seqStrictlyIncreasing(%v) = %v, want %v", c.name, c.seqs, got, c.want)
		}
	}
}

func TestMissingFromPageUsesGoldenEventPage(t *testing.T) {
	raw := goldenBytes(t, "eventpage.json")
	var page session.EventPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("unmarshal eventpage.json: %v", err)
	}
	if page.Session != "a1b2c3d4e5f6a7b8" || page.Oldest != 1 || page.Latest != 3 {
		t.Fatalf("unexpected page envelope: %+v", page)
	}

	if missing := missingFromPage([]uint64{1, 2, 3}, page); len(missing) != 0 {
		t.Fatalf("expected nothing missing, got %v", missing)
	}
	if missing := missingFromPage([]uint64{1, 4}, page); len(missing) != 1 || missing[0] != 4 {
		t.Fatalf("expected [4] missing, got %v", missing)
	}
}

// TestAnyFailDoesNotTreatSkipAsPass pins main's whole exit-code decision: a
// run of SKIPs (the expected shape against synthetic, per the plan) must not
// exit non-zero, and a single FAIL among PASS/SKIP must. A mutation that
// widens the FAIL comparison to also match "SKIP" -- or narrows it to match
// nothing -- fails this directly.
func TestAnyFailDoesNotTreatSkipAsPass(t *testing.T) {
	cases := []struct {
		name    string
		results []checkResult
		want    bool
	}{
		{"empty", nil, false},
		{"all pass", []checkResult{pass("a", ""), pass("b", "")}, false},
		{"all skip", []checkResult{skip("a", ""), skip("b", "")}, false},
		{"pass and skip, no fail", []checkResult{pass("a", ""), skip("b", "")}, false},
		{"one fail among pass and skip", []checkResult{pass("a", ""), skip("b", ""), fail("c", "")}, true},
		{"fail first", []checkResult{fail("a", ""), pass("b", "")}, true},
	}
	for _, c := range cases {
		if got := anyFail(c.results); got != c.want {
			t.Errorf("%s: anyFail(%v) = %v, want %v", c.name, c.results, got, c.want)
		}
	}
}

func TestMissingFromPageEmptyPage(t *testing.T) {
	page := session.EventPage{}
	missing := missingFromPage([]uint64{1, 2}, page)
	if len(missing) != 2 {
		t.Fatalf("expected both seqs missing from an empty page, got %v", missing)
	}
}
