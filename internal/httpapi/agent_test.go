package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// fakeAgentTarget is an Actuator and a ScreenSizer both, so /api/input can be
// driven and the resulting event's coordinates checked against a known
// screen size. Configure tapErr/swipeErr/textErr/keyErr to make the
// underlying adb call fail without touching a device.
type fakeAgentTarget struct {
	id string

	tapErr, swipeErr, textErr, keyErr error

	// screenW, screenH are what ScreenSize reports. sizeErr, when set, makes
	// ScreenSize fail instead -- both are exercised because "the size read
	// failed" and "the size read succeeded and said 0x0" must be handled the
	// same way (see input.go's screenSize helper).
	screenW, screenH int
	sizeErr          error

	// screenSizeCalls counts every ScreenSize call this target received.
	screenSizeCalls int
}

func (f *fakeAgentTarget) ID() string        { return f.id }
func (f *fakeAgentTarget) Kind() target.Kind { return target.KindDevice }
func (f *fakeAgentTarget) Label() string     { return f.id }
func (f *fakeAgentTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	return &stubStream{frames: make(chan target.Frame)}, nil
}

func (f *fakeAgentTarget) Tap(ctx context.Context, x, y int) error { return f.tapErr }
func (f *fakeAgentTarget) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error {
	return f.swipeErr
}
func (f *fakeAgentTarget) TypeText(ctx context.Context, s string) error    { return f.textErr }
func (f *fakeAgentTarget) PressKey(ctx context.Context, keycode int) error { return f.keyErr }

// screenSizeCalls counts every ScreenSize call, so a test can assert text and
// key actions never trigger one (opus review item 4).
func (f *fakeAgentTarget) ScreenSize(ctx context.Context) (int, int, error) {
	f.screenSizeCalls++
	// A failing read still reports the configured, non-zero size alongside
	// the error (opus review item 7) rather than zeroing it out: production
	// code that read w, h and ignored err would then publish 1080x2400
	// coordinates for a size read that actually failed, which is exactly the
	// bug agentScreenSize exists to prevent. Zeroing it here would let that
	// mutation survive.
	if f.sizeErr != nil {
		return f.screenW, f.screenH, f.sizeErr
	}
	return f.screenW, f.screenH, nil
}

// fakeAgentTargetNoSizer is an Actuator that does not implement ScreenSizer
// at all -- the "never had a chance to answer" case, distinct from
// fakeAgentTarget reporting an error or a zero size.
type fakeAgentTargetNoSizer struct {
	id     string
	tapErr error
}

func (f *fakeAgentTargetNoSizer) ID() string        { return f.id }
func (f *fakeAgentTargetNoSizer) Kind() target.Kind { return target.KindDevice }
func (f *fakeAgentTargetNoSizer) Label() string     { return f.id }
func (f *fakeAgentTargetNoSizer) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	return &stubStream{frames: make(chan target.Frame)}, nil
}
func (f *fakeAgentTargetNoSizer) Tap(ctx context.Context, x, y int) error { return f.tapErr }
func (f *fakeAgentTargetNoSizer) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error {
	return nil
}
func (f *fakeAgentTargetNoSizer) TypeText(ctx context.Context, s string) error    { return nil }
func (f *fakeAgentTargetNoSizer) PressKey(ctx context.Context, keycode int) error { return nil }

// fakeRotatorTarget is an Actuator and a Rotator both, so /api/input's
// "rotate" and "button" actions can be driven without a device.
// keycodePressed records the last keycode PressKey received, which is what
// the "button" action's test checks against -- named buttons resolve to a
// keycode through target.ButtonKeycode before PressKey ever runs.
type fakeRotatorTarget struct {
	id string

	rotation    int
	rotationErr error
	rotateErr   error
	pressErr    error

	rotateCalls    []int
	rotationCalls  int
	keycodePressed int
	pinches        []pinchCall
	keyboards      []bool
	keyboardErr    error
}

func (f *fakeRotatorTarget) SetHardwareKeyboard(ctx context.Context, on bool) error {
	f.keyboards = append(f.keyboards, on)
	return f.keyboardErr
}

type pinchCall struct {
	x, y  int
	scale float64
	ms    int
}

func (f *fakeRotatorTarget) Pinch(ctx context.Context, x, y int, scale float64, ms int) error {
	f.pinches = append(f.pinches, pinchCall{x, y, scale, ms})
	return nil
}

func (f *fakeRotatorTarget) ID() string        { return f.id }
func (f *fakeRotatorTarget) Kind() target.Kind { return target.KindDevice }
func (f *fakeRotatorTarget) Label() string     { return f.id }
func (f *fakeRotatorTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	return &stubStream{frames: make(chan target.Frame)}, nil
}
func (f *fakeRotatorTarget) Tap(ctx context.Context, x, y int) error { return nil }
func (f *fakeRotatorTarget) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error {
	return nil
}
func (f *fakeRotatorTarget) TypeText(ctx context.Context, s string) error { return nil }
func (f *fakeRotatorTarget) PressKey(ctx context.Context, keycode int) error {
	f.keycodePressed = keycode
	return f.pressErr
}
func (f *fakeRotatorTarget) Rotate(ctx context.Context, to int) error {
	f.rotateCalls = append(f.rotateCalls, to)
	if f.rotateErr != nil {
		return f.rotateErr
	}
	f.rotation = to
	return nil
}
func (f *fakeRotatorTarget) Rotation(ctx context.Context) (int, error) {
	f.rotationCalls++
	return f.rotation, f.rotationErr
}

// fakeInspectorTarget is an Actuator and an Inspector, so the password
// redaction path in handleInput/publishAgentEvent can be driven end to end
// against a controlled view hierarchy -- and a controlled Inspect failure or
// delay -- without a device. calls, when non-nil, records "inspect" and
// "type" in the order the fake's methods were invoked, which is what pins
// that the dump happens before TypeText rather than after.
type fakeInspectorTarget struct {
	id string

	ui         *target.UI
	inspectErr error
	// blockOnCtx makes Inspect block until its context is cancelled and then
	// return ctx.Err(), standing in for a dump that outlives
	// passwordDumpTimeout.
	blockOnCtx bool

	textErr error

	calls *[]string
}

func (f *fakeInspectorTarget) ID() string        { return f.id }
func (f *fakeInspectorTarget) Kind() target.Kind { return target.KindDevice }
func (f *fakeInspectorTarget) Label() string     { return f.id }
func (f *fakeInspectorTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	return &stubStream{frames: make(chan target.Frame)}, nil
}

func (f *fakeInspectorTarget) Tap(ctx context.Context, x, y int) error { return nil }
func (f *fakeInspectorTarget) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error {
	return nil
}
func (f *fakeInspectorTarget) TypeText(ctx context.Context, s string) error {
	if f.calls != nil {
		*f.calls = append(*f.calls, "type")
	}
	return f.textErr
}
func (f *fakeInspectorTarget) PressKey(ctx context.Context, keycode int) error { return nil }

func (f *fakeInspectorTarget) Inspect(ctx context.Context) (*target.UI, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, "inspect")
	}
	if f.blockOnCtx {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	return f.ui, nil
}

// uiWithFocused builds a one-node *target.UI, focused, with the given
// password bit -- enough for redactionForFocusedField to read.
func uiWithFocused(password bool) *target.UI {
	return &target.UI{Root: &target.Node{
		Class:    "android.widget.EditText",
		ID:       "com.android.settings:id/password",
		Focused:  true,
		Password: password,
	}}
}

// uiWithNoFocused builds a *target.UI with a node present but not focused --
// the "dump succeeded, nothing has focus" case.
func uiWithNoFocused() *target.UI {
	return &target.UI{Root: &target.Node{
		Class: "android.widget.EditText",
		ID:    "com.android.settings:id/password",
	}}
}

