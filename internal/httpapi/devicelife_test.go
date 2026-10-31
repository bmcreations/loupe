package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/record"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

// lifeTarget is a stub that records Stop and Delete instead of touching a
// device.
type lifeTarget struct {
	describingTarget
	stopErr, deleteErr error
	stops, deletes     atomic.Int32
	saves              atomic.Int32
	resets, bootCalls  atomic.Int32
	bootErr            error
	// savedBeforeStop is how many saves Stop found already made.
	savedBeforeStop atomic.Int32
	boots           bool
	// bootWaitsForCtx makes Boot block until its context ends, as a boot that
	// outruns ?wait= does.
	bootWaitsForCtx bool
	// holdDelete and holdStop, when set, make Delete or Stop signal entered
	// and wait for them.
	holdDelete, holdStop chan struct{}
	entered              chan struct{}
	// holdBoot, when set, makes Boot signal bootEntered and wait for it.
	holdBoot, bootEntered chan struct{}
}

func (l *lifeTarget) BootsOnOpen() bool { return l.boots }

// stuckBootTarget is a lifeTarget whose emulator process is alive but never
// attached, as a boot adb still calls offline.
type stuckBootTarget struct{ *lifeTarget }

func (stuckBootTarget) BootStatus(context.Context) target.BootStatus {
	return target.BootStatus{State: target.BootStateBooting, ProcessAlive: true, PID: 42}
}

func (l *lifeTarget) SaveState(context.Context) (bool, error) {
	l.saves.Add(1)
	return true, nil
}

func (l *lifeTarget) ResetState(context.Context) error {
	l.resets.Add(1)
	return nil
}

func (l *lifeTarget) Boot(ctx context.Context) error {
	l.bootCalls.Add(1)
	if l.holdBoot != nil {
		l.bootEntered <- struct{}{}
		<-l.holdBoot
	}
	if l.bootWaitsForCtx {
		<-ctx.Done()
	}
	return l.bootErr
}

func (l *lifeTarget) Stop(context.Context) error {
	l.savedBeforeStop.Store(l.saves.Load())
	l.stops.Add(1)
	if l.holdStop != nil {
		l.entered <- struct{}{}
		<-l.holdStop
	}
	return l.stopErr
}

func (l *lifeTarget) Delete(context.Context) error {
	l.deletes.Add(1)
	if l.holdDelete != nil {
		l.entered <- struct{}{}
		<-l.holdDelete
	}
	return l.deleteErr
}

func newLifeServer(t *testing.T, id string, tw *trace.Writer) (*Server, *lifeTarget) {
	t.Helper()
	m := NewManager(target.Options{})
	m.SetTrace(tw)
	lt := &lifeTarget{describingTarget: describingTarget{id: id}}
	m.Register(lt)
	s := &Server{mgr: m, trace: tw}
	t.Cleanup(m.Shutdown)
	return s, lt
}

func TestDeviceActionsRule(t *testing.T) {
	cases := []struct {
		ti   TargetInfo
		want string
	}{
		{TargetInfo{ID: "avd:a", Available: true}, "shutdown,delete"},
		{TargetInfo{ID: "avd:a", Available: true, BootsOnOpen: true}, "delete"},
		{TargetInfo{ID: "sim:U", Available: true}, "shutdown,delete"},
		{TargetInfo{ID: "sim-new:x", Available: true}, ""},
		{TargetInfo{ID: "avd:a", Available: true, State: "creating"}, ""},
		{TargetInfo{ID: "avd:a", Available: false, State: "adbUnreachable"}, ""},
		{TargetInfo{ID: "emulator-5554", Kind: "emulator", Available: true}, ""},
		{TargetInfo{ID: "ios:00008", Kind: "iosDevice", Available: true}, ""},
		{TargetInfo{ID: "synthetic", Available: true}, ""},
	}
	for _, c := range cases {
		if got := strings.Join(deviceActions(c.ti), ","); got != c.want {
			t.Errorf("%+v: got %q want %q", c.ti, got, c.want)
		}
	}
}

