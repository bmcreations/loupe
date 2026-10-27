package target

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pauseFake is an adb whose emulator console keeps a paused flag in a file,
// as a real one does across server restarts. While paused, anything through
// adbd fails and is logged as such, which stands in for the hang a real
// paused guest gives: a test that sees "shell-while-paused" in the log has
// reached adbd without waking first.
type pauseFake struct {
	adb    ADB
	flag   string
	log    string
	paused bool
}

func newPauseFake(t *testing.T, paused bool) *pauseFake {
	t.Helper()
	dir := t.TempDir()
	f := &pauseFake{
		flag: filepath.Join(dir, "paused"),
		log:  filepath.Join(dir, "calls"),
	}
	if paused {
		if err := os.WriteFile(f.flag, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
flag="` + f.flag + `"
log="` + f.log + `"
case "$*" in
  *"emu avd status"*)
    echo status >> "$log"
    if [ -e "$flag" ]; then echo "virtual device is stopped"; else echo "virtual device is running"; fi
    echo OK ;;
  *"emu avd stop"*)
    echo stop >> "$log"
    if [ -e "$flag" ]; then echo "KO: virtual device already stopped"; exit 0; fi
    : > "$flag"
    if [ -e "$flag.stopfail" ]; then echo "KO: busy"; else echo OK; fi ;;
  *"emu avd start"*)
    echo start >> "$log"
    if [ ! -e "$flag" ]; then echo "KO: virtual device already running"; exit 0; fi
    rm -f "$flag"; echo OK ;;
  *"emu avd name"*)
    echo name >> "$log"; echo test_avd; echo OK ;;
  *"emu avd snapshot save"*)
    echo save >> "$log"; echo OK ;;
  *"emu kill"*)
    echo kill >> "$log"; : > "$flag.killed"; echo OK ;;
  *get-state*)
    [ -e "$flag.killed" ] && exit 1; echo device ;;
  *shell*)
    if [ -e "$flag" ]; then echo shell-while-paused >> "$log"; exit 1; fi
    echo shell >> "$log"
    case "$*" in
      *"getprop sys.boot_completed"*) echo 1 ;;
      *"getprop ro.boot.qemu.avd_name"*) echo test_avd ;;
      *getprop*)
        echo '[ro.build.version.release]: [14]'
        echo '[ro.build.version.sdk]: [34]'
        echo '[ro.build.characteristics]: [emulator]' ;;
    esac ;;
  *"logcat"*)
    echo logcat >> "$log"; sleep 30 ;;