// uiWithTwoFocusedNodes builds a *target.UI with two independently focused
// nodes -- a host and a leaf, the WebView/Compose shape -- in the given
// order, so a test can drive both document orders against
// redactionForFocusedField. Only one of the two is ever the password field;
// the other is an ordinary focused control, which is what makes this catch a
// check that reads only the first focused node or only the deepest one.
func uiWithTwoFocusedNodes(firstIsPassword bool) *target.UI {
	host := &target.Node{Class: "android.webkit.WebView", ID: "host", Focused: true, Password: firstIsPassword}
	leaf := &target.Node{Class: "android.widget.EditText", ID: "leaf", Focused: true, Password: !firstIsPassword}
	host.Children = []*target.Node{leaf}
	return &target.UI{Root: host}
}

// uiWithTwoFocusedWindows builds a *target.UI with two sibling root windows,
// each carrying its own focused node -- the dialog-over-an-activity shape,
// where ParseUI merges every root's children under one synthetic Root. In
// document order the first sibling is checked at index 0, the second at
// index 1, so passing both orders here exercises the same "any focused node,
// not just the first" requirement as uiWithTwoFocusedNodes from a different
// shape.
func uiWithTwoFocusedWindows(firstIsPassword bool) *target.UI {
	a := &target.Node{Class: "android.widget.EditText", ID: "window-a", Focused: true, Password: firstIsPassword}
	b := &target.Node{Class: "android.widget.EditText", ID: "window-b", Focused: true, Password: !firstIsPassword}
	return &target.UI{Root: &target.Node{
		Class:    "android.widget.FrameLayout",
		Children: []*target.Node{a, b},
	}}
}

func postInput(s *Server, targetID, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/input?target="+targetID, strings.NewReader(body))
	s.handleInput(w, r)
	return w
}

// screenshotTarget is a minimal target.Screenshotter, used to check that
// GET /api/screenshot's success path really answers a PNG and not JSON --
// apischema's routeExemptions names this route "returns a PNG, not JSON"
// rather than giving it a response schema entry, and this is the check that
// claim is not just asserted in a comment.
type screenshotTarget struct {
	id  string
	png []byte
}

func (f *screenshotTarget) ID() string        { return f.id }
func (f *screenshotTarget) Kind() target.Kind { return target.KindDevice }
func (f *screenshotTarget) Label() string     { return f.id }
func (f *screenshotTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	return &stubStream{frames: make(chan target.Frame)}, nil
}
func (f *screenshotTarget) Screenshot(ctx context.Context) ([]byte, error) {
	return f.png, nil
}

// TestScreenshotContentTypeIsNotJSON pins the claim behind apischema's
// routeExemptions entry for /api/screenshot.
func TestScreenshotContentTypeIsNotJSON(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&screenshotTarget{id: "dev1", png: []byte("\x89PNG\r\n\x1a\n")})
	s := &Server{mgr: m}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/screenshot?target=dev1", nil)
	s.handleScreenshot(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png (not JSON)", ct)
	}
}

// TestAgentTapPublishesANormalizedEvent is Task 5's first pin: a tap at a
// known device-pixel coordinate against a known screen size publishes an
// agent event with the coordinate normalized to a [0,1] fraction.
func TestAgentTapPublishesANormalizedEvent(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":540,"y":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Source != "agent" || e.Kind != "tap" {
		t.Fatalf("event = %+v; want source agent, kind tap", e)
	}
	if e.From == nil || e.From.X != 0.5 || e.From.Y != 0.25 {
		t.Fatalf("From = %+v; want [0.5, 0.25]", e.From)
	}
	if e.Err != "" {
		t.Fatalf("Err = %q; want empty on a successful tap", e.Err)
	}
}

// TestAFailedAgentTapStillPublishesWithErr is the other half: a tap that
// fails at adb must still land in the event log, with Err set, or the log
// silently omits a tap that was attempted.
func TestAFailedAgentTapStillPublishesWithErr(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400, tapErr: errors.New("input tap: exit status 1")}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":540,"y":600}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one, published despite the failure", events)
	}
	if events[0].Err == "" {
		t.Fatalf("event = %+v; want Err set for a failed tap", events[0])
	}
	if events[0].From == nil || events[0].From.X != 0.5 {
		t.Fatalf("From = %+v; a failed tap still carries where it was aimed", events[0].From)
	}
}

// TestAgentTextEventIsTruncatedTo64Runes: the event log is not a keylogger's
// archive -- the trace already records the action -- so long text is cut to
// 64 runes, not bytes, so a multi-byte character is never split. The focused
// field here is a real, non-password field, so this also pins the "positively
// not a password" branch: raw text, not the marker.
func TestAgentTextEventIsTruncatedTo64Runes(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithFocused(false)}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	// 80 multi-byte runes (é is 2 bytes in UTF-8), so a byte-based truncation
	// would both cut short of 64 runes and risk splitting one in half.
	long := strings.Repeat("é", 80)
	rec := postInput(s, "dev1", `{"action":"text","text":"`+long+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Kind != "text" {
		t.Fatalf("kind = %q, want text", e.Kind)
	}
	gotRunes := []rune(e.Text)
	if len(gotRunes) != 64 {
		t.Fatalf("len([]rune(Text)) = %d, want 64", len(gotRunes))
	}
	wantRunes := []rune(long)[:64]
	if string(gotRunes) != string(wantRunes) {
		t.Fatalf("Text = %q, want the first 64 runes of the input", e.Text)
	}
	if e.Redacted {
		t.Fatalf("Redacted = true for a positively non-password focused field")
	}
	if e.TextLen != 0 {
		t.Fatalf("TextLen = %d, want 0 (omitempty) when the event is not redacted", e.TextLen)
	}
}

// TestAgentTextIntoAPasswordFieldIsRedacted is the design's central case: a
// dump before typing finds the focused node carries password="true", so the
// event publishes the fixed marker and a rune count, never the characters.
func TestAgentTextIntoAPasswordFieldIsRedacted(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithFocused(true)}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Text != "•••" {
		t.Fatalf("Text = %q, want the fixed marker, never the characters", e.Text)
	}
	if !e.Redacted {
		t.Fatalf("Redacted = false for a password field; want true")
	}
	if e.TextLen != 7 {
		t.Fatalf("TextLen = %d, want 7 (len(\"hunter2\"))", e.TextLen)
	}
}

// TestAgentTextWithNoFocusedNodeIsRedacted: the dump succeeded but nothing on
// screen has focus. Fail closed -- there is no positive answer that the field
// is safe, so redact.
func TestAgentTextWithNoFocusedNodeIsRedacted(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithNoFocused()}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if !events[0].Redacted {
		t.Fatalf("Redacted = false with no focused node; want true")
	}
}

// TestAgentTextWithADumpErrorIsRedacted: Inspect returning an error is
// inconclusive, not "not a password" -- redact.
func TestAgentTextWithADumpErrorIsRedacted(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{id: "dev1", inspectErr: errors.New("uiautomator dump: device offline")}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if !events[0].Redacted {
		t.Fatalf("Redacted = false after a dump error; want true")
	}
}

// TestAgentTextWithNoInspectorIsRedacted: Synthetic and any other target
// without an Inspector cannot be asked, so a text action against one is
// always redacted -- there is no dump to say otherwise.
func TestAgentTextWithNoInspectorIsRedacted(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if !events[0].Redacted {
		t.Fatalf("Redacted = false with no Inspector; want true")
	}
}

// TestAgentTextWithADumpTimeoutIsRedacted: passwordDumpTimeout bounds the
// dump so a hung uiautomator cannot hold the request past agentTimeout. A
// dump that outlives it is inconclusive, same as an outright error -- redact.
func TestAgentTextWithADumpTimeoutIsRedacted(t *testing.T) {
	orig := passwordDumpTimeout
	passwordDumpTimeout = 20 * time.Millisecond
	defer func() { passwordDumpTimeout = orig }()

	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{id: "dev1", blockOnCtx: true}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	start := time.Now()
	rec := postInput(s, "dev1", `{"action":"text","text":"hello"}`)
	// This is the property passwordDumpTimeout exists for: a dump that never
	// returns must not hold the request for anywhere near agentTimeout
	// (10s). Bounding on wall-clock here, not just on the redacted outcome,
	// is what catches a mutant that drops the inner timeout and leaves the
	// dump bounded only by the outer one -- fail-closed still redacts in
	// that case, so the outcome alone does not distinguish them.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("request took %v; want it bounded by passwordDumpTimeout (20ms here), not agentTimeout", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if !events[0].Redacted {
		t.Fatalf("Redacted = false after a dump timeout; want true")
	}
}

