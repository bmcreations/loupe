package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func savingEmulator(t *testing.T, f *pauseFake, keep bool) (*Emulator, string) {
	t.Helper()
	dir := t.TempDir()
	e := NewEmulator(f.adb, EmulatorConfig{AVD: "test_avd", Port: 5554, Dir: dir, KeepState: func() bool { return keep }}, "", "")
	e.Attach(DeviceInfo{Serial: "emulator-5554", State: "device", Emulator: true})
	return e, dir
}

// An explicit save is what the person asked for, so emulatorKeepState, which
// is about automatic saves at shutdown, does not gate it.
func TestSaveWarmIgnoresKeepState(t *testing.T) {
	f := newPauseFake(t, false)
	e, _ := savingEmulator(t, f, false)
	if err := e.SaveWarm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(t); count(calls, "save") != 1 {
		t.Fatalf("calls %v, want one save", calls)
	}
}

// A snapshot with a scrcpy server inside restores it with its socket gone,
// and every later boot would start from that.
func TestSaveWarmRefusesWhileScrcpyLingers(t *testing.T) {
	f := newPauseFake(t, false)
	if err := os.WriteFile(f.flag+".scrcpy", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	saved := scrcpyGone
	scrcpyGone = 300 * time.Millisecond
	t.Cleanup(func() { scrcpyGone = saved })
	e, _ := savingEmulator(t, f, true)
	err := e.SaveWarm(context.Background())
	if !errors.Is(err, ErrStreamLingering) {
		t.Fatalf("SaveWarm = %v, want ErrStreamLingering", err)
	}
	if calls := f.calls(t); count(calls, "save") != 0 {
		t.Fatalf("calls %v, want no save", calls)
	}
	if !e.StartSavedAt().IsZero() {
		t.Error("a refused save left a start marker")
	}
}

func TestSaveWarmNotRunningErrors(t *testing.T) {
	f := newPauseFake(t, false)
	e := NewEmulator(f.adb, EmulatorConfig{AVD: "test_avd", Port: 5554, Dir: t.TempDir()}, "", "")
	if err := e.SaveWarm(context.Background()); err == nil {
		t.Fatal("SaveWarm of a stopped emulator returned nil")
	}
	if calls := f.calls(t); count(calls, "save") != 0 {
		t.Fatalf("calls %v, want no save", calls)
	}
}

func TestSaveWarmNoDirErrors(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	if err := e.SaveWarm(context.Background()); err == nil {
		t.Fatal("SaveWarm with no AVD directory returned nil")
	}
	if calls := f.calls(t); count(calls, "save") != 0 {
		t.Fatalf("calls %v, want no save", calls)
	}
}

func TestSaveWarmLeavesTheStartMarker(t *testing.T) {
	f := newPauseFake(t, false)
	e, _ := savingEmulator(t, f, false)
	if err := e.SaveWarm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if at := e.StartSavedAt(); at.IsZero() || time.Since(at) > 5*time.Second {
		t.Fatalf("StartSavedAt = %v, want about now", at)
	}
	if err := e.ResetState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if at := e.StartSavedAt(); !at.IsZero() {
		t.Errorf("StartSavedAt after a reset = %v, want zero", at)
	}
}

// Keep-state saves happen at every shutdown on their own; the marker is for
// a start state someone chose.
func TestSaveStateLeavesNoStartMarker(t *testing.T) {
	f := newPauseFake(t, false)
	e, _ := savingEmulator(t, f, true)
	if ok, err := e.SaveState(context.Background()); !ok || err != nil {
		t.Fatalf("SaveState = %v, %v", ok, err)
	}
	if at := e.StartSavedAt(); !at.IsZero() {
		t.Errorf("StartSavedAt after a keep-state save = %v, want zero", at)
	}
}

// A cold boot that saves a fresh loupe-warm has replaced the chosen state,
// so the marker describing it goes too.
func TestColdBootSaveClearsTheStartMarker(t *testing.T) {
	f := newPauseFake(t, false)
	e, dir := savingEmulator(t, f, false)
	if err := os.WriteFile(filepath.Join(dir, startMarker), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		t.Fatal(err)
	}
	if e.StartSavedAt().IsZero() {
		t.Fatal("marker written but StartSavedAt is zero")
	}
	e.keepWarm(context.Background(), "emulator-5554", "", "", time.Now())
	if calls := f.calls(t); count(calls, "save") != 1 {
		t.Fatalf("calls %v, want the cold boot's save", calls)
	}
	if at := e.StartSavedAt(); !at.IsZero() {
		t.Errorf("StartSavedAt after a cold-boot save = %v, want zero", at)
	}
}
