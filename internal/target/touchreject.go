package target

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrTouchRejected is a touch `input` sent and Android's input dispatcher
// threw away. `input` exits 0 regardless, so without reading the log a tap
// that did nothing reads as one that landed (docs/traps.md, trap 50).
var ErrTouchRejected = errors.New("the device rejected the touch")

// touchLogMarker separates `input`'s own output from the log read after it.
const touchLogMarker = "__loupe_log__"

// touchInput runs `input <args>` and then reads the dispatcher's errors
// logged since just before it. The rejection is logged before `input`
// returns: on emulator-5554 the read straight after found it every time.
// Args are digits and a verb, never caller text, so they go into the script
// as they are.
func (t *ADBTarget) touchInput(ctx context.Context, args ...string) error {
	if !t.info.Ready() {
		return fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}
	// Device time to the millisecond, the form `logcat -v epoch -T` takes.
	script := `s=$(date +%s.%N); s=${s%??????}; input ` + strings.Join(args, " ") +
		`; r=$?; echo ` + touchLogMarker +
		`; logcat -d -v epoch -T "$s" -s InputDispatcher:E; exit $r`
	out, err := t.adb.Shell(ctx, t.info.Serial, script)
	head, log, _ := strings.Cut(out, touchLogMarker)
	if err != nil {
		return fmt.Errorf("input %s: %w: %s", args[0], err, strings.TrimSpace(head))
	}
	if msg := strings.TrimSpace(head); strings.Contains(msg, "Exception") {
		return fmt.Errorf("input %s: %s", args[0], msg)
	}
	if reason, ok := touchRejection(log); ok {
		return fmt.Errorf("input %s: %w: %s", args[0], ErrTouchRejected, reason)
	}
	return nil
}

// touchRejection finds an injected event the dispatcher refused in a
// logcat excerpt and returns its reason.
func touchRejection(log string) (string, bool) {
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "InputDispatcher") || !strings.Contains(line, "Inconsistent event") {
			continue
		}
		_, reason, ok := strings.Cut(line, "reason: ")
		if !ok {
			continue
		}
		// "Injection on 0: <why>" names the display; the why is what helps.
		if _, why, ok := strings.Cut(reason, ": "); ok && strings.HasPrefix(reason, "Injection") {
			reason = why
		}
		return strings.TrimSpace(reason), true
	}
	return "", false
}