// TestAgentTextWithNoSessionNeverDumps: publishing is already gated on a
// session existing (TestAgentInputWithNoSessionPublishesNothing). This pins
// the cost side of that gate for the password check specifically -- a bare
// /api/input with no session open must not cost a second adb call for a dump
// nothing will ever read.
func TestAgentTextWithNoSessionNeverDumps(t *testing.T) {
	m := NewManager(target.Options{})
	var calls []string
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithFocused(true), calls: &calls}
	m.Register(ft)

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, c := range calls {
		if c == "inspect" {
			t.Fatalf("calls = %v; Inspect must not run with no session to publish to", calls)
		}
	}
}

// TestTraceObserverNeverSeesRawPasswordText is the design's explicit
// end-to-end pin: redaction happens at publish time in the server, so every
// consumer fed from PublishEvent -- viewers, the ring, and the manager's
// SetEventObserver hook that feeds loupe-trace.jsonl -- receives only the
// redacted form. This drives that hook directly, the same one
// internal/httpapi/manager.go wires to trace recording, rather than
// inspecting the ring, so a future consumer added the same way is covered
// without a new test.
func TestTraceObserverNeverSeesRawPasswordText(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithFocused(true)}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	var observed []session.Event
	sess.SetEventObserver(func(e session.Event) {
		observed = append(observed, e)
	})

	s := &Server{mgr: m}
	const secret = "hunter2"
	rec := postInput(s, "dev1", `{"action":"text","text":"`+secret+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if len(observed) != 1 {
		t.Fatalf("observed = %+v; want exactly one event reaching the trace hook", observed)
	}
	e := observed[0]
	if strings.Contains(e.Text, secret) {
		t.Fatalf("Text = %q; the trace observer must never see the raw characters", e.Text)
	}
	if !e.Redacted {
		t.Fatalf("Redacted = false on the event the trace observer saw; want true")
	}
}

// TestAgentTextDumpsBeforeTyping pins the ordering the design calls out
// explicitly: typing can move focus (an IME "next" action on the last field
// of a form is the ordinary case), so the dump has to happen before TypeText
// or it can read the wrong field's focus state entirely.
func TestAgentTextDumpsBeforeTyping(t *testing.T) {
	m := NewManager(target.Options{})
	var calls []string
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithFocused(false), calls: &calls}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if len(calls) != 2 || calls[0] != "inspect" || calls[1] != "type" {
		t.Fatalf("calls = %v; want [inspect type]", calls)
	}
}

// TestAgentInputWithNoSessionPublishesNothing is the plan's explicit case:
// /api/input works with no session open (unlike a viewer, which always has
// one), and must not create one just to have somewhere to publish.
func TestAgentInputWithNoSessionPublishesNothing(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}
	m.Register(ft)

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":540,"y":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if _, ok := m.Session("dev1"); ok {
		t.Fatal("a session exists after /api/input; publishing an event must not create one")
	}
}

// TestAgentTapWithUnknownScreenSizeOmitsCoordinates covers the size read
// failing outright: publish the event, but never guess a coordinate.
func TestAgentTapWithUnknownScreenSizeOmitsCoordinates(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", sizeErr: errors.New("read display size: device offline")}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":540,"y":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one, published despite the unknown size", events)
	}
	if events[0].From != nil {
		t.Fatalf("From = %+v; want nil when the screen size is unknown", events[0].From)
	}
}

// TestAgentTapWithZeroScreenSizeOmitsCoordinates covers the size read
// succeeding and reporting 0x0 -- distinct from an error, and the one case
// that would divide by zero into a NaN or Inf if it were not caught before
// the division.
func TestAgentTapWithZeroScreenSizeOmitsCoordinates(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 0, screenH: 0}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":540,"y":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if events[0].From != nil {
		t.Fatalf("From = %+v; want nil for a reported 0x0 screen", events[0].From)
	}
}

// TestAgentTapWithNoScreenSizerOmitsCoordinates covers a target that never
// implements ScreenSizer at all -- Synthetic, today. The event still
// publishes, with no coordinates rather than a guess.
func TestAgentTapWithNoScreenSizerOmitsCoordinates(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTargetNoSizer{id: "dev1"}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":540,"y":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if events[0].From != nil {
		t.Fatalf("From = %+v; want nil for a target with no ScreenSizer", events[0].From)
	}
}

// TestAgentSwipePublishesFromAndTo covers the other coordinate-bearing
// action: both ends of a swipe are normalized against the same screen size.
func TestAgentSwipePublishesFromAndTo(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1000, screenH: 2000}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"swipe","x":500,"y":1800,"x2":500,"y2":200,"ms":180}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Kind != "swipe" {
		t.Fatalf("kind = %q, want swipe", e.Kind)
	}
	if e.From == nil || e.From.X != 0.5 || e.From.Y != 0.9 {
		t.Fatalf("From = %+v; want [0.5, 0.9]", e.From)
	}
	if e.To == nil || e.To.X != 0.5 || e.To.Y != 0.1 {
		t.Fatalf("To = %+v; want [0.5, 0.1]", e.To)
	}
}

// TestAgentKeyPublishesKeycode covers an action with no coordinate at all --
// From/To must both stay nil rather than reading stray zeroed X/Y fields
// into the event.
// TestAgentKeyOfANamedButtonPublishesAsButton pins the coordinator's addition
// to Task 8: a "key" action whose keycode names a device button (4 is back)
// publishes kind:"button" with the name, the same as the "button" action
// itself, not kind:"key" with the raw code.
func TestAgentKeyOfANamedButtonPublishesAsButton(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"key","keycode":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Kind != "button" || e.Button != "back" {
		t.Fatalf("event = %+v; want kind button, button back", e)
	}
	if e.From != nil || e.To != nil {
		t.Fatalf("event = %+v; want no coordinates on a button event", e)
	}
}

// TestAgentKeyOfAnUnnamedCodeStaysAKeyEvent is the other half: a keycode that
// names no device button still publishes kind:"key" with the raw code.
func TestAgentKeyOfAnUnnamedCodeStaysAKeyEvent(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"key","keycode":29}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Kind != "key" || e.Keycode != 29 {
		t.Fatalf("event = %+v; want kind key, keycode 29", e)
	}
	if e.From != nil || e.To != nil {
		t.Fatalf("event = %+v; want no coordinates on a key event", e)
	}
}

// TestAgentTextAndKeyNeverReadTheScreenSize pins opus review item 4: text and
// key actions carry no coordinate, so reading the screen size for them would
// cost an adb connection for nothing. Only tap and swipe may call it.
func TestAgentTextAndKeyNeverReadTheScreenSize(t *testing.T) {
	for _, body := range []string{
		`{"action":"text","text":"hello"}`,
		`{"action":"key","keycode":4}`,
	} {
		m := NewManager(target.Options{})
		ft := &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}
		m.Register(ft)
		sess, err := m.Acquire(t.Context(), "dev1")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		s := &Server{mgr: m}
		rec := postInput(s, "dev1", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", body, rec.Code, rec.Body.String())
		}
		if ft.screenSizeCalls != 0 {
			t.Errorf("%s: ScreenSize called %d times, want 0", body, ft.screenSizeCalls)
		}
		sess.Close()
	}
}

// TestAgentTapOutsideTheScreenSetsErr pins opus review item 5: a tap whose
// coordinate falls outside the reported screen is clamped into [0,1] rather
// than published raw, and Err names it so the log distinguishes a
// deliberate edge tap from one aimed off the glass entirely.
func TestAgentTapOutsideTheScreenSetsErr(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":2000,"y":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.From == nil || e.From.X != 1 {
		t.Fatalf("From = %+v; want X clamped to 1", e.From)
	}
	if !strings.Contains(e.Err, "outside the screen") || !strings.Contains(e.Err, "1080x2400") {
		t.Fatalf("Err = %q; want it to say the tap fell outside the 1080x2400 screen", e.Err)
	}
}

