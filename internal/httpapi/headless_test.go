package httpapi

import (
	"context"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// Only a running AVD is marked, and only from the process table: a shut-down
// AVD whose name a stale process still carries stays unmarked, and a target
// that is not an AVD is never looked up.
func TestMarkHeadlessMarksRunningAVDsOnly(t *testing.T) {
	restore := headlessAVDs
	calls := 0
	headlessAVDs = func(context.Context) map[string]bool {
		calls++
		return map[string]bool{"fold": true, "stopped": true}
	}
	t.Cleanup(func() { headlessAVDs = restore })

	adb := target.ADB{Path: "/nonexistent/loupe-test-adb"}
	targets := map[string]target.Target{
		"avd:fold":     target.NewEmulator(adb, target.EmulatorConfig{AVD: "fold"}, "", ""),
		"avd:Pixel_10": target.NewEmulator(adb, target.EmulatorConfig{AVD: "Pixel_10"}, "", ""),
		"avd:stopped":  target.NewEmulator(adb, target.EmulatorConfig{AVD: "stopped"}, "", ""),
		"synthetic":    &target.Synthetic{},
	}
	out := []TargetInfo{
		{ID: "avd:fold"},
		{ID: "avd:Pixel_10"},
		{ID: "avd:stopped", BootsOnOpen: true},
		{ID: "synthetic"},
	}
	(&Manager{}).markHeadless(context.Background(), out, targets)

	want := map[string]bool{"avd:fold": true}
	for _, info := range out {
		if info.Headless != want[info.ID] {
			t.Errorf("%s: Headless = %v, want %v", info.ID, info.Headless, want[info.ID])
		}
	}
	if calls != 1 {
		t.Errorf("read the process table %d times, want once", calls)
	}
}

func TestMarkHeadlessSkipsThePsWithNoAVDRunning(t *testing.T) {
	restore := headlessAVDs
	headlessAVDs = func(context.Context) map[string]bool {
		t.Fatal("read the process table with no AVD running")
		return nil
	}
	t.Cleanup(func() { headlessAVDs = restore })

	adb := target.ADB{Path: "/nonexistent/loupe-test-adb"}
	targets := map[string]target.Target{"avd:a": target.NewEmulator(adb, target.EmulatorConfig{AVD: "a"}, "", "")}
	(&Manager{}).markHeadless(context.Background(), []TargetInfo{{ID: "avd:a", BootsOnOpen: true}}, targets)
}
