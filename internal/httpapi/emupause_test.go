package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// pauseADB is an adb whose console logs each `avd stop` with the serial it
// went to, and reports every emulator running.
func pauseADB(t *testing.T) (target.ADB, func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "stops")
	script := `#!/bin/sh
case "$*" in
  *"emu avd stop"*) echo "$2" >> "` + log + `"; echo OK ;;
  *"emu avd status"*) echo "virtual device is running"; echo OK ;;
esac
`
	bin := filepath.Join(dir, "adb")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return target.ADB{Path: bin}, func() []string {
		b, _ := os.ReadFile(log)
		return strings.Fields(string(b))
	}
}

func attachedEmulator(m *Manager, adb target.ADB, avd, serial string) *target.Emulator {
	e := target.NewEmulator(adb, target.EmulatorConfig{AVD: avd}, "", "")
	e.Attach(target.DeviceInfo{Serial: serial, State: "device"})
	m.Register(e)
	return e
}

// Only the headless emulator with no session is paused: a windowed one may
// be on someone's screen, and one with a session is being watched.
func TestPauseIdlePausesOnlyHeadlessEmulatorsWithoutASession(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool {
		return map[string]bool{"idle": true, "watched": true}
	}
	t.Cleanup(func() { headlessAVDs = restore })

	adb, stops := pauseADB(t)
	m := NewManager(target.Options{})
	idle := attachedEmulator(m, adb, "idle", "emulator-5554")
	windowed := attachedEmulator(m, adb, "windowed", "emulator-5556")
	watched := attachedEmulator(m, adb, "watched", "emulator-5558")
	m.mu.Lock()
	m.sessions[watched.ID()] = nil
	m.mu.Unlock()
	m.SetEmulatorPause(time.Nanosecond)
	time.Sleep(time.Millisecond)

	m.pauseIdle(context.Background())

	if got := stops(); len(got) != 1 || got[0] != "emulator-5554" {
		t.Fatalf("stopped %v, want only emulator-5554", got)
	}
	if !idle.Paused() || windowed.Paused() || watched.Paused() {
		t.Errorf("paused: idle %v windowed %v watched %v, want only idle",
			idle.Paused(), windowed.Paused(), watched.Paused())
	}
}

func TestPauseIdleOffPausesNothing(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool { return map[string]bool{"idle": true} }
	t.Cleanup(func() { headlessAVDs = restore })

	adb, stops := pauseADB(t)
	m := NewManager(target.Options{})
	attachedEmulator(m, adb, "idle", "emulator-5554")
	m.SetEmulatorPause(0)
	time.Sleep(time.Millisecond)

	m.pauseIdle(context.Background())

	if got := stops(); len(got) != 0 {
		t.Fatalf("stopped %v with the setting off", got)
	}
}

// A session restarts the clock, so the emulator is not paused the moment
// the last viewer leaves.
func TestPauseIdleRestartsTheClockWhileASessionIsOpen(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool { return map[string]bool{"idle": true} }
	t.Cleanup(func() { headlessAVDs = restore })

	adb, stops := pauseADB(t)
	m := NewManager(target.Options{})
	e := attachedEmulator(m, adb, "idle", "emulator-5554")
	m.SetEmulatorPause(time.Hour)
	m.mu.Lock()
	m.sessions[e.ID()] = nil
	m.mu.Unlock()
	before := e.IdleFor(time.Now())
	time.Sleep(5 * time.Millisecond)

	m.pauseIdle(context.Background())

	if after := e.IdleFor(time.Now()); after >= before+5*time.Millisecond {
		t.Errorf("idle went from %v to %v, want the clock restarted", before, after)
	}
	if got := stops(); len(got) != 0 {
		t.Fatalf("stopped %v with a session open", got)
	}
}

