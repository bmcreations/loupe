package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// fakeRevylScript stands in for the revyl CLI. It logs argv, prints
// <cmd>-<sub>.json from its directory, fails with <cmd>-<sub>.fail, and
// waits for a <cmd>-<sub>.hold file to go away first. `auth login` prints
// what revyl-cli v0.2.27 prints (cmd/revyl/auth.go:370-373) to stderr and
// then waits for "approve", as the real one waits for the browser.
//
// With a <cmd>-<sub>.int file it catches SIGINT, as `device start` does
// (revyl-cli v0.2.27 cmd/revyl/device.go:1111), and logs it to
// interrupted.log. Then it exits 1, or with "finish" in the file prints
// its JSON and exits 0, as a start interrupted just after it finished
// does. With "ignore" it carries on as if nothing happened.
const fakeRevylScript = `#!/bin/sh
dir="$(dirname "$0")"
# Before argv.log, so a test that waits for argv knows the trap is set.
if [ -f "$dir/$1-$2.int" ]; then
  case "$(cat "$dir/$1-$2.int")" in
  ignore) trap 'echo "$1 $2" >> "$dir/interrupted.log"' INT ;;
  finish) trap 'echo "$1 $2" >> "$dir/interrupted.log"; cat "$dir/$1-$2.json"; exit 0' INT ;;
  *) trap 'echo "$1 $2" >> "$dir/interrupted.log"; echo "Error: cancelled while waiting for device to connect: context canceled" >&2; exit 1' INT ;;
  esac
fi
printf '%s\037' "$@" >> "$dir/argv.log"
printf '\n' >> "$dir/argv.log"
while [ -f "$dir/$1-$2.hold" ]; do sleep 0.02; done
if [ "$1 $2" = "auth login" ]; then
  echo "Open this URL to approve, then confirm the code below:" >&2
  echo "" >&2
  echo "  https://auth.revyl.test/device?user_code=WXYZ-2345" >&2
  echo "  Code: WXYZ-2345" >&2
  echo "Waiting for approval (press Ctrl+C to cancel)..." >&2
  while [ ! -f "$dir/approve" ]; do sleep 0.02; done
  cp "$dir/auth-status.json.signed-in" "$dir/auth-status.json"
  exit 0
fi
if [ -f "$dir/$1-$2.fail" ]; then cat "$dir/$1-$2.fail" >&2; exit 1; fi
[ -f "$dir/$1-$2.json" ] && cat "$dir/$1-$2.json"
exit 0
`

type revylRig struct {
	t     *testing.T
	s     *Server
	rv    *RevylDevices
	dir   string
	mu    sync.Mutex
	trace []map[string]any
}

func revylServer(t *testing.T) *revylRig {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "revyl")
	if err := os.WriteFile(bin, []byte(fakeRevylScript), 0o755); err != nil {
		t.Fatal(err)
	}
	rig := &revylRig{t: t, dir: dir}
	s := &Server{mgr: NewManager(target.Options{})}
	// The work directory does not exist yet, as on a first run.
	work := filepath.Join(dir, "work")
	rig.rv = NewRevylDevices(func() string { return bin }, work, s.mgr.Register, s.mgr.UnregisterAs, func(rec map[string]any) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.trace = append(rig.trace, rec)
	})
	s.SetRevylDevices(rig.rv)
	rig.s = s
	return rig
}

func (r *revylRig) file(name, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *revylRig) remove(name string) { _ = os.Remove(filepath.Join(r.dir, name)) }

func (r *revylRig) argv() [][]string {
	raw, _ := os.ReadFile(filepath.Join(r.dir, "argv.log"))
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line != "" {
			out = append(out, strings.Split(strings.TrimSuffix(line, "\037"), "\037"))
		}
	}
	return out
}

func (r *revylRig) ran(prefix ...string) []string {
	for _, a := range r.argv() {
		if len(a) >= len(prefix) && reflect.DeepEqual(a[:len(prefix)], prefix) {
			return a
		}
	}
	return nil
}

