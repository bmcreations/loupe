package android

import (
	"os"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

func find(n *target.Node, pred func(*target.Node) bool) *target.Node {
	if n == nil {
		return nil
	}
	if pred(n) {
		return n
	}
	for _, c := range n.Children {
		if got := find(c, pred); got != nil {
			return got
		}
	}
	return nil
}

// The crash buffer for a dump refused while a Maestro driver held
// emulator-5554, captured with `logcat -d -b crash --pid=`. Nothing reached
// stdout or stderr; adb exited 137.
func TestDumpCrashIsUIAutomationConflict(t *testing.T) {
	crash, err := os.ReadFile("testdata/uiautomator-dump-conflict.crash.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !dumpCrashIsUIAutomationConflict(string(crash)) {
		t.Fatal("captured UiAutomation conflict not recognised")
	}

	other := `09-24 22:32:24.264 25170 25170 E AndroidRuntime: FATAL EXCEPTION: main
09-24 22:32:24.264 25170 25170 E AndroidRuntime: java.lang.IllegalStateException: Receiver already registered!`
	for name, log := range map[string]string{"empty": "", "other crash": other} {
		if dumpCrashIsUIAutomationConflict(log) {
			t.Errorf("%s: read as a UiAutomation conflict", name)
		}
	}
}

func TestSplitDumpPID(t *testing.T) {
	cases := []struct {
		name, out, rest string
		pid             int
	}{
		// adb shell over a pty ends lines with \r\n.
		{"dumped", "loupe-pid=25170\r\nUI hierchary dumped to: /data/local/tmp/loupe-ui.xml\r\n",
			"UI hierchary dumped to: /data/local/tmp/loupe-ui.xml\r\n", 25170},
		{"killed with no output", "loupe-pid=25170\n", "", 25170},
		{"no pid line", "sh: exec: not found\n", "sh: exec: not found\n", 0},
		{"empty", "", "", 0},
	}
	for _, c := range cases {
		pid, rest := splitDumpPID(c.out)
		if pid != c.pid || rest != c.rest {
			t.Errorf("%s: got (%d, %q), want (%d, %q)", c.name, pid, rest, c.pid, c.rest)
		}
	}
}