// TestAgentTapOutsideTheScreenKeepsTheActionsOwnErr covers the priority
// opus review item 5 calls for: when the underlying adb call itself failed,
// that error must survive on the event rather than being overwritten by the
// generic "outside the screen" message, which would bury what actually went
// wrong.
func TestAgentTapOutsideTheScreenKeepsTheActionsOwnErr(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{
		id: "dev1", screenW: 1080, screenH: 2400,
		tapErr: errors.New("input tap: exit status 1"),
	}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":2000,"y":600}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if events[0].Err != "input tap: exit status 1" {
		t.Fatalf("Err = %q; want the adb failure preserved over the outside-screen note", events[0].Err)
	}
}

// TestAgentSwipePastTheEdgeIsClampedWithNoErr is the other half of item 5: a
// swipe's far end landing past the screen edge is ordinary -- a fling or a
// scroll-to-top gesture routinely overshoots -- so it is clamped silently,
// with no Err.
func TestAgentSwipePastTheEdgeIsClampedWithNoErr(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1000, screenH: 2000}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"swipe","x":500,"y":1800,"x2":500,"y2":-200,"ms":180}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.To == nil || e.To.Y != 0 {
		t.Fatalf("To = %+v; want Y clamped to 0", e.To)
	}
	if e.Err != "" {
		t.Fatalf("Err = %q; want empty, a swipe overshooting the edge is ordinary", e.Err)
	}
}

// TestAgentSwipeMsReflectsWhatRan pins opus review item 6: the event carries
// the duration the swipe actually ran with, not a raw echo of the request --
// an explicit Ms passes through, and an omitted one publishes the 300ms
// default handleInput resolved before calling Swipe.
func TestAgentSwipeMsReflectsWhatRan(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"explicit", `{"action":"swipe","x":500,"y":1800,"x2":500,"y2":200,"ms":180}`, 180},
		{"omitted", `{"action":"swipe","x":500,"y":1800,"x2":500,"y2":200}`, 300},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewManager(target.Options{})
			ft := &fakeAgentTarget{id: "dev1", screenW: 1000, screenH: 2000}
			m.Register(ft)
			sess, err := m.Acquire(t.Context(), "dev1")
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			defer sess.Close()

			s := &Server{mgr: m}
			rec := postInput(s, "dev1", c.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}

			events := sess.EventsAfter(0)
			if len(events) != 1 {
				t.Fatalf("events = %+v; want exactly one", events)
			}
			if events[0].Ms != c.want {
				t.Fatalf("Ms = %d, want %d", events[0].Ms, c.want)
			}
		})
	}
}

// TestAgentTextActionWithEmptyTextPublishesNothing is the nit from the opus
// review: TypeText itself no-ops on empty input, and an event with no text
// and no error would be noise on the log.
func TestAgentTextActionWithEmptyTextPublishesNothing(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if events := sess.EventsAfter(0); len(events) != 0 {
		t.Fatalf("events = %+v; want none for an empty text action", events)
	}
}

// TestAgentEmptyTextNeverDumps: an empty text action publishes no event (see
// above), so a dump made ahead of it would be spent for nothing -- an adb
// call with no event, no viewer and no trace record to show for it.
func TestAgentEmptyTextNeverDumps(t *testing.T) {
	m := NewManager(target.Options{})
	var calls []string
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithFocused(true), calls: &calls}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, c := range calls {
		if c == "inspect" {
			t.Fatalf("calls = %v; empty text must not trigger a dump", calls)
		}
	}
}

// TestAgentTextTextLenCountsRunesNotBytes: "pässwörd" is 8 runes and 10
// bytes (ä and ö are each 2 bytes in UTF-8). TextLen must read 8 -- a
// byte-count mutation is exactly what this catches, since a byte count would
// read 10 and pass unnoticed against an ASCII-only fixture.
func TestAgentTextTextLenCountsRunesNotBytes(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithFocused(true)}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"pässwörd"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if events[0].TextLen != 8 {
		t.Fatalf("TextLen = %d, want 8 (rune count of \"pässwörd\", not its 10 bytes)", events[0].TextLen)
	}
}

// TestAgentTextRedactionNilFallsBackToRedactedWithARuneCount drives
// publishAgentEvent directly with a nil *textRedaction -- the shape
// handleInput produces whenever no session exists to publish to, which
// should mean no call reaches publishAgentEvent at all, but the fallback
// inside it is exercised here as its own unit so a future caller that forgets
// to compute a redaction still fails closed rather than leaking raw text.
func TestAgentTextRedactionNilFallsBackToRedactedWithARuneCount(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithFocused(true)}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	s.publishAgentEvent(t.Context(), ft, InputRequest{Action: "text", Text: "hunter2"}, inputCaller{}, nil, nil)

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Text != passwordMarker {
		t.Fatalf("Text = %q, want the fixed marker with redaction == nil", e.Text)
	}
	if !e.Redacted {
		t.Fatalf("Redacted = false with redaction == nil; want true (fail closed)")
	}
	if e.TextLen != 7 {
		t.Fatalf("TextLen = %d, want 7 even with redaction == nil", e.TextLen)
	}
}

// TestAgentTextFailedTypeIntoAPasswordFieldNeverLeaksTheErr is the low-severity
// review finding: ADB.ExecOut already folds its command's args into the
// errors it returns, so a failed `input text <secret>` could echo the secret
// straight back through the event's Err. The fake's textErr embeds the
// password on purpose, standing in for that shape, and the published event's
// Err must not contain it -- while Text, Redacted and TextLen still report
// the redaction normally.
func TestAgentTextFailedTypeIntoAPasswordFieldNeverLeaksTheErr(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{
		id:      "dev1",
		ui:      uiWithFocused(true),
		textErr: errors.New(`input text "hunter2": exit status 1`),
	}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hunter2"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (the action itself failed)", rec.Code, http.StatusBadGateway)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("HTTP body = %s; must not contain the typed password", rec.Body.String())
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Text != passwordMarker || !e.Redacted || e.TextLen != 7 {
		t.Fatalf("redaction fields = %+v; want the marker, Redacted true and TextLen 7 even when the action failed", e)
	}
	if strings.Contains(e.Err, "hunter2") {
		t.Fatalf("Err = %q; must not contain the typed password", e.Err)
	}
	if e.Err == "" {
		t.Fatalf("Err = %q; want a non-empty message saying the action failed", e.Err)
	}
}

// TestAgentTextAnyFocusedPasswordNodeRedactsHostThenLeaf and its sibling
// below are the HIGH-severity fix: checking only the first focused node in
// document order let a non-password host above a password leaf (a WebView or
// Compose host is the plausible real case) answer for the field, publishing
// the password in plain text. Both orders are driven so that restricting the
// match to the first node, or to the deepest node, is caught either way.
func TestAgentTextAnyFocusedPasswordNodeRedactsHostThenLeaf(t *testing.T) {
	m := NewManager(target.Options{})
	// firstIsPassword=false: the host (checked first, and the shallower node)
	// is not the password field; the leaf (deeper) is. A check that stops at
	// the first focused node, or that only ever looks at the shallowest one,
	// would read "not a password" here and leak the text.
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithTwoFocusedNodes(false)}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if !events[0].Redacted {
		t.Fatalf("Redacted = false with a password leaf under a non-password focused host; want true")
	}
}

func TestAgentTextAnyFocusedPasswordNodeRedactsLeafThenHost(t *testing.T) {
	m := NewManager(target.Options{})
	// firstIsPassword=true: the host (checked first) IS the password field
	// and the leaf is not. This is the order a naive "first focused node"
	// check would get right by accident, so it is here to make sure the fix
	// is "check every focused node," not "check the last one instead of the
	// first."
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithTwoFocusedNodes(true)}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if !events[0].Redacted {
		t.Fatalf("Redacted = false with a password host; want true")
	}
}