func (r *revylRig) list() []RevylDevice {
	r.t.Helper()
	var l RevylListResponse
	if err := json.Unmarshal(do(r.s, http.MethodGet, "/api/revyl", "").Body.Bytes(), &l); err != nil {
		r.t.Fatal(err)
	}
	return l.Devices
}

func (r *revylRig) waitFor(what string, ok func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Shaped from mcp.DeviceSession (internal/mcp/device_session.go:109) and
// deviceStartOutput (cmd/revyl/device.go:185), not recorded.
const revylStartJSON = `{
  "index": 0,
  "session_id": "6f1c2a8e-3b4d-4e5f-9a0b-1c2d3e4f5a6b",
  "workflow_run_id": "wr-1",
  "worker_base_url": "https://worker.revyl.test/w/1",
  "viewer_url": "https://app.revyl.test/sessions/6f1c2a8e",
  "whep_url": "https://stream.revyl.test/whep/6f1c2a8e",
  "platform": "android",
  "screen_width": 1080,
  "screen_height": 2400,
  "device_ready": true,
  "started_at": "2026-11-18T15:00:00Z",
  "last_activity": "2026-11-18T15:00:00Z",
  "idle_timeout": 900000000000,
  "computer_session_id": "",
  "billing_notice": "Billed per minute"
}`

func TestRevylStartShowsAStartingRowThenRegistersTheTargetAndStopEndsIt(t *testing.T) {
	rig := revylServer(t)
	rig.file("device-start.json", revylStartJSON)
	rig.file("device-start.hold", "")
	rec := do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"android","model":"Pixel 7","osVersion":"Android 14"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	var started StartRevylResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	row, ok := targetRow(t, rig.s, "revyl-starting:"+started.Key)
	if !ok || row.Kind != "revyl" || row.State != revylStarting || row.Label != "Revyl Pixel 7" || row.Transport != "cloud" {
		t.Fatalf("starting row = %+v (found %v)", row, ok)
	}
	if rec := do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"ios"}`); rec.Code != http.StatusConflict {
		t.Errorf("second start while one runs = %d", rec.Code)
	}
	if rec := do(rig.s, http.MethodDelete, "/api/revyl?key="+started.Key, ""); rec.Code != http.StatusConflict {
		t.Errorf("stop while starting = %d", rec.Code)
	}

	rig.remove("device-start.hold")
	rig.waitFor("the device to start", func() bool {
		l := rig.list()
		return len(l) == 1 && l[0].State == revylActive
	})
	got := rig.list()[0]
	id := "revyl:6f1c2a8e-3b4d-4e5f-9a0b-1c2d3e4f5a6b"
	if got.Target != id || got.Adopted || got.IdleTimeoutSec != 900 || got.Model != "Pixel 7" {
		t.Errorf("device = %+v", got)
	}
	want := []string{"device", "start", "--platform", "android", "--json", "--open=false", "--computer", "none",
		"--device-model", "Pixel 7", "--os-version", "Android 14"}
	if a := rig.ran("device", "start"); !reflect.DeepEqual(a, want) {
		t.Errorf("start argv = %q, want %q", a, want)
	}
	row, ok = targetRow(t, rig.s, id)
	if !ok || row.Kind != "revyl" || row.Label != "Revyl Pixel 7" || row.OS != "android" || row.OSVersion != "Android 14" {
		t.Errorf("target row = %+v (found %v)", row, ok)
	}
	if _, ok := targetRow(t, rig.s, "revyl-starting:"+started.Key); ok {
		t.Error("the starting row outlived the start")
	}
	body := do(rig.s, http.MethodGet, "/api/targets", "").Body.String() + do(rig.s, http.MethodGet, "/api/revyl", "").Body.String()
	for _, secret := range []string{"whep", "worker.revyl", "app.revyl"} {
		if strings.Contains(body, secret) {
			t.Errorf("a listing carries %q: %s", secret, body)
		}
	}

	rig.file("device-stop.json", `{"stopped":true}`)
	if rec := do(rig.s, http.MethodDelete, "/api/revyl?target="+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", rec.Code, rec.Body)
	}
	if a := rig.ran("device", "stop"); !reflect.DeepEqual(a, []string{"device", "stop", "--session-id", "6f1c2a8e-3b4d-4e5f-9a0b-1c2d3e4f5a6b", "--json"}) {
		t.Errorf("stop argv = %q", a)
	}
	if _, ok := targetRow(t, rig.s, id); ok {
		t.Error("the target outlived the stop")
	}
	if len(rig.list()) != 0 {
		t.Errorf("list after stop = %+v", rig.list())
	}
}

func TestRevylAFailedStartSaysWhyAndIsForgottenOnDelete(t *testing.T) {
	rig := revylServer(t)
	rig.file("device-start.fail", "Error: not authenticated: run 'revyl auth login'\n")
	rec := do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"ios"}`)
	var started StartRevylResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	rig.waitFor("the start to fail", func() bool {
		l := rig.list()
		return len(l) == 1 && l[0].State == revylFailed
	})
	if e := rig.list()[0].Error; !strings.Contains(e, "not authenticated") {
		t.Errorf("error = %q", e)
	}
	if rec := do(rig.s, http.MethodDelete, "/api/revyl?key="+started.Key, ""); rec.Code != http.StatusOK {
		t.Errorf("forget = %d", rec.Code)
	}
	if rig.ran("device", "stop") != nil {
		t.Error("a failed start was stopped at Revyl")
	}
}