func TestShutdownStopsTheTargetAndEndsItsSession(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	sess, err := s.mgr.Acquire(t.Context(), "avd:a")
	if err != nil {
		t.Fatal(err)
	}
	w := doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:a", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("shutdown = %d %s", w.Code, w.Body.String())
	}
	var ti TargetInfo
	if err := json.Unmarshal(w.Body.Bytes(), &ti); err != nil || ti.ID != "avd:a" {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	if n := lt.stops.Load(); n != 1 {
		t.Errorf("Stop called %d times, want 1", n)
	}
	if got := sess.EndReason(); got != "shutDown" {
		t.Errorf("session end reason = %q, want shutDown", got)
	}
	if lt.deletes.Load() != 0 {
		t.Error("a shutdown deleted the device")
	}
	if lt.saves.Load() != 1 || lt.savedBeforeStop.Load() != 1 {
		t.Errorf("saves %d, %d before Stop; want one save before the stop", lt.saves.Load(), lt.savedBeforeStop.Load())
	}
}

func TestDeleteStopsDeletesAndUnregisters(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	sess, err := s.mgr.Acquire(t.Context(), "avd:a")
	if err != nil {
		t.Fatal(err)
	}
	w := doRecord(s, http.MethodDelete, "/api/devices?target=avd:a", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	if lt.deletes.Load() != 1 {
		t.Errorf("Delete called %d times, want 1", lt.deletes.Load())
	}
	if lt.stops.Load() != 0 {
		t.Errorf("handler called Stop %d times; Delete stops the device itself", lt.stops.Load())
	}
	if lt.saves.Load() != 0 {
		t.Error("a delete saved state it is about to remove")
	}
	if got := sess.EndReason(); got != "deleted" {
		t.Errorf("session end reason = %q, want deleted", got)
	}
	list := doRecord(s, http.MethodGet, "/api/targets", nil)
	if strings.Contains(list.Body.String(), "avd:a") {
		t.Errorf("target still listed: %s", list.Body.String())
	}
}

func TestShutdownRefusesWhileRecording(t *testing.T) {
	m := NewManager(target.Options{})
	lt := &lifeTarget{describingTarget: describingTarget{id: "avd:a"}}
	m.Register(lt)
	st, err := record.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{mgr: m}
	s.SetRecordings(st)
	t.Cleanup(func() {
		for _, r := range st.List() {
			if r.InProgress {
				_, _ = st.Stop(r.ID)
			}
		}
		m.Shutdown()
	})
	w := doRecord(s, http.MethodPost, "/api/record?target=avd:a", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var rec RecordStartResponse
	_ = json.Unmarshal(w.Body.Bytes(), &rec)

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/devices/shutdown?target=avd:a"},
		{http.MethodDelete, "/api/devices?target=avd:a"},
	} {
		w = doRecord(s, c.method, c.path, nil)
		var e ErrorResponse
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != http.StatusConflict || e.Code != ErrCodeRecording || e.ID != rec.ID {
			t.Errorf("%s = %d %s; want 409 recording id %s", c.path, w.Code, w.Body.String(), rec.ID)
		}
	}
	if lt.stops.Load() != 0 || lt.deletes.Load() != 0 {
		t.Error("the device was touched during a recording")
	}
}

func TestShutdownRefusesAnAgentWhileAPersonHolds(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	sess, err := s.mgr.Acquire(t.Context(), "avd:a")
	if err != nil {
		t.Fatal(err)
	}
	vid, _ := sess.AddViewer()
	_ = sess.ViewerTouch(vid, session.TouchEvent{Action: 0, PointerID: 1})

	w := doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:a", map[string]string{HeaderAgent: "claude"})
	if w.Code != http.StatusConflict || errCode(t, w) != ErrCodeHumanHeld {
		t.Fatalf("agent = %d %s; want 409 humanHeld", w.Code, w.Body.String())
	}
	if lt.stops.Load() != 0 {
		t.Error("Stop ran for a refused agent")
	}
	w = doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:a", map[string]string{HeaderViewer: itoa(vid)})
	if w.Code != http.StatusOK {
		t.Fatalf("the person's own request = %d %s; want 200", w.Code, w.Body.String())
	}
	if lt.stops.Load() != 1 {
		t.Error("Stop did not run for the person")
	}
}

func itoa(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestShutdownAndDeleteOfASyntheticTargetOrABareEmulatorSerialAre422(t *testing.T) {
	for _, id := range []string{"synthetic", "emulator-5554"} {
		s, lt := newLifeServer(t, id, nil)
		for _, c := range []struct{ method, path string }{
			{http.MethodPost, "/api/devices/shutdown?target=" + id},
			{http.MethodDelete, "/api/devices?target=" + id},
		} {
			w := doRecord(s, c.method, c.path, nil)
			if w.Code != http.StatusUnprocessableEntity || errCode(t, w) != ErrCodeNotSupported {
				t.Errorf("%s %s = %d %s; want 422 notSupported", c.method, id, w.Code, w.Body.String())
			}
		}
		if lt.stops.Load() != 0 || lt.deletes.Load() != 0 {
			t.Errorf("%s: the target was touched", id)
		}
	}
}

func TestShutdownOfAnUnknownTargetIs404(t *testing.T) {
	s, _ := newLifeServer(t, "avd:a", nil)
	if w := doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("= %d, want 404", w.Code)
	}
	if w := doRecord(s, http.MethodDelete, "/api/devices", nil); w.Code != http.StatusBadRequest {
		t.Errorf("no target = %d, want 400", w.Code)
	}
}

func TestStopFailureIs502TracedAndLiftsTheRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Close()
	s, lt := newLifeServer(t, "avd:a", tw)
	lt.stopErr = errors.New("emulator wedged")

	w := doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:a", nil)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "emulator wedged") {
		t.Fatalf("= %d %s; want 502 naming the error", w.Code, w.Body.String())
	}
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); err != nil {
		t.Errorf("Acquire after a failed Stop = %v; the guard should be lifted", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var found map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r map[string]any
		if json.Unmarshal(sc.Bytes(), &r) == nil && r["kind"] == "agent" && r["action"] == "shutdown" {
			found = r
		}
	}
	if found == nil || found["outcome"] != "error" || found["platform"] != "android" || found["err"] != "emulator wedged" {
		t.Errorf("trace record = %v", found)
	}
}

