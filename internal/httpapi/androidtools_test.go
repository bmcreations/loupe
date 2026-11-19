package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// toolsEnv is a Mac with no CLI and no adb. The installer downloads
// fakeCLIBin into its private path; the fake CLI's `sdk install
// platform-tools` writes adb to adbPath, unless sdkInstall says otherwise.
// Nothing here execs a real CLI or brew.
type toolsEnv struct {
	s      *Server
	rd     *RemoteDevices
	tools  *AndroidTools
	in     *CLIInstaller
	adb    string
	casks  *[]string
	kicks  atomic.Int32
	mu     sync.Mutex
	calls  [][]string
	result func(args []string) (stdout string)
}

func newToolsEnv(t *testing.T) *toolsEnv {
	t.Helper()
	in, asked := testInstaller(t, caskFor(t, fakeCLIBin), fakeCLIBin)
	e := &toolsEnv{in: in, adb: filepath.Join(t.TempDir(), "adb"), casks: asked}
	e.tools = NewAndroidTools(AndroidToolsConfig{
		Installer:    in,
		FindCLI:      func() string { return fileOrEmpty(in.PrivatePath) },
		SDK:          func() string { return "/sdk" },
		FindADB:      func() string { return fileOrEmpty(e.adb) },
		ADBInstalled: func() { e.kicks.Add(1) },
	})
	e.tools.run = func(_ context.Context, cli string, args ...string) ([]byte, []byte, error) {
		e.mu.Lock()
		e.calls = append(e.calls, append([]string{cli}, args...))
		result := e.result
		e.mu.Unlock()
		if result != nil {
			return []byte(result(args)), nil, nil
		}
		_ = os.WriteFile(e.adb, []byte("adb"), 0o755)
		return nil, nil, nil
	}
	e.rd = NewRemoteDevices(e.tools.findCLI, func(string, string) {}, nil)
	e.rd.SetAndroidTools(e.tools)
	e.s = &Server{mgr: NewManager(target.Options{})}
	e.s.SetAndroidTools(e.tools)
	e.s.SetRemoteDevices(e.rd)
	return e
}

// installs counts CLI installs: each looks up the cask record once.
func (e *toolsEnv) installs() int {
	n := 0
	for _, u := range *e.casks {
		if u == cliCaskURL {
			n++
		}
	}
	return n
}