// TestAgentTextAnyFocusedPasswordWindowRedacts and its "Reversed" sibling are
// the sibling-window shape of the same fix: a dialog over an activity leaves
// a focused node behind it in another window, and uiautomator reports one
// per window. Both document orders are covered.
func TestAgentTextAnyFocusedPasswordWindowRedacts(t *testing.T) {
	m := NewManager(target.Options{})
	// The non-password window is first, the password window second.
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithTwoFocusedWindows(false)}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if !events[0].Redacted {
		t.Fatalf("Redacted = false with a password field focused in a second window; want true")
	}
}

func TestAgentTextAnyFocusedPasswordWindowRedactsReversed(t *testing.T) {
	m := NewManager(target.Options{})
	// The password window is first, the non-password window second.
	ft := &fakeInspectorTarget{id: "dev1", ui: uiWithTwoFocusedWindows(true)}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"text","text":"hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	if !events[0].Redacted {
		t.Fatalf("Redacted = false with a password field focused in the first window; want true")
	}
}

// TestAgentRotateWithExplicitToCallsRotate pins the plan's exact example: a
// fake Rotator receives 2 for {"action":"rotate","to":2}, and the published
// event carries that rotation.
func TestAgentRotateWithExplicitToCallsRotate(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1"}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"rotate","to":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ft.rotateCalls) != 1 || ft.rotateCalls[0] != 2 {
		t.Fatalf("rotateCalls = %v; want [2]", ft.rotateCalls)
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Kind != "rotate" || e.Rotation == nil || *e.Rotation != 2 {
		t.Fatalf("event = %+v; want kind rotate, rotation 2", e)
	}
}

// TestAgentRotateWithNoToReadsCurrentAndIncrements pins the plan's other
// example: an empty "to" reads the current rotation and advances it by one,
// mod 4, rather than requiring the caller to track state of its own.
func TestAgentRotateWithNoToReadsCurrentAndIncrements(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1", rotation: 1}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"rotate"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ft.rotateCalls) != 1 || ft.rotateCalls[0] != 2 {
		t.Fatalf("rotateCalls = %v; want [2] ((1+1) mod 4)", ft.rotateCalls)
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Kind != "rotate" || e.Rotation == nil || *e.Rotation != 2 {
		t.Fatalf("event = %+v; want kind rotate, rotation 2 -- the resolved value, not the caller-omitted nil", e)
	}
}

// TestAgentRotateWraps3To0 pins the mod-4 wraparound at the top of the range.
func TestAgentRotateWraps3To0(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1", rotation: 3}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"rotate"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ft.rotateCalls) != 1 || ft.rotateCalls[0] != 0 {
		t.Fatalf("rotateCalls = %v; want [0] ((3+1) mod 4)", ft.rotateCalls)
	}
}

// TestAgentRotateRejectsAnOutOfRangeToBeforeCallingRotate covers the review
// finding against 2b8546c: an explicit "to" outside 0..3 must 400 without
// ever reaching the Rotator or the event log -- not a 502 from a device (or a
// non-ADB Rotator) that happened to accept the value and not an event
// carrying a rotation nothing sane maps to.
func TestAgentRotateRejectsAnOutOfRangeToBeforeCallingRotate(t *testing.T) {
	for _, to := range []int{-1, 4} {
		m := NewManager(target.Options{})
		ft := &fakeRotatorTarget{id: "dev1"}
		m.Register(ft)
		sess, err := m.Acquire(t.Context(), "dev1")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		s := &Server{mgr: m}
		rec := postInput(s, "dev1", fmt.Sprintf(`{"action":"rotate","to":%d}`, to))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("to=%d: status = %d, want 400: %s", to, rec.Code, rec.Body.String())
		}
		if len(ft.rotateCalls) != 0 {
			t.Fatalf("to=%d: rotateCalls = %v; want none, Rotate must not run on an invalid value", to, ft.rotateCalls)
		}
		if len(sess.EventsAfter(0)) != 0 {
			t.Fatalf("to=%d: an out-of-range rotation must not publish an event", to)
		}
		sess.Close()
	}
}

// TestAgentRotateToZeroReachesRotateAsZero guards against reading req.To's
// pointee with the Go zero value's own falsy-looking shape: {"to":0} must
// decode to a non-nil pointer at 0 and take the explicit-to branch, not the
// "no to given, read current and increment" branch a mutation that treated 0
// as absent would take instead.
func TestAgentRotateToZeroReachesRotateAsZero(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1", rotation: 2}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"rotate","to":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ft.rotateCalls) != 1 || ft.rotateCalls[0] != 0 {
		t.Fatalf("rotateCalls = %v; want [0]", ft.rotateCalls)
	}
	if ft.rotationCalls != 0 {
		t.Fatalf("rotationCalls = %d; want 0, an explicit to=0 must never read the current rotation", ft.rotationCalls)
	}
}

// TestAgentRotateFailurePropagatesAndPublishesErr covers a Rotate call that
// reaches the device and fails there: 502, and the published event carries
// the failure rather than being silently dropped or published as a success.
// Uses the no-"to" path so it also pins that err is checked after Rotate
// runs, not left over from the (successful) Rotation read before it.
func TestAgentRotateFailurePropagatesAndPublishesErr(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1", rotation: 1, rotateErr: errors.New("rotate boom")}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"rotate"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if len(ft.rotateCalls) != 1 || ft.rotateCalls[0] != 2 {
		t.Fatalf("rotateCalls = %v; want [2], Rotate must still be attempted with the resolved value", ft.rotateCalls)
	}
	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one, a failed action still publishes", events)
	}
	if events[0].Err == "" {
		t.Fatalf("event = %+v; want a non-empty Err carrying the Rotate failure", events[0])
	}
}

// TestAgentRotationReadFailurePropagatesAndPublishesErr covers the other half:
// when "to" is omitted, a failed Rotation read must itself 502 and publish,
// and must stop the request before Rotate ever runs against a rotation
// nothing actually resolved.
func TestAgentRotationReadFailurePropagatesAndPublishesErr(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1", rotationErr: errors.New("rotation read boom")}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"rotate"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if len(ft.rotateCalls) != 0 {
		t.Fatalf("rotateCalls = %v; want none, Rotate must not run when the current rotation could not be read", ft.rotateCalls)
	}
	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one, a failed action still publishes", events)
	}
	if events[0].Err == "" {
		t.Fatalf("event = %+v; want a non-empty Err carrying the Rotation read failure", events[0])
	}
}

// TestAgentButtonPressesTheNamedKeycode pins the "button" action: a named
// button resolves through target.ButtonKeycode to the keycode PressKey
// receives, and the published event carries the name, not the code.
func TestAgentButtonPressesTheNamedKeycode(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1"}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"button","button":"back"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ft.keycodePressed != 4 {
		t.Fatalf("keycodePressed = %d; want 4 (back)", ft.keycodePressed)
	}

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("events = %+v; want exactly one", events)
	}
	e := events[0]
	if e.Kind != "button" || e.Button != "back" {
		t.Fatalf("event = %+v; want kind button, button back", e)
	}
}

// TestAgentButtonUnknownNameIs400 covers appSwitcher by name: it is iOS's
// button, not Android's, so asking an Android target for it is a 400 naming
// the buttons that target actually has, not a silent guess or a 501.
func TestAgentButtonUnknownNameIs400(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1"}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"button","button":"appSwitcher"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(sess.EventsAfter(0)) != 0 {
		t.Fatalf("an unknown button must not publish or press anything")
	}
	if ft.keycodePressed != 0 {
		t.Fatalf("keycodePressed = %d; want 0, an unknown button must never reach PressKey", ft.keycodePressed)
	}
	for _, name := range []string{"back", "home", "lock", "recents"} {
		if !strings.Contains(rec.Body.String(), name) {
			t.Fatalf("error body %q does not name button %q", rec.Body.String(), name)
		}
	}
}