// After a restart the first listing is what asks the console about an
// emulator the previous process paused, so the row reads paused at once.
func TestTargetsReportsAPauseLeftByAPreviousProcess(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool { return map[string]bool{"left": true} }
	t.Cleanup(func() { headlessAVDs = restore })

	dir := t.TempDir()
	bin := filepath.Join(dir, "adb")
	script := `#!/bin/sh
case "$*" in
  *"emu avd status"*) echo "virtual device is stopped"; echo OK ;;
  *) sleep 5 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(target.Options{})
	e := attachedEmulator(m, target.ADB{Path: bin}, "left", "emulator-5554")

	for _, info := range m.Targets(context.Background()) {
		if info.ID == e.ID() && !info.Paused {
			t.Fatalf("row reads paused false for an emulator the console says is stopped: %+v", info)
		}
	}
}

// killADB is an adb whose console logs each `emu kill` and `avd stop` with
// the serial it went to, and drops a serial from get-state once killed. A
// kill waits for release to be closed, when it is not nil.
func killADB(t *testing.T, release chan struct{}) (target.ADB, func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	gate := filepath.Join(dir, "gate")
	wait := ""
	if release != nil {
		wait = `while [ ! -e "` + gate + `" ]; do sleep 0.01; done; `
		go func() {
			<-release
			_ = os.WriteFile(gate, nil, 0o644)
		}()
	}
	script := `#!/bin/sh
case "$*" in
  *"emu kill"*) ` + wait + `echo "kill:$2" >> "` + log + `"; : > "` + dir + `/killed-$2"; echo OK ;;
  *"emu avd stop"*) echo "stop:$2" >> "` + log + `"; echo OK ;;
  *"emu avd status"*) echo "virtual device is running"; echo OK ;;
  *get-state*) [ -e "` + dir + `/killed-$2" ] && exit 1; echo device ;;
