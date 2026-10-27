package target

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The outputs below are what emulator-5554 (Android 16) printed, captured
// with `adb shell '<cmd>; echo exit=$?'`.
const (
	resolveSettings = "priority=0 preferredOrder=0 match=0x108000 specificIndex=-1 isDefault=true\n" +
		"com.android.settings/.Settings\n"
	resolveNone  = "No activity found\n"
	amStartCold  = "Starting: Intent { cmp=com.android.settings/.Settings }\nStatus: ok\nLaunchState: WARM\nActivity: com.android.settings/.homepage.SettingsHomepageActivity\nTotalTime: 654\nWaitTime: 657\nComplete\n"
	amStartFront = "Starting: Intent { cmp=com.android.settings/.Settings }\nWarning: Activity not started, intent has been delivered to currently running top-most instance.\nStatus: ok\nLaunchState: UNKNOWN (0)\nActivity: com.android.settings/.homepage.SettingsHomepageActivity\nTotalTime: 0\nWaitTime: 6\nComplete\n"
	amStartNope  = "Starting: Intent { cmp=com.android.settings/.Nope }\nError type 3\nError: Activity class {com.android.settings/com.android.settings.Nope} does not exist.\n"
)

const (
	resolveCmd = "cmd package resolve-activity --brief -c android.intent.category.LAUNCHER 'com.android.settings'"
	amCmd      = "am start -W -n 'com.android.settings/.Settings'"
)

func TestLaunchAndroid(t *testing.T) {
	for _, tc := range []struct {
		name     string
		activity string
		answers  map[string]string
		wantErr  error
		wantMsg  string
		wantAct  string
		wantCmds []string
	}{
		{
			name:     "launcher activity",
			answers:  map[string]string{resolveCmd: resolveSettings, amCmd: amStartCold},
			wantAct:  "com.android.settings/.homepage.SettingsHomepageActivity",
			wantCmds: []string{resolveCmd, amCmd},
		},
		{
			name:    "already in front is success",
			answers: map[string]string{resolveCmd: resolveSettings, amCmd: amStartFront},
			wantAct: "com.android.settings/.homepage.SettingsHomepageActivity",
		},
		{
			name: "not installed",
			answers: map[string]string{
				resolveCmd:                       resolveNone,
				"pm path 'com.android.settings'": "",
			},
			wantErr: ErrAppNotInstalled,
		},
		{
			name: "installed with no launcher activity",
			answers: map[string]string{
				resolveCmd:                       resolveNone,
				"pm path 'com.android.settings'": "package:/system/priv-app/Settings/Settings.apk\n",
			},
			wantErr: ErrNoLaunchActivity,
		},
		{
			name:     "explicit activity skips resolve",
			activity: ".Settings",
			answers:  map[string]string{amCmd: amStartCold},
			wantAct:  "com.android.settings/.homepage.SettingsHomepageActivity",
			wantCmds: []string{amCmd},
		},
		{
			// Older devices exit 0 after the Error line, so it decides.
			name:     "Error line with a zero exit",
			activity: ".Nope",
			answers:  map[string]string{"am start -W -n 'com.android.settings/.Nope'": amStartNope},
			wantErr:  ErrNoSuchActivity,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeShell{answers: tc.answers}
			res, err := launchAndroid(context.Background(), f.run, "com.android.settings", tc.activity)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantMsg != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantMsg)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if res.Activity != tc.wantAct {
					t.Errorf("Activity = %q, want %q", res.Activity, tc.wantAct)
				}
			}
			if tc.wantCmds != nil && strings.Join(f.calls, "\n") != strings.Join(tc.wantCmds, "\n") {
				t.Errorf("ran %q, want %q", f.calls, tc.wantCmds)
			}
		})
	}
}

// emulator-5554 exits 1 for a missing component, and the Error line is
// still what the caller is told.
func TestLaunchAndroidNonZeroExitNamesTheError(t *testing.T) {
	sh := func(ctx context.Context, args ...string) (string, error) {
		return amStartNope, errors.New("exit status 1")
	}
	_, err := launchAndroid(context.Background(), sh, "com.android.settings", ".Nope")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want the Error line", err)
	}
}

