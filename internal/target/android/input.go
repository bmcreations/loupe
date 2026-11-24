package android

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// ScreenSize is guaranteed to implement ScreenSizer; a target that stops
// doing so fails the build here rather than an agent silently losing
// coordinates on every event (opus review item 2).
var _ target.ScreenSizer = (*Device)(nil)

// ScreenSize is the display in device pixels as currently oriented: the
// natural, unrotated size dumpsys reports for display 0, with the axes
// swapped when the device is in landscape.
//
// Both the size and the rotation come from one dumpsys window displays call
// (ADB.ScreenGeometry) rather than a cached `wm size` plus a per-call
// rotation read. Caching bought nothing worth having: `wm size` can change
// under an override, and the same serial can come back as a different AVD
// after a restart, so a size cached at construction could answer for a
// device that is no longer there. Reading it fresh alongside the rotation,
// off the one dumpsys call the rotation already pays for, costs no more and
// cannot go stale either way.
func (t *Device) ScreenSize(ctx context.Context) (int, int, error) {
	w, h, rotation, rotErr, err := t.adb.ScreenGeometry(ctx, t.info.Serial)
	if err != nil {
		return 0, 0, err
	}
	return orientedSize(w, h, rotation, rotErr)
}

// orientedSize swaps a natural, unrotated size to match the current
// rotation. Android's Surface.ROTATION_90 and ROTATION_270 (1 and 3) are the
// two landscape orientations; 0 and 2 (upside down) keep the natural axes.
//
// A failed rotation read fails ScreenSize outright rather than falling back
// to the unswapped size: publishing an unswapped landscape tap as though it
// were portrait puts a wrong fraction on the event log and says nothing about
// it, which is worse than omitting the coordinate the way agentScreenSize
// already does for every other unknown-size case. The one exception is a
// square screen, where swapping would not have changed the answer anyway.
func orientedSize(w, h, rotation int, rotErr error) (int, int, error) {
	if rotErr != nil {
		if w == h {
			return w, h, nil
		}
		return 0, 0, fmt.Errorf("orient %dx%d: %w", w, h, rotErr)
	}
	if rotation == 1 || rotation == 3 {
		return h, w, nil
	}
	return w, h, nil
}

func (t *Device) Tap(ctx context.Context, x, y int) error {
	return t.touchInput(ctx, "tap", strconv.Itoa(x), strconv.Itoa(y))
}

// Swipe duration is the caller's responsibility now: handleInput resolves a
// zero ms to the 300ms default before calling this, so the value passed here
// is the value it publishes to the event log (opus review item 6) -- this no
// longer silently substitutes one of its own.
func (t *Device) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error {
	return t.touchInput(ctx, "swipe",
		strconv.Itoa(x1), strconv.Itoa(y1),
		strconv.Itoa(x2), strconv.Itoa(y2), strconv.Itoa(ms))
}

// TypeText goes through an open stream's control socket when there is one,
// and through `adb shell input text` when there is not. Both type through
// the virtual keyboard's key-character map, so they accept the same
// characters; the socket skips a few hundred milliseconds of adb per call,
// which is the difference between typing and waiting for it.
func (t *Device) TypeText(ctx context.Context, s string) error {
	if s == "" {
		return nil
	}
	if st := t.liveStream(); st != nil {
		return st.typeText(s)
	}
	return t.input(ctx, "text", shellQuote(s))
}

// PressKey takes the same route as TypeText, for the same reason.
func (t *Device) PressKey(ctx context.Context, keycode int) error {
	if st := t.liveStream(); st != nil {
		return st.pressKey(keycode)
	}
	return t.input(ctx, "keyevent", strconv.Itoa(keycode))
}

// input runs `input <args>` on the device. It reports the device's own output
// on failure because `input` exits zero on some argument errors and explains
// itself on stdout instead.
func (t *Device) input(ctx context.Context, args ...string) error {
	if !t.info.Ready() {
		return fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}
	out, err := t.adb.Shell(ctx, t.info.Serial, append([]string{"input"}, args...)...)
	if err != nil {
		return fmt.Errorf("input %s: %w: %s", args[0], err, strings.TrimSpace(out))
	}
	if msg := strings.TrimSpace(out); msg != "" && strings.Contains(msg, "Exception") {
		return fmt.Errorf("input %s: %s", args[0], msg)
	}
	return nil
}

// shellQuote wraps a string for the shell that adb runs ON THE DEVICE.
//
// exec.Command passes arguments without a host shell, but `adb shell` joins
// them and hands the result to the device's sh, so an unquoted string is
// command injection whatever the host does. Single quotes suppress every
// metacharacter; the only thing they cannot contain is a single quote, which
// is closed, escaped and reopened in the usual way.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
