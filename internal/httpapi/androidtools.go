package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrCodeInstalling is the 409 of POST /api/android/cli while an install
// runs, whichever route started it.
const ErrCodeInstalling = "installing"

// platformToolsTimeout bounds `android sdk install platform-tools`, about
// 37 MB.
const platformToolsTimeout = 10 * time.Minute

// The steps of an install, in order.
const (
	stepCLI           = "cli"
	stepPlatformTools = "platform-tools"
)

// AndroidToolsConfig says how AndroidTools finds what it installs. Every func
// runs per call, so something installed while loupe runs is seen at once.
type AndroidToolsConfig struct {
	// Installer installs the CLI. Nil means it cannot, and a missing CLI is
	// then a 501.
	Installer *CLIInstaller
	// FindCLI locates the android CLI; "" means not installed.
	FindCLI func() string
	// SDK is the SDK root passed as --sdk, the same one AVDCreator passes.
	// Nil or "" lets the CLI choose, which is ~/Library/Android/sdk.
	SDK func() string
	// FindADB is the adb loupe would run, or "" when there is none. It must
	// resolve adb the way target.ADB does, so "installed" here means adb
	// calls work.
	FindADB func() string
	// ADBInstalled, if set, runs once the platform-tools step leaves adb
	// where FindADB sees it. It must not block.
	ADBInstalled func()
	Trace        func(map[string]any)
}

// AndroidTools installs what loupe needs for Android on a Mac with neither
// Android Studio nor an SDK: Google's android CLI, then adb through
// `android sdk install platform-tools`. The CLI fetches the emulator and a
// system image itself when it creates an AVD, so those are not installed
// here. POST /api/android/cli and POST /api/remote/cli share one, so two
// installs never run at once.
type AndroidTools struct {
	cfg AndroidToolsConfig
	// run executes the CLI; tests replace it.
	run func(ctx context.Context, cli string, args ...string) (stdout, stderr []byte, err error)

	mu sync.Mutex
	// step is the step running, or "" when no install is.
	step string
	// cliErr is why the last CLI install failed; err is why the last
	// install failed at either step.
	cliErr string
	err    string
}

// NewAndroidTools returns an installer owner for cfg.
func NewAndroidTools(cfg AndroidToolsConfig) *AndroidTools {
	return &AndroidTools{cfg: cfg, run: runCLI}
}

// SetAndroidTools enables /api/android/cli. Nil leaves it answering 404.
// Pass the same one to RemoteDevices.SetAndroidTools.
func (s *Server) SetAndroidTools(t *AndroidTools) { s.android = t }

// AndroidCLIResponse is GET /api/android/cli and POST's 200 and 202.
type AndroidCLIResponse struct {
	// Installed is true when the android CLI is found, at Path.
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	// CanInstall is true when POST can install the CLI. It can always
	// install adb once the CLI is there.
	CanInstall bool `json:"canInstall"`
	// Installing is true while an install runs, from this route or POST
	// /api/remote/cli; Step is "cli" or "platform-tools" then.
	Installing bool   `json:"installing"`
	Step       string `json:"step,omitempty"`
	// Error is why the last install failed.
	Error string `json:"error,omitempty"`
	// ADBInstalled is true when there is an adb for loupe to run, at
	// ADBPath: -adb, else adb on PATH.
	ADBInstalled bool   `json:"adbInstalled"`
	ADBPath      string `json:"adbPath,omitempty"`
}

func (t *AndroidTools) findCLI() string {
	if t.cfg.FindCLI == nil {
		return ""
	}
	return t.cfg.FindCLI()
}

func (t *AndroidTools) findADB() string {
	if t.cfg.FindADB == nil {
		return ""
	}
	return t.cfg.FindADB()
}

func (t *AndroidTools) canInstallCLI() bool { return t != nil && t.cfg.Installer != nil }

