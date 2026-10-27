package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSimcap writes a shell script that answers simcap's -input protocol and
// appends every command it receives to a log, and returns both paths. It
// routes input through the legacy client; fakeSimcapRoute picks the route.
func fakeSimcap(t *testing.T) (bin, logPath string) {
	return fakeSimcapRoute(t, "legacy")
}

// The `route` probe each process start sends is answered and not logged, so
// a test's log holds only the input it caused.
func fakeSimcapRoute(t *testing.T, route string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "commands")
	bin = filepath.Join(dir, "simcap")
	script := `#!/bin/sh
while read -r line; do
  if [ "$line" = route ]; then echo "ok ` + route + `"; continue; fi
  echo "$line" >> "` + logPath + `"
  case "$line" in
    size) echo "ok 1206 2622" ;;
    refuse*) echo "err refused" ;;
    die) exit 3 ;;
    *) echo ok ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func commands(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), " ", "_"))
}

func TestSimInputRepliesAndRestartsAfterAnExit(t *testing.T) {
	bin, _ := fakeSimcap(t)
	in := &simInput{bin: bin, udid: "u"}
	defer in.close()
	ctx := context.Background()

	if out, err := in.do(ctx, 0, "size"); err != nil || out != "1206 2622" {
		t.Fatalf("size = %q, %v", out, err)
	}
	if _, err := in.do(ctx, 0, "refuse this"); err == nil || err.Error() != "refused" {
		t.Fatalf("refusal = %v, want the reply's own words", err)
	}
	if _, err := in.do(ctx, 0, "die"); err == nil {
		t.Fatal("a command that killed simcap reported success")
	}
	if _, err := in.do(ctx, 0, "tap 1 2"); err != nil {
		t.Fatalf("the next command did not start a fresh simcap: %v", err)
	}
}

func TestSimStreamTouchFollowsOnlyTheFirstPointer(t *testing.T) {
	bin, logPath := fakeSimcap(t)
	in := &simInput{bin: bin, udid: "u"}
	defer in.close()
	st := &simStream{in: in, sim: &Simulator{}}

	for _, e := range []Touch{
		{Action: TouchDown, PointerID: 7, X: 10, Y: 20},
		{Action: TouchDown, PointerID: 8, X: 99, Y: 99}, // a second finger
		{Action: TouchMove, PointerID: 8, X: 98, Y: 98},
		{Action: TouchMove, PointerID: 7, X: 11, Y: 21},
		{Action: TouchUp, PointerID: 8, X: 98, Y: 98},
		{Action: TouchUp, PointerID: 7, X: 12, Y: 22},
		{Action: TouchMove, PointerID: 7, X: 13, Y: 23}, // after lift
	} {
		if err := st.Touch(e); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(commands(t, logPath), " ")
	want := "touch_down_10_20 touch_move_11_21 touch_up_12_22"
	if got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

func TestSimStreamTouchCarriesASecondFingerThroughCoreDevice(t *testing.T) {
	bin, logPath := fakeSimcapRoute(t, "hub")
	in := &simInput{bin: bin, udid: "u"}
	defer in.close()
	st := &simStream{in: in, sim: &Simulator{}}

	for _, e := range []Touch{
		{Action: TouchDown, PointerID: 7, X: 10, Y: 20},
		{Action: TouchDown, PointerID: 8, X: 90, Y: 20},
		{Action: TouchDown, PointerID: 9, X: 50, Y: 50}, // a third finger
		{Action: TouchMove, PointerID: 8, X: 95, Y: 20},
		{Action: TouchMove, PointerID: 7, X: 5, Y: 20},
		{Action: TouchUp, PointerID: 7, X: 4, Y: 20}, // the first lifts
		{Action: TouchMove, PointerID: 8, X: 96, Y: 21},
		{Action: TouchUp, PointerID: 8, X: 97, Y: 22},
	} {
		if err := st.Touch(e); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(commands(t, logPath), " ")
	want := "touch_down_10_20 touch2_move_10_20_90_20 touch2_move_10_20_95_20 touch2_move_5_20_95_20 " +
		"touch_move_95_20 touch_move_96_21 touch_up_97_22"
	if got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

// Under MaxSize simcap encodes a smaller picture than the framebuffer its
// input reads, so a touch at the middle of a 496x1080 stream has to land at
// the middle of the 1206x2622 screen, not in its top-left quarter.
func TestSimStreamTouchScalesStreamPixelsToTheFramebuffer(t *testing.T) {
	bin, logPath := fakeSimcapRoute(t, "hub")
	in := &simInput{bin: bin, udid: "u"}
	defer in.close()
	st := &simStream{in: in, sim: &Simulator{}, meta: Meta{Width: 496, Height: 1080}}

	for _, e := range []Touch{
		{Action: TouchDown, PointerID: 7, X: 248, Y: 540},
		{Action: TouchDown, PointerID: 8, X: 0, Y: 0},
		{Action: TouchUp, PointerID: 8, X: 496, Y: 1080},
		{Action: TouchUp, PointerID: 7, X: 248, Y: 540},
	} {
		if err := st.Touch(e); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(commands(t, logPath), " ")
	want := "size touch_down_603_1311 touch2_move_603_1311_0_0 touch_move_603_1311 touch_up_603_1311"
	if got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

func TestPinchSpreadsAreFractionsOfTheShortSide(t *testing.T) {
	bin, logPath := fakeSimcapRoute(t, "hub")
	in := &simInput{bin: bin, udid: "u"}
	defer in.close()
	s := &Simulator{}
	s.inputOnce.Do(func() { s.in = in })
	s.booted.Store(true)

	// The fake answers size with 1206x2622, so the wide spread is
	// 0.6 * 1206 = 723.
	for _, scale := range []float64{2, 0.5} {
		if err := s.Pinch(context.Background(), 603, 1311, scale, 300); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(commands(t, logPath), " ")
	want := "size pinch_603_1311_361_723_300 size pinch_603_1311_723_361_300"
	if got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

func TestAndroidKeycodesMapOntoIOS(t *testing.T) {
	for code, want := range map[int32]iosKey{
		3:   {button: "home"},
		26:  {button: "lock"},
		187: {button: "appSwitcher"},
		29:  {usage: 4},  // A
		54:  {usage: 29}, // Z
		8:   {usage: 30}, // 1
		16:  {usage: 38}, // 9
		7:   {usage: 39}, // 0
		66:  {usage: 40}, // enter
		67:  {usage: 42}, // backspace
	} {
		if got := androidToIOS[code]; got != want {
			t.Errorf("keycode %d = %+v, want %+v", code, got, want)
		}
	}
	if _, ok := androidToIOS[4]; ok {
		t.Error("back maps to something; iOS has no back button")
	}
}

func TestEveryPrintableASCIICharacterCanBeTyped(t *testing.T) {
	for r := rune(32); r < 127; r++ {
		if _, ok := charToIOS[r]; !ok {
			t.Errorf("%q has no key", r)
		}
	}
	for r, want := range map[rune]iosKey{
		'a': {usage: 4}, 'A': {usage: 4, shift: true}, '0': {usage: 39}, ')': {usage: 39, shift: true},
		'1': {usage: 30}, '!': {usage: 30, shift: true}, '/': {usage: 56}, '?': {usage: 56, shift: true},
	} {
		if got := charToIOS[r]; got != want {
			t.Errorf("%q = %+v, want %+v", r, got, want)
		}
	}
}

func TestTypeTextSendsNothingWhenOneCharacterCannotBeTyped(t *testing.T) {
	bin, logPath := fakeSimcap(t)
	s := NewSimulator(SimInfo{UDID: "u", Name: "iPhone", State: "Booted"}, bin)
	if err := s.TypeText(context.Background(), "ok é"); err == nil {
		t.Fatal("typed a character with no key")
	}
	if got := commands(t, logPath); len(got) != 0 {
		t.Errorf("sent %v before refusing", got)
	}
}

// TestAppForPidNamesOnlyApps pins the launchctl parse behind reopening the
// app that was in front: the label's instance suffix is dropped, and a pid
// that is not an app -- SpringBoard on the Home Screen -- names nothing.
func TestAppForPidNamesOnlyApps(t *testing.T) {
	list := "PID\tStatus\tLabel\n" +
		"94401\t0\tUIKitApplication:com.apple.Preferences[974f][rb-legacy]\n" +
		"94380\t0\tcom.apple.SpringBoard\n" +
		"-\t0\tUIKitApplication:com.apple.mobilesafari[1a2b][rb-legacy]\n"
	for pid, want := range map[string]string{"94401": "com.apple.Preferences", "94380": "", "1": ""} {
		if got := appForPid(list, pid); got != want {
			t.Errorf("appForPid(%s) = %q; want %q", pid, got, want)
		}
	}
}

// TestUnclaimedSimulatorAsksBeforeReclaiming pins that a hold on a simulator
// this server neither booted nor reclaimed fails with ErrHubHoldsInput
// instead of restarting SpringBoard under someone else.
func TestUnclaimedSimulatorAsksBeforeReclaiming(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u", Name: "iPhone", State: "Booted"}, "")
	s.hubHeld, s.hubChecked = true, time.Now()
	if _, err := s.checkHub(t.Context()); !errors.Is(err, ErrHubHoldsInput) {
		t.Fatalf("checkHub = %v; want ErrHubHoldsInput", err)
	}
}

// TestCoreDeviceRouteNeedsNoReclaim pins that input going through CoreDevice
// is sent while Device Hub holds the simulator, with neither the ask nor the
// SpringBoard restart: that path reaches the simulator either way.
func TestCoreDeviceRouteNeedsNoReclaim(t *testing.T) {
	bin, _ := fakeSimcapRoute(t, "hub")
	s := NewSimulator(SimInfo{UDID: "u", Name: "iPhone", State: "Booted"}, bin)
	defer s.input().close()
	s.hubHeld, s.hubChecked = true, time.Now()
	if _, err := s.checkHub(t.Context()); err != nil {
		t.Fatalf("checkHub = %v; want nil through CoreDevice", err)
	}
	if _, ok := s.LastReclaim(); ok {
		t.Fatal("a CoreDevice route reclaimed anyway")
	}
}

// TestHardwareKeyboardGoesAroundDeviceHub pins that switching the hardware
// keyboard is sent on the legacy route while Device Hub holds the
// simulator's keys: it is a CoreSimulator call, not HID input, so asking to
// reclaim first would restart SpringBoard for nothing.
func TestHardwareKeyboardGoesAroundDeviceHub(t *testing.T) {
	bin, logPath := fakeSimcap(t)
	s := NewSimulator(SimInfo{UDID: "u", Name: "iPhone", State: "Booted"}, bin)
	defer s.input().close()
	s.booted.Store(true)
	s.hubHeld, s.hubChecked = true, time.Now()

	for _, on := range []bool{true, false} {
		if err := s.SetHardwareKeyboard(t.Context(), on); err != nil {
			t.Fatalf("SetHardwareKeyboard(%v) = %v", on, err)
		}
	}
	got := strings.Join(commands(t, logPath), " ")
	if want := "hwkeyboard_on hwkeyboard_off"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

// simcap's own CoreDevice open waits 5s, and its clock starts after the
// process has read `route`. A route answered at 5.5s, as a slow open that
// falls back to the legacy client does, must still be heard: the process is
// kept, the command is sent, and nothing restarts. This runs with the real
// simInputRouteBudget on purpose.
func TestSimInputWaitsOutASlowRouteAnswer(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "simcap")
	logPath := filepath.Join(dir, "commands")
	launches := filepath.Join(dir, "launches")
	script := `#!/bin/sh
