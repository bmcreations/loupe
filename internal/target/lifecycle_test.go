package target

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeleteAVDRemovesThePathDirectoryAndTheIni(t *testing.T) {
	home := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "custom.avd")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(elsewhere, "config.ini"), "x=1\n")
	ini := filepath.Join(home, "swipe.ini")
	mustWrite(t, ini, "path="+elsewhere+"\n")
	keep := filepath.Join(home, "other.avd")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := DeleteAVD(home, "swipe"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{elsewhere, ini} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists", p)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("deleted a neighbour: %v", err)
	}
}

func TestDeleteAVDRefusesNamesThatLeaveTheHome(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"../x", "a/b", "..", "", "-flag"} {
		if err := DeleteAVD(home, name); err == nil {
			t.Errorf("DeleteAVD(%q) succeeded", name)
		}
	}
}

func TestDeleteAVDRefusesAPathThatIsTheHome(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "bad.ini"), "path="+home+"\n")
	if err := DeleteAVD(home, "bad"); err == nil {
		t.Fatal("deleted the AVD home")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal(err)
	}
}

// fakeStopADB writes a script that answers `emu kill` with emuReply and
// `get-state` with success until the marker file exists.
func fakeStopADB(t *testing.T, emuReply string) (ADB, string) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'emu kill'*) echo '" + emuReply + "'; touch " + gone + " ;;\n" +
		"  *get-state*) [ -e " + gone + " ] && exit 1; echo device ;;\n" +
		"esac\n"
	p := filepath.Join(dir, "adb")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return ADB{Path: p}, gone
}

func TestEmulatorStopKillsThroughTheConsoleAndWaitsForADB(t *testing.T) {
	adb, _ := fakeStopADB(t, "OK: killing emulator, bye bye")
	stopPoll = 10 * time.Millisecond
	alive := true
	findAVDPid = func(context.Context, string) int {
		if alive {
			alive = false // gone on the second look
			return 4242
		}
		return 0
	}
	killed := 0
	killPid = func(int) error { killed++; return nil }
	t.Cleanup(func() { findAVDPid, killPid, stopPoll = runningAVDPid, defaultKillPid, 200*time.Millisecond })

	e := NewEmulator(adb, EmulatorConfig{AVD: "swipe"}, "", "")
	e.Attach(DeviceInfo{Serial: "emulator-5560"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if killed != 0 {
		t.Errorf("killed the pid %d times; the console should have done it", killed)
	}
	if !e.BootsOnOpen() {
		t.Error("still reads as running after Stop")
	}
}

func TestEmulatorStopFallsBackToThePidWhenADBCannotReachIt(t *testing.T) {
	adb, _ := fakeStopADB(t, "error: device offline")
	stopPoll = 10 * time.Millisecond
	pid := 4242
	findAVDPid = func(context.Context, string) int { return pid }
	killPid = func(p int) error {
		if p != 4242 {
			t.Errorf("killed pid %d", p)
		}
		pid = 0
		return nil
	}
	t.Cleanup(func() { findAVDPid, killPid, stopPoll = runningAVDPid, defaultKillPid, 200*time.Millisecond })

	e := NewEmulator(adb, EmulatorConfig{AVD: "swipe"}, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if pid != 0 {
		t.Error("the pid was never killed")
	}
}

func TestEmulatorStopClearsTheBootThisServerStarted(t *testing.T) {
	adb, _ := fakeStopADB(t, "OK: killing emulator, bye bye")
	stopPoll = 10 * time.Millisecond
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	findAVDPid = func(context.Context, string) int {
		if closed(exited) {
			return 0
		}
		return cmd.Process.Pid
	}
	killPid = defaultKillPid
	t.Cleanup(func() { cmd.Process.Kill(); findAVDPid, stopPoll = runningAVDPid, 200*time.Millisecond })

	e := NewEmulator(adb, EmulatorConfig{AVD: "swipe"}, "", "")
	e.proc, e.exited, e.port = cmd, exited, 5560
	// No serial attached, so the console is skipped and the pid path kills sleep.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.proc != nil || e.port != 0 {
		t.Errorf("proc=%v port=%d after Stop; the next open would wait on a dead boot", e.proc, e.port)
	}
}

func TestSimulatorStopTreatsAlreadyShutDownAsDone(t *testing.T) {
	var calls []string
	simctl = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "shutdown" {
			return []byte("Unable to shutdown device in current state: Shutdown"), errExit
		}
		return nil, nil
	}
	t.Cleanup(func() { simctl = defaultSimctl })

	s := NewSimulator(SimInfo{UDID: "U1", Name: "swipe", State: "Booted"}, "")
	s.booted.Store(true)
	if err := s.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ";"); got != "shutdown U1;delete U1" {
		t.Errorf("calls = %q", got)
	}
	if !s.BootsOnOpen() {
		t.Error("still booted after Stop")
	}
}

var errExit = errors.New("exit status 1")

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteAVDRefusesEmptyHomeAndNonAVDDirs(t *testing.T) {
	if err := DeleteAVD("", "x"); err == nil {
		t.Error("empty home accepted")
	}
	home := t.TempDir()
	victim := filepath.Join(t.TempDir(), "documents")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, "x.ini"), "path="+victim+"\n")
	if err := DeleteAVD(home, "x"); err == nil {
		t.Error("removed a directory not named .avd")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Error(err)
	}
}

func TestEmulatorStopOnANotRunningEmulatorIsNil(t *testing.T) {
	adb, _ := fakeStopADB(t, "OK")
	findAVDPid = func(context.Context, string) int { return 0 }
	t.Cleanup(func() { findAVDPid = runningAVDPid })
	e := NewEmulator(adb, EmulatorConfig{AVD: "swipe"}, "", "")
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEmulatorStopKillsItsOwnProcWhenNoPidIsFound(t *testing.T) {
	adb, _ := fakeStopADB(t, "OK")
	stopPoll = 10 * time.Millisecond
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	findAVDPid = func(context.Context, string) int { return 0 }
	t.Cleanup(func() { cmd.Process.Kill(); findAVDPid, stopPoll = runningAVDPid, 200*time.Millisecond })
	e := NewEmulator(adb, EmulatorConfig{AVD: "swipe"}, "", "")
	e.proc, e.exited = cmd, exited
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("own process was not killed")
	}
}

func TestEmulatorStopRefusesWhenItBootedWhileShuttingDown(t *testing.T) {
	adb, _ := fakeStopADB(t, "OK")
	stopPoll = 10 * time.Millisecond
	calls := 0
	findAVDPid = func(context.Context, string) int {
		calls++
		if calls <= 2 { // kill lookup, then the poll: nothing running
			return 0
		}
		return 99 // the boot's process appeared once mu was free
	}
	t.Cleanup(func() { findAVDPid, stopPoll = runningAVDPid, 200*time.Millisecond })
	e := NewEmulator(adb, EmulatorConfig{AVD: "swipe"}, "", "")
	if err := e.Stop(context.Background()); err == nil {
		t.Fatal("Stop reported success over a live emulator")
	}
}