// TestSyntheticRotateAndButtonAnswer501: Synthetic implements neither
// Actuator nor Rotator, so both new actions answer 501, the same as tap does
// today, rather than a panic or a silently ignored call.
func TestSyntheticRotateAndButtonAnswer501(t *testing.T) {
	for _, body := range []string{
		`{"action":"rotate","to":1}`,
		`{"action":"button","button":"home"}`,
	} {
		m := NewManager(target.Options{})
		m.Register(&target.Synthetic{})
		s := &Server{mgr: m}
		rec := postInput(s, "synthetic", body)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: status = %d, want 501: %s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestUnknownActionErrorListsAllSixActions pins the updated error text.
func TestUnknownActionErrorListsAllSixActions(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeAgentTarget{id: "dev1"}
	m.Register(ft)
	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"nonsense"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	for _, action := range []string{"tap", "swipe", "text", "key", "rotate", "button"} {
		if !strings.Contains(rec.Body.String(), action) {
			t.Fatalf("error body %q does not name action %q", rec.Body.String(), action)
		}
	}
}

// TestHandleUIInteractiveWithNothingInterestingReturnsAnEmptyArrayNotNull
// pins the null-array fix from the apischema review: an interesting-free
// tree (a blank activity, one dull root node with no children) makes
// target.Prune return nil, and UIResponse.Nodes carries no omitempty --
// docs/api.schema.json promises an array. Encoding the real handler output
// and decoding it back is what catches a regression that reintroduces the
// bare `resp.Nodes = target.Prune(ui.Root)` assignment: a caller that
// unmarshals `"nodes":null` into a `[]*target.Node` gets nil back, and this
// test only passes when the slice comes back non-nil.
func TestHandleUIInteractiveWithNothingInterestingReturnsAnEmptyArrayNotNull(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeInspectorTarget{id: "dev1", ui: &target.UI{
		Root: &target.Node{Class: "android.widget.FrameLayout"},
	}}
	m.Register(ft)
	s := &Server{mgr: m}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/ui?target=dev1&interactive=1", nil)
	s.handleUI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"nodes":[]`) {
		t.Fatalf("body = %s, want a literal \"nodes\":[] rather than null", body)
	}
	var resp UIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.Nodes == nil {
		t.Fatalf("Nodes decoded as nil despite the wire promising a non-null array")
	}
	if len(resp.Nodes) != 0 {
		t.Fatalf("Nodes = %v, want empty", resp.Nodes)
	}
}

// TestInputMissingRequiredFieldIs400 drives every (action, missing field)
// pair InputRequiredFields names -- one full request per action with
// exactly one required field deleted -- and checks that handleInput 400s
// and names the missing field, rather than falling through to an adb call
// with a Go zero value the caller never actually sent. This is the review's
// round-4 item 2: the schema decision was to make the handler enforce what
// InputRequiredFields (and the generated oneOf) requires, not to relax the
// schema to match a looser handler.
func TestInputMissingRequiredFieldIs400(t *testing.T) {
	// full carries every field a caller might send, so deleting one key at a
	// time still leaves a well-formed body for every other required field of
	// that action.
	full := map[string]any{
		"x": 1, "y": 2, "x2": 3, "y2": 4,
		"text": "hi", "keycode": 66, "button": "back",
	}
	for action, required := range InputRequiredFields {
		for _, missing := range required {
			t.Run(action+"/"+missing, func(t *testing.T) {
				body := map[string]any{"action": action}
				for k, v := range full {
					if k != missing {
						body[k] = v
					}
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatalf("Marshal: %v", err)
				}

				m := NewManager(target.Options{})
				ft := &fakeRotatorTarget{id: "dev1"}
				m.Register(ft)
				sess, err := m.Acquire(t.Context(), "dev1")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				defer sess.Close()

				s := &Server{mgr: m}
				rec := postInput(s, "dev1", string(raw))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
				}
				var resp ErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("Unmarshal error body: %v", err)
				}
				if !strings.Contains(resp.Error, missing) {
					t.Fatalf("error %q does not name the missing field %q", resp.Error, missing)
				}
				if len(sess.EventsAfter(0)) != 0 {
					t.Fatalf("a rejected request must not publish an event")
				}
			})
		}
	}
}

// TestInputZeroIsAcceptedAsACoordinate is the other half of the presence
// check above: x=0, y=0, x2=0, y2=0 and keycode=0 are all values a caller
// can legitimately mean (the top-left corner, hardware key 0), and
// validateInputRequest must not reject them the way it rejects a field that
// was never sent. It checks presence in the raw decoded JSON, not the Go
// zero value, specifically so this passes.
func TestInputZeroIsAcceptedAsACoordinate(t *testing.T) {
	cases := []string{
		`{"action":"tap","x":0,"y":0}`,
		`{"action":"swipe","x":0,"y":0,"x2":0,"y2":0}`,
		`{"action":"key","keycode":0}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			m := NewManager(target.Options{})
			ft := &fakeRotatorTarget{id: "dev1"}
			m.Register(ft)
			sess, err := m.Acquire(t.Context(), "dev1")
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			defer sess.Close()

			s := &Server{mgr: m}
			rec := postInput(s, "dev1", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (0 is a legitimate value, not a missing field): %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestInputRequiredFieldsArePinnedPerAction is round-5 item 1:
// TestInputMissingRequiredFieldIs400 above iterates InputRequiredFields
// itself, so an entry dropped from that map (e.g. removing "y2" from
// swipe's list) simply stops being exercised rather than failing --  the
// loop and the thing it is checking share one source of truth. This test
// hardcodes the expected required-field set per action independently, with
// a literal well-formed body per action and one field deleted at a time, so
// a change to InputRequiredFields itself is what gets caught.
func TestInputRequiredFieldsArePinnedPerAction(t *testing.T) {
	cases := []struct {
		action  string
		missing string
		full    map[string]any
	}{
		{"tap", "x", map[string]any{"x": 1, "y": 2}},
		{"tap", "y", map[string]any{"x": 1, "y": 2}},
		{"swipe", "x", map[string]any{"x": 1, "y": 2, "x2": 3, "y2": 4}},
		{"swipe", "y", map[string]any{"x": 1, "y": 2, "x2": 3, "y2": 4}},
		{"swipe", "x2", map[string]any{"x": 1, "y": 2, "x2": 3, "y2": 4}},
		{"swipe", "y2", map[string]any{"x": 1, "y": 2, "x2": 3, "y2": 4}},
		{"text", "text", map[string]any{"text": "hi"}},
		{"key", "keycode", map[string]any{"keycode": 66}},
		{"button", "button", map[string]any{"button": "back"}},
	}
	for _, tc := range cases {
		t.Run(tc.action+"/"+tc.missing, func(t *testing.T) {
			body := map[string]any{"action": tc.action}
			for k, v := range tc.full {
				if k != tc.missing {
					body[k] = v
				}
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			m := NewManager(target.Options{})
			ft := &fakeRotatorTarget{id: "dev1"}
			m.Register(ft)
			sess, err := m.Acquire(t.Context(), "dev1")
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			defer sess.Close()

			s := &Server{mgr: m}
			rec := postInput(s, "dev1", string(raw))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("Unmarshal error body: %v", err)
			}
			if !strings.Contains(resp.Error, tc.missing) {
				t.Fatalf("error %q does not name the missing field %q", resp.Error, tc.missing)
			}
		})
	}
}

// TestInputExplicitNullCountsAsMissing is round-5 item 2: validateInputRequest
// treats a required field sent as JSON null the same as one never sent at
// all -- it names the field the same way -- and this is checked without
// relying on a Go compile error, since InputRequest's X is a plain int and
// json:"x":null unmarshals into it as zero, indistinguishable at the struct
// level from an omitted or explicit-zero x. Only the raw-body presence check
// in validateInputRequest can catch this.
func TestInputExplicitNullCountsAsMissing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"tap x:null", `{"action":"tap","x":null,"y":5}`},
		{"swipe y2:null", `{"action":"swipe","x":1,"y":2,"x2":3,"y2":null}`},
		{"key keycode:null", `{"action":"key","keycode":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(target.Options{})
			ft := &fakeRotatorTarget{id: "dev1"}
			m.Register(ft)
			s := &Server{mgr: m}
			rec := postInput(s, "dev1", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (null must count as missing): %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestInputBodyOver64KBIs400 pins the 64KB request body cap handleInput
// applies via http.MaxBytesReader before it even attempts to decode JSON
// (round-5 item 3). A body one byte past the limit must 400, not hang, panic
// or 500.
func TestInputBodyOver64KBIs400(t *testing.T) {
	// A well-formed JSON object padded past 64KB with whitespace before the
	// closing brace -- MaxBytesReader cuts the read off, so the body handed
	// to json.Unmarshal is truncated and invalid regardless of what padding
	// looks like; what matters is total byte length.
	pad := strings.Repeat(" ", 64<<10)
	body := `{"action":"tap","x":1,"y":2,"pad":"` + pad + `"}`
	if len(body) <= 64<<10 {
		t.Fatalf("test body is %d bytes, want > %d", len(body), 64<<10)
	}

	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1"}
	m.Register(ft)
	s := &Server{mgr: m}
	rec := postInput(s, "dev1", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a body over the 64KB cap: %s", rec.Code, rec.Body.String())
	}
}

// TestInputBodyAtExactly64KBIsNotRejectedForSize is
// TestInputBodyOver64KBIs400's control: a body at or under the cap must not
// be rejected merely for its size (it can still fail to parse, but not
// because MaxBytesReader cut it off).
func TestInputBodyAtExactly64KBIsNotRejectedForSize(t *testing.T) {
	prefix := `{"action":"tap","x":1,"y":2,"pad":"`
	suffix := `"}`
	padLen := 64<<10 - len(prefix) - len(suffix)
	body := prefix + strings.Repeat(" ", padLen) + suffix
	if len(body) != 64<<10 {
		t.Fatalf("test body is %d bytes, want exactly %d", len(body), 64<<10)
	}

	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1"}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	s := &Server{mgr: m}
	rec := postInput(s, "dev1", body)
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "http: request body too large") {
		t.Fatalf("a body at exactly the 64KB cap must not be rejected for size: %s", rec.Body.String())
	}
}

// TestUnrecognizedActionFlingIs400 is round-5 item 5: it names "fling"
// specifically, the exact action the review used as its mutation example for
// "an if req.Action == \"fling\" added before the switch" -- a special case
// spliced in ahead of handleInput's dispatch switch that handled "fling" on
// its own would not be caught by TestActionOutsideTheListIs400, which never
// sends that literal string.
func TestUnrecognizedActionFlingIs400(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1"}
	m.Register(ft)
	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"fling","x":1,"y":2}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown action") {
		t.Fatalf("error body %q does not say unknown action", rec.Body.String())
	}
}

// TestEveryInputActionDispatches drives every httpapi.InputActions entry
// through handleInput with a well-formed request for that action, and
// requires the response to be something other than the "unknown action"
// 400. It exists so an action added to handleInput's switch and forgotten in
// InputActions (which apischema reads to build the oneOf schema) fails a
// test instead of silently shipping a schema a generated client cannot see,
// per round-4 item 3. TestActionOutsideTheListIs400 is the other direction.
func TestEveryInputActionDispatches(t *testing.T) {
	bodies := map[string]string{
		"tap":              `{"action":"tap","x":1,"y":2}`,
		"swipe":            `{"action":"swipe","x":1,"y":2,"x2":3,"y2":4}`,
		"pinch":            `{"action":"pinch","x":1,"y":2,"scale":2}`,
		"text":             `{"action":"text","text":"hi"}`,
		"key":              `{"action":"key","keycode":66}`,
		"button":           `{"action":"button","button":"back"}`,
		"rotate":           `{"action":"rotate"}`,
		"reclaim":          `{"action":"reclaim"}`,
		"hardwareKeyboard": `{"action":"hardwareKeyboard","on":true}`,
		"launch":           `{"action":"launch","package":"com.example.app"}`,
		"openURL":          `{"action":"openURL","url":"https://example.com/"}`,
		"move":             `{"action":"move","x":1,"y":2}`,
		"wait":             `{"action":"wait","ms":1}`,
	}
	if len(bodies) != len(InputActions) {
		t.Fatalf("this test covers %d actions, InputActions names %d: %v", len(bodies), len(InputActions), InputActions)
	}
	for _, action := range InputActions {
		body, ok := bodies[action]
		if !ok {
			t.Fatalf("InputActions names %q, which this test has no request body for", action)
		}
		t.Run(action, func(t *testing.T) {
			m := NewManager(target.Options{})
			ft := &fakeRotatorTarget{id: "dev1"}
			m.Register(ft)
			sess, err := m.Acquire(t.Context(), "dev1")
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			defer sess.Close()

			s := &Server{mgr: m}
			rec := postInput(s, "dev1", body)
			if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "unknown action") {
				t.Fatalf("action %q fell through to the unknown-action branch: %s", action, rec.Body.String())
			}
		})
	}
}