func TestRevylRoutesRefuseBadInputAndAMissingCLI(t *testing.T) {
	rig := revylServer(t)
	for _, body := range []string{
		`{"platform":"web"}`,
		`{"platform":"android","model":"-rf"}`,
		`{"platform":"android","osVersion":"14; rm -rf /"}`,
	} {
		if rec := do(rig.s, http.MethodPost, "/api/revyl", body); rec.Code != http.StatusBadRequest {
			t.Errorf("start %s = %d", body, rec.Code)
		}
	}
	if len(rig.argv()) != 0 {
		t.Errorf("bad input reached the CLI: %v", rig.argv())
	}
	if rec := do(rig.s, http.MethodDelete, "/api/revyl?key=nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("stop unknown = %d", rec.Code)
	}

	rig.rv.find = func() string { return "" }
	rig.rv.SetInstaller(&RevylInstaller{})
	if rec := do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("start without CLI = %d", rec.Code)
	}
	if rec := do(rig.s, http.MethodGet, "/api/revyl/targets", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("targets without CLI = %d", rec.Code)
	}
	var acct RevylAccountResponse
	_ = json.Unmarshal(do(rig.s, http.MethodGet, "/api/revyl/account", "").Body.Bytes(), &acct)
	if acct.CLIInstalled || !acct.CanInstallCLI || acct.SignedIn {
		t.Errorf("account without CLI = %+v", acct)
	}
	if rec := do(rig.s, http.MethodPost, "/api/revyl/account", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("sign in without CLI = %d", rec.Code)
	}
}

func TestRevylTargetsListsModelsPerPlatform(t *testing.T) {
	rig := revylServer(t)
	// internal/devicetargets/targets.go:25 has no JSON tags.
	rig.file("device-targets.json", `{"android":[{"Model":"Pixel 7","Runtime":"Android 14"}],"ios":[{"Model":"iPhone 16 Pro","Runtime":"iOS 18.5"}],"web":[{"Model":"Chrome","Runtime":"1"}]}`)
	var got RevylTargetsResponse
	if err := json.Unmarshal(do(rig.s, http.MethodGet, "/api/revyl/targets", "").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string][]RevylModel{"android": {{"Pixel 7", "Android 14"}}, "ios": {{"iPhone 16 Pro", "iOS 18.5"}}}
	if !reflect.DeepEqual(got.Platforms, want) {
		t.Errorf("targets = %+v", got.Platforms)
	}
}

