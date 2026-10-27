package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

// settingsTarget is a stubTarget that also implements target.DeviceSettings,
// mirroring appTarget's shape in app_test.go: the interface-having and
// interface-lacking cases are the same underlying stub, present or absent.
type settingsTarget struct {
	stubTarget
	settings    []target.Setting
	settingsErr error
	failed      []string
	notRestored []string
	applyErr    error
	gotChanges  map[string]any

	// requireLiveCtx makes Settings fail whenever the context it is handed
	// is already expired, so a test can tell a call made on the request's
	// own (possibly expired) context apart from one made on a fresh,
	// detached context.
	requireLiveCtx bool

	// blockUntilCtxDone makes Settings wait for ctx.Done() and then return
	// ctx.Err(), so a test can prove the re-read context carries its own
	// deadline: without one, this call would block forever rather than
	// completing the request.
	blockUntilCtxDone bool
}

func (a *settingsTarget) ID() string    { return "settings-target" }
func (a *settingsTarget) Label() string { return "settings-target" }

func (a *settingsTarget) Settings(ctx context.Context) ([]target.Setting, error) {
	if a.settingsErr != nil {
		return nil, a.settingsErr
	}
	if a.requireLiveCtx && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if a.blockUntilCtxDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.settings, nil
}

func (a *settingsTarget) ApplySettings(_ context.Context, changes map[string]any) ([]string, []string, error) {
	a.gotChanges = changes
	return a.failed, a.notRestored, a.applyErr
}

func settingsServer(t *testing.T, tgt target.Target) *Server {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(tgt)
	return &Server{mgr: m}
}

func sampleSettings() []target.Setting {
	return []target.Setting{
		{ID: "darkMode", Type: target.SettingBool, Value: false, Supported: true, Applies: "immediately"},
		{ID: "fontScale", Type: target.SettingRange, Value: 1.0, Min: 0.85, Max: 2.0, Supported: true, Applies: "immediately"},
	}
}

func TestHandleSettingsGetReturnsTheList(t *testing.T) {
	s := settingsServer(t, &settingsTarget{settings: sampleSettings()})

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings?target=settings-target", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got SettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Settings) != 2 || got.Settings[0].ID != "darkMode" {
		t.Errorf("got %+v", got.Settings)
	}
}

// TestHandleSettingsGetSettingsIsNeverNullEvenWhenTheUnderlyingListIsNil
// pins the same null-array fix TestHandleSettingsPut409FailedIsNeverNullEvenWhenTheUnderlyingListIsNil
// makes for Failed, one field over: target.DeviceSettings.Settings can
// return a nil slice alongside a nil error, and SettingsResponse.Settings
// carries no omitempty, so both GET and a successful PUT must normalize
// nil to [] before writeJSON ever sees it.
func TestHandleSettingsGetSettingsIsNeverNullEvenWhenTheUnderlyingListIsNil(t *testing.T) {
	s := settingsServer(t, &settingsTarget{settings: nil})

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings?target=settings-target", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"settings":[]`) {
		t.Fatalf("body = %s, want a literal \"settings\":[] rather than null", body)
	}
	var got SettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Settings == nil {
		t.Fatalf("Settings decoded as nil despite the wire promising a non-null array")
	}
}

