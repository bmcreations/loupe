package target

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Both lines are from `ps -axww -o pid=,args=` on this machine: Pixel_10 as
// Android Studio starts it, and acmechat_blockstore as the emulator launcher
// starts it.
const psSample = `  501 /usr/libexec/logd
96811 /Users/me/Library/Android/sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64 @Pixel_10 -no-snapshot
56314 /Users/me/Library/Android/sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64-headless -avd acmechat_blockstore -port 5556 -no-window
70001 /usr/bin/vim notes-about-@Pixel_10
70002 /Users/me/Library/Android/sdk/emulator/emulator -avd Pixel_10_Pro -port 5558
`

func TestRunningAVDPidFindsBothSpellings(t *testing.T) {
	ps := parseProcesses(psSample)
	restore := listProcesses
	listProcesses = func(context.Context) ([]process, error) { return ps, nil }
	t.Cleanup(func() { listProcesses = restore })

	for name, want := range map[string]int{
		"Pixel_10":            96811,
		"acmechat_blockstore": 56314,
		"Pixel_10_Pro":        70002,
		"Pixel_9":             0,
	} {
		if got := runningAVDPid(context.Background(), name); got != want {
			t.Errorf("runningAVDPid(%q) = %d, want %d", name, got, want)
		}
	}
}

// The 2026-09-27 shape: Pixel_10 runs, adb resets every connection, and Open
// used to start a second Pixel_10. It must refuse, say why, and start nothing.
func TestEnsureBootedRefusesAnAVDRunningWithoutAdb(t *testing.T) {
	dir := t.TempDir()
	fakeADB := filepath.Join(dir, "adb")
	script := "#!/bin/sh\n" +
		"echo \"adb: protocol fault (couldn't read status): Connection reset by peer\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(dir, "started")
	fakeEmulator := filepath.Join(dir, "emulator")
	if err := os.WriteFile(fakeEmulator, []byte("#!/bin/sh\ntouch "+started+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	restore := listProcesses
	listProcesses = func(context.Context) ([]process, error) { return parseProcesses(psSample), nil }
	t.Cleanup(func() { listProcesses = restore })

	e := NewEmulator(ADB{Path: fakeADB}, EmulatorConfig{
		AVD:          "Pixel_10",
		Port:         5554,
		BootBudget:   50 * time.Millisecond,
		EmulatorPath: fakeEmulator,
	}, "", "")

	_, err := e.ensureBooted(context.Background())
	if err == nil {
		t.Fatal("ensureBooted with Pixel_10 already running: want an error, got nil")
	}
	for _, want := range []string{"Pixel_10 is already running", "pid 96811", "adb cannot reach it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not contain %q", err, want)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if _, statErr := os.Stat(started); statErr == nil {
		t.Error("a second emulator was started for an AVD that is already running")
	}
}

// With adb answering but not yet listing the emulator as ready, the error
// says to wait rather than blaming adb.
func TestEnsureBootedRefusesAnAVDStillBootingElsewhere(t *testing.T) {
	dir := t.TempDir()
	fakeADB := filepath.Join(dir, "adb")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  \"devices -l\") echo \"List of devices attached\"; echo \"emulator-5554 offline\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	restore := listProcesses
	listProcesses = func(context.Context) ([]process, error) { return parseProcesses(psSample), nil }
	t.Cleanup(func() { listProcesses = restore })

	e := NewEmulator(ADB{Path: fakeADB}, EmulatorConfig{AVD: "Pixel_10", Port: 5554, EmulatorPath: "/nonexistent/emulator"}, "", "")
	_, err := e.ensureBooted(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wait for it to finish booting") {
		t.Fatalf("err = %v, want it to say the running AVD is still booting", err)
	}
}

// Pixel_10 is Android Studio's, with its window. acmechat_blockstore runs the
// headless qemu, and Pixel_10_Pro is still only the launcher, which says
// nothing about the window qemu will have.
func TestHeadlessAVDsReadsTheQemuBinary(t *testing.T) {
	restore := listProcesses
	listProcesses = func(context.Context) ([]process, error) { return parseProcesses(psSample), nil }
	t.Cleanup(func() { listProcesses = restore })

	got := HeadlessAVDs(context.Background())
	if len(got) != 1 || !got["acmechat_blockstore"] {
		t.Fatalf("HeadlessAVDs = %v, want only acmechat_blockstore", got)
	}
}

func TestQemuAVDCountsNoWindowOnTheWindowedBinary(t *testing.T) {
	name, headless := qemuAVD([]string{"/sdk/qemu-system-aarch64", "-avd", "Pixel_9", "-no-window"})
	if name != "Pixel_9" || !headless {
		t.Fatalf("qemuAVD = %q, %v; want Pixel_9, true", name, headless)
	}
}

// The argv qemu read when loupe booted a throwaway AVD with its window hidden.
// Missing it would keep every AVD this server boots out of the idle pause and
// shutdown.
func TestQemuAVDCountsAHiddenWindowAsHeadless(t *testing.T) {
	name, headless := qemuAVD(strings.Fields("/Users/me/Library/Android/sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64 -avd loupe_fps -port 5600 -gpu host -qt-hide-window -no-audio -snapshot loupe-warm -no-snapshot-save -no-boot-anim"))
	if name != "loupe_fps" || !headless {
		t.Fatalf("qemuAVD = %q, %v; want loupe_fps, true", name, headless)
	}
}
