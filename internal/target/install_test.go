package target

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The outputs below are adb's own, from adb 35 against emulator-5554
// (streamed install) and from the older one-line form a device's package
// manager prints on its own line.
func TestParseInstall(t *testing.T) {
	exit1 := exec.Command("sh", "-c", "exit 1").Run()
	cases := []struct {
		name     string
		out      string
		runErr   error
		wantOK   bool
		wantCode string
		wantWhy  string
	}{
		{name: "streamed success", out: "Performing Streamed Install\nSuccess\n", wantOK: true},
		{
			name:     "adb 35 failure exits nonzero",
			out:      "Performing Streamed Install\nadb: failed to install /tmp/x.apk: Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: Existing package com.example signatures do not match newer version; ignoring!]\n",
			runErr:   exit1,
			wantCode: "INSTALL_FAILED_UPDATE_INCOMPATIBLE",
			wantWhy:  "Existing package com.example signatures do not match newer version; ignoring!",
		},
		{
			// The case the exit status hides: adb says it failed and exits 0.
			name:     "failure with exit 0",
			out:      "Performing Streamed Install\nFailure [INSTALL_PARSE_FAILED_NOT_APK]\n",
			wantCode: "INSTALL_PARSE_FAILED_NOT_APK",
		},
		{name: "exit 0 with neither line", out: "Performing Streamed Install\n"},
		{name: "adb itself failed", out: "adb: device 'emulator-5560' not found\n", runErr: exit1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := parseInstall(c.out, c.runErr)
			if c.wantOK {
				if err != nil || res == nil {
					t.Fatalf("parseInstall = %v, %v; want success", res, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseInstall = %+v, nil; want an error", res)
			}
			ie, refused := IsInstallRefused(err)
			if c.wantCode == "" {
				if refused {
					t.Fatalf("err = %v reads as a refusal; want adb's own failure", err)
				}
				return
			}
			if !refused {
				t.Fatalf("err = %v; want an *InstallError", err)
			}
			if ie.Code != c.wantCode || ie.Reason != c.wantWhy {
				t.Errorf("code, reason = %q, %q; want %q, %q", ie.Code, ie.Reason, c.wantCode, c.wantWhy)
			}
			if ie.Output == "" {
				t.Error("Output is empty; want adb's text kept for the caller")
			}
		})
	}
}

// fakeADB writes an adb stand-in that records its argv, one per line, and
// runs body.
func fakeADB(t *testing.T, body string) (path, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	path = filepath.Join(dir, "adb")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argvFile + "'\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argvFile
}

func readArgv(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestInstallRunsInstallReplaceOnOneAPK(t *testing.T) {
	adb, argv := fakeADB(t, `echo Success`)
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "emulator-5554", State: "device"}, "", "")

	res, err := tgt.Install(context.Background(), []string{"/stage/0.apk"}, InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Output != "Success" {
		t.Errorf("Output = %q", res.Output)
	}
	if got := readArgv(t, argv); len(got) != 1 || got[0] != "-s emulator-5554 install -r /stage/0.apk" {
		t.Fatalf("adb argv = %q; want -s <serial> install -r <file>", got)
	}
}

// A base and its splits go to install-multiple as one package; installing
// the base alone is refused with INSTALL_FAILED_MISSING_SPLIT.
func TestInstallRunsInstallMultipleOnSplits(t *testing.T) {
	adb, argv := fakeADB(t, `echo Success`)
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")

	apks := []string{"/stage/0.apk", "/stage/1.apk", "/stage/2.apk"}
	if _, err := tgt.Install(context.Background(), apks, InstallOptions{AllowTest: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := readArgv(t, argv); len(got) != 1 || got[0] != "-s s install-multiple -r -t /stage/0.apk /stage/1.apk /stage/2.apk" {
		t.Fatalf("adb argv = %q; want install-multiple -r -t and every file", got)
	}
}

func TestInstallWithNoAPKRunsNothing(t *testing.T) {
	tgt := NewADBTarget(ADB{Path: "/nonexistent/adb"}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	if _, err := tgt.Install(context.Background(), nil, InstallOptions{}); err == nil {
		t.Fatal("Install(nil) succeeded")
	}
}

func TestInstallOnADeviceNotReadyRunsNothing(t *testing.T) {
	tgt := NewADBTarget(ADB{Path: "/nonexistent/adb"}, DeviceInfo{Serial: "s", State: "offline"}, "", "")
	if _, err := tgt.Install(context.Background(), []string{"/stage/0.apk"}, InstallOptions{}); !errors.Is(err, ErrDeviceUnreachable) {
		t.Fatalf("err = %v; want ErrDeviceUnreachable", err)
	}
}

func TestLogcatArgs(t *testing.T) {
	since := time.UnixMilli(1790607053042)
	cases := []struct {
		name string
		q    LogcatQuery
		pid  int
		want string
	}{
		{"tail", LogcatQuery{Lines: 500}, 0, "logcat -v threadtime -t 500"},
		{"tail one pid", LogcatQuery{Lines: 10}, 959, "logcat -v threadtime -t 10 --pid=959"},
		// -T does not imply -d, so a dump since a time has to say it.
		{"since", LogcatQuery{Since: since, Lines: 10}, 0, "logcat -v threadtime -d -T 1790607053.042"},
		{"follow", LogcatQuery{Follow: true, Lines: 20}, 0, "logcat -v threadtime -T 20"},
		{"follow since", LogcatQuery{Follow: true, Since: since}, 7, "logcat -v threadtime -T 1790607053.042 --pid=7"},
	}
	for _, c := range cases {
		if got := strings.Join(logcatArgs(c.q, c.pid), " "); got != c.want {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
}

func TestValidPackageName(t *testing.T) {
	for _, ok := range []string{"com.android.settings", "dev.bmcreations.loupe_test", "a.b"} {
		if !ValidPackageName(ok) {
			t.Errorf("ValidPackageName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "settings", "com.x;reboot", "com.x' ; id '", "com..x", ".com.x", "com.1x", "com.x -a"} {
		if ValidPackageName(bad) {
			t.Errorf("ValidPackageName(%q) = true", bad)
		}
	}
}

// A package goes to pidof through adb shell, which hands its arguments to the
// device's sh unescaped, so it must arrive quoted.
func TestLogcatPackageIsQuotedForPidof(t *testing.T) {
	adb, argv := fakeADB(t, `case "$3" in shell) echo '4242 4243';; exec-out) echo 'line';; esac`)
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")

	lc, err := tgt.Logcat(context.Background(), LogcatQuery{Package: "com.example.app", Lines: 5})
	if err != nil {
		t.Fatalf("Logcat: %v", err)
	}
	body, _ := io.ReadAll(lc)
	if err := lc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if lc.PID != 4242 || lc.Package != "com.example.app" || string(body) != "line\n" {
		t.Errorf("Logcat = pid %d package %q body %q", lc.PID, lc.Package, body)
	}
	got := readArgv(t, argv)
	want := []string{
		"-s s shell pidof 'com.example.app'",
		"-s s exec-out logcat -v threadtime -t 5 --pid=4242",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("adb argv =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLogcatPackageNotRunning(t *testing.T) {
	adb, _ := fakeADB(t, `exit 1`)
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	if _, err := tgt.Logcat(context.Background(), LogcatQuery{Package: "com.example.app"}); !errors.Is(err, ErrAppNotRunning) {
		t.Fatalf("err = %v; want ErrAppNotRunning", err)
	}
}

// Closing a follow before it ends is how every follow ends, and must not
// read as adb failing.
func TestExecOutStreamClosedEarlyIsNotAnError(t *testing.T) {
	adb, _ := fakeADB(t, `echo first; exec sleep 30`)
	rc, err := ADB{Path: adb}.ExecOutStream(context.Background(), "s", "logcat")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(rc, buf); err != nil || string(buf) != "first\n" {
		t.Fatalf("read %q, %v", buf, err)
	}
	began := time.Now()
	if err := rc.Close(); err != nil {
		t.Errorf("Close = %v; want nil for a stream stopped on purpose", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Error("Close waited for the command instead of stopping it")
	}
}

// The capabilities follow Inspector's shape: a target without one answers
// 501. Install is Android's alone; a simulator has a log, synthetic none.
func TestInstallAndLogcatCapabilities(t *testing.T) {
	var (
		_ Installer = (*ADBTarget)(nil)
		_ Logcatter = (*ADBTarget)(nil)
		_ Logcatter = (*Simulator)(nil)
	)
	for name, tgt := range map[string]Target{
		"synthetic": &Synthetic{},
		"simulator": NewSimulator(SimInfo{UDID: "u"}, ""),
	} {
		if _, ok := tgt.(Installer); ok {
			t.Errorf("%s implements Installer", name)
		}
	}
	if _, ok := Target(&Synthetic{}).(Logcatter); ok {
		t.Error("synthetic implements Logcatter")
	}
}

func TestShutDownAVDSaysToBootIt(t *testing.T) {
	e := NewEmulator(ADB{Path: "/nonexistent/adb"}, EmulatorConfig{AVD: "Pixel_10", Port: 5554}, "", "")
	_, ierr := e.Install(context.Background(), []string{"/stage/0.apk"}, InstallOptions{})
	_, lerr := e.Logcat(context.Background(), LogcatQuery{})
	for _, err := range []error{ierr, lerr} {
		if err == nil || !strings.Contains(err.Error(), "open it in the viewer to boot it") {
			t.Errorf("err = %v; want the not-running error", err)
		}
	}
}

func TestInstallAllowTestPassesT(t *testing.T) {
	adb, argv := fakeADB(t, `echo Success`)
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	if _, err := tgt.Install(context.Background(), []string{"/stage/0.apk"}, InstallOptions{AllowTest: true}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, argv); len(got) != 1 || !strings.HasPrefix(got[0], "-s s install -r -t ") {
		t.Errorf("adb argv = %q; want install -r -t", got)
	}
}
