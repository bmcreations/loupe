package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// launchTarget is an androidTarget that can also launch, with a canned
// answer.
type launchTarget struct {
	androidTarget
	launchErr   error
	launched    []string
	gotActivity string
}

func (f *launchTarget) Launch(ctx context.Context, pkg, activity string) (*target.LaunchResult, error) {
	f.launched = append(f.launched, pkg)
	f.gotActivity = activity
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	return &target.LaunchResult{Activity: pkg + "/.MainActivity"}, nil
}

func (f *launchTarget) CheckLaunch(pkg, activity string) error {
	if !target.ValidPackageName(pkg) {
		return fmt.Errorf("%w: %q", target.ErrBadLaunchName, pkg)
	}
	return nil
}

func newLaunchServer(t *testing.T, f *launchTarget) *Server {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(f)
	return &Server{mgr: m, token: "secret"}
}

func serveOn(s *Server, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// fakeAAPT2 writes a script that answers `dump packagename` with name.
func fakeAAPT2(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "aapt2")
	script := "#!/bin/sh\necho " + name + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLaunchActionOpensThePackage(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	s := newLaunchServer(t, f)
	w := serveOn(s, http.MethodPost, "/api/input?target=dev1",
		`{"action":"launch","package":"com.example.app","activity":".Settings"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(f.launched) != 1 || f.launched[0] != "com.example.app" || f.gotActivity != ".Settings" {
		t.Errorf("launched %v activity %q", f.launched, f.gotActivity)
	}
}

func TestLaunchActionNeedsAPackage(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	w := serveOn(newLaunchServer(t, f), http.MethodPost, "/api/input?target=dev1", `{"action":"launch"}`, nil)
	if w.Code != http.StatusBadRequest || len(f.launched) != 0 {
		t.Fatalf("status = %d, launched %v; want 400 and nothing launched", w.Code, f.launched)
	}
}

// A missing app and an app with nothing to open are the request naming the
// wrong thing, each with a code to branch on; a bad name is a 400.
func TestLaunchActionErrorCodes(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{target.ErrAppNotInstalled, http.StatusNotFound, ErrCodeNotInstalled},
		{target.ErrNoLaunchActivity, http.StatusUnprocessableEntity, ErrCodeNoLaunchActivity},
		{target.ErrNoSuchActivity, http.StatusNotFound, ErrCodeNoSuchActivity},
		{target.ErrBadLaunchName, http.StatusBadRequest, ""},
		{target.ErrDeviceUnreachable, http.StatusBadGateway, ""},
	} {
		f := &launchTarget{androidTarget: androidTarget{id: "dev1"}, launchErr: c.err}
		w := serveOn(newLaunchServer(t, f), http.MethodPost, "/api/input?target=dev1",
			`{"action":"launch","package":"com.example.app"}`, nil)
		var body ErrorResponse
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != c.status || body.Code != c.code {
			t.Errorf("%v: status %d code %q; want %d %q", c.err, w.Code, body.Code, c.status, c.code)
		}
	}
}

func TestLaunchActionIs501WithoutTheCapability(t *testing.T) {
	w := serveAgent(t, &androidTarget{id: "dev1"}, http.MethodPost, "/api/input?target=dev1",
		strings.NewReader(`{"action":"launch","package":"com.example.app"}`))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501: %s", w.Code, w.Body)
	}
}

// heldLaunchSession opens dev1 with a viewer that has just touched it.
func heldLaunchSession(t *testing.T, f *launchTarget) (*Server, uint64) {
	t.Helper()
	s := newLaunchServer(t, f)
	sess, err := s.mgr.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	id, _ := sess.AddViewer()
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 0, PointerID: 1})
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 1, PointerID: 1})
	return s, id
}

// A launch changes what is on the screen, so it is held like a tap.
func TestLaunchActionIsHeldWhileAPersonDrives(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	s, id := heldLaunchSession(t, f)
	w := serveOn(s, http.MethodPost, "/api/input?target=dev1", `{"action":"launch","package":"com.example.app"}`,
		map[string]string{HeaderAgent: "claude"})
	if w.Code != http.StatusConflict || len(f.launched) != 0 {
		t.Fatalf("agent: status %d launched %v; want 409 and nothing launched", w.Code, f.launched)
	}
	w = serveOn(s, http.MethodPost, "/api/input?target=dev1", `{"action":"launch","package":"com.example.app"}`,
		map[string]string{HeaderViewer: strconv.FormatUint(id, 10)})
	if w.Code != http.StatusOK || len(f.launched) != 1 {
		t.Fatalf("viewer: status %d launched %v; want 200 and one launch", w.Code, f.launched)
	}
}

// A name that can never launch is a 400 under a hold too: a 409 would send
// the agent to retry something no retry fixes.
func TestLaunchActionChecksTheNameBeforeTheHold(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	s, _ := heldLaunchSession(t, f)
	w := serveOn(s, http.MethodPost, "/api/input?target=dev1", `{"action":"launch","package":"com.example;reboot"}`,
		map[string]string{HeaderAgent: "claude"})
	if w.Code != http.StatusBadRequest || len(f.launched) != 0 {
		t.Fatalf("status %d launched %v; want 400 and nothing launched: %s", w.Code, f.launched, w.Body)
	}
}

func installLaunch(t *testing.T, s *Server, query string, hdr map[string]string) InstallResponse {
	t.Helper()
	w := serveOn(s, http.MethodPost, "/api/install?target=dev1"+query, apk, hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200: %s", w.Code, w.Body)
	}
	var resp InstallResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestInstallReportsThePackageAndLaunchesIt(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	s := newLaunchServer(t, f)
	s.aapt2 = fakeAAPT2(t, "com.example.pr")
	resp := installLaunch(t, s, "&launch=1", nil)
	if resp.Package != "com.example.pr" || resp.Launch == nil || !resp.Launch.OK ||
		resp.Launch.Activity != "com.example.pr/.MainActivity" {
		t.Fatalf("response = %+v launch %+v", resp, resp.Launch)
	}
	if len(f.launched) != 1 || f.launched[0] != "com.example.pr" || f.gotActivity != "" {
		t.Errorf("launched %v activity %q", f.launched, f.gotActivity)
	}
}

func TestInstallWithoutLaunchStillNamesThePackage(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	s := newLaunchServer(t, f)
	s.aapt2 = fakeAAPT2(t, "com.example.pr")
	resp := installLaunch(t, s, "", nil)
	if resp.Package != "com.example.pr" || resp.Launch != nil || len(f.launched) != 0 {
		t.Fatalf("response = %+v, launched %v", resp, f.launched)
	}
}

// Without aapt2, ?package= names the app; with neither the install stands
// and the launch says why it did not run.
func TestInstallPackageFallback(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	s := newLaunchServer(t, f)
	resp := installLaunch(t, s, "&launch=1&package=com.example.given", nil)
	if resp.Package != "com.example.given" || resp.Launch == nil || !resp.Launch.OK {
		t.Fatalf("with ?package=: %+v launch %+v", resp, resp.Launch)
	}
	resp = installLaunch(t, s, "&launch=1", nil)
	if !resp.OK || resp.Package != "" || resp.PackageError == "" || resp.Launch == nil || resp.Launch.OK {
		t.Fatalf("with neither: %+v launch %+v", resp, resp.Launch)
	}
	if len(f.launched) != 1 {
		t.Errorf("launched %v; want only the ?package= one", f.launched)
	}
}

// A ?package= that disagrees with the APK is not launched: it would open
// some other app.
func TestInstallPackageMismatchDoesNotLaunch(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	s := newLaunchServer(t, f)
	s.aapt2 = fakeAAPT2(t, "com.example.pr")
	resp := installLaunch(t, s, "&launch=1&package=com.example.other", nil)
	if resp.Package != "com.example.pr" || resp.PackageError == "" || resp.Launch.OK || len(f.launched) != 0 {
		t.Fatalf("response = %+v launch %+v launched %v", resp, resp.Launch, f.launched)
	}
}

// The install is done whatever the launch says, so a launch that fails or
// is held off is reported inside a 200.
func TestInstallLaunchFailureKeepsTheInstall(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}, launchErr: target.ErrNoLaunchActivity}
	s := newLaunchServer(t, f)
	s.aapt2 = fakeAAPT2(t, "com.example.pr")
	resp := installLaunch(t, s, "&launch=1", nil)
	if !resp.OK || resp.Launch.OK || resp.Launch.Code != ErrCodeNoLaunchActivity {
		t.Fatalf("response = %+v launch %+v", resp, resp.Launch)
	}
}

func TestInstallLaunchIsHeldWhileAPersonDrives(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	s, _ := heldLaunchSession(t, f)
	s.aapt2 = fakeAAPT2(t, "com.example.pr")
	resp := installLaunch(t, s, "&launch=1", map[string]string{HeaderAgent: "claude"})
	if !resp.OK || resp.Launch.OK || resp.Launch.Code != ErrCodeHumanHeld || resp.Launch.RetryAfterMs <= 0 {
		t.Fatalf("response = %+v launch %+v", resp, resp.Launch)
	}
	if len(f.launched) != 0 {
		t.Errorf("launched %v while held", f.launched)
	}
}
