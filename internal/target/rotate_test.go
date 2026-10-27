package target

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestButtonKeycodeLooksUpKnownButtons pins the Android button table itself:
// a name maps to the keycode it advertises, in both directions with
// ButtonForKeycode below.
func TestButtonKeycodeLooksUpKnownButtons(t *testing.T) {
	want := map[string]int{"home": 3, "back": 4, "recents": 187, "lock": 26}
	for name, code := range want {
		got, ok := ButtonKeycode(name)
		if !ok || got != code {
			t.Fatalf("ButtonKeycode(%q) = %d, %v; want %d, true", name, got, ok, code)
		}
	}
}

// TestButtonKeycodeRejectsAnUnknownName covers the case the plan calls out by
// name: appSwitcher is iOS's button, and asking the Android table for it must
// fail rather than silently resolve to something else.
func TestButtonKeycodeRejectsAnUnknownName(t *testing.T) {
	if _, ok := ButtonKeycode("appSwitcher"); ok {
		t.Fatal(`ButtonKeycode("appSwitcher") = true; want false, Android has no such button`)
	}
	if _, ok := ButtonKeycode(""); ok {
		t.Fatal(`ButtonKeycode("") = true; want false`)
	}
}

func TestButtonForKeycodeLooksUpKnownCodes(t *testing.T) {
	want := map[int]string{3: "home", 4: "back", 187: "recents", 26: "lock"}
	for code, name := range want {
		got, ok := ButtonForKeycode(code)
		if !ok || got != name {
			t.Fatalf("ButtonForKeycode(%d) = %q, %v; want %q, true", code, got, ok, name)
		}
	}
}

// TestButtonForKeycodeRejectsAnUnknownCode: keycode 29 is KEYCODE_A, an
// ordinary key with no button name -- this is the "stays kind:key" case
// ViewerKey's classification relies on.
func TestButtonForKeycodeRejectsAnUnknownCode(t *testing.T) {
	if _, ok := ButtonForKeycode(29); ok {
		t.Fatal("ButtonForKeycode(29) = true; want false, 29 is not a named button")
	}
}

// TestRotateCommandsSetsUserRotationThenPinsAutoRotateOff pins the two-command
// shape the Rotator doc explains: Android ignores a user_rotation write while
// auto-rotate is on, so the target value is staged first and auto-rotate is
// turned off second, applying both in one transition. The reverse order
// would leave the display locked at a stale user_rotation, however briefly.
func TestRotateCommandsSetsUserRotationThenPinsAutoRotateOff(t *testing.T) {
	tgt := &ADBTarget{}
	got, err := tgt.rotateCommands(1)
	if err != nil {
		t.Fatalf("rotateCommands(1): %v", err)
	}
	want := [][]string{
		{"settings", "put", "system", "user_rotation", "1"},
		{"settings", "put", "system", "accelerometer_rotation", "0"},
	}
	if len(got) != len(want) {
		t.Fatalf("rotateCommands(1) = %v; want %v", got, want)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("rotateCommands(1)[%d] = %v; want %v", i, got[i], want[i])
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("rotateCommands(1)[%d] = %v; want %v", i, got[i], want[i])
			}
		}
	}
}

// TestRotateCommandsRejectsAnOutOfRangeRotation: rotation is 0..3 quarter
// turns; 4 has no meaning and must fail before anything runs on the device.
func TestRotateCommandsRejectsAnOutOfRangeRotation(t *testing.T) {
	tgt := &ADBTarget{}
	if _, err := tgt.rotateCommands(4); err == nil {
		t.Fatal("rotateCommands(4) succeeded; want an error, rotation is 0..3")
	}
	if _, err := tgt.rotateCommands(-1); err == nil {
		t.Fatal("rotateCommands(-1) succeeded; want an error, rotation is 0..3")
	}
}

