package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

// uninstallTarget is an androidTarget that also removes packages.
type uninstallTarget struct {
	androidTarget
	removed []string
	err     error
}

func (f *uninstallTarget) Uninstall(ctx context.Context, pkg string) (*target.InstallResult, error) {
	f.removed = append(f.removed, pkg)
	if f.err != nil {
		return nil, f.err
	}
	return &target.InstallResult{Output: "Success"}, nil
}

// appInstallTarget stands in for an iPhone or simulator: an AppInstaller with
// no Uninstall. (install_ios_test.go is named like a GOOS=ios file, so Go
// does not build its fakes here.)
type appInstallTarget struct {
	stubTarget
	id  string
	got bool
}

func (f *appInstallTarget) ID() string { return f.id }

func (f *appInstallTarget) InstallApp(ctx context.Context, archive string) (*target.AppInstallResult, error) {
	f.got = true
	return &target.AppInstallResult{BundleID: "dev.example.app"}, nil
}

const iosArchive = "PK\x03\x04an ipa"

func newUninstallServer(t *testing.T, f *uninstallTarget) *Server {
	t.Helper()
	s := newLaunchServer(t, &launchTarget{androidTarget: f.androidTarget})
	m := NewManager(target.Options{})
	m.Register(f)
	s.mgr = m
	s.claims = newClaimBook(nil)
	return s
}

func TestInstallDowngradeReachesTheTarget(t *testing.T) {
	f := &androidTarget{id: "dev1"}
	serveAgent(t, f, http.MethodPost, "/api/install?target=dev1", strings.NewReader(apk))
	if f.gotOpts.Downgrade {
		t.Error("Downgrade set without ?downgrade=1")
	}
	serveAgent(t, f, http.MethodPost, "/api/install?target=dev1&downgrade=1", strings.NewReader(apk))
	if !f.gotOpts.Downgrade {
		t.Error("?downgrade=1 did not reach the target")
	}
}