func TestRevylSignInShowsTheApprovalURLAndCodeWhileTheCLIWaits(t *testing.T) {
	rig := revylServer(t)
	rig.file("auth-status.json", `{"authenticated":false}`)
	rig.file("auth-status.json.signed-in", `{"authenticated":true,"email":"test@example.com","user_id":"u1","org_id":"o1","org_name":"Test Org","auth_method":"browser","expires_in_seconds":3600,"expired":false}`)

	rec := do(rig.s, http.MethodPost, "/api/revyl/account", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("sign in = %d %s", rec.Code, rec.Body)
	}
	var acct RevylAccountResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &acct)
	if !acct.SigningIn || acct.SignedIn || acct.LoginURL != "https://auth.revyl.test/device?user_code=WXYZ-2345" || acct.LoginCode != "WXYZ-2345" {
		t.Fatalf("while waiting = %+v", acct)
	}
	if a := rig.ran("auth", "login"); !reflect.DeepEqual(a, []string{"auth", "login"}) {
		t.Errorf("login argv = %q", a)
	}
	// A second POST joins the login rather than starting another.
	_ = do(rig.s, http.MethodPost, "/api/revyl/account", "")

	rig.file("approve", "")
	rig.waitFor("the login to finish", func() bool {
		var a RevylAccountResponse
		_ = json.Unmarshal(do(rig.s, http.MethodGet, "/api/revyl/account", "").Body.Bytes(), &a)
		acct = a
		return a.SignedIn && !a.SigningIn
	})
	if acct.Email != "test@example.com" || acct.Org != "Test Org" || acct.LoginURL != "" || acct.LoginCode != "" {
		t.Errorf("after approval = %+v", acct)
	}
	n := 0
	for _, a := range rig.argv() {
		if reflect.DeepEqual(a, []string{"auth", "login"}) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("auth login ran %d times", n)
	}
	rig.waitFor("the signin trace", func() bool {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		return len(rig.trace) > 0
	})
	rig.mu.Lock()
	tr, _ := json.Marshal(rig.trace)
	rig.mu.Unlock()
	if strings.Contains(string(tr), "WXYZ") || strings.Contains(string(tr), "auth.revyl.test") {
		t.Errorf("the trace carries the login: %s", tr)
	}

	if rec := do(rig.s, http.MethodPost, "/api/revyl/account", ""); rec.Code != http.StatusOK {
		t.Errorf("sign in while signed in = %d", rec.Code)
	}
}