// writeFakeRotateADB writes an executable shell script standing in for the
// adb binary, tailored to Rotate's two `settings put` commands. Every
// invocation is appended to logPath (so a test can confirm both commands ran
// and in what order), and each command's own exit code and combined
// stdout+stderr are controlled independently through env vars read via
// t.Setenv -- FAKE_ADB_USER_ROTATION_FAIL / _OUT for the user_rotation
// command, FAKE_ADB_ACCEL_FAIL / _OUT for accelerometer_rotation. A *_FAIL
// value is written to stderr and exits 1, the way a real adb shell failure
// looks; a *_OUT value is written to stdout with exit 0, the way `settings
// put` reporting a SecurityException on some devices looks -- ADB.Shell uses
// CombinedOutput, so both land in Rotate's out either way.
func writeFakeRotateADB(t *testing.T, logPath string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-adb")
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
case "$*" in
  *user_rotation*)
    if [ -n "$FAKE_ADB_USER_ROTATION_FAIL" ]; then
      echo "$FAKE_ADB_USER_ROTATION_FAIL" >&2
      exit 1
    fi
    if [ -n "$FAKE_ADB_USER_ROTATION_OUT" ]; then
      echo "$FAKE_ADB_USER_ROTATION_OUT"
    fi
    ;;
  *accelerometer_rotation*)
    if [ -n "$FAKE_ADB_ACCEL_FAIL" ]; then
      echo "$FAKE_ADB_ACCEL_FAIL" >&2
      exit 1
    fi
    if [ -n "$FAKE_ADB_ACCEL_OUT" ]; then
      echo "$FAKE_ADB_ACCEL_OUT"
    fi
    ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rotate adb: %v", err)
	}
	return path
}

// readLines reads a log file's non-empty lines, for asserting how many
// commands a fake adb actually saw and in what order.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestADBTargetRotateRunsBothCommandsInOrder drives Rotate against a fake adb
// and checks both `settings put` commands actually ran, in the order
// rotateCommands built them (user_rotation, then accelerometer_rotation) --
// the command-list test above never runs anything, so it cannot by itself
// catch a mutation that skips a command when Rotate executes the list.
func TestADBTargetRotateRunsBothCommandsInOrder(t *testing.T) {
	log := filepath.Join(t.TempDir(), "adb.log")
	adb := writeFakeRotateADB(t, log)
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "fake-1", State: "device"}, "", "")

	if err := tgt.Rotate(t.Context(), 2); err != nil {
		t.Fatalf("Rotate(2): %v", err)
	}
	lines := readLines(t, log)
	if len(lines) != 2 {
		t.Fatalf("adb saw %d commands: %v; want 2", len(lines), lines)
	}
	if !strings.Contains(lines[0], "user_rotation 2") {
		t.Fatalf("first command = %q; want it to set user_rotation", lines[0])
	}
	if !strings.Contains(lines[1], "accelerometer_rotation 0") {
		t.Fatalf("second command = %q; want it to pin accelerometer_rotation off", lines[1])
	}
}

// TestADBTargetRotateSurfacesAFailedShellCall fails the second command (the
// one that pins auto-rotate off) and checks two things at once: Rotate
// returns the failure rather than swallowing it, and the first command still
// ran -- proving the failure comes from the device call itself and not from
// rotateCommands or the auto-rotate command being skipped entirely.
func TestADBTargetRotateSurfacesAFailedShellCall(t *testing.T) {
	log := filepath.Join(t.TempDir(), "adb.log")
	adb := writeFakeRotateADB(t, log)
	t.Setenv("FAKE_ADB_ACCEL_FAIL", "Failure calling service")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "fake-1", State: "device"}, "", "")

	err := tgt.Rotate(t.Context(), 1)
	if err == nil {
		t.Fatal("Rotate(1) succeeded; want an error from the failed accelerometer_rotation write")
	}
	lines := readLines(t, log)
	if len(lines) != 2 {
		t.Fatalf("adb saw %d commands: %v; want 2 (both attempted)", len(lines), lines)
	}
}

// TestADBTargetRotateFailsOnExceptionInOutputEvenWithZeroExit pins item 3: not
// every adb version passes the device shell's exit status through, so `settings
// put` can fail on the device (a SecurityException on stdout, in this case
// from the user_rotation write) while the process itself exits 0. Rotate must
// read that the way input.go's own `input` helper already does, not trust the
// exit code alone.
func TestADBTargetRotateFailsOnExceptionInOutputEvenWithZeroExit(t *testing.T) {
	log := filepath.Join(t.TempDir(), "adb.log")
	adb := writeFakeRotateADB(t, log)
	t.Setenv("FAKE_ADB_USER_ROTATION_OUT", "java.lang.SecurityException: Permission Denial")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "fake-1", State: "device"}, "", "")

	err := tgt.Rotate(t.Context(), 1)
	if err == nil {
		t.Fatal("Rotate(1) succeeded; want an error, the device reported a SecurityException with exit 0")
	}
	if !strings.Contains(err.Error(), "SecurityException") {
		t.Fatalf("Rotate(1) error = %q; want it to carry the device's own output", err.Error())
	}
}