func TestDeleteFailureKeepsTheTargetAndLiftsTheRefusal(t *testing.T) {
	s, lt := newLifeServer(t, "sim:U", nil)
	lt.deleteErr = errors.New("simctl delete failed")
	w := doRecord(s, http.MethodDelete, "/api/devices?target=sim:U", nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("= %d %s; want 502", w.Code, w.Body.String())
	}
	if _, ok := s.mgr.Target("sim:U"); !ok {
		t.Error("a failed delete unregistered the target")
	}
	if _, err := s.mgr.Acquire(t.Context(), "sim:U"); err != nil {
		t.Errorf("Acquire after a failed Delete = %v", err)
	}
}

// Between EndSession and Unregister a delete runs Stop and Delete, which can
// take many seconds. A reconnect in that window must not boot the device.
func TestADeleteInFlightRefusesReconnectsWithItsOwnReason(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "avd:gone"})
	if _, err := m.Acquire(t.Context(), "avd:gone"); err != nil {
		t.Fatal(err)
	}
	armed := m.EndSession("avd:gone", "deleted")
	_, err := m.Acquire(t.Context(), "avd:gone")
	if !errors.Is(err, ErrJustShutDown) {
		t.Fatalf("Acquire = %v, want ErrJustShutDown", err)
	}
	if got := endedReason(err); got != "deleted" {
		t.Errorf("reason = %q, want deleted", got)
	}
	m.ClearEnded("avd:gone", armed)
	if _, err := m.Acquire(t.Context(), "avd:gone"); err != nil {
		t.Errorf("Acquire after ClearEnded = %v", err)
	}
}

// A request lifts only the guard it armed: a later EndSession's guard stays.
func TestClearEndedLeavesALaterGuardAlone(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "avd:gone"})
	first := m.EndSession("avd:gone", "shutDown")
	time.Sleep(2 * time.Millisecond)
	m.EndSession("avd:gone", "deleted")
	m.ClearEnded("avd:gone", first)
	_, err := m.Acquire(t.Context(), "avd:gone")
	if got := endedReason(err); got != "deleted" {
		t.Errorf("after the first request's ClearEnded: Acquire = %v, reason %q; want the delete's guard", err, got)
	}
}