// Nothing a caller sends reaches the device's shell unless it passed its
// pattern check.
func TestLaunchAndroidRejectsNamesBeforeTheShell(t *testing.T) {
	for _, tc := range []struct{ pkg, activity string }{
		{"com.example; reboot", ""},
		{"settings", ""},
		{"com.example.app", ".Main; reboot"},
		{"com.example.app", "../Main"},
		{"com.example.app", "Main'"},
	} {
		f := &fakeShell{}
		if _, err := launchAndroid(context.Background(), f.run, tc.pkg, tc.activity); err == nil {
			t.Errorf("launch(%q, %q) succeeded", tc.pkg, tc.activity)
		}
		if len(f.calls) != 0 {
			t.Errorf("launch(%q, %q) ran %q", tc.pkg, tc.activity, f.calls)
		}
	}
}

func TestValidActivityName(t *testing.T) {
	for s, want := range map[string]bool{
		".MainActivity":                 true,
		"com.example.app.MainActivity":  true,
		".ui.Main$Inner":                true,
		"MainActivity":                  true,
		"":                              false,
		".":                             false,
		"..Main":                        false,
		".Main Activity":                false,
		"com.example/.Main":             false,
		".1Main":                        false,
		".Main\ncom.example/.Other":     false,
		"com.example.app.MainActivity'": false,
	} {
		if got := ValidActivityName(s); got != want {
			t.Errorf("ValidActivityName(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestParseResolveActivityTakesTheLastLine(t *testing.T) {
	if got, ok := parseResolveActivity(resolveSettings); !ok || got != "com.android.settings/.Settings" {
		t.Errorf("got %q, %v", got, ok)
	}
	for _, out := range []string{resolveNone, "", "Exception occurred while executing 'resolve-activity'"} {
		if got, ok := parseResolveActivity(out); ok {
			t.Errorf("parseResolveActivity(%q) = %q", out, got)
		}
	}
}

func TestSimulatorLaunchRefusesAnActivityAndABadBundle(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u"}, "")
	if _, err := s.Launch(context.Background(), "com.apple.Preferences", ".Main"); err == nil {
		t.Error("an activity on a simulator was accepted")
	}
	if _, err := s.Launch(context.Background(), "com.apple Preferences", ""); err == nil {
		t.Error("a bundle ID with a space was accepted")
	}
}

// The newest build-tools wins by number, and a directory without aapt2 or
// with a preview name is passed over.
func TestAAPT2BinaryPicksTheNewestBuildTools(t *testing.T) {
	sdk := t.TempDir()
	for _, v := range []string{"9.0.0", "36.0.0", "36.1.0", "37.0.0-rc1"} {
		dir := filepath.Join(sdk, "build-tools", v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "aapt2"), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(sdk, "build-tools", "40.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sdk, "build-tools", "36.1.0", "aapt2")
	if got := AAPT2Binary(sdk); got != want {
		t.Errorf("AAPT2Binary = %q, want %q", got, want)
	}
	if got := AAPT2Binary(""); got != "" {
		t.Errorf("AAPT2Binary(\"\") = %q", got)
	}
}

func TestAPKPackageWithoutAAPT2(t *testing.T) {
	if _, err := APKPackage(context.Background(), "", "x.apk"); err == nil {
		t.Error("no aapt2 read a package")
	}
}

func TestAppContainerMissingOnlyOnExitStatusTwo(t *testing.T) {
	run := func(code string) error { return exec.Command("sh", "-c", "exit "+code).Run() }
	if !appContainerMissing(run("2")) {
		t.Error("exit 2 is the not-installed answer")
	}
	for _, code := range []string{"0", "1", "149"} {
		if appContainerMissing(run(code)) {
			t.Errorf("exit %s read as not installed", code)
		}
	}
	if appContainerMissing(exec.Command("/nonexistent/xcrun").Run()) {
		t.Error("a binary that never ran read as not installed")
	}
}