echo $$ >> "` + launches + `"
while read -r line; do
  if [ "$line" = route ]; then sleep 5.5; echo "ok legacy"; continue; fi
  echo "$line" >> "` + logPath + `"
  echo ok
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in := &simInput{bin: bin, udid: "u"}
	defer in.close()

	if _, err := in.do(t.Context(), 0, "tap %d %d", 1, 2); err != nil {
		t.Fatalf("tap with a route answered after 5.5s: %v", err)
	}
	if _, err := in.do(t.Context(), 0, "key %d", 4); err != nil {
		t.Fatalf("second command: %v", err)
	}
	if got := commands(t, logPath); len(got) != 2 || got[0] != "tap_1_2" || got[1] != "key_4" {
		t.Errorf("commands = %v; want the tap then the key", got)
	}
	if raw, _ := os.ReadFile(launches); len(strings.Fields(string(raw))) != 1 {
		t.Errorf("launches = %q; want one process, not a restart", raw)
	}
	if in.viaHub.Load() {
		t.Error("a legacy route was read as hub")
	}
}

// The viewer's key strip sends these Android keycodes to a simulator
// (STRIP_KEYS in web/keyboard.js), so each needs an iOS key.
func TestTheKeyStripKeysReachASimulator(t *testing.T) {
	for _, code := range []int32{111, 61, 19, 20, 21, 22} { // Esc, Tab, up, down, left, right
		if _, ok := androidToIOS[code]; !ok {
			t.Errorf("keycode %d has no iOS key", code)
		}
	}
}