// While a delete runs, a second shutdown or delete of the same target is
// refused, so its failure cannot lift the delete's guard.
func TestALifecycleRequestWhileOneIsRunningIs409Busy(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	lt.holdDelete = make(chan struct{})
	lt.entered = make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- doRecord(s, http.MethodDelete, "/api/devices?target=avd:a", nil) }()
	<-lt.entered

	w := doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:a", nil)
	if w.Code != http.StatusConflict || errCode(t, w) != ErrCodeBusy {
		t.Errorf("second request = %d %s; want 409 busy", w.Code, w.Body.String())
	}
	if lt.stops.Load() != 0 {
		t.Error("the refused request reached Stop")
	}
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); !errors.Is(err, ErrJustShutDown) {
		t.Errorf("Acquire during the delete = %v; want the guard held", err)
	}

	close(lt.holdDelete)
	if w := <-done; w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
}

func TestShutdownOfAnAVDThatIsNotRunningIs422(t *testing.T) {
	s, lt := newLifeServer(t, "avd:cold", nil)
	lt.boots = true
	w := doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:cold", nil)
	if w.Code != http.StatusUnprocessableEntity || errCode(t, w) != ErrCodeNotSupported {
		t.Fatalf("shutdown = %d %s; want 422 notSupported", w.Code, w.Body.String())
	}
	if lt.stops.Load() != 0 {
		t.Error("Stop was called on an AVD that is not running")
	}
}

func TestShutdownStopsABootThatNeverCameUpWithoutSavingIt(t *testing.T) {
	m := NewManager(target.Options{})
	lt := &lifeTarget{describingTarget: describingTarget{id: "avd:stuck"}, boots: true}
	m.Register(stuckBootTarget{lt})
	s := &Server{mgr: m}
	t.Cleanup(m.Shutdown)
	w := doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:stuck", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("shutdown = %d %s; want 200", w.Code, w.Body.String())
	}
	if n := lt.stops.Load(); n != 1 {
		t.Errorf("Stop called %d times, want 1", n)
	}
	if n := lt.saves.Load(); n != 0 {
		t.Errorf("saved state %d times for a boot that never came up, want 0", n)
	}
}

// shortGrace shortens shutDownGrace for one test.
func shortGrace(t *testing.T) {
	t.Helper()
	orig := shutDownGrace
	shutDownGrace = 50 * time.Millisecond
	t.Cleanup(func() { shutDownGrace = orig })
}

// The guard holds for the whole Stop, however long it runs, and its grace
// counts from when Stop returned. An Acquire past the grace mid-Stop would
// boot an emulator the Stop is killing.
func TestTheGuardHoldsThroughASlowStopAndCountsItsGraceFromTheReturn(t *testing.T) {
	shortGrace(t)
	s, lt := newLifeServer(t, "avd:a", nil)
	lt.holdStop = make(chan struct{})
	lt.entered = make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:a", nil) }()
	<-lt.entered

	time.Sleep(3 * shutDownGrace)
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); !errors.Is(err, ErrJustShutDown) || endedReason(err) != "shutDown" {
		t.Errorf("Acquire mid-Stop, past the grace = %v; want refused as shutDown", err)
	}
	s.mgr.ClearShutDown("avd:a") // what start=1 would reach without lifeInFlight
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); !errors.Is(err, ErrJustShutDown) {
		t.Errorf("ClearShutDown lifted a guard whose Stop is still running: Acquire = %v", err)
	}

	close(lt.holdStop)
	if w := <-done; w.Code != http.StatusOK {
		t.Fatalf("shutdown = %d %s", w.Code, w.Body.String())
	}
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); !errors.Is(err, ErrJustShutDown) {
		t.Errorf("Acquire right after Stop returned = %v; want the grace to start now", err)
	}
	time.Sleep(2 * shutDownGrace)
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); err != nil {
		t.Errorf("Acquire a grace after Stop returned = %v", err)
	}
}

func TestASlowStopThatFailsLiftsTheGuard(t *testing.T) {
	shortGrace(t)
	s, lt := newLifeServer(t, "avd:a", nil)
	lt.holdStop = make(chan struct{})
	lt.entered = make(chan struct{})
	lt.stopErr = errors.New("emulator wedged")
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:a", nil) }()
	<-lt.entered
	time.Sleep(3 * shutDownGrace)
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); !errors.Is(err, ErrJustShutDown) {
		t.Errorf("Acquire mid-Stop = %v; want refused", err)
	}
	close(lt.holdStop)
	if w := <-done; w.Code != http.StatusBadGateway {
		t.Fatalf("shutdown = %d %s; want 502", w.Code, w.Body.String())
	}
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); err != nil {
		t.Errorf("Acquire after the failed Stop = %v; want the guard lifted", err)
	}
}