func TestPinchReachesAPincherWithTheDefaultDuration(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1"}
	m.Register(ft)
	s := &Server{mgr: m}

	if rec := postInput(s, "dev1", `{"action":"pinch","x":600,"y":1300,"scale":0.5}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	want := []pinchCall{{600, 1300, 0.5, defaultSwipeMs}}
	if !reflect.DeepEqual(ft.pinches, want) {
		t.Errorf("pinches = %+v, want %+v", ft.pinches, want)
	}

	for _, body := range []string{
		`{"action":"pinch","x":1,"y":2,"scale":0}`,
		`{"action":"pinch","x":1,"y":2,"scale":-2}`,
		`{"action":"pinch","x":1,"y":2}`,
	} {
		if rec := postInput(s, "dev1", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
	if len(ft.pinches) != 1 {
		t.Errorf("a rejected pinch reached the target: %+v", ft.pinches)
	}
}

func TestPinchOnATargetWithOneFingerIs501(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&fakeInspectorTarget{id: "dev1"}) // an Actuator, one finger
	s := &Server{mgr: m}
	if rec := postInput(s, "dev1", `{"action":"pinch","x":1,"y":2,"scale":2}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}
}

// TestHardwareKeyboardNeedsADirection pins that the switch goes the way it
// was asked, false included, that a request naming no direction is refused
// rather than read as off, and that a switch publishes no event: it moves
// nothing on the screen.
func TestHardwareKeyboardNeedsADirection(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1"}
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()
	s := &Server{mgr: m}

	for _, body := range []string{`{"action":"hardwareKeyboard","on":true}`, `{"action":"hardwareKeyboard","on":false}`} {
		if rec := postInput(s, "dev1", body); rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", body, rec.Code, rec.Body.String())
		}
	}
	if want := []bool{true, false}; !reflect.DeepEqual(ft.keyboards, want) {
		t.Errorf("keyboards = %v, want %v", ft.keyboards, want)
	}
	if rec := postInput(s, "dev1", `{"action":"hardwareKeyboard"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no direction: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if evs := sess.EventsAfter(0); len(evs) != 0 {
		t.Errorf("a keyboard switch published %d events", len(evs))
	}
}

// TestHardwareKeyboardWithNoSessionIs409 covers an Android target asked for
// its keyboard with no session open: the keyboard lives in the scrcpy server,
// so there is nothing to attach it to yet, and that is a conflict with the
// target's state rather than a failed device.
func TestHardwareKeyboardWithNoSessionIs409(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&fakeRotatorTarget{id: "dev1", keyboardErr: target.ErrNoSession})
	s := &Server{mgr: m}
	if rec := postInput(s, "dev1", `{"action":"hardwareKeyboard","on":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestActionOutsideTheListIs400 is TestEveryInputActionDispatches's
// complement: a value that is not in InputActions must still be rejected,
// with the "unknown action" message that names the real list.
func TestActionOutsideTheListIs400(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1"}
	m.Register(ft)
	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"uninstall"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown action") {
		t.Fatalf("error body %q does not say unknown action", rec.Body.String())
	}
}

// TestHandleInputSwitchCasesMatchInputActions parses agent.go's own source
// and walks handleInput's action switch (both the outer switch and the
// Actuator-group's inner switch, since "tap"/"swipe"/"text"/"key"/"button"
// share one Actuator branch and dispatch on a nested switch) to collect
// every case string literal it finds. This exists because a case added to
// the switch without a matching entry in InputActions would dispatch
// successfully -- TestEveryInputActionDispatches only walks InputActions, so
// it cannot see a case the list itself does not name, and the schema
// generator reads InputActions too, so an orphaned case would also go
// undocumented. Parsing the source is the only way to see the switch's
// actual case set from a test.
func TestHandleInputSwitchCasesMatchInputActions(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "agent.go", nil, 0)
	if err != nil {
		t.Fatalf("parse agent.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		if d, ok := n.(*ast.FuncDecl); ok && d.Name.Name == "handleInput" {
			fn = d
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatal("handleInput not found in agent.go")
	}

	cases := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		// Only the switches keyed on req.Action -- handleInput has others
		// (e.g. over req.To) that are not this one.
		tag, ok := sw.Tag.(*ast.SelectorExpr)
		if !ok || tag.Sel.Name != "Action" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cc.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				cases[v] = true
			}
		}
		return true
	})

	if len(cases) == 0 {
		t.Fatal("found no case string literals in handleInput's Action switch; the AST walk is broken")
	}

	got := make([]string, 0, len(cases))
	for c := range cases {
		got = append(got, c)
	}
	sort.Strings(got)
	want := append([]string(nil), InputActions...)
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("handleInput's switch cases %v do not match InputActions %v", got, want)
	}
}