// A target without DeviceSettings -- synthetic, in production -- answers 501
// rather than a broken response, the same as Inspector and Screenshotter.
func TestHandleSettingsReturns501WithoutTheInterface(t *testing.T) {
	s := settingsServer(t, &stubTarget{})

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings?target=stub", nil))

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSettingsGetReturns502WhenTheReadFails(t *testing.T) {
	s := settingsServer(t, &settingsTarget{settingsErr: errors.New("device offline")})

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings?target=settings-target", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSettingsReturns400WithNoTarget(t *testing.T) {
	s := settingsServer(t, &stubTarget{})

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSettingsReturns404ForAnUnknownTarget(t *testing.T) {
	s := settingsServer(t, &stubTarget{})

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings?target=missing", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSettingsPutAppliesChangesAndReturnsTheFreshList(t *testing.T) {
	target := &settingsTarget{settings: sampleSettings()}
	s := settingsServer(t, target)

	body := bytes.NewBufferString(`{"changes":{"darkMode":true}}`)
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if v, ok := target.gotChanges["darkMode"]; !ok || v != true {
		t.Errorf("gotChanges = %+v, want darkMode: true", target.gotChanges)
	}
	var got SettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Settings) != 2 {
		t.Errorf("got %d settings, want 2 (the freshly-read list)", len(got.Settings))
	}
}

// A failed apply reports 409 with which settings failed, not a bare error:
// a caller that cannot see this will assume every requested change took.
func TestHandleSettingsPutReturns409WithFailedOnPartialFailure(t *testing.T) {
	target := &settingsTarget{
		settings:    sampleSettings(),
		failed:      []string{"reduceMotion", "layoutBounds"},
		notRestored: []string{"reduceMotion"},
		applyErr:    errors.New("reduceMotion: device offline"),
	}
	s := settingsServer(t, target)

	body := bytes.NewBufferString(`{"changes":{"darkMode":true,"reduceMotion":true,"layoutBounds":true}}`)
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	failed, ok := got["failed"].([]any)
	if !ok || len(failed) != 2 {
		t.Fatalf("failed = %v, want 2 entries", got["failed"])
	}
	if _, ok := got["settings"]; !ok {
		t.Errorf("response has no settings field; a caller needs to see what actually took")
	}
	// A caller that cannot see which settings failed to restore will assume
	// the device is back at its pre-request state when it is not.
	notRestored, ok := got["notRestored"].([]any)
	if !ok || len(notRestored) != 1 || notRestored[0] != "reduceMotion" {
		t.Fatalf("response notRestored = %v, want [\"reduceMotion\"]", got["notRestored"])
	}
}

// A rejection that never touched the device -- an unknown id, or a value out
// of range -- is 400, not 409: nothing was applied and nothing needs rolling
// back, so there is no "failed" list to report.
func TestHandleSettingsPutReturns400WhenNothingWasApplied(t *testing.T) {
	tgt := &settingsTarget{
		settings: sampleSettings(),
		applyErr: fmt.Errorf("%w: unknown setting %q", target.ErrInvalidSetting, "notReal"),
	}
	s := settingsServer(t, tgt)

	body := bytes.NewBufferString(`{"changes":{"notReal":true}}`)
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["failed"]; ok {
		t.Errorf("response has a failed field, want none: nothing was applied")
	}
}

// A pre-flight that could not even ask the device -- the talkBack
// pm-path check failing to reach it, as opposed to reaching it and finding
// TalkBack genuinely absent -- is 502, not 400 or 409: nothing was applied
// (so no "failed" list, same as 400), but the cause is the device being
// unreachable rather than the request being invalid, and a caller retrying a
// 400 forever on a device that is merely offline would be the wrong lesson
// to teach it.
func TestHandleSettingsPutReturns502WhenTheDeviceCannotBeReachedDuringPreflight(t *testing.T) {
	tgt := &settingsTarget{
		settings: sampleSettings(),
		applyErr: fmt.Errorf("%w: talkBack: %s", target.ErrDeviceUnreachable, "adb: device offline"),
	}
	s := settingsServer(t, tgt)

	body := bytes.NewBufferString(`{"changes":{"talkBack":true}}`)
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["failed"]; ok {
		t.Errorf("response has a failed field, want none: nothing was applied")
	}
}

// Bad input -- empty changes -- is rejected before ApplySettings is ever
// called, so the target never sees an empty batch to reason about.
func TestHandleSettingsPutRejectsEmptyChanges(t *testing.T) {
	target := &settingsTarget{settings: sampleSettings()}
	s := settingsServer(t, target)

	body := bytes.NewBufferString(`{"changes":{}}`)
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if target.gotChanges != nil {
		t.Errorf("ApplySettings was called with %v, want it never called", target.gotChanges)
	}
}

func TestHandleSettingsPutRejectsMalformedJSON(t *testing.T) {
	target := &settingsTarget{settings: sampleSettings()}
	s := settingsServer(t, target)

	body := bytes.NewBufferString(`{not json`)
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSettingsRejectsOtherMethods(t *testing.T) {
	s := settingsServer(t, &settingsTarget{settings: sampleSettings()})

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodDelete, "/api/settings?target=settings-target", nil))

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body: %s", w.Code, w.Body.String())
	}
}