func TestTheGuardHoldsThroughASlowDeleteAndTheTargetStaysGone(t *testing.T) {
	shortGrace(t)
	s, lt := newLifeServer(t, "sim:U", nil)
	lt.holdDelete = make(chan struct{})
	lt.entered = make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- doRecord(s, http.MethodDelete, "/api/devices?target=sim:U", nil) }()
	<-lt.entered
	time.Sleep(3 * shutDownGrace)
	if _, err := s.mgr.Acquire(t.Context(), "sim:U"); !errors.Is(err, ErrJustShutDown) || endedReason(err) != "deleted" {
		t.Errorf("Acquire mid-Delete, past the grace = %v; want refused as deleted", err)
	}
	close(lt.holdDelete)
	if w := <-done; w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	time.Sleep(2 * shutDownGrace)
	// Still deleted, not "unknown target": a tab that missed MsgEnded should
	// land on the deleted screen rather than retry a failed open.
	if _, err := s.mgr.Acquire(t.Context(), "sim:U"); !errors.Is(err, ErrJustShutDown) || endedReason(err) != "deleted" {
		t.Errorf("Acquire a grace after the delete = %v; want refused as deleted", err)
	}
}

func TestResetEndsTheSessionAndSavesNothing(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	sess, err := s.mgr.Acquire(t.Context(), "avd:a")
	if err != nil {
		t.Fatal(err)
	}
	w := doRecord(s, http.MethodPost, "/api/devices/reset?target=avd:a", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", w.Code, w.Body.String())
	}
	if lt.resets.Load() != 1 || lt.saves.Load() != 0 {
		t.Errorf("resets %d saves %d; want one reset and no save", lt.resets.Load(), lt.saves.Load())
	}
	if got := sess.EndReason(); got != "shutDown" {
		t.Errorf("session end reason = %q, want shutDown", got)
	}
}

// Reset is for a stopped AVD as much as a running one: the kept state is
// on disk either way.
func TestResetOfAStoppedAVDIsAllowed(t *testing.T) {
	s, lt := newLifeServer(t, "avd:cold", nil)
	lt.boots = true
	if w := doRecord(s, http.MethodPost, "/api/devices/reset?target=avd:cold", nil); w.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", w.Code, w.Body.String())
	}
	if lt.resets.Load() != 1 {
		t.Error("ResetState was not called")
	}
}

func TestResetOfASimulatorIs422(t *testing.T) {
	s, lt := newLifeServer(t, "sim:abc", nil)
	w := doRecord(s, http.MethodPost, "/api/devices/reset?target=sim:abc", nil)
	if w.Code != http.StatusUnprocessableEntity || errCode(t, w) != ErrCodeNotSupported {
		t.Fatalf("reset = %d %s; want 422 notSupported", w.Code, w.Body.String())
	}
	if lt.resets.Load() != 0 || lt.stops.Load() != 0 {
		t.Error("the simulator was touched")
	}
}

func TestBootBootsAnAVDOrASimulator(t *testing.T) {
	for _, id := range []string{"avd:a", "sim:abc"} {
		s, lt := newLifeServer(t, id, nil)
		lt.boots = true
		w := doRecord(s, http.MethodPost, "/api/devices/boot?target="+id, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: boot = %d %s", id, w.Code, w.Body.String())
		}
		var ti TargetInfo
		if err := json.Unmarshal(w.Body.Bytes(), &ti); err != nil || ti.ID != id {
			t.Errorf("%s: body %q: %v", id, w.Body.String(), err)
		}
		if lt.bootCalls.Load() != 1 {
			t.Errorf("%s: Boot called %d times, want 1", id, lt.bootCalls.Load())
		}
		if _, open := s.mgr.Session(id); open {
			t.Errorf("%s: a boot opened a session", id)
		}
	}
}