func installRefusedHint(t *testing.T, query string) InstallResponse {
	t.Helper()
	f := &androidTarget{id: "dev1", installErr: &target.InstallError{
		Code: "INSTALL_FAILED_VERSION_DOWNGRADE", Reason: "Downgrade detected",
		Output: "Failure [INSTALL_FAILED_VERSION_DOWNGRADE: Downgrade detected]",
	}}
	w := serveAgent(t, f, http.MethodPost, "/api/install?target=dev1"+query, strings.NewReader(apk))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp InstallResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestVersionDowngradeRefusalSaysHowToGetPastIt(t *testing.T) {
	resp := installRefusedHint(t, "")
	for _, want := range []string{"downgrade=1", "DELETE /api/install"} {
		if !strings.Contains(resp.Hint, want) {
			t.Errorf("hint %q does not mention %q", resp.Hint, want)
		}
	}
	if resp.Code != "INSTALL_FAILED_VERSION_DOWNGRADE" || resp.Output == "" {
		t.Errorf("response = %+v", resp)
	}
}

// With -d already sent, the app is not debuggable: the only way left is the
// uninstall, so the hint must not send the caller round the same loop.
func TestVersionDowngradeRefusedAfterDowngradeSaysUninstall(t *testing.T) {
	resp := installRefusedHint(t, "&downgrade=1")
	if !strings.Contains(resp.Hint, "DELETE /api/install") || !strings.Contains(resp.Hint, "not debuggable") {
		t.Errorf("hint = %q", resp.Hint)
	}
	if strings.Contains(resp.Hint, "Retry with downgrade=1") {
		t.Errorf("hint = %q; it tells the caller to retry what they sent", resp.Hint)
	}
}

func TestOtherRefusalsHaveNoHint(t *testing.T) {
	if h := installHint("INSTALL_FAILED_TEST_ONLY", false); h != "" {
		t.Errorf("hint = %q", h)
	}
}

func TestDowngradeOnIOSIsA400BeforeTheBodyIsRead(t *testing.T) {
	f := &appInstallTarget{id: "ios:phone"}
	w := serveAgent(t, f, http.MethodPost, "/api/install?target=ios:phone&downgrade=1", strings.NewReader(iosArchive))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"unsupported"`) {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if f.got {
		t.Error("the archive was staged and installed")
	}
}

func TestUninstallRemovesThePackage(t *testing.T) {
	f := &uninstallTarget{androidTarget: androidTarget{id: "dev1"}}
	s := newUninstallServer(t, f)
	w := serveOn(s, http.MethodDelete, "/api/install?target=dev1&package=com.example.app", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp UninstallResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Package != "com.example.app" || len(f.removed) != 1 || f.removed[0] != "com.example.app" {
		t.Errorf("response = %+v removed %v", resp, f.removed)
	}
}

func TestUninstallRefusesABadPackageName(t *testing.T) {
	for _, pkg := range []string{"com.x;rm -rf /", "", "$(reboot)", "com.x com.y"} {
		f := &uninstallTarget{androidTarget: androidTarget{id: "dev1"}}
		s := newUninstallServer(t, f)
		w := serveOn(s, http.MethodDelete, "/api/install?target=dev1&package="+url.QueryEscape(pkg), "", nil)
		if w.Code != http.StatusBadRequest || len(f.removed) != 0 || !strings.Contains(w.Body.String(), "package name") {
			t.Errorf("package %q: status %d removed %v; want 400 and nothing removed", pkg, w.Code, f.removed)
		}
	}
}

func TestUninstallIsRefusedForAnotherSessionsClaim(t *testing.T) {
	f := &uninstallTarget{androidTarget: androidTarget{id: "dev1"}}
	s := newUninstallServer(t, f)
	s.claims.take("dev1", "claude", "s1", "")
	q := "/api/install?target=dev1&package=com.example.app"
	for name, hdr := range map[string]map[string]string{
		"other session": {HeaderAgent: "claude", HeaderSession: "s2"},
		"no headers":    nil,
	} {
		w := serveOn(s, http.MethodDelete, q, "", hdr)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), ErrCodeClaimed) || len(f.removed) != 0 {
			t.Errorf("%s: status %d removed %v: %s; want 409 claimedBy and nothing removed", name, w.Code, f.removed, w.Body)
		}
	}
	w := serveOn(s, http.MethodDelete, q, "", map[string]string{HeaderAgent: "claude", HeaderSession: "s1"})
	if w.Code != http.StatusOK || len(f.removed) != 1 {
		t.Errorf("holder: status %d removed %v; want 200", w.Code, f.removed)
	}
}

func TestUninstallIsRefusedWhenHandsOff(t *testing.T) {
	f := &uninstallTarget{androidTarget: androidTarget{id: "dev1"}}
	s := newUninstallServer(t, f)
	s.claims.handsOff["dev1"] = true
	w := serveOn(s, http.MethodDelete, "/api/install?target=dev1&package=com.example.app", "", nil)
	if w.Code != http.StatusConflict || len(f.removed) != 0 {
		t.Errorf("status %d removed %v: %s", w.Code, f.removed, w.Body)
	}
}

func TestUninstallErrorsMapToStatuses(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{target.ErrAppNotInstalled, http.StatusNotFound},
		{&target.InstallError{Code: "DELETE_FAILED_DEVICE_POLICY_MANAGER", Output: "Failure [DELETE_FAILED_DEVICE_POLICY_MANAGER]"}, http.StatusUnprocessableEntity},
		{context.DeadlineExceeded, http.StatusBadGateway},
	} {
		f := &uninstallTarget{androidTarget: androidTarget{id: "dev1"}, err: c.err}
		s := newUninstallServer(t, f)
		w := serveOn(s, http.MethodDelete, "/api/install?target=dev1&package=com.example.app", "", nil)
		if w.Code != c.want {
			t.Errorf("%v: status %d; want %d: %s", c.err, w.Code, c.want, w.Body)
		}
	}
}

func TestUninstallOnIOSIsA400(t *testing.T) {
	f := &appInstallTarget{id: "ios:phone"}
	w := serveAgent(t, f, http.MethodDelete, "/api/install?target=ios:phone&package=dev.bmcreations.handset", nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"unsupported"`) {
		t.Errorf("status = %d: %s", w.Code, w.Body)
	}
}

func TestUninstallIsTracedLikeAnInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &uninstallTarget{androidTarget: androidTarget{id: "dev1"}}
	s := newUninstallServer(t, f)
	s.trace = tw
	serveOn(s, http.MethodDelete, "/api/install?target=dev1&package=com.example.app", "", map[string]string{HeaderAgent: "claude"})
	tw.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["action"] == "uninstall" {
			rec = m
		}
	}
	if rec == nil || rec["package"] != "com.example.app" || rec["outcome"] != "ok" || rec["target"] != "dev1" || rec["agent"] != "claude" {
		t.Errorf("trace = %s; want an uninstall record naming target, package, agent and outcome", b)
	}
}
