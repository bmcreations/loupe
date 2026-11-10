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
  *"emu sensor get"*)
    printf 'acceleration = %s\r\nOK\r\n' "$FAKE_ADB_GRAVITY"
    ;;
  *"emu sensor set"*)
    if [ -n "$FAKE_ADB_SENSOR_OUT" ]; then
      echo "$FAKE_ADB_SENSOR_OUT"
    else
      echo "OK"
    fi
    ;;
  *dumpsys*)
    if [ -n "$FAKE_ADB_DISPLAY" ]; then
      echo "Display: mDisplayId=0"
      echo "  mDisplayRotation=ROTATION_$FAKE_ADB_DISPLAY"
    fi
    ;;
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

// TestADBTargetRotateTiltsAnEmulator: an emulator turns by its tilt sensor
// with auto-rotate on, so an app that locks its orientation stays put the way
// it does on a phone, and user_rotation is never written.
func TestADBTargetRotateTiltsAnEmulator(t *testing.T) {
	log := filepath.Join(t.TempDir(), "adb.log")
	adb := writeFakeRotateADB(t, log)
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "emulator-5554", State: "device", Emulator: true}, "", "")

	if err := tgt.Rotate(t.Context(), 1); err != nil {
		t.Fatalf("Rotate(1): %v", err)
	}
	lines := readLines(t, log)
	if len(lines) != 3 {
		t.Fatalf("adb saw %d commands: %v; want 3", len(lines), lines)
	}
	if !strings.Contains(lines[0], "emu sensor set acceleration "+gravities[1]) {
		t.Fatalf("first command = %q; want the tilt for 90", lines[0])
	}
	if !strings.Contains(lines[1], "accelerometer_rotation 1") {
		t.Fatalf("second command = %q; want auto-rotate on", lines[1])
	}
	if !strings.Contains(lines[2], "fixed-to-user-rotation default") {
		t.Fatalf("third command = %q; want the display handed back to the app", lines[2])
	}
	for _, l := range lines {
		if strings.Contains(l, "user_rotation") && !strings.Contains(l, "fixed-to-user-rotation") {
			t.Fatalf("command %q writes user_rotation; an emulator turns by its sensor", l)
		}
	}
}

func TestADBTargetRotateSurfacesARefusedTilt(t *testing.T) {
	adb := writeFakeRotateADB(t, filepath.Join(t.TempDir(), "adb.log"))
	t.Setenv("FAKE_ADB_SENSOR_OUT", "KO: bad sensor value")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "emulator-5554", State: "device", Emulator: true}, "", "")
	if err := tgt.Rotate(t.Context(), 1); err == nil || !strings.Contains(err.Error(), "KO") {
		t.Fatalf("Rotate(1) = %v; want the console's KO", err)
	}
}

// TestADBTargetRotationCountsFromTheHeldTurn: the Pixel launcher keeps its
// display at 0 however the emulator is tilted, so a Rotate that counted from
// the display would ask for 90 on every click.
func TestADBTargetRotationCountsFromTheHeldTurn(t *testing.T) {
	adb := writeFakeRotateADB(t, filepath.Join(t.TempDir(), "adb.log"))
	t.Setenv("FAKE_ADB_DISPLAY", "0")
	t.Setenv("FAKE_ADB_GRAVITY", "9.81:0:0")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "emulator-5554", State: "device", Emulator: true}, "", "")
	got, err := tgt.Rotation(t.Context())
	if err != nil || got != 1 {
		t.Fatalf("Rotation() = %d, %v; want 1, the held turn", got, err)
	}
}

// TestADBTargetRotationOfAPhoneIsItsDisplay: a physical phone has no sensor
// loupe can set, so it stays on the display rotation it always answered.
func TestADBTargetRotationOfAPhoneIsItsDisplay(t *testing.T) {
	log := filepath.Join(t.TempDir(), "adb.log")
	adb := writeFakeRotateADB(t, log)
	t.Setenv("FAKE_ADB_DISPLAY", "180")
	t.Setenv("FAKE_ADB_GRAVITY", "9.81:0:0")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "phone-1", State: "device"}, "", "")
	got, err := tgt.Rotation(t.Context())
	if err != nil || got != 2 {
		t.Fatalf("Rotation() = %d, %v; want 2, the display", got, err)
	}
	for _, l := range readLines(t, log) {
		if strings.Contains(l, "emu") {
			t.Fatalf("command %q asks a phone for its emulator console", l)
		}
	}
}

func TestHeldFromGravity(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		display int
		want    int
	}{
		{"upright, as the console answers", "acceleration = 0:9.77631:0.812349\r\nOK\r\n", 1, 0},
		{"top to the left, as the console answers", "acceleration = 9.81:0:0\r\nOK\r\n", 2, 1},
		{"top to the left", "acceleration = 9.81:0:0", 0, 1},
		{"upside down", "acceleration = 0:-9.81:0", 0, 2},
		{"top to the right", "acceleration = -9.81:0:0", 0, 3},
		{"each value set by gravities", "acceleration = " + gravities[3], 0, 3},
		{"flat on a table", "acceleration = 0:0:9.81", 2, 2},
		{"unreadable", "KO: unknown command", 1, 1},
		{"short", "acceleration = 1:2", 3, 3},
	}
	for _, c := range cases {
		if got := heldFromGravity(c.out, c.display); got != c.want {
			t.Errorf("%s: heldFromGravity(%q, %d) = %d; want %d", c.name, c.out, c.display, got, c.want)
		}
	}
	for to, g := range gravities {
		if got := heldFromGravity("acceleration = "+g, 0); got != to {
			t.Errorf("gravities[%d] = %q reads back as %d", to, g, got)
		}
	}
}

// TestContentRotationFor pins the direction against the simulator's: a device
// turned counter-clockwise (1) showing an upright picture needs the same
// three clockwise quarter turns simContentRotation(1) gives.
func TestContentRotationFor(t *testing.T) {
	if got, want := contentRotationFor(1, 0), simContentRotation(1); got != want {
		t.Fatalf("contentRotationFor(1, 0) = %d; want %d, the simulator's", got, want)
	}
	for held := 0; held < 4; held++ {
		if got := contentRotationFor(held, held); got != 0 {
			t.Errorf("contentRotationFor(%d, %d) = %d; want 0, the display followed", held, held, got)
		}
	}
	if got := contentRotationFor(0, 1); got != 1 {
		t.Errorf("contentRotationFor(0, 1) = %d; want 1, a landscape-locked app on an upright phone", got)
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
		t.Fatalf("adb saw %d commands: %v; want 2 (both attempted, nothing after the failure)", len(lines), lines)
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