// settingsServerWithTrace wires a real trace.Writer to a temp file so a test
// can assert on what actually got recorded, the same shape a live server
// would append to loupe-trace.jsonl.
func settingsServerWithTrace(t *testing.T, tgt target.Target) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	w, err := trace.Open(path)
	if err != nil {
		t.Fatalf("trace.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	m := NewManager(target.Options{})
	m.Register(tgt)
	return &Server{mgr: m, trace: w}, path
}

func readTraceRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer f.Close()
	var recs []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode trace line %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func lastTraceRecordWithAction(recs []map[string]any, action string) map[string]any {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i]["action"] == action {
			return recs[i]
		}
	}
	return nil
}

// item 10: a GET is traced with action "settings".
func TestHandleSettingsTracesAGetAsSettings(t *testing.T) {
	s, path := settingsServerWithTrace(t, &settingsTarget{settings: sampleSettings()})

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings?target=settings-target", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	rec := lastTraceRecordWithAction(readTraceRecords(t, path), "settings")
	if rec == nil {
		t.Fatalf("no settings trace record")
	}
	if rec["target"] != "settings-target" {
		t.Errorf("target = %v", rec["target"])
	}
}

// item 10: a PUT is traced as "settings-put" and carries changes, failed and
// notRestored.
func TestHandleSettingsPutTracesChangesFailedAndNotRestored(t *testing.T) {
	tgt := &settingsTarget{
		settings:    sampleSettings(),
		failed:      []string{"reduceMotion"},
		notRestored: []string{"darkMode"},
		applyErr:    errors.New("reduceMotion: device offline"),
	}
	s, path := settingsServerWithTrace(t, tgt)

	body := bytes.NewBufferString(`{"changes":{"darkMode":true,"reduceMotion":true}}`)
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	rec := lastTraceRecordWithAction(readTraceRecords(t, path), "settings-put")
	if rec == nil {
		t.Fatalf("no settings-put trace record")
	}
	changes, ok := rec["changes"].(map[string]any)
	if !ok || len(changes) != 2 {
		t.Fatalf("changes = %v", rec["changes"])
	}
	failed, ok := rec["failed"].([]any)
	if !ok || len(failed) != 1 || failed[0] != "reduceMotion" {
		t.Fatalf("failed = %v", rec["failed"])
	}
	notRestored, ok := rec["notRestored"].([]any)
	if !ok || len(notRestored) != 1 || notRestored[0] != "darkMode" {
		t.Fatalf("notRestored = %v", rec["notRestored"])
	}
}