func fileOrEmpty(p string) string {
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

// holdFetch makes the installer wait for release before each fetch.
func (e *toolsEnv) holdFetch() (release func()) {
	ch := make(chan struct{})
	fetch := e.in.fetch
	e.in.fetch = func(ctx context.Context, url string) ([]byte, error) {
		<-ch
		return fetch(ctx, url)
	}
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func androidStatus(t *testing.T, s *Server) AndroidCLIResponse {
	t.Helper()
	rec := do(s, http.MethodGet, "/api/android/cli", "")
	var st AndroidCLIResponse
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func remoteAccount(t *testing.T, s *Server) RemoteAccountResponse {
	t.Helper()
	var acct RemoteAccountResponse
	if err := json.Unmarshal(do(s, http.MethodGet, "/api/remote/account", "").Body.Bytes(), &acct); err != nil {
		t.Fatal(err)
	}
	return acct
}

func TestAndroidCLIRouteInstallsTheCLIThenADB(t *testing.T) {
	e := newToolsEnv(t)
	if st := androidStatus(t, e.s); st.Installed || st.ADBInstalled || !st.CanInstall || st.Installing {
		t.Fatalf("before = %+v", st)
	}
	release := e.holdFetch()
	rec := do(e.s, http.MethodPost, "/api/android/cli", "")
	var st AndroidCLIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); rec.Code != http.StatusAccepted || err != nil || !st.Installing || st.Step != stepCLI {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	release()
	waitFor(t, func() bool { return !androidStatus(t, e.s).Installing })
	st = androidStatus(t, e.s)
	if !st.Installed || st.Path != e.in.PrivatePath || !st.ADBInstalled || st.ADBPath != e.adb || st.Error != "" {
		t.Fatalf("after = %+v", st)
	}
	want := []string{e.in.PrivatePath, "--sdk=/sdk", "sdk", "install", "platform-tools"}
	if len(e.calls) != 1 || strings.Join(e.calls[0], " ") != strings.Join(want, " ") {
		t.Errorf("CLI calls = %q, want one %q", e.calls, want)
	}
	// The device watcher is told adb is there, so it need not wait out
	// its backoff.
	if n := e.kicks.Load(); n != 1 {
		t.Errorf("ADBInstalled ran %d times, want 1", n)
	}
	// Nothing missing: 200, and nothing runs.
	if rec := do(e.s, http.MethodPost, "/api/android/cli", ""); rec.Code != http.StatusOK {
		t.Errorf("POST with both installed = %d %s", rec.Code, rec.Body)
	}
	if len(e.calls) != 1 || e.installs() != 1 {
		t.Errorf("second POST ran something: calls %q, installs %d", e.calls, e.installs())
	}
}

func TestAndroidCLIRouteInstallsOnlyADBWhenTheCLIIsThere(t *testing.T) {
	e := newToolsEnv(t)
	_ = os.MkdirAll(filepath.Dir(e.in.PrivatePath), 0o755)
	_ = os.WriteFile(e.in.PrivatePath, fakeCLIBin, 0o755)
	e.in.fetch = func(context.Context, string) ([]byte, error) {
		t.Error("the CLI was installed again")
		return nil, nil
	}
	if rec := do(e.s, http.MethodPost, "/api/android/cli", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	waitFor(t, func() bool { return !androidStatus(t, e.s).Installing })
	if st := androidStatus(t, e.s); !st.ADBInstalled || st.Error != "" {
		t.Errorf("after = %+v", st)
	}
}

func TestAndroidCLIRouteIs409WhileAnyInstallRuns(t *testing.T) {
	e := newToolsEnv(t)
	release := e.holdFetch()
	defer release()
	if rec := do(e.s, http.MethodPost, "/api/android/cli", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("first POST = %d %s", rec.Code, rec.Body)
	}
	rec := do(e.s, http.MethodPost, "/api/android/cli", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"installing"`) {
		t.Errorf("second POST = %d %s", rec.Code, rec.Body)
	}
	// The remote flow sees the same install and does not start another.
	if rec := do(e.s, http.MethodPost, "/api/remote/cli", ""); rec.Code != http.StatusAccepted {
		t.Errorf("remote POST = %d %s", rec.Code, rec.Body)
	}
	if acct := remoteAccount(t, e.s); !acct.CLIInstalling || acct.CLIInstalled {
		t.Errorf("remote account mid-install = %+v", acct)
	}
	release()
	waitFor(t, func() bool { return !androidStatus(t, e.s).Installing })
	if n := e.installs(); n != 1 {
		t.Errorf("%d CLI installs, want 1", n)
	}
	if acct := remoteAccount(t, e.s); !acct.CLIInstalled || acct.CLIInstalling || acct.CLIInstallError != "" {
		t.Errorf("remote account after = %+v", acct)
	}
}

func TestAndroidCLIRouteIs409WhileTheRemoteFlowInstalls(t *testing.T) {
	e := newToolsEnv(t)
	release := e.holdFetch()
	defer release()
	if rec := do(e.s, http.MethodPost, "/api/remote/cli", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("remote POST = %d %s", rec.Code, rec.Body)
	}
	if st := androidStatus(t, e.s); !st.Installing || st.Step != stepCLI {
		t.Errorf("GET during the remote install = %+v", st)
	}
	if rec := do(e.s, http.MethodPost, "/api/android/cli", ""); rec.Code != http.StatusConflict {
		t.Errorf("POST during the remote install = %d %s", rec.Code, rec.Body)
	}
	release()
	waitFor(t, func() bool { return !androidStatus(t, e.s).Installing })
	// The remote flow installs the CLI only; adb is left to this route.
	if st := androidStatus(t, e.s); !st.Installed || st.ADBInstalled || len(e.calls) != 0 {
		t.Errorf("after the remote install = %+v, CLI calls %q", st, e.calls)
	}
}

func TestAndroidCLIRouteFailsOnAnErrorLineDespiteExitZero(t *testing.T) {
	e := newToolsEnv(t)
	_ = os.MkdirAll(filepath.Dir(e.in.PrivatePath), 0o755)
	_ = os.WriteFile(e.in.PrivatePath, fakeCLIBin, 0o755)
	e.result = func([]string) string { return "Downloading\nError: Failed to fetch the repository\n" }
	do(e.s, http.MethodPost, "/api/android/cli", "")
	waitFor(t, func() bool { return !androidStatus(t, e.s).Installing })
	st := androidStatus(t, e.s)
	if st.ADBInstalled || st.Error != "Failed to fetch the repository" {
		t.Errorf("after = %+v", st)
	}
	// A CLI failure at the adb step is not a CLI install failure.
	if acct := remoteAccount(t, e.s); acct.CLIInstallError != "" {
		t.Errorf("remote account = %+v", acct)
	}
	if n := e.kicks.Load(); n != 0 {
		t.Errorf("ADBInstalled ran %d times after a failed install", n)
	}
}

func TestAndroidCLIRouteSaysWhenADBLandsOffPATH(t *testing.T) {
	e := newToolsEnv(t)
	sdk := t.TempDir()
	e.tools.cfg.SDK = func() string { return sdk }
	_ = os.MkdirAll(filepath.Dir(e.in.PrivatePath), 0o755)
	_ = os.WriteFile(e.in.PrivatePath, fakeCLIBin, 0o755)
	inSDK := filepath.Join(sdk, "platform-tools", "adb")
	e.result = func([]string) string {
		_ = os.MkdirAll(filepath.Dir(inSDK), 0o755)
		_ = os.WriteFile(inSDK, []byte("adb"), 0o755)
		return ""
	}
	do(e.s, http.MethodPost, "/api/android/cli", "")
	waitFor(t, func() bool { return !androidStatus(t, e.s).Installing })
	if st := androidStatus(t, e.s); st.ADBInstalled || !strings.Contains(st.Error, "-adb "+inSDK) {
		t.Errorf("after = %+v", st)
	}
}

func TestAndroidCLIRouteWithoutAnInstaller(t *testing.T) {
	if rec := do(&Server{mgr: NewManager(target.Options{})}, http.MethodGet, "/api/android/cli", ""); rec.Code != http.StatusNotFound {
		t.Errorf("no tools = %d", rec.Code)
	}
	e := newToolsEnv(t)
	e.tools.cfg.Installer = nil
	if st := androidStatus(t, e.s); st.CanInstall {
		t.Errorf("GET = %+v", st)
	}
	if rec := do(e.s, http.MethodPost, "/api/android/cli", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("CLI missing = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.s, http.MethodPost, "/api/remote/cli", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("remote CLI missing = %d %s", rec.Code, rec.Body)
	}
	// With the CLI there, adb needs no installer.
	_ = os.MkdirAll(filepath.Dir(e.in.PrivatePath), 0o755)
	_ = os.WriteFile(e.in.PrivatePath, fakeCLIBin, 0o755)
	if rec := do(e.s, http.MethodPost, "/api/android/cli", ""); rec.Code != http.StatusAccepted {
		t.Errorf("adb missing = %d %s", rec.Code, rec.Body)
	}
	waitFor(t, func() bool { return !androidStatus(t, e.s).Installing })
}