func TestRevylSignOutIsRefusedWhileLoupeHoldsADevice(t *testing.T) {
	rig := revylServer(t)
	rig.file("device-start.json", revylStartJSON)
	rig.file("auth-status.json", `{"authenticated":true,"email":"test@example.com"}`)
	_ = do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`)
	rig.waitFor("the device to start", func() bool {
		l := rig.list()
		return len(l) == 1 && l[0].State == revylActive
	})
	if rec := do(rig.s, http.MethodDelete, "/api/revyl/account", ""); rec.Code != http.StatusConflict {
		t.Errorf("sign out holding a device = %d", rec.Code)
	}
	if rig.ran("auth", "logout") != nil {
		t.Error("signed out under a held device")
	}
}

func TestRevylSyncAdoptsAccountSessionsAndDropsEndedOnes(t *testing.T) {
	rig := revylServer(t)
	// `device list --json` is []deviceListEntry (cmd/revyl/device.go:3296).
	rig.file("device-list.json", `[`+revylStartJSON+`,{"index":-1,"session_id":"queued-1","platform":"ios","started_at":"2026-11-18T15:01:00Z","idle_timeout":900000000000,"status":"queued"},{"index":-1,"session_id":"web-1","platform":"web","started_at":"2026-11-18T15:01:00Z"}]`)
	if rec := do(rig.s, http.MethodGet, "/api/revyl?sync=1", ""); rec.Code != http.StatusOK {
		t.Fatalf("sync = %d", rec.Code)
	}
	l := rig.list()
	if len(l) != 2 || !l[0].Adopted || !l[1].Adopted {
		t.Fatalf("adopted = %+v", l)
	}
	ids := map[string]bool{l[0].Target: true, l[1].Target: true}
	if !ids["revyl:6f1c2a8e-3b4d-4e5f-9a0b-1c2d3e4f5a6b"] || !ids["revyl:queued-1"] {
		t.Errorf("adopted targets = %v", ids)
	}
	if row, ok := targetRow(t, rig.s, "revyl:queued-1"); !ok || row.Label != "Revyl iOS" {
		t.Errorf("queued row = %+v (found %v)", row, ok)
	}
	if a := rig.ran("device", "list"); !reflect.DeepEqual(a, []string{"device", "list", "--json"}) {
		t.Errorf("list argv = %q", a)
	}

	// Exit leaves adopted devices running.
	rig.rv.StopAll(context.Background())
	if rig.ran("device", "stop") != nil {
		t.Error("exit stopped a device loupe did not start")
	}

	rig.file("device-list.json", `[]`)
	rig.rv.Sync(context.Background())
	if len(rig.list()) != 0 {
		t.Errorf("after the account dropped them = %+v", rig.list())
	}
	if _, ok := targetRow(t, rig.s, "revyl:queued-1"); ok {
		t.Error("an ended session is still a target")
	}

	// A failing list (signed out, offline) changes nothing.
	_ = do(rig.s, http.MethodGet, "/api/revyl?sync=1", "")
	rig.file("device-list.json", `[`+revylStartJSON+`]`)
	rig.rv.Sync(context.Background())
	rig.file("device-list.fail", "Error: network\n")
	rig.rv.Sync(context.Background())
	if len(rig.list()) != 1 {
		t.Errorf("a failed list dropped devices: %+v", rig.list())
	}
}

func TestRevylExitStopsTheDevicesLoupeStarted(t *testing.T) {
	rig := revylServer(t)
	rig.file("device-start.json", revylStartJSON)
	rig.file("device-stop.json", `{"stopped":true}`)
	_ = do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`)
	rig.waitFor("the device to start", func() bool {
		l := rig.list()
		return len(l) == 1 && l[0].State == revylActive
	})
	rig.rv.StopAll(context.Background())
	if rig.ran("device", "stop") == nil {
		t.Error("exit left a started device running")
	}
}

func (r *revylRig) interrupted() string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "interrupted.log"))
	return string(b)
}

