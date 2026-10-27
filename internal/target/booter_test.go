package target

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUnattachedEmulatorBootsOnOpen pins the field the review asked for: an
// Emulator that has never gone through ensureBooted -- the state a
// registered-but-never-opened AVD is in for the life of the process, per
// CLAUDE.md's "target list is a snapshot of startup" -- reports true.
func TestUnattachedEmulatorBootsOnOpen(t *testing.T) {
	e := NewEmulator(ADB{Path: "/nonexistent/loupe-test-adb-should-not-run"}, EmulatorConfig{
		AVD:  "test_avd",
		Port: 5554,
	}, "", "")

	if !e.BootsOnOpen() {
		t.Fatal("unattached Emulator.BootsOnOpen() = false, want true")
	}
}

// TestADBTargetIsNotABooter is the other half: a plain device or an already
// running emulator adopted as an ADBTarget never boots anything on Open, so
// it must not satisfy target.Booter at all -- not implement it and return
// false, since a caller that only checks the interface assertion would then
// have no way to distinguish "never boots" from "answered false this time".
func TestADBTargetIsNotABooter(t *testing.T) {
	tg := NewADBTarget(ADB{}, DeviceInfo{Serial: "emulator-5554", State: "device", Emulator: true}, "", "")

	if _, ok := any(tg).(Booter); ok {
		t.Fatal("ADBTarget implements target.Booter, want it not to")
	}
}

// TestBootsOnOpenStaysTrueWhenWaitForBootFails pins ensureBooted's ordering
// directly: Store(true) must run only after WaitForBoot succeeds, in both of
// ensureBooted's branches (already-attached-but-not-yet-booted, and
// freshly-launched). A mutation that hoists either Store ahead of its
// WaitForBoot call would report a failed boot as bootable-and-done, which is
// worse than the stale "still boots on open" this field exists to replace --
// it would tell the drawer to treat a dead AVD as already running.
//
// The fake adb here reports the device already attached ("device" state, the
// first branch) but never answers sys.boot_completed with 1, so
// WaitForBoot's own BootBudget timeout is what ends ensureBooted, not a
// process failure -- pinning the ordering rather than an unrelated error
// path.
func TestBootsOnOpenStaysTrueWhenWaitForBootFails(t *testing.T) {
	dir := t.TempDir()
	fakeADB := filepath.Join(dir, "adb")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  \"devices -l\")\n" +
		"    echo \"List of devices attached\"\n" +
		"    echo \"emulator-5554 device\"\n" +
		"    ;;\n" +
		"  *\"shell getprop sys.boot_completed\"*)\n" +
		"    echo \"0\"\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	e := NewEmulator(ADB{Path: fakeADB}, EmulatorConfig{
		AVD:        "test_avd",
		Port:       5554,
		BootBudget: 50 * time.Millisecond,
	}, "", "")

	if _, err := e.ensureBooted(context.Background()); err == nil {
		t.Fatal("ensureBooted with a boot that never completes: want an error, got nil")
	}

	if e.BootsOnOpen() != true {
		t.Fatal("BootsOnOpen() = false after a failed boot, want true -- attached must not be set when WaitForBoot errors")
	}
}

// TestBootsOnOpenStaysTrueWhenLaunchedBootFails is the other of
// ensureBooted's two branches: nothing is attached at our port yet
// (`devices -l` lists nothing), so ensureBooted takes the fresh-launch path
// -- starts e.cfg.EmulatorPath, then calls WaitForBoot. The test above only
// ever exercised the already-attached branch, so a mutation hoisting THIS
// branch's Store(true) ahead of its WaitForBoot call was not caught by it.
// EmulatorPath is /usr/bin/true so `cmd.Start()` succeeds instantly and
// exits, leaving WaitForBoot to fail on its own BootBudget timeout -- same
// shape as the already-attached case, pinning the ordering rather than an
// unrelated process-start error.
func TestBootsOnOpenStaysTrueWhenLaunchedBootFails(t *testing.T) {
	dir := t.TempDir()
	fakeADB := filepath.Join(dir, "adb")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  \"devices -l\")\n" +
		"    echo \"List of devices attached\"\n" +
		"    ;;\n" +
		"  *\"shell getprop sys.boot_completed\"*)\n" +
		"    echo \"0\"\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	e := NewEmulator(ADB{Path: fakeADB}, EmulatorConfig{
		AVD:          "test_avd",
		Port:         5554,
		BootBudget:   50 * time.Millisecond,
		EmulatorPath: "/usr/bin/true",
	}, "", "")

	if _, err := e.ensureBooted(context.Background()); err == nil {
		t.Fatal("ensureBooted on a launched boot that never completes: want an error, got nil")
	}

	if e.BootsOnOpen() != true {
		t.Fatal("BootsOnOpen() = false after a failed launched boot, want true -- attached must not be set when WaitForBoot errors on the fresh-launch branch")
	}
}