func TestBootOfASyntheticTargetIs422(t *testing.T) {
	for _, id := range []string{"synthetic", "emulator-5554"} {
		s, lt := newLifeServer(t, id, nil)
		w := doRecord(s, http.MethodPost, "/api/devices/boot?target="+id, nil)
		if w.Code != http.StatusUnprocessableEntity || errCode(t, w) != ErrCodeNotSupported {
			t.Errorf("%s: boot = %d %s; want 422 notSupported", id, w.Code, w.Body.String())
		}
		if lt.bootCalls.Load() != 0 {
			t.Errorf("%s: Boot was called", id)
		}
	}
}

func TestBootFailureIs502(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	lt.boots = true
	lt.bootErr = errors.New("no emulator")
	if w := doRecord(s, http.MethodPost, "/api/devices/boot?target=avd:a", nil); w.Code != http.StatusBadGateway {
		t.Errorf("boot = %d %s; want 502", w.Code, w.Body.String())
	}
}

// A boot is an explicit start, so it lifts the guard a shutdown left, as the
// shut-down screen's Start it does.
func TestBootLiftsTheShutDownGuard(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	if w := doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:a", nil); w.Code != http.StatusOK {
		t.Fatalf("shutdown = %d %s", w.Code, w.Body.String())
	}
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); !errors.Is(err, ErrJustShutDown) {
		t.Fatalf("Acquire after the shutdown = %v; want the guard", err)
	}
	lt.boots = true
	if w := doRecord(s, http.MethodPost, "/api/devices/boot?target=avd:a", nil); w.Code != http.StatusOK {
		t.Fatalf("boot = %d %s", w.Code, w.Body.String())
	}
	if _, err := s.mgr.Acquire(t.Context(), "avd:a"); err != nil {
		t.Errorf("Acquire after the boot = %v; want it open", err)
	}
}

func TestBootWhileAShutdownRunsIs409Busy(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	lt.holdStop = make(chan struct{})
	lt.entered = make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- doRecord(s, http.MethodPost, "/api/devices/shutdown?target=avd:a", nil) }()
	<-lt.entered

	w := doRecord(s, http.MethodPost, "/api/devices/boot?target=avd:a", nil)
	if w.Code != http.StatusConflict || errCode(t, w) != ErrCodeBusy {
		t.Errorf("boot = %d %s; want 409 busy", w.Code, w.Body.String())
	}
	if lt.bootCalls.Load() != 0 {
		t.Error("the refused boot reached Boot")
	}
	close(lt.holdStop)
	<-done
}