// startHeld starts a device and waits for `revyl device start` to be
// running and held there, as one queued at Revyl is.
func (r *revylRig) startHeld(onInterrupt string) {
	r.t.Helper()
	r.file("device-start.json", revylStartJSON)
	r.file("device-start.hold", "")
	r.file("device-start.int", onInterrupt)
	if rec := do(r.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`); rec.Code != http.StatusAccepted {
		r.t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	r.waitFor("the start to be running", func() bool { return r.ran("device", "start") != nil })
}

// Exit can't wait out a start: Revyl's queue can hold one for minutes.
// The CLI is interrupted instead, so it cancels the device at Revyl, and
// no start begins after.
func TestRevylExitInterruptsAStartInFlight(t *testing.T) {
	rig := revylServer(t)
	rig.startHeld("")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rig.rv.StopAll(ctx)
	// Were the start still running, its device would come up now.
	rig.remove("device-start.hold")
	if got := rig.interrupted(); got != "device start\n" {
		t.Fatalf("interrupted = %q; exit left the start running, and its device bills until it idles out", got)
	}
	if l := rig.list(); len(l) != 1 || l[0].State != revylFailed {
		t.Errorf("after exit = %+v", l)
	}
	if rec := do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("start after exit = %d %s", rec.Code, rec.Body)
	}
}

// A start interrupted just as it finished prints its session anyway. Exit
// stops that device rather than leave it billing.
func TestRevylExitStopsTheDeviceAnInterruptedStartBroughtUp(t *testing.T) {
	rig := revylServer(t)
	rig.file("device-stop.json", `{"stopped":true}`)
	rig.startHeld("finish")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rig.rv.StopAll(ctx)
	rig.remove("device-start.hold")
	want := []string{"device", "stop", "--session-id", "6f1c2a8e-3b4d-4e5f-9a0b-1c2d3e4f5a6b", "--json"}
	if a := rig.ran("device", "stop"); !reflect.DeepEqual(a, want) {
		t.Errorf("stop argv = %q, want %q", a, want)
	}
	if _, ok := targetRow(t, rig.s, "revyl:6f1c2a8e-3b4d-4e5f-9a0b-1c2d3e4f5a6b"); ok {
		t.Error("the device was registered after exit")
	}
	if l := rig.list(); len(l) != 1 || l[0].State != revylFailed {
		t.Errorf("after exit = %+v", l)
	}
}

// A CLI that ignores the interrupt is killed after startGrace, so exit
// still ends within its budget.
func TestRevylExitKillsAStartThatIgnoresTheInterrupt(t *testing.T) {
	rig := revylServer(t)
	rig.rv.startGrace = 100 * time.Millisecond
	rig.startHeld("ignore")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	rig.rv.StopAll(ctx)
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("exit took %v", took)
	}
	if got := rig.interrupted(); got != "device start\n" {
		t.Errorf("interrupted = %q", got)
	}
	if l := rig.list(); len(l) != 1 || l[0].State != revylFailed {
		t.Errorf("after exit = %+v; the start outlived exit", l)
	}
}

// A client that hangs up mid-stop must not cut the stop short: the row and
// the target are gone by then, and the device would bill until its idle
// timeout.
func TestRevylDeleteFinishesTheStopWhenTheClientHangsUp(t *testing.T) {
	rig := revylServer(t)
	rig.file("device-start.json", revylStartJSON)
	_ = do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`)
	rig.waitFor("the device to start", func() bool {
		l := rig.list()
		return len(l) == 1 && l[0].State == revylActive
	})
	rig.file("device-stop.json", `{"stopped":true}`)
	rig.file("device-stop.hold", "")

	ctx, hangUp := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/revyl?target="+rig.list()[0].Target, nil).WithContext(ctx)
	answered := make(chan struct{})
	go func() {
		rig.s.Handler().ServeHTTP(rec, req)
		close(answered)
	}()
	rig.waitFor("the stop to be running", func() bool { return rig.ran("device", "stop") != nil })
	hangUp()
	// Long enough for a stop tied to the request to be killed.
	time.Sleep(200 * time.Millisecond)
	rig.remove("device-stop.hold")
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop never finished")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("stop after the client hung up = %d %s", rec.Code, rec.Body)
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	for _, r := range rig.trace {
		if r["action"] == "stop" && r["error"] != nil {
			t.Errorf("stop record = %v", r)
		}
	}
}

func TestRevylInstallerDownloadsThePinnedBinaryAndChecksIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bin", "revyl")
	var asked []string
	body := []byte("#!/bin/sh\necho revyl\n")
	in := &RevylInstaller{PrivatePath: path, OS: "darwin", Arch: "arm64", fetch: func(_ context.Context, url string, max int64) ([]byte, error) {
		asked = append(asked, url)
		if max < 50<<20 {
			return nil, errors.New("cap below the published binaries")
		}
		return body, nil
	}}
	if err := in.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("mismatched sum = %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a binary that failed its checksum was installed")
	}
	if want := "https://github.com/RevylAI/revyl-cli/releases/download/v0.2.27/revyl-darwin-arm64"; len(asked) != 1 || asked[0] != want {
		t.Errorf("fetched %q", asked)
	}

	sum := sha256.Sum256(body)
	old := revylCLISums["arm64"]
	revylCLISums["arm64"] = hex.EncodeToString(sum[:])
	defer func() { revylCLISums["arm64"] = old }()
	if err := in.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("installed = %v %v", fi, err)
	}

	in.OS = "linux"
	if err := in.Install(context.Background()); err == nil {
		t.Error("installed a darwin binary on linux")
	}
}

