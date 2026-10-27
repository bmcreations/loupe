package target

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestEmulatorDescribeErrorsOnAnUnbootedAVDWithNoADBCall pins the review
// requirement directly: describing a registered AVD that has never been
// booted (Open/ensureBooted never ran) must return an error without running
// adb at all -- not adb devices, not adb shell. There is nothing attached
// yet, so there is nothing worth asking; and unlike ADBTarget, which always
// has a DeviceInfo.State to consult for free, an Emulator before its first
// Open has no live state, so the only way to answer "is it ready" without an
// adb call is to not ask until ensureBooted has actually recorded an
// attachment.
func TestEmulatorDescribeErrorsOnAnUnbootedAVDWithNoADBCall(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "calls")
	if err := os.WriteFile(countFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fakeADB := filepath.Join(dir, "adb")
	script := "#!/bin/sh\necho x >> \"" + countFile + "\"\nexit 1\n"
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	e := NewEmulator(ADB{Path: fakeADB}, EmulatorConfig{AVD: "test_avd", Port: 5554}, "", "")

	if _, err := e.Describe(context.Background()); err == nil {
		t.Fatal("Describe on an unbooted AVD: want an error, got nil")
	}

	calls, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("fake adb was invoked (%d bytes of markers) describing an unbooted AVD, want zero adb calls", len(calls))
	}
}

// TestEmulatorDescribeAfterEnsureBootedDescribes covers the other half: once
// ensureBooted has recorded the emulator as attached and ready -- the same
// state Open reaches before it ever streams -- Describe goes through the same
// getprop path ADBTarget uses, with its own cache. ensureBooted is called
// directly (same package) rather than through Open, so this does not launch
// scrcpy or touch anything beyond adb devices / adb shell.
func TestEmulatorDescribeAfterEnsureBootedDescribes(t *testing.T) {
	dir := t.TempDir()
	fakeADB := filepath.Join(dir, "adb")
	script := `#!/bin/sh
case "$*" in
  "devices -l")
    echo "List of devices attached"
    echo "emulator-5554	device"
    ;;
  *"shell getprop sys.boot_completed"*)
    echo "1"
    ;;
  *"shell getprop ro.boot.qemu.avd_name"*)
    echo "test_avd"
    ;;
  *"shell getprop"*)
    echo '[ro.build.version.release]: [17]'
    echo '[ro.build.version.sdk]: [37]'
    echo '[ro.build.characteristics]: [emulator]'
    ;;
esac
`
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	e := NewEmulator(ADB{Path: fakeADB}, EmulatorConfig{AVD: "test_avd", Port: 5554}, "", "")
	if _, err := e.ensureBooted(context.Background()); err != nil {
		t.Fatalf("ensureBooted: %v", err)
	}

	d, err := e.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.OS != "android" || d.OSVersion != "17" || d.APILevel != 37 || d.Form != "phone" {
		t.Fatalf("Describe = %+v, want android/17/37/phone", d)
	}
}

// TestEmulatorDescribeNeverCallsWaitForBootOrLaunchesAnEmulator guards the
// "never call ensureBooted or boot anything" requirement from the other
// direction: pointed at an EmulatorPath that does not exist, Describe on an
// unattached emulator must still just error rather than trying (and failing
// loudly, or worse, succeeding) to start a process.
func TestEmulatorDescribeNeverCallsWaitForBootOrLaunchesAnEmulator(t *testing.T) {
	e := NewEmulator(ADB{Path: "/nonexistent/loupe-test-adb-should-not-run"}, EmulatorConfig{
		AVD:          "test_avd",
		Port:         5554,
		EmulatorPath: "/nonexistent/loupe-test-emulator-should-not-run",
	}, "", "")

	if _, err := e.Describe(context.Background()); err == nil {
		t.Fatal("Describe on an unattached emulator: want an error, got nil")
	}
}