// TestInputRequestFieldsAreOmittedWhenZero pins InputRequest's own
// omitempty tags directly against json.Marshal, independent of the
// generated schema. apischema's oneOf variants build "required" from
// InputRequiredFields rather than from omitempty (each field's own type
// schema, {"type":"integer"} for X, does not change either way), so
// TestSchemaFileMatchesGenerated cannot see omitempty being dropped from a
// field -- regenerating with X or Button missing omitempty reproduces a
// byte-identical docs/api.schema.json. What omitempty still controls is
// publishAgentEvent's own trace/event JSON, which marshals the very
// InputRequest a handler decoded: dropping it would make every "rotate"
// event's log line carry a spurious "x":0 and "button":"" it never used.
func TestInputRequestFieldsAreOmittedWhenZero(t *testing.T) {
	req := InputRequest{Action: "rotate"}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	for _, field := range []string{"x", "y", "x2", "y2", "ms", "text", "keycode", "to", "button"} {
		if _, present := raw[field]; present {
			t.Errorf("marshaled %s; %q must be omitted from a rotate request that never set it", b, field)
		}
	}
	if _, present := raw["action"]; !present {
		t.Fatalf("marshaled %s; \"action\" must always be present (no omitempty)", b)
	}
}

// TestAgentHubHoldIsAConflictWithACode pins what the viewer's reclaim prompt
// keys on: a press Device Hub is holding answers 409 with code "hubHeld",
// not the 502 every other input failure gets.
func TestAgentHubHoldIsAConflictWithACode(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &fakeRotatorTarget{id: "dev1", pressErr: fmt.Errorf("%w; reclaim it", target.ErrHubHoldsInput)}
	m.Register(ft)
	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"button","button":"back"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != ErrCodeHubHeld {
		t.Fatalf("code = %q; want %q", body.Code, ErrCodeHubHeld)
	}
}

// postPhoneInputErr runs a tap that fails with err against a fake target and
// returns the 503 body.
func postPhoneInputErr(t *testing.T, err error) ErrorResponse {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400, tapErr: err})
	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":10,"y":20}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	if e := json.Unmarshal(rec.Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	return body
}

// A phone that CoreDevice refused twice, with a wake between, answers 503
// "phoneUnreachable" with CoreDevice's text and devicectl's outcome. Nothing
// was sent, so the caller may retry.
func TestAgentUnreachablePhoneIsA503WithACode(t *testing.T) {
	cause := "CoreDeviceError 1001: not supported by this device (devicectl wake: ok)"
	body := postPhoneInputErr(t, fmt.Errorf("%w: %s", target.ErrPhoneUnreachable, cause))
	if body.Code != ErrCodePhoneUnreachable {
		t.Fatalf("code = %q; want %q", body.Code, ErrCodePhoneUnreachable)
	}
	if !strings.Contains(body.Error, cause) {
		t.Errorf("error %q lost the cause %q", body.Error, cause)
	}
}

// A send that went out with no confirmation, whether simcap said undelivered
// or said nothing, answers 503 "phoneDeliveryUnknown", a different code from
// phoneUnreachable because a caller retries one and must not retry the other.
func TestAgentPhoneDeliveryUnknownIsA503WithItsOwnCode(t *testing.T) {
	cause := "no reply in 15s"
	body := postPhoneInputErr(t, fmt.Errorf("%w: %s", target.ErrPhoneDeliveryUnknown, cause))
	if body.Code != ErrCodePhoneDeliveryUnknown {
		t.Fatalf("code = %q; want %q", body.Code, ErrCodePhoneDeliveryUnknown)
	}
	if body.Code == ErrCodePhoneUnreachable {
		t.Errorf("delivery-unknown shares the retry-safe code %q", body.Code)
	}
	if !strings.Contains(body.Error, cause) {
		t.Errorf("error %q lost the cause %q", body.Error, cause)
	}
}

// A touch the device threw away answers 409 "touchRejected" rather than the
// 200 it used to get (trap 50): the caller has to know nothing happened.
func TestAgentRejectedTouchIsAConflictWithACode(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400,
		tapErr: fmt.Errorf("input tap: %w: Invalid DOWN event - pointers already down", target.ErrTouchRejected)})
	s := &Server{mgr: m}
	rec := postInput(s, "dev1", `{"action":"tap","x":10,"y":20}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != ErrCodeTouchRejected {
		t.Fatalf("code = %q; want %q", body.Code, ErrCodeTouchRejected)
	}
	if !strings.Contains(body.Error, "pointers already down") {
		t.Errorf("error %q lost the device's reason", body.Error)
	}
}

// A text action's error is redacted because an error can echo what was typed.
// A phone's 503s are built without the command, so they pass through with
// their code and CoreDevice's text; the typed string is in neither the
// response nor the event, and any other text error still reads "input failed".
func TestAgentTextOnAPhoneKeepsItsPhoneErrorDetail(t *testing.T) {
	const secret = "hunter2-secret"
	cause := "CoreDeviceError 1001: not supported by this device (devicectl wake: ok)"
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"unreachable", fmt.Errorf("%w: %s", target.ErrPhoneUnreachable, cause), ErrCodePhoneUnreachable},
		{"delivery unknown", fmt.Errorf("%w: %s", target.ErrPhoneDeliveryUnknown, cause), ErrCodePhoneDeliveryUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(target.Options{})
			m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400, textErr: tc.err})
			sess, err := m.Acquire(t.Context(), "dev1")
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			defer sess.Close()
			s := &Server{mgr: m}
			rec := postInput(s, "dev1", `{"action":"text","text":"`+secret+`"}`)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d; want 503: %s", rec.Code, rec.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != tc.code || !strings.Contains(body.Error, cause) {
				t.Errorf("body = %+v; want code %q and the cause", body, tc.code)
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("response %s contains the typed text", rec.Body.String())
			}
			for _, e := range sess.EventsAfter(0) {
				if strings.Contains(e.Err, secret) || strings.Contains(e.Text, secret) {
					t.Errorf("event %+v contains the typed text", e)
				}
			}
		})
	}

	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400, textErr: fmt.Errorf("input text %s: exit 1", secret)})
	rec := postInput(&Server{mgr: m}, "dev1", `{"action":"text","text":"`+secret+`"}`)
	if strings.Contains(rec.Body.String(), secret) || !strings.Contains(rec.Body.String(), errInputFailed.Error()) {
		t.Errorf("an ordinary text error answered %s; want it redacted", rec.Body.String())
	}
}