// A Revyl target has no adb serial and no simctl UDID, so the routes that
// need one refuse it instead of reaching for either.
func TestRevylTargetIsRefusedByLocalOnlyRoutes(t *testing.T) {
	rig := revylServer(t)
	rig.file("device-start.json", revylStartJSON)
	if rec := do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	rig.waitFor("the device to start", func() bool {
		l := rig.list()
		return len(l) == 1 && l[0].State == revylActive
	})
	id := rig.list()[0].Target
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/ui?target=" + id},
		{http.MethodGet, "/api/screenshot?target=" + id},
		{http.MethodGet, "/api/logcat?target=" + id},
		{http.MethodGet, "/api/crashes?target=" + id},
		{http.MethodGet, "/api/settings?target=" + id},
		{http.MethodGet, "/api/clipboard?target=" + id},
		{http.MethodPost, "/api/install?target=" + id},
		{http.MethodPost, "/api/devices/boot?target=" + id},
		{http.MethodPost, "/api/devices/shutdown?target=" + id},
		{http.MethodPost, "/api/input?target=" + id},
		{http.MethodPost, "/api/sequence?target=" + id},
	} {
		body := ""
		switch {
		case strings.HasPrefix(c.path, "/api/input"):
			body = `{"action":"tap","x":10,"y":10}`
		case strings.HasPrefix(c.path, "/api/sequence"):
			body = `{"steps":[{"action":"tap","x":10,"y":10}]}`
		}
		rec := do(rig.s, c.method, c.path, body)
		t.Logf("%s %s = %d %s", c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()))
		if rec.Code < 400 || rec.Code >= 500 && rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want a refusal", c.method, c.path, rec.Code)
		}
	}
}

// Sync reads the list before it takes the lock, so a start that finishes
// while the list is read must not be dropped as ended, and a stop must not
// be undone by a list that still shows the session.
func TestRevylSyncTrustsLoupeOverAListReadBeforeAStartOrStop(t *testing.T) {
	rig := revylServer(t)
	rig.rv.listGrace = 0 // only the list's read time protects the start
	rig.file("device-list.json", `[]`)
	rig.file("device-list.hold", "")
	synced := make(chan struct{})
	go func() {
		rig.rv.Sync(context.Background())
		close(synced)
	}()
	rig.waitFor("the list to be running", func() bool { return rig.ran("device", "list") != nil })

	rig.file("device-start.json", revylStartJSON)
	if rec := do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	rig.waitFor("the device to start", func() bool {
		l := rig.list()
		return len(l) == 1 && l[0].State == revylActive
	})
	rig.remove("device-list.hold")
	<-synced
	if l := rig.list(); len(l) != 1 || l[0].State != revylActive || l[0].Adopted {
		t.Fatalf("a start during the list = %+v", l)
	}

	rig.rv.listGrace = revylListGrace
	rig.file("device-stop.json", `{"stopped":true}`)
	if rec := do(rig.s, http.MethodDelete, "/api/revyl?target="+rig.list()[0].Target, ""); rec.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", rec.Code, rec.Body)
	}
	rig.file("device-list.json", `[`+revylStartJSON+`]`)
	rig.rv.Sync(context.Background())
	if l := rig.list(); len(l) != 0 {
		t.Errorf("a list that still shows a stopped session adopted it: %+v", l)
	}
}

