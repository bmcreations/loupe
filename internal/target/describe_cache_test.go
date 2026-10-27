package target

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDescribeIsCachedForTheTargetsLife pins the plan's caching requirement
// directly against ADBTarget: describing a target must not cost an adb call
// per /api/targets request (trap 33 is exactly this shape of adb connection
// churn, one layer up). A fake "adb" binary counts its own invocations to a
// file, since ADB.Shell always execs a binary rather than going through an
// interface that could be stubbed in-process.
func TestDescribeIsCachedForTheTargetsLife(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "calls")
	if err := os.WriteFile(countFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	fakeADB := filepath.Join(dir, "adb")
	script := "#!/bin/sh\n" +
		"echo x >> \"" + countFile + "\"\n" +
		"echo '[ro.build.version.release]: [17]'\n" +
		"echo '[ro.build.version.sdk]: [37]'\n" +
		"echo '[ro.build.characteristics]: [emulator]'\n"
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	tgt := NewADBTarget(ADB{Path: fakeADB}, DeviceInfo{Serial: "fake-1", State: "device"}, "", "")

	for i := 0; i < 3; i++ {
		d, err := tgt.Describe(context.Background())
		if err != nil {
			t.Fatalf("Describe call %d: %v", i, err)
		}
		if d.OSVersion != "17" || d.APILevel != 37 {
			t.Fatalf("Describe call %d = %+v, want release 17 / sdk 37", i, d)
		}
	}

	calls, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	got := len(calls) // one "x\n" (2 bytes) per invocation
	if got != 2 {
		t.Fatalf("fake adb invoked %d bytes worth of markers across 3 Describe calls, want exactly one invocation (2 bytes)", got)
	}
}

// TestDescribeIsNotCachedOnFailure covers the other half: a target that
// answered late (an AVD still booting, say) must be asked again on the next
// call rather than being stuck reporting nothing for the server's life.
//
// The adb path deliberately does not exist. Describe on a not-ready device
// must return before it ever runs adb, purely off the cached DeviceInfo.State
// -- so this ADB is never invoked as written. But if that Ready() guard were
// ever removed, ADB{Path: "adb"} would happily exec whatever "adb" is first
// on this machine's PATH, against a real device, from a test. Pointing at a
// path nothing resolves makes that failure mode loud (exec: no such file)
// instead of quietly reaching a real adb.
func TestDescribeIsNotCachedOnFailure(t *testing.T) {
	tgt := NewADBTarget(ADB{Path: "/nonexistent/loupe-test-adb-should-not-run"}, DeviceInfo{Serial: "not-ready", State: "offline"}, "", "")

	if _, err := tgt.Describe(context.Background()); err == nil {
		t.Fatal("Describe on a not-ready device: want an error, got nil")
	}
	if _, ok := tgt.describeCache.get(); ok {
		t.Fatal("describeCache has a value after a failed Describe")
	}
}

// TestDescribeIsNotCachedOnIncompleteOutput covers trap-33-adjacent ground
// from the other side: a shell that exits 0 with no stdout at all (adbd
// killed mid-command, a device that dropped between dial and read) must not
// be cached as a Description with every field blank, and must be retried on
// the next call rather than latching an empty answer for the server's life.
func TestDescribeIsNotCachedOnIncompleteOutput(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "calls")
	if err := os.WriteFile(countFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	fakeADB := filepath.Join(dir, "adb")
	script := "#!/bin/sh\n" +
		"echo x >> \"" + countFile + "\"\n" +
		"exit 0\n"
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	tgt := NewADBTarget(ADB{Path: fakeADB}, DeviceInfo{Serial: "fake-1", State: "device"}, "", "")

	if _, err := tgt.Describe(context.Background()); err == nil {
		t.Fatal("Describe on empty getprop output: want an error, got nil")
	}
	if _, ok := tgt.describeCache.get(); ok {
		t.Fatal("describeCache has a value after an incomplete Describe")
	}
	if _, err := tgt.Describe(context.Background()); err == nil {
		t.Fatal("second Describe on empty getprop output: want an error, got nil")
	}

	calls, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(calls); got != 4 {
		t.Fatalf("fake adb invoked %d bytes worth of markers across 2 failed Describe calls, want exactly two invocations (4 bytes) -- a failure must not be cached", got)
	}
}