// item 2: the 409 re-read must run on a context detached from the request,
// with its own timeout, so a request whose own deadline has already passed
// still gets back the settings list rather than an empty one.
func TestHandleSettingsPutReReadsOnADetachedContextAfterFailure(t *testing.T) {
	tgt := &settingsTarget{
		settings:       sampleSettings(),
		failed:         []string{"reduceMotion"},
		applyErr:       errors.New("reduceMotion: device offline"),
		requireLiveCtx: true,
	}
	s := settingsServer(t, tgt)

	body := bytes.NewBufferString(`{"changes":{"reduceMotion":true}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body)

	// Simulate a request whose deadline has already passed by the time
	// ApplySettings returns: any read against this context directly
	// returns context.DeadlineExceeded.
	ctx, cancel := context.WithTimeout(req.Context(), 0)
	defer cancel()
	// Give the deadline time to actually elapse.
	time.Sleep(time.Millisecond)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	s.handleSettings(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["settings"]; !ok {
		t.Fatalf("response has no settings field even though the re-read should have run on a live, detached context; body: %s", w.Body.String())
	}
}

// TestHandleSettingsPutBoundsTheReReadWithItsOwnTimeout proves the re-read's
// context carries a deadline of its own, not just detachment from the
// request. A Settings that never returns on its own -- it only unblocks when
// its context is done -- would hang the request forever if the re-read
// context had no deadline; shrinking settingsRereadTimeout for the duration
// of this test keeps that bounded and fast.
func TestHandleSettingsPutBoundsTheReReadWithItsOwnTimeout(t *testing.T) {
	orig := settingsRereadTimeout
	settingsRereadTimeout = 20 * time.Millisecond
	defer func() { settingsRereadTimeout = orig }()

	tgt := &settingsTarget{
		settings:          sampleSettings(),
		failed:            []string{"reduceMotion"},
		applyErr:          errors.New("reduceMotion: device offline"),
		blockUntilCtxDone: true,
	}
	s := settingsServer(t, tgt)

	body := bytes.NewBufferString(`{"changes":{"reduceMotion":true}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body)

	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		s.handleSettings(w, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleSettings did not return -- the re-read has no deadline of its own")
	}

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["settings"]; ok {
		t.Fatalf("response has a settings field even though the re-read never completed; body: %s", w.Body.String())
	}
}

// item 12: a body over the limit is rejected rather than buffered without
// bound.
func TestHandleSettingsPutRejectsOversizedBody(t *testing.T) {
	s := settingsServer(t, &settingsTarget{settings: sampleSettings()})

	huge := `{"changes":{"darkMode":true},"padding":"` + strings.Repeat("x", maxSettingsBody+1) + `"}`
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", bytes.NewBufferString(huge)))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleSettingsPut409FailedIsNeverNullEvenWhenTheUnderlyingListIsNil
// pins the null-array fix from the apischema review: ApplySettings can in
// principle return a nil failed slice alongside a non-nil error (nothing in
// target.DeviceSettings' contract rules that out, even though every real
// implementation names at least one setting on this path), and
// SettingsConflictResponse.Failed carries no omitempty -- docs/api.schema.json
// promises an array. Encoding the real handler output and decoding it back
// is what catches a regression that reintroduces the bare `Failed: failed`
// assignment.
func TestHandleSettingsPut409FailedIsNeverNullEvenWhenTheUnderlyingListIsNil(t *testing.T) {
	tgt := &settingsTarget{
		settings: sampleSettings(),
		failed:   nil,
		applyErr: errors.New("darkMode: device offline"),
	}
	s := settingsServer(t, tgt)

	body := bytes.NewBufferString(`{"changes":{"darkMode":true}}`)
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target", body))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, `"failed":[]`) {
		t.Fatalf("body = %s, want a literal \"failed\":[] rather than null", respBody)
	}
	var got SettingsConflictResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Failed == nil {
		t.Fatalf("Failed decoded as nil despite the wire promising a non-null array")
	}
	if len(got.Failed) != 0 {
		t.Fatalf("Failed = %v, want empty", got.Failed)
	}
}

// TestHandleSettingsPut409SettingsReflectsAnEmptyReReadExactly and
// TestHandleSettingsPut409SettingsReflectsANilReReadExactly are the review's
// item 4: TestSettingsConflictResponseMatchesTheOldMapEncoding below pins
// SettingsConflictResponse's own marshaling against the pre-1be73c4
// map-literal encoding, but it builds the struct directly and never calls
// putSettings, so it cannot catch a regression in the handler code that
// decides what goes into Settings. These two go through handleSettings for
// a real 409, with a settingsTarget whose re-read answers an empty list and
// a nil one respectively, and pin the two mutations the review named:
// setting Settings only when len(settings) > 0 (which would make the empty
// case indistinguishable from a failed re-read, dropping the field instead
// of writing []), and normalizing a nil re-read into [] (which would erase
// the nil/empty distinction SettingsConflictResponse's own doc comment
// says is deliberate here, unlike SettingsResponse.Settings above).
func TestHandleSettingsPut409SettingsReflectsAnEmptyReReadExactly(t *testing.T) {
	tgt := &settingsTarget{
		settings: []target.Setting{},
		applyErr: errors.New("darkMode: device offline"),
	}
	s := settingsServer(t, tgt)

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target",
		bytes.NewBufferString(`{"changes":{"darkMode":true}}`)))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"settings":[]`) {
		t.Fatalf("body = %s, want a literal \"settings\":[] for an empty re-read, not an absent field", body)
	}
	var got SettingsConflictResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Settings == nil {
		t.Fatalf("Settings decoded as absent; want a present pointer to an empty slice")
	}
	if len(*got.Settings) != 0 {
		t.Fatalf("Settings = %v, want empty", *got.Settings)
	}
}

func TestHandleSettingsPut409SettingsReflectsANilReReadExactly(t *testing.T) {
	tgt := &settingsTarget{
		settings: nil,
		applyErr: errors.New("darkMode: device offline"),
	}
	s := settingsServer(t, tgt)

	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings?target=settings-target",
		bytes.NewBufferString(`{"changes":{"darkMode":true}}`)))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"settings":null`) {
		t.Fatalf("body = %s, want a literal \"settings\":null for a nil re-read, not [] or an absent field", body)
	}
}

