package android

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// dumpPath is where uiautomator is told to write its XML. /data/local/tmp is
// writable by the shell user on every device and needs no storage permission,
// which /sdcard does on newer Android.
const dumpPath = "/data/local/tmp/loupe-ui.xml"

// Inspect dumps the current view hierarchy.
//
// This deliberately does not go through the live stream. uiautomator reads the
// accessibility tree, which is available whether or not anything is streaming,
// and asking the video path for it would couple a text query to an encoder.
func (t *Device) Inspect(ctx context.Context) (*target.UI, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}
	// Android allows one UiAutomation client at a time and kills the second,
	// so two of loupe's own dumps must not overlap.
	t.inspectMu.Lock()
	defer t.inspectMu.Unlock()
	if a11yDumpEnabled() {
		ui, err := t.inspectA11y(ctx)
		if err == nil || errors.Is(err, target.ErrUIAutomationBusy) || ctx.Err() != nil {
			return ui, err
		}
		log.Printf("a11y dump on %s failed, using uiautomator: %v", t.info.Serial, err)
	}
	return t.inspectUIAutomator(ctx)
}

// inspectUIAutomator is the dump through `uiautomator dump`, the fallback
// when loupe's own (a11ydump.go) cannot run.
func (t *Device) inspectUIAutomator(ctx context.Context) (*target.UI, error) {

	// uiautomator writes the file itself and prints only a status line, so the
	// dump and the read are two commands. Errors arrive on stdout as plain
	// prose rather than a non-zero exit, which is why the output is inspected
	// instead of trusted.
	//
	// The echo is for the one failure that prints nothing at all. `exec` keeps
	// the pid, so $$ -- expanded by sh on the device -- is uiautomator's own,
	// and its crash can be read back out of logcat by that pid.
	out, err := t.adb.Shell(ctx, t.info.Serial,
		"echo "+dumpPIDPrefix+"$$; exec uiautomator dump "+dumpPath)
	pid, out := splitDumpPID(out)
	if err != nil {
		if pid > 0 {
			crash, _ := t.adb.Shell(ctx, t.info.Serial,
				"logcat", "-d", "-b", "crash", "--pid="+strconv.Itoa(pid))
			if dumpCrashIsUIAutomationConflict(crash) {
				return nil, fmt.Errorf("uiautomator dump: %w", target.ErrUIAutomationBusy)
			}
		}
		return nil, fmt.Errorf("uiautomator dump: %w: %s", err, strings.TrimSpace(out))
	}
	if !strings.Contains(out, dumpPath) {
		return nil, fmt.Errorf("uiautomator dump did not produce a hierarchy: %s", strings.TrimSpace(out))
	}

	raw, err := t.adb.ExecOut(ctx, t.info.Serial, "cat", dumpPath)
	if err != nil {
		return nil, err
	}
	return target.ParseUI(raw)
}

const dumpPIDPrefix = "loupe-pid="

// splitDumpPID takes the pid line off the front of the dump's output and
// returns the rest as uiautomator printed it. The pid is 0 when the line is
// missing, which is the device shell failing before echo ran.
func splitDumpPID(out string) (int, string) {
	line, rest, _ := strings.Cut(out, "\n")
	v, ok := strings.CutPrefix(strings.TrimSpace(line), dumpPIDPrefix)
	if !ok {
		return 0, out
	}
	pid, err := strconv.Atoi(v)
	if err != nil {
		return 0, out
	}
	return pid, rest
}

// dumpCrashIsUIAutomationConflict reads the crash buffer for the dump's pid.
// Both halves of the message are required: "already registered!" alone is
// thrown by other registries, and the pid filter already ties it to this
// process.
func dumpCrashIsUIAutomationConflict(crash string) bool {
	for _, line := range strings.Split(crash, "\n") {
		if strings.Contains(line, "IllegalStateException: UiAutomationService") &&
			strings.Contains(line, "already registered") {
			return true
		}
	}
	return false
}