// The CLI's stderr reaches a failed row and the trace, so URLs in it,
// which can carry a session's access, are cut out.
func TestRevylStartErrorsCarryNoURL(t *testing.T) {
	rig := revylServer(t)
	rig.file("device-start.fail", "Error: worker https://worker.revyl.test/s/abc?token=sekrit did not answer\n")
	if rec := do(rig.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	rig.waitFor("the start to fail", func() bool {
		l := rig.list()
		return len(l) == 1 && l[0].State == revylFailed
	})
	rig.mu.Lock()
	trace, _ := json.Marshal(rig.trace)
	rig.mu.Unlock()
	e := rig.list()[0].Error
	if !strings.Contains(e, "did not answer") || strings.Contains(e, "sekrit") || strings.Contains(string(trace), "sekrit") {
		t.Errorf("error = %q, trace = %s", e, trace)
	}
}

func (r *revylRig) started() RevylDevice {
	r.t.Helper()
	r.file("device-start.json", revylStartJSON)
	if rec := do(r.s, http.MethodPost, "/api/revyl", `{"platform":"android"}`); rec.Code != http.StatusAccepted {
		r.t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	r.waitFor("the device to start", func() bool {
		l := r.list()
		return len(l) == 1 && l[0].State == revylActive
	})
	return r.list()[0]
}

// A stop that fails leaves the device billing, so it must stay where a
// client can see it and stop it again.
func TestRevylAFailedStopKeepsTheDeviceForARetry(t *testing.T) {
	rig := revylServer(t)
	dev := rig.started()
	rig.file("device-stop.fail", "Error: network unreachable\n")
	if rec := do(rig.s, http.MethodDelete, "/api/revyl?target="+dev.Target, ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("failed stop = %d %s", rec.Code, rec.Body)
	}
	l := rig.list()
	if len(l) != 1 || l[0].State != revylActive || !strings.Contains(l[0].StopError, "network unreachable") {
		t.Fatalf("list after a failed stop = %+v", l)
	}
	if _, ok := targetRow(t, rig.s, dev.Target); !ok {
		t.Fatal("a failed stop dropped the target")
	}

	rig.remove("device-stop.fail")
	rig.file("device-stop.json", `{"stopped":true}`)
	if rec := do(rig.s, http.MethodDelete, "/api/revyl?target="+dev.Target, ""); rec.Code != http.StatusOK {
		t.Fatalf("retried stop = %d %s", rec.Code, rec.Body)
	}
	if l := rig.list(); len(l) != 0 {
		t.Errorf("list after the retry = %+v", l)
	}
	if _, ok := targetRow(t, rig.s, dev.Target); ok {
		t.Error("the target outlived the stop")
	}
}

// A client can wait on a stop: GET /api/revyl shows it until it ends, and
// a second stop meanwhile is refused rather than run twice.
func TestRevylAStopInProgressShowsAsStopping(t *testing.T) {
	rig := revylServer(t)
	dev := rig.started()
	rig.file("device-stop.json", `{"stopped":true}`)
	rig.file("device-stop.hold", "")
	answered := make(chan int, 1)
	go func() { answered <- do(rig.s, http.MethodDelete, "/api/revyl?target="+dev.Target, "").Code }()
	rig.waitFor("the row to show the stop", func() bool {
		l := rig.list()
		return len(l) == 1 && l[0].State == revylStopping
	})
	if rec := do(rig.s, http.MethodDelete, "/api/revyl?target="+dev.Target, ""); rec.Code != http.StatusConflict {
		t.Errorf("second stop = %d %s", rec.Code, rec.Body)
	}
	rig.remove("device-stop.hold")
	if code := <-answered; code != http.StatusOK {
		t.Fatalf("stop = %d", code)
	}
	if l := rig.list(); len(l) != 0 {
		t.Errorf("list after the stop = %+v", l)
	}
	if n := len(rig.argvMatching("device", "stop")); n != 1 {
		t.Errorf("ran device stop %d times", n)
	}
}

// Exit waits for a stop a client asked for, within its budget, rather
// than quitting with the CLI mid-stop.
func TestRevylExitWaitsForAStopInProgress(t *testing.T) {
	rig := revylServer(t)
	dev := rig.started()
	rig.file("device-stop.json", `{"stopped":true}`)
	rig.file("device-stop.hold", "")
	go func() { _ = do(rig.s, http.MethodDelete, "/api/revyl?target="+dev.Target, "") }()
	rig.waitFor("the stop to be running", func() bool { return rig.ran("device", "stop") != nil })

	exited := make(chan struct{})
	go func() {
		rig.rv.StopAll(context.Background())
		close(exited)
	}()
	select {
	case <-exited:
		t.Fatal("exit did not wait for the stop in progress")
	case <-time.After(200 * time.Millisecond):
	}
	rig.remove("device-stop.hold")
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("exit never returned")
	}
}

func (r *revylRig) argvMatching(prefix ...string) [][]string {
	var out [][]string
	for _, a := range r.argv() {
		if len(a) >= len(prefix) && reflect.DeepEqual(a[:len(prefix)], prefix) {
			out = append(out, a)
		}
	}
	return out
}