// TestSettingsConflictResponseMatchesTheOldMapEncoding pins
// SettingsConflictResponse's wire bytes against the map[string]any literal
// this type replaced (see 1be73c4's putSettings), for the one field the
// review flagged as changed by the replacement: Settings. The old code did
//
//	resp := map[string]any{"error": ..., "failed": failed}
//	...
//	if settings, err := ds.Settings(rctx); err == nil {
//	        resp["settings"] = settings
//	}
//
// so "settings" was absent entirely when the re-read failed, and otherwise
// present carrying exactly whatever ds.Settings() returned -- a nil slice
// marshaling as null, an empty one as [], a populated one as an array.
// SettingsConflictResponse.Settings is a *[]target.Setting with omitempty
// precisely to reproduce that: omitempty drops the field only when the
// pointer itself is nil (the re-read failed), and a non-nil pointer to a
// nil slice still marshals as null rather than being omitted.
func TestSettingsConflictResponseMatchesTheOldMapEncoding(t *testing.T) {
	settings := sampleSettings()

	cases := []struct {
		name     string
		settings *[]target.Setting
		old      map[string]any
	}{
		{
			name:     "re-read succeeded with a populated list",
			settings: &settings,
			old: map[string]any{
				"error": "boom", "failed": []string{"darkMode"},
				"settings": settings,
			},
		},
		{
			name:     "re-read succeeded with a nil list",
			settings: func() *[]target.Setting { var s []target.Setting; return &s }(),
			old: map[string]any{
				"error": "boom", "failed": []string{"darkMode"},
				"settings": []target.Setting(nil),
			},
		},
		{
			name:     "re-read succeeded with an empty list",
			settings: &[]target.Setting{},
			old: map[string]any{
				"error": "boom", "failed": []string{"darkMode"},
				"settings": []target.Setting{},
			},
		},
		{
			name:     "re-read failed",
			settings: nil,
			old: map[string]any{
				"error": "boom", "failed": []string{"darkMode"},
				// no "settings" key at all
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			newBytes, err := json.Marshal(SettingsConflictResponse{
				Error: "boom", Failed: []string{"darkMode"}, Settings: c.settings,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			oldBytes, err := json.Marshal(c.old)
			if err != nil {
				t.Fatalf("marshal old: %v", err)
			}

			var newMap, oldMap map[string]any
			if err := json.Unmarshal(newBytes, &newMap); err != nil {
				t.Fatalf("unmarshal new: %v", err)
			}
			if err := json.Unmarshal(oldBytes, &oldMap); err != nil {
				t.Fatalf("unmarshal old: %v", err)
			}
			if !reflect.DeepEqual(newMap, oldMap) {
				t.Errorf("new encoding = %s, want the same shape as the old map encoding %s", newBytes, oldBytes)
			}
			// Presence, not just value, is the thing under test: a settings
			// key entirely absent from the old map must stay absent here
			// too, which reflect.DeepEqual on the decoded maps above already
			// covers (a missing key and a key with a null value produce
			// different Go maps), but assert it directly as well so a
			// future change to the comparison above cannot silently drop
			// this check.
			_, newHas := newMap["settings"]
			_, oldHas := oldMap["settings"]
			if newHas != oldHas {
				t.Errorf("settings key presence = %v, want %v (matching the old map)", newHas, oldHas)
			}
		})
	}
}