// wait=0 answers 202 at once and finishes the boot in the background; a
// second wait=0 while it runs starts nothing more.
func TestBootWithWaitZeroAnswers202AndBootsInTheBackground(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	lt.boots = true
	w := doRecord(s, http.MethodPost, "/api/devices/boot?target=avd:a&wait=0", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("boot wait=0 = %d %s; want 202", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for lt.bootCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if lt.bootCalls.Load() != 1 {
		t.Fatalf("Boot called %d times, want 1", lt.bootCalls.Load())
	}
}

func TestBootWaitMustBeADuration(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	for _, v := range []string{"soon", "-1s", "99h"} {
		w := doRecord(s, http.MethodPost, "/api/devices/boot?target=avd:a&wait="+v, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("wait=%s: %d %s; want 400", v, w.Code, w.Body.String())
		}
	}
	if lt.bootCalls.Load() != 0 {
		t.Error("a bad wait reached Boot")
	}
}

func TestParseBootWait(t *testing.T) {
	for in, want := range map[string]time.Duration{"": bootTimeout, "0": 0, "90": 90 * time.Second, "90s": 90 * time.Second, "10m": 10 * time.Minute} {
		if got, err := parseBootWait(in); err != nil || got != want {
			t.Errorf("parseBootWait(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

// A boot that outruns its wait is a 504, not a failure of the emulator.
func TestBootTimeoutIs504(t *testing.T) {
	s, lt := newLifeServer(t, "avd:a", nil)
	lt.boots = true
	lt.bootErr = context.DeadlineExceeded
	lt.bootWaitsForCtx = true
	w := doRecord(s, http.MethodPost, "/api/devices/boot?target=avd:a&wait=1s", nil)
	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("boot = %d %s; want 504", w.Code, w.Body.String())
	}
}

// GET reports on a target that has no boot status as 422, not a panic.
func TestBootStatusOfANonEmulatorIs422(t *testing.T) {
	s, _ := newLifeServer(t, "avd:a", nil)
	w := doRecord(s, http.MethodGet, "/api/devices/boot?target=avd:a", nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("GET boot = %d %s; want 422", w.Code, w.Body.String())
	}
}

// bootClaimServer is a lifeServer for avd:a with a claim book on a clock the
// test moves, and avd:a claimed by agent a.
func bootClaimServer(t *testing.T) (*Server, *lifeTarget, *time.Time) {
	t.Helper()
	s, lt := newLifeServer(t, "avd:a", nil)
	lt.boots = true
	lt.holdBoot = make(chan struct{})
	lt.bootEntered = make(chan struct{})
	s.claims = newClaimBook(nil)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s.claims.now = func() time.Time { return now }
	if _, holder := s.claims.take("avd:a", "a", "s1", ""); holder != nil {
		t.Fatalf("claim refused: %+v", holder)
	}
	return s, lt, &now
}

// A boot that waits longer than the lease keeps its caller's claim: the
// caller makes no other call while it waits, so nothing else renews it.
func TestABootWaitHoldsTheCallersClaim(t *testing.T) {
	s, lt, now := bootClaimServer(t)
	a := map[string]string{HeaderAgent: "a", HeaderSession: "s1"}
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- doRecord(s, http.MethodPost, "/api/devices/boot?target=avd:a&wait=15m", a) }()
	<-lt.bootEntered

	*now = now.Add(claimLease + 2*time.Minute)
	if r := s.claims.refusal("avd:a", "b", "s2"); r == nil || r.Code != ErrCodeClaimed {
		t.Fatalf("mid-boot refusal for another agent = %+v; want claimedBy", r)
	}
	if got := s.claims.list(); len(got) != 1 || got[0].IdleMs != 0 {
		t.Fatalf("claims mid-boot = %+v; want one, not idle", got)
	}

	close(lt.holdBoot)
	if w := <-done; w.Code != http.StatusOK {
		t.Fatalf("boot = %d %s", w.Code, w.Body.String())
	}
	// The boot's end renews the claim, so a full lease runs from there.
	*now = now.Add(claimLease - time.Minute)
	if !s.claims.held("avd:a") {
		t.Fatal("claim lapsed within a lease of the boot ending")
	}
	*now = now.Add(2 * time.Minute)
	if s.claims.held("avd:a") {
		t.Fatal("claim outlived its lease after the boot")
	}
}

// wait=0 holds the claim until the background boot ends.
func TestABackgroundBootHoldsTheCallersClaim(t *testing.T) {
	s, lt, now := bootClaimServer(t)
	a := map[string]string{HeaderAgent: "a", HeaderSession: "s1"}
	if w := doRecord(s, http.MethodPost, "/api/devices/boot?target=avd:a&wait=0", a); w.Code != http.StatusAccepted {
		t.Fatalf("boot wait=0 = %d %s", w.Code, w.Body.String())
	}
	<-lt.bootEntered
	*now = now.Add(claimLease + time.Minute)
	if !s.claims.held("avd:a") {
		t.Fatal("claim lapsed during the background boot")
	}
	close(lt.holdBoot)
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.claims.mu.Lock()
		holds := s.claims.claims["avd:a"].holds
		s.claims.mu.Unlock()
		if holds == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background boot never let go of the claim")
		}
		time.Sleep(5 * time.Millisecond)
	}
	*now = now.Add(claimLease + time.Minute)
	if s.claims.held("avd:a") {
		t.Fatal("claim outlived its lease after the background boot")
	}
}

// Only the holder's boot holds the claim; another agent's is refused before
// it boots anything, and a caller with no claim holds nothing.
func TestABootHoldsOnlyTheHoldersClaim(t *testing.T) {
	s, _, _ := bootClaimServer(t)
	b := map[string]string{HeaderAgent: "b", HeaderSession: "s2"}
	if w := doRecord(s, http.MethodPost, "/api/devices/boot?target=avd:a", b); w.Code != http.StatusConflict {
		t.Fatalf("other agent's boot = %d %s; want 409", w.Code, w.Body.String())
	}
	s.claims.hold("avd:a", "b", "s2")()
	s.claims.mu.Lock()
	holds := s.claims.claims["avd:a"].holds
	s.claims.mu.Unlock()
	if holds != 0 {
		t.Fatalf("holds = %d after a non-holder's hold; want 0", holds)
	}
}