// cliState is what /api/remote/account reports: whether the CLI step runs,
// and why the last one failed.
func (t *AndroidTools) cliState() (installing bool, failure string) {
	if t == nil {
		return false, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.step == stepCLI, t.cliErr
}

func (t *AndroidTools) status() AndroidCLIResponse {
	t.mu.Lock()
	resp := AndroidCLIResponse{CanInstall: t.cfg.Installer != nil, Installing: t.step != "", Step: t.step, Error: t.err}
	t.mu.Unlock()
	resp.Path = t.findCLI()
	resp.Installed = resp.Path != ""
	resp.ADBPath = t.findADB()
	resp.ADBInstalled = resp.ADBPath != ""
	return resp
}

var errInstalling = errors.New("an install of the Android tools is already running")

// begin claims the one install slot.
func (t *AndroidTools) begin(step string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.step != "" {
		return errInstalling
	}
	t.step, t.cliErr, t.err = step, "", ""
	return nil
}

func (t *AndroidTools) setStep(step string) {
	t.mu.Lock()
	t.step = step
	t.mu.Unlock()
}

// finish frees the slot and keeps why the install failed, if it did.
func (t *AndroidTools) finish(step string, err error) {
	t.mu.Lock()
	t.step = ""
	if err != nil {
		t.err = err.Error()
		if step == stepCLI {
			t.cliErr = t.err
		}
	}
	t.mu.Unlock()
}

func (t *AndroidTools) record(rec map[string]any) {
	if t.cfg.Trace != nil {
		rec["kind"] = "android"
		t.cfg.Trace(rec)
	}
}

// installCLI starts a CLI install and returns at once. It does nothing while
// another install runs: POST /api/remote/cli then reports that one.
func (t *AndroidTools) installCLI() {
	if t.cfg.Installer == nil || t.begin(stepCLI) != nil {
		return
	}
	go func() {
		err := t.cliStep()
		t.finish(stepCLI, err)
	}()
}

// installAll starts whichever of the CLI and adb are missing, or says why it
// cannot. started is false when nothing was missing.
func (t *AndroidTools) installAll() (started bool, err error) {
	needCLI, needADB := t.findCLI() == "", t.findADB() == ""
	if !needCLI && !needADB {
		return false, nil
	}
	first := stepPlatformTools
	if needCLI {
		if t.cfg.Installer == nil {
			return false, errCannotInstallCLI
		}
		first = stepCLI
	}
	if err := t.begin(first); err != nil {
		return false, err
	}
	go func() {
		step := first
		var err error
		if step == stepCLI {
			err = t.cliStep()
		}
		if err == nil && t.findADB() == "" {
			step = stepPlatformTools
			t.setStep(step)
			err = t.platformToolsStep()
			if err == nil && t.cfg.ADBInstalled != nil {
				t.cfg.ADBInstalled()
			}
		}
		t.finish(step, err)
	}()
	return true, nil
}

var errCannotInstallCLI = errors.New("this server cannot install the android CLI")

func (t *AndroidTools) cliStep() error {
	ctx, cancel := context.WithTimeout(context.Background(), cliInstallTimeout)
	defer cancel()
	how, err := t.cfg.Installer.Install(ctx)
	if err == nil && t.findCLI() == "" {
		err = fmt.Errorf("installed the android CLI by %s, but it is not where this server looks", how)
	}
	rec := map[string]any{"action": "install-cli", "how": how}
	if err != nil {
		rec["error"] = err.Error()
	}
	t.record(rec)
	return err
}

// platformToolsStep runs `android sdk install platform-tools`. The verdict is
// adb being there after it with no "Error:" line, never the exit status: the
// CLI exits 0 after printing one.
func (t *AndroidTools) platformToolsStep() error {
	start := time.Now()
	err := t.installPlatformTools()
	rec := map[string]any{"action": "install-platform-tools", "tookMs": time.Since(start).Milliseconds()}
	if err != nil {
		rec["error"] = err.Error()
	}
	t.record(rec)
	return err
}

func (t *AndroidTools) installPlatformTools() error {
	cli := t.findCLI()
	if cli == "" {
		return errNoCLI
	}
	ctx, cancel := context.WithTimeout(context.Background(), platformToolsTimeout)
	defer cancel()
	args := append(sdkArgs(t.cfg.SDK), "sdk", "install", "platform-tools")
	out, stderr, runErr := t.run(ctx, cli, args...)
	if failure := cliErrorLine(out, stderr); failure != "" {
		return errors.New(failure)
	}
	if t.findADB() != "" {
		return nil
	}
	// Installed into the SDK, but loupe runs adb from -adb or PATH, and
	// neither reaches it. Handset puts <sdk>/platform-tools on loupe's PATH
	// up front, so this is a loupe started some other way.
	if t.cfg.SDK != nil {
		if root := t.cfg.SDK(); root != "" {
			p := filepath.Join(root, "platform-tools", "adb")
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return fmt.Errorf("adb is installed at %s, but this server runs adb from its PATH, which does not reach it; restart loupe with -adb %s", p, p)
			}
		}
	}
	failure := strings.TrimSpace(string(stderr))
	if failure == "" && runErr != nil {
		failure = runErr.Error()
	}
	if failure == "" {
		failure = "the android CLI finished without installing adb"
	}
	return errors.New(failure)
}

// handleAndroidCLI is GET and POST /api/android/cli. It sits beside
// /api/avds rather than under /api/remote: emulators need the CLI and adb
// with no Firebase account at all.
func (s *Server) handleAndroidCLI(w http.ResponseWriter, r *http.Request) {
	t := s.android
	if t == nil {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "this server cannot install the Android tools"})
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, t.status())
		return
	}
	started, err := t.installAll()
	switch {
	case errors.Is(err, errInstalling):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: ErrCodeInstalling})
	case errors.Is(err, errCannotInstallCLI):
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: err.Error()})
	case started:
		writeJSON(w, http.StatusAccepted, t.status())
	default:
		writeJSON(w, http.StatusOK, t.status())
	}
}
