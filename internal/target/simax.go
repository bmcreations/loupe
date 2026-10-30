package target

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// errAXNoFrame is a tree whose root came back with no frame: an app with no
// accessibility loaded, or one still coming up.
var errAXNoFrame = errors.New("accessibility root has no frame")

// axPrefs are the com.apple.Accessibility keys that make apps on a simulator
// load accessibility. An XCUITest run sets them and turns them off again
// when it ends, after which every app answers with an empty root and its
// axserver port is unknown ("AX Lookup problem - errorCode:1102 ... Unknown
// service name portName:'com.apple.iphone.axserver'"). See traps.md, 60.
var axPrefs = []string{"AccessibilityEnabled", "ApplicationAccessibilityEnabled"}

// axNotifications tell running processes to reread those keys.
var axNotifications = []string{"com.apple.accessibility.cache.ax", "com.apple.accessibility.cache.app.ax"}

// axSettle is how long apps get to load accessibility once it is turned on.
const axSettle = time.Second

// enableAppAccessibility turns axPrefs on where any is off, and reports
// whether it changed anything. A simulator whose keys are already on is
// left alone: its empty root is an app still launching.
func (s *Simulator) enableAppAccessibility(ctx context.Context) (bool, error) {
	spawn := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "xcrun", append([]string{"simctl", "spawn", s.UDID()}, args...)...).Output()
	}
	changed := false
	for _, k := range axPrefs {
		out, _ := spawn("defaults", "read", "com.apple.Accessibility", k)
		if axPrefOn(string(out)) {
			continue
		}
		if _, err := spawn("defaults", "write", "com.apple.Accessibility", k, "-bool", "true"); err != nil {
			return changed, fmt.Errorf("turn on %s: %w", k, err)
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	for _, n := range axNotifications {
		_, _ = spawn("notifyutil", "-p", n)
	}
	return true, sleepCtx(ctx, axSettle)
}

// axPrefOn reads `defaults read`'s answer for a boolean key.
func axPrefOn(out string) bool {
	v := strings.TrimSpace(out)
	return v == "1" || strings.EqualFold(v, "true")
}