esac
`
	bin := filepath.Join(dir, "adb")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.adb = ADB{Path: bin}
	return f
}

func (f *pauseFake) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

func count(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

func attachedEmulator(f *pauseFake) *Emulator {
	e := NewEmulator(f.adb, EmulatorConfig{AVD: "test_avd", Port: 5554}, "", "")
	e.Attach(DeviceInfo{Serial: "emulator-5554", State: "device", Emulator: true})
	return e
}

// A listing must not resume an emulator paused for being idle, and getprop
// on a paused guest hangs, so Describe answers from what it knew.
func TestDescribeWhilePausedRunsNoGetprop(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	want, err := e.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe running: %v", err)
	}
	e.MarkUsed()
	e.pause.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())
	if ok, err := e.PauseIfIdle(context.Background(), time.Minute); !ok || err != nil {
		t.Fatalf("PauseIfIdle = %v, %v, want paused", ok, err)
	}
	before := len(f.calls(t))

	got, err := e.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe paused: %v", err)
	}
	if got != want {
		t.Fatalf("Describe paused = %+v, want the cached %+v", got, want)
	}
	if after := f.calls(t)[before:]; len(after) != 0 {
		t.Fatalf("Describe on a paused emulator ran adb: %v", after)
	}
	if !e.Paused() {
		t.Fatal("Describe resumed the emulator")
	}
}

// Several calls that find the emulator paused send one `avd start` between
// them, and none reaches adbd before it.
func TestConcurrentCallsResumeOnce(t *testing.T) {
	f := newPauseFake(t, true)
	e := attachedEmulator(f)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Describe(context.Background()); err != nil && !strings.Contains(err.Error(), "paused") {
				t.Error(err)
			}
			if _, done, err := e.running(); err != nil {
				t.Error(err)
			} else {
				done()
			}
		}()
	}
	wg.Wait()

	calls := f.calls(t)
	if n := count(calls, "start"); n != 1 {
		t.Fatalf("avd start sent %d times, want 1: %v", n, calls)
	}
	if n := count(calls, "shell-while-paused"); n != 0 {
		t.Fatalf("reached adbd while paused %d times: %v", n, calls)
	}
	if e.Paused() {
		t.Fatal("still marked paused after a resume")
	}
}

// An emulator a previous server process paused is still paused when this
// one attaches, and adb's listing does not say so. The first call learns it
// from the console and resumes before reaching adbd.
func TestAttachLearnsAPauseLeftByAPreviousProcess(t *testing.T) {
	f := newPauseFake(t, true)
	e := attachedEmulator(f)
	if e.Paused() {
		t.Fatal("Paused before the console was asked; it should not guess")
	}
	if _, err := e.Describe(context.Background()); err == nil {
		t.Fatal("Describe of a paused emulator with nothing cached: want an error, not a getprop")
	}
	if !e.Paused() {
		t.Fatal("Describe did not learn the pause from the console")
	}
	if _, done, err := e.running(); err != nil {
		t.Fatalf("running: %v", err)
	} else {
		done()
	}
	calls := f.calls(t)
	if count(calls, "start") != 1 || count(calls, "shell-while-paused") != 0 {
		t.Fatalf("want one start and no shell while paused: %v", calls)
	}
}

// getprop hangs on a paused guest, so naming an emulator falls back to the
// console, or after a restart a paused emulator would never be attached.
func TestNameEmulatorFallsBackToTheConsole(t *testing.T) {
	f := newPauseFake(t, true)
	name, err := nameEmulator(context.Background(), f.adb, "emulator-5554")
	if err != nil || name != "test_avd" {
		t.Fatalf("nameEmulator = %q, %v, want test_avd", name, err)
	}
	if calls := f.calls(t); count(calls, "name") != 1 {
		t.Fatalf("console not asked: %v", calls)
	}
}

// A log follow holds the device without a session, so the idle pass must
// leave it running until the follow is closed.
func TestLogcatFollowHoldsOffAPause(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	l := e.holdLogcat(&Logcat{ReadCloser: io.NopCloser(strings.NewReader(""))})
	e.pause.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())

	if ok, _ := e.PauseIfIdle(context.Background(), time.Minute); ok {
		t.Fatal("paused with a log follow open")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	_ = l.Close() // a second Close must not release twice
	if e.InUse() {
		t.Fatal("still in use after Close")
	}
	// Closing restarts the clock, so it is not paused straight away either.
	if ok, _ := e.PauseIfIdle(context.Background(), time.Minute); ok {
		t.Fatal("paused the moment the follow closed")
	}
	e.pause.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())
	if ok, err := e.PauseIfIdle(context.Background(), time.Minute); !ok || err != nil {
		t.Fatalf("PauseIfIdle after the follow = %v, %v, want paused", ok, err)
	}
}

// A call that reached the device recently keeps it running.
func TestRecentUseHoldsOffAPause(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	_, done, err := e.running()
	if err != nil {
		t.Fatal(err)
	}
	done()
	if ok, _ := e.PauseIfIdle(context.Background(), time.Minute); ok {
		t.Fatal("paused an emulator used a moment ago")
	}
	if calls := f.calls(t); count(calls, "stop") != 0 {
		t.Fatalf("avd stop sent: %v", calls)
	}
}

// A call that is still running when the idle window passes, such as a large
// install, must not be paused under its adb shell.
func TestInFlightCallHoldsOffAPause(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	_, done, err := e.running()
	if err != nil {
		t.Fatal(err)
	}
	e.pause.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())
	if ok, _ := e.PauseIfIdle(context.Background(), time.Minute); ok {
		t.Fatal("paused under a call still in flight")
	}
	done()
	if e.InUse() {
		t.Fatal("still in use after done")
	}
	e.pause.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())
	if ok, err := e.PauseIfIdle(context.Background(), time.Minute); !ok || err != nil {
		t.Fatalf("PauseIfIdle after the call = %v, %v, want paused", ok, err)
	}
}

// A paused emulator is shut down without being resumed first: `emu kill`
// goes to the console, which answers while the VM is stopped.
func TestStopIfIdleKillsAPausedEmulatorWithoutResumingIt(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	if ok, err := e.PauseIfIdle(context.Background(), 0); !ok || err != nil {
		t.Fatalf("pause: %v %v", ok, err)
	}

	ok, err := e.StopIfIdle(context.Background(), 0)
	if !ok || err != nil {
		t.Fatalf("StopIfIdle = %v, %v; want stopped", ok, err)
	}
	calls := f.calls(t)
	if count(calls, "start") != 0 || count(calls, "kill") != 1 {
		t.Errorf("calls %v, want one kill and no start", calls)
	}
	if e.Serial() != "" || e.Paused() {
		t.Errorf("after the stop: serial %q paused %v, want detached and not paused", e.Serial(), e.Paused())
	}
}

func TestStopIfIdleLeavesARecentlyUsedEmulator(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	e.MarkUsed()

	if ok, _ := e.StopIfIdle(context.Background(), time.Hour); ok {
		t.Fatal("stopped an emulator used just now")
	}
	if n := count(f.calls(t), "kill"); n != 0 {
		t.Errorf("%d kills, want none", n)
	}
}

func TestStopIfIdleLeavesAnEmulatorInUse(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	done := e.hold()
	defer done()

	if ok, _ := e.StopIfIdle(context.Background(), 0); ok {
		t.Fatal("stopped an emulator with a call in flight")
	}
}

func keepingEmulator(f *pauseFake, keep bool) *Emulator {
	e := NewEmulator(f.adb, EmulatorConfig{AVD: "test_avd", Port: 5554, Dir: "unused", KeepState: func() bool { return keep }}, "", "")
	e.Attach(DeviceInfo{Serial: "emulator-5554", State: "device", Emulator: true})
	return e
}

// emulatorKeepState is opt-in: off, an idle shutdown saves nothing, so the
// next boot returns to the snapshot taken after the first boot.
func TestStopIfIdleSavesNothingWithKeepStateOff(t *testing.T) {
	f := newPauseFake(t, false)
	e := keepingEmulator(f, false)
	if ok, err := e.StopIfIdle(context.Background(), 0); !ok || err != nil {
		t.Fatalf("StopIfIdle = %v, %v", ok, err)
	}
	if calls := f.calls(t); count(calls, "save") != 0 || count(calls, "kill") != 1 {
		t.Fatalf("calls %v, want a kill and no save", calls)
	}
}

// With it on, the state is saved once, before the kill.
func TestStopIfIdleSavesBeforeTheKill(t *testing.T) {
	f := newPauseFake(t, false)
	e := keepingEmulator(f, true)
	if ok, err := e.StopIfIdle(context.Background(), 0); !ok || err != nil {
		t.Fatalf("StopIfIdle = %v, %v", ok, err)
	}
	calls := f.calls(t)
	save, kill := -1, -1
	for i, c := range calls {
		switch c {
		case "save":
			save = i
		case "kill":
			kill = i
		}
	}
	if count(calls, "save") != 1 || save < 0 || kill < save {
		t.Fatalf("calls %v, want one save and then the kill", calls)
	}
}

// A paused emulator is saved through the console without a resume, and
// nothing reaches adbd, where a paused guest would hang.
func TestSaveStateOnAPausedEmulatorDoesNotResume(t *testing.T) {
	f := newPauseFake(t, false)
	e := keepingEmulator(f, true)
	if ok, err := e.PauseIfIdle(context.Background(), 0); !ok || err != nil {
		t.Fatalf("pause: %v %v", ok, err)
	}
	if ok, err := e.StopIfIdle(context.Background(), 0); !ok || err != nil {
		t.Fatalf("StopIfIdle = %v, %v", ok, err)
	}
	calls := f.calls(t)
	if count(calls, "save") != 1 || count(calls, "start") != 0 || count(calls, "shell-while-paused") != 0 {
		t.Fatalf("calls %v, want a save with no start and no shell while paused", calls)
	}
}

func TestResetStopsRemovesTheWarmSnapshotAndWipesTheNextBoot(t *testing.T) {
	f := newPauseFake(t, false)
	dir := t.TempDir()
	warm := filepath.Join(dir, "snapshots", WarmSnapshot)
	if err := os.MkdirAll(warm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(warm, "ram.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewEmulator(f.adb, EmulatorConfig{AVD: "test_avd", Port: 5554, Dir: dir, KeepState: func() bool { return true }}, "", "")
	e.Attach(DeviceInfo{Serial: "emulator-5554", State: "device", Emulator: true})

	if err := e.ResetState(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	if count(calls, "kill") != 1 || count(calls, "save") != 0 {
		t.Errorf("calls %v; want one kill and no save, even with KeepState on", calls)
	}
	if _, err := os.Stat(warm); !os.IsNotExist(err) {
		t.Errorf("loupe-warm still there: %v", err)
	}
	if snap, wipe := bootFrom(dir); snap != "" || !wipe {
		t.Errorf("bootFrom after a reset = %q, %v; want a wipe, since a cold boot alone keeps the disk as the last session left it", snap, wipe)
	}
}

func TestBootFromPrefersTheWarmSnapshotUntilAResetAsksForAWipe(t *testing.T) {
	dir := t.TempDir()
	if snap, wipe := bootFrom(dir); snap != "" || wipe {
		t.Errorf("empty dir: %q, %v; want a plain cold boot", snap, wipe)
	}
	warm := filepath.Join(dir, "snapshots", WarmSnapshot)
	if err := os.MkdirAll(warm, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snapshot.pb", "ram.bin"} {
		if err := os.WriteFile(filepath.Join(warm, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if snap, wipe := bootFrom(dir); snap != WarmSnapshot || wipe {
		t.Errorf("with loupe-warm: %q, %v; want loupe-warm", snap, wipe)
	}
	if err := os.WriteFile(filepath.Join(dir, wipeMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if snap, wipe := bootFrom(dir); snap != "" || !wipe {
		t.Errorf("with a wipe pending: %q, %v; want a wipe and no snapshot", snap, wipe)
	}
}

// An emulator stopped behind this server's back, or by a stop that reported
// a failure and paused it anyway, answers the next stop with "already
// stopped". That must read as paused, so the next call resumes it: on
// Pixel_7a it did not, and every open waited on adbd for frames.
func TestAlreadyStoppedReadsAsPaused(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	if _, done, err := e.running(); err != nil {
		t.Fatalf("running: %v", err)
	} else {
		done()
	}
	if err := os.WriteFile(f.flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.pause.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())
	if ok, err := e.PauseIfIdle(context.Background(), time.Minute); ok || err != nil {
		t.Fatalf("PauseIfIdle = %v, %v, want not paused by us and no error", ok, err)
	}
	if !e.Paused() {
		t.Fatal("Paused() = false after the console said already stopped")
	}
	if _, done, err := e.running(); err != nil {
		t.Fatalf("running after: %v", err)
	} else {
		done()
	}
	calls := f.calls(t)
	if n := count(calls, "start"); n != 1 {
		t.Fatalf("start sent %d times, want 1: %v", n, calls)
	}
	if n := count(calls, "shell-while-paused"); n != 0 {
		t.Fatalf("adbd reached while paused: %v", calls)
	}
}

// A stop that reports a failure may still have paused the VM, so the next
// wake asks the console rather than trusting what it last knew.
func TestFailedStopAsksTheConsoleOnWake(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	if _, done, err := e.running(); err != nil {
		t.Fatalf("running: %v", err)
	} else {
		done()
	}
	if err := os.WriteFile(f.flag+".stopfail", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.pause.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())
	if ok, err := e.PauseIfIdle(context.Background(), time.Minute); ok || err == nil {
		t.Fatalf("PauseIfIdle = %v, %v, want an error", ok, err)
	}
	before := count(f.calls(t), "status")
	if _, done, err := e.running(); err != nil {
		t.Fatalf("running after: %v", err)
	} else {
		done()
	}
	calls := f.calls(t)
	if count(calls, "status") != before+1 {
		t.Fatalf("wake did not ask the console: %v", calls)
	}
	if n := count(calls, "start"); n != 1 {
		t.Fatalf("start sent %d times, want 1: %v", n, calls)
	}
	if n := count(calls, "shell-while-paused"); n != 0 {
		t.Fatalf("adbd reached while paused: %v", calls)
	}
}

// A VM resumed outside this server while it read paused answers the next
// `avd start` with "already running". That must read as resumed: on
// Pixel_7a every open failed with a 502 because wake returned the KO as an
// error and left paused set.
func TestAlreadyRunningReadsAsResumed(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	e.pause.paused.Store(true)
	e.pause.known.Store(true)
	if _, done, err := e.running(); err != nil {
		t.Fatalf("running: %v", err)
	} else {
		done()
	}
	if e.Paused() {
		t.Fatal("Paused() = true after the console said already running")
	}
	calls := f.calls(t)
	if n := count(calls, "start"); n != 1 {
		t.Fatalf("start sent %d times, want 1: %v", n, calls)
	}
}

// A VM stopped outside this server, after it already knew the VM was
// running, is resumed by the next call before it reaches adbd. Before, wake
// trusted what it knew and every adbd call hung until something else sent
// `avd start`.
func TestExternalStopIsResumedBeforeTheNextCall(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	if _, done, err := e.running(); err != nil {
		t.Fatalf("running: %v", err)
	} else {
		done()
	}
	if !e.pause.known.Load() || e.Paused() {
		t.Fatalf("known = %v, paused = %v after a call to a running VM", e.pause.known.Load(), e.Paused())
	}
	// `adb emu avd stop` from a terminal.
	if err := os.WriteFile(f.flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, done, err := e.running(); err != nil {
		t.Fatalf("running after the stop: %v", err)
	} else {
		done()
	}
	if _, err := os.Stat(f.flag); !os.IsNotExist(err) {
		t.Fatal("VM still stopped after the call")
	}
	calls := f.calls(t)
	if n := count(calls, "start"); n != 1 {
		t.Fatalf("start sent %d times, want 1: %v", n, calls)
	}
	if n := count(calls, "shell-while-paused"); n != 0 {
		t.Fatalf("adbd reached while paused: %v", calls)
	}
}

// Open on an attached emulator goes through the same check, so a stream
// is not pushed at a VM someone else stopped.
func TestOpenResumesAnExternallyStoppedVM(t *testing.T) {
	f := newPauseFake(t, false)
	e := attachedEmulator(f)
	if _, done, err := e.running(); err != nil {
		t.Fatalf("running: %v", err)
	} else {
		done()
	}
	if err := os.WriteFile(f.flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := e.ensureBooted(context.Background())
	if err != nil {
		t.Fatalf("ensureBooted: %v", err)
	}
	if in == nil {
		t.Fatal("ensureBooted returned no device")
	}
	calls := f.calls(t)
	if n := count(calls, "start"); n != 1 {
		t.Fatalf("start sent %d times, want 1: %v", n, calls)
	}
	if n := count(calls, "shell-while-paused"); n != 0 {
		t.Fatalf("adbd reached while paused: %v", calls)
	}
}
