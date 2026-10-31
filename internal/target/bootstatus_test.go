package target

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// offlineADB lists emulator-5560 as offline, as a wedged boot looks.
func offlineADB(t *testing.T) ADB {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "adb")
	script := `#!/bin/sh
case "$*" in
  devices*) printf 'List of devices attached\nemulator-5560\toffline transport_id:3\n' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return ADB{Path: bin}
}

func TestBootStatusReportsAnOfflineBootingEmulator(t *testing.T) {
	e := NewEmulator(offlineADB(t), EmulatorConfig{AVD: "no_such_avd_for_ps"}, "", "")
	e.bootingAs.Store("emulator-5560")
	e.boot.alive.Store(true)
	e.boot.pid.Store(4242)
	e.boot.started.Store(time.Now().Add(-time.Minute).UnixNano())
	e.boot.logPath.Store("/tmp/emulator-x.log")

	st := e.BootStatus(context.Background())

	if st.State != BootStateBooting || !st.ProcessAlive || st.PID != 4242 {
		t.Errorf("state %q alive %v pid %d, want booting alive 4242", st.State, st.ProcessAlive, st.PID)
	}
	if st.ADBState != "offline" || st.Serial != "emulator-5560" || st.BootCompleted {
		t.Errorf("adb %q serial %q completed %v, want offline emulator-5560 false", st.ADBState, st.Serial, st.BootCompleted)
	}
	if st.LogPath != "/tmp/emulator-x.log" || st.ElapsedMs < 50_000 {
		t.Errorf("log %q elapsed %dms", st.LogPath, st.ElapsedMs)
	}
}

func TestBootStatusOfAnAVDThatNeverBootedIsStopped(t *testing.T) {
	e := NewEmulator(offlineADB(t), EmulatorConfig{AVD: "no_such_avd_for_ps"}, "", "")
	if st := e.BootStatus(context.Background()); st.State != BootStateStopped || st.ProcessAlive {
		t.Errorf("state %q alive %v, want stopped", st.State, st.ProcessAlive)
	}
}

// A timed-out boot says the emulator is still tracked, and where its output is.
func TestBootFailureNamesTheLogAndTheStillRunningProcess(t *testing.T) {
	log := filepath.Join(t.TempDir(), "emulator.log")
	if err := os.WriteFile(log, []byte("start\nWARNING: gpu\nERROR: image rev mismatch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewEmulator(ADB{}, EmulatorConfig{AVD: "x"}, "", "")
	e.boot.alive.Store(true)
	e.boot.pid.Store(77)
	e.boot.logPath.Store(log)

	err := e.bootFailure(context.DeadlineExceeded, "emulator-5554").Error()

	for _, want := range []string{"pid 77", "still running", "GET /api/devices/boot?target=avd:x", log, "image rev mismatch"} {
		if !strings.Contains(err, want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	e.boot.alive.Store(false)
	if err := e.bootFailure(context.DeadlineExceeded, "").Error(); !strings.Contains(err, "has exited") {
		t.Errorf("error %q should say the process exited", err)
	}
}

func TestBootBudgetFollowsTheContextDeadline(t *testing.T) {
	e := NewEmulator(ADB{}, EmulatorConfig{AVD: "x"}, "", "")
	if got := e.bootBudget(context.Background()); got != 3*time.Minute {
		t.Errorf("no deadline: %v, want the configured 3m", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if got := e.bootBudget(ctx); got < 9*time.Minute || got > 10*time.Minute {
		t.Errorf("10m deadline: %v", got)
	}
}