esac
`
	bin := filepath.Join(dir, "adb")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return target.ADB{Path: bin}, func() []string {
		b, _ := os.ReadFile(log)
		return strings.Fields(string(b))
	}
}

// An idle headless emulator is shut down, a windowed one is not, and the
// guard is gone afterwards, so a plain open boots it again rather than
// being refused as shut down.
func TestPauseIdleShutsDownAnIdleHeadlessEmulatorAndLeavesItOpenable(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool { return map[string]bool{"idle": true} }
	t.Cleanup(func() { headlessAVDs = restore })

	adb, calls := killADB(t, nil)
	m := NewManager(target.Options{})
	idle := attachedEmulator(m, adb, "idle", "emulator-5554")
	windowed := attachedEmulator(m, adb, "windowed", "emulator-5556")
	m.SetEmulatorShutdown(time.Nanosecond)
	time.Sleep(time.Millisecond)

	m.pauseIdle(context.Background())

	if got := calls(); len(got) != 1 || got[0] != "kill:emulator-5554" {
		t.Fatalf("calls %v, want one kill of emulator-5554", got)
	}
	if idle.Serial() != "" || windowed.Serial() == "" {
		t.Errorf("serials after: idle %q windowed %q, want idle detached only", idle.Serial(), windowed.Serial())
	}
	m.mu.Lock()
	_, guarded := m.shutAt[idle.ID()]
	busy := m.lifeBusy[idle.ID()]
	m.mu.Unlock()
	if guarded || busy {
		t.Errorf("after an idle stop: guard %v busy %v, want neither", guarded, busy)
	}
}

// While the kill runs, an open is refused as shut down and a picker Shut
// Down is told the target is busy, instead of either racing the kill.
func TestAnIdleShutdownInFlightRefusesOpensAndPickerShutdowns(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool { return map[string]bool{"idle": true} }
	t.Cleanup(func() { headlessAVDs = restore })

	release := make(chan struct{})
	adb, _ := killADB(t, release)
	m := NewManager(target.Options{})
	e := attachedEmulator(m, adb, "idle", "emulator-5554")
	m.SetEmulatorShutdown(time.Nanosecond)
	time.Sleep(time.Millisecond)

	done := make(chan struct{})
	go func() { m.pauseIdle(context.Background()); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !m.lifeInFlight(e.ID()) {
		if time.Now().After(deadline) {
			t.Fatal("the idle stop never claimed the target")
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, err := m.Acquire(context.Background(), e.ID())
	var refused *refusedError
	if !errors.As(err, &refused) || refused.reason != "shutDown" {
		t.Errorf("Acquire during the kill = %v, want refused as shutDown", err)
	}
	if m.lifeBegin(e.ID()) {
		t.Error("a second shutdown claimed the target during the idle stop")
	}
	close(release)
	<-done
	if m.lifeInFlight(e.ID()) {
		t.Error("the target is still claimed after the idle stop")
	}
}

// With the shutdown shorter than the pause, the emulator is stopped without
// being paused first.
func TestPauseIdleShutsDownBeforePausingWhenTheShutdownIsShorter(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool { return map[string]bool{"idle": true} }
	t.Cleanup(func() { headlessAVDs = restore })

	adb, calls := killADB(t, nil)
	m := NewManager(target.Options{})
	attachedEmulator(m, adb, "idle", "emulator-5554")
	m.SetEmulatorPause(time.Hour)
	m.SetEmulatorShutdown(time.Nanosecond)
	time.Sleep(time.Millisecond)

	m.pauseIdle(context.Background())

	if got := calls(); len(got) != 1 || got[0] != "kill:emulator-5554" {
		t.Fatalf("calls %v, want a kill and no pause", got)
	}
}

func TestPauseIdleWithShutdownOffStopsNothing(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool { return map[string]bool{"idle": true} }
	t.Cleanup(func() { headlessAVDs = restore })

	adb, calls := killADB(t, nil)
	m := NewManager(target.Options{})
	attachedEmulator(m, adb, "idle", "emulator-5554")
	m.SetEmulatorShutdown(0)
	time.Sleep(time.Millisecond)

	m.pauseIdle(context.Background())

	if got := calls(); len(got) != 0 {
		t.Fatalf("calls %v with both settings off", got)
	}
}

// An agent's claim holds the emulator awake: raw adb from the agent's shell
// hangs on a paused VM, and a claimed one is not shut down under it either.
func TestPauseIdleSkipsAClaimedEmulatorUntilTheClaimLapses(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool {
		return map[string]bool{"claimed": true, "free": true}
	}
	t.Cleanup(func() { headlessAVDs = restore })

	adb, stops := pauseADB(t)
	m := NewManager(target.Options{})
	claimed := attachedEmulator(m, adb, "claimed", "emulator-5554")
	free := attachedEmulator(m, adb, "free", "emulator-5556")
	holds := true
	m.SetClaimed(func(id string) bool { return holds && id == claimed.ID() })
	m.SetEmulatorPause(300 * time.Millisecond)
	time.Sleep(350 * time.Millisecond)

	m.pauseIdle(context.Background())

	if got := stops(); len(got) != 1 || got[0] != "emulator-5556" {
		t.Fatalf("stopped %v, want only the unclaimed emulator-5556", got)
	}
	if claimed.Paused() || !free.Paused() {
		t.Fatalf("paused: claimed %v free %v, want only free", claimed.Paused(), free.Paused())
	}

	// The skip restarted the clock, so a lapsed claim does not pause it at once.
	holds = false
	m.pauseIdle(context.Background())
	if claimed.Paused() {
		t.Fatal("paused right after the claim lapsed; the idle clock should have restarted")
	}
	time.Sleep(350 * time.Millisecond)
	m.pauseIdle(context.Background())
	if !claimed.Paused() {
		t.Fatal("not paused a full period after the claim lapsed")
	}
}

func TestPauseIdleSkipsAClaimedEmulatorForShutdownToo(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool { return map[string]bool{"claimed": true} }
	t.Cleanup(func() { headlessAVDs = restore })

	adb, calls := killADB(t, nil)
	m := NewManager(target.Options{})
	e := attachedEmulator(m, adb, "claimed", "emulator-5554")
	m.SetClaimed(func(id string) bool { return id == e.ID() })
	m.SetEmulatorShutdown(time.Nanosecond)
	time.Sleep(time.Millisecond)

	m.pauseIdle(context.Background())

	if got := calls(); len(got) != 0 {
		t.Fatalf("calls %v, want none for a claimed emulator", got)
	}
}
