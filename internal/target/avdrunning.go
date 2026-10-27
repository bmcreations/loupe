package target

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// process is one row of the process table: its pid and its argv.
type process struct {
	pid  int
	args []string
}

// listProcesses reads the process table. A package var so a test can supply
// one without a real emulator running.
var listProcesses = func(ctx context.Context) ([]process, error) {
	// -ww so a long SDK path does not truncate the argv the AVD name is in.
	out, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,args=").Output()
	if err != nil {
		return nil, err
	}
	return parseProcesses(string(out)), nil
}

func parseProcesses(out string) []process {
	var ps []process
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		ps = append(ps, process{pid: pid, args: f[1:]})
	}
	return ps
}

// runningAVDPid returns the pid of an emulator process running the named AVD,
// or 0 if there is none.
//
// This is the process table rather than adb, because it is for when adb
// cannot answer. On 2026-09-27 a wedged adb server reset every connection, so
// Pixel_10 on emulator-5554 was never attached and every Open tried to boot a
// second Pixel_10, which the emulator refuses; the viewer read that as a boot
// failure about -gpu host.
//
// Not the AVD's lock files. hardware-qemu.ini.lock does hold qemu's pid, but
// it and multiinstance.lock survive an emulator that crashed, so a lock file
// alone would refuse to boot an AVD nothing is running.
//
// qemu is started as `qemu-system-<arch> @Pixel_10` by Android Studio and as
// `qemu-system-<arch>-headless -avd acmechat_blockstore` by the emulator
// launcher, so both spellings count. The launcher itself counts too, since it
// is what is still running while qemu starts.
func runningAVDPid(ctx context.Context, name string) int {
	ps, err := listProcesses(ctx)
	if err != nil {
		return 0
	}
	for _, p := range ps {
		if runsAVD(p.args, name) {
			return p.pid
		}
	}
	return 0
}

func runsAVD(args []string, name string) bool {
	if len(args) == 0 {
		return false
	}
	exe := filepath.Base(args[0])
	if !strings.HasPrefix(exe, "qemu-system") && exe != "emulator" {
		return false
	}
	for i, a := range args[1:] {
		if a == "@"+name {
			return true
		}
		if a == "-avd" && i+2 < len(args) && args[i+2] == name {
			return true
		}
	}
	return false
}

// HeadlessAVDs returns the AVDs whose emulator runs with no window, by name,
// read from the process table. A headless emulator's qemu is the
// `qemu-system-<arch>-headless` binary, and one started with -no-window or
// -qt-hide-window counts even if it is not; the second is how loupe boots one. Only qemu says: the launcher is gone once qemu is up. An
// AVD that is not running, or runs with its window, is absent.
//
// This is how an emulator loupe did not start is told apart, since only the
// process knows how it was started: Android Studio's Pixel_10 runs with its
// window whatever emulatorWindow says.
func HeadlessAVDs(ctx context.Context) map[string]bool {
	ps, err := listProcesses(ctx)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, p := range ps {
		if name, headless := qemuAVD(p.args); name != "" && headless {
			out[name] = true
		}
	}
	return out
}

// qemuAVD reads the AVD name out of a qemu argv, and whether it runs without
// a window. Anything but qemu answers "".
func qemuAVD(args []string) (name string, headless bool) {
	if len(args) == 0 {
		return "", false
	}
	exe := filepath.Base(args[0])
	if !strings.HasPrefix(exe, "qemu-system") {
		return "", false
	}
	headless = strings.HasSuffix(exe, "-headless")
	for i, a := range args[1:] {
		switch {
		case strings.HasPrefix(a, "@") && name == "":
			name = a[1:]
		case a == "-avd" && i+2 < len(args) && name == "":
			name = args[i+2]
		case a == "-no-window", a == "-qt-hide-window":
			headless = true
		}
	}
	return name, headless
}
