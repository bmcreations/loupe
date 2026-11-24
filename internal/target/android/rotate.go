package android

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

var _ target.Rotator = (*Device)(nil)

// androidButtons names the hardware and software buttons /api/input's
// "button" action can press, and the Android keycode each one sends.
// Buttons are named rather than keycoded on the wire so an iOS target can map
// "home" to its own HID event instead of an Android-specific number; this
// table is what an Android target resolves a name against.
//
// appSwitcher is deliberately absent: it is iOS's name for what Android calls
// "recents", and an Android target asked for it should get a 400 naming the
// buttons it does have, not a silent guess.
var androidButtons = map[string]int{
	"home":    3,
	"back":    4,
	"recents": 187,
	"lock":    26,
}

// ButtonKeycode resolves a named button to the Android keycode that presses
// it.
func ButtonKeycode(name string) (int, bool) {
	code, ok := androidButtons[name]
	return code, ok
}

// ButtonForKeycode is the reverse lookup: a keycode a viewer or an agent
// pressed, classified as a named button if one exists for it. Session.ViewerKey
// uses this to decide whether a key-up publishes as kind:"button" (named) or
// kind:"key" (a raw keycode with no name).
func ButtonForKeycode(code int) (string, bool) {
	for name, kc := range androidButtons {
		if kc == code {
			return name, true
		}
	}
	return "", false
}

// ButtonNames lists the buttons this table knows, sorted, for an error
// message that names what is actually available rather than just rejecting
// the one that was not.
func ButtonNames() []string {
	names := make([]string, 0, len(androidButtons))
	for name := range androidButtons {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// rotateCommands builds the shell command list Rotate runs, without running
// it, so the sequence is testable without a device. to is a quarter-turn
// count, 0..3; anything else has no meaning to Android's user_rotation
// setting and is rejected before a command is built.
//
// user_rotation is written before accelerometer_rotation is turned off, not
// after. Android ignores a user_rotation write while auto-rotate is still on,
// so staging the target value first and then flipping auto-rotate off applies
// both in a single transition. The reverse order turns auto-rotate off while
// user_rotation still holds whatever it last was, which locks the display at
// that stale value -- however briefly -- and can relaunch the foreground
// activity twice as the rotation settles twice instead of once; and if the
// second write then fails, the device is left locked at the wrong rotation
// rather than merely un-pinned.
func (t *Device) rotateCommands(to int) ([][]string, error) {
	if to < 0 || to > 3 {
		return nil, fmt.Errorf("rotation %d out of range [0,3]", to)
	}
	return [][]string{
		{"settings", "put", "system", "user_rotation", strconv.Itoa(to)},
		{"settings", "put", "system", "accelerometer_rotation", "0"},
	}, nil
}

// unforceRotationCommand hands the display back to the app in front. A build
// of loupe that never merged turned `fixed-to-user-rotation` on so the Pixel
// launcher would turn, which also turned every app that locks portrait; an
// emulator it rotated keeps that setting until something sets it back. An
// emulator's Rotate sends this every time, best effort: Android before 10 has no such command,
// and a device that never had the setting on is unchanged by it.
var unforceRotationCommand = []string{"cmd", "window", "fixed-to-user-rotation", "default"}

// gravities is the emulator's accelerometer reading, x:y:z in m/s^2, for a
// device held at each quarter turn counter-clockwise (Surface.ROTATION_*):
// upright, top to the left, upside down, top to the right.
var gravities = [4]string{"0:9.81:0", "9.81:0:0", "0:-9.81:0", "-9.81:0:0"}

// heldFromGravity reads `adb emu sensor get acceleration` ("acceleration =
// x:y:z") back into the quarter turn the device is held at. A device lying
// flat, or an answer it cannot read, says nothing about how it is turned, so
// it falls back to the display: the frame every client saw before.
func heldFromGravity(out string, display int) int {
	// The console ends every answer with an OK line.
	line, _, _ := strings.Cut(out, "\n")
	_, v, ok := strings.Cut(line, "=")
	if !ok {
		return display
	}
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 3 {
		return display
	}
	var g [3]float64
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return display
		}
		g[i] = f
	}
	x, y, z := math.Abs(g[0]), math.Abs(g[1]), math.Abs(g[2])
	switch {
	case x < z && y < z:
		return display
	case x > y && g[0] > 0:
		return 1
	case x > y:
		return 3
	case g[1] < 0:
		return 2
	}
	return 0
}

// held is the quarter turn the device is held at. An emulator's tilt sensor
// says, and keeps saying across a loupe restart. Anything else has no sensor
// loupe can set, so it is held the way its display is.
func (t *Device) held(ctx context.Context, display int) int {
	if !t.info.Emulator {
		return display
	}
	out, err := t.adb.Emu(ctx, t.info.Serial, "sensor", "get", "acceleration")
	if err != nil {
		return display
	}
	return heldFromGravity(out, display)
}

// Rotate pins auto-rotate off and sets the display to the given quarter-turn
// count. See rotateCommands for why two commands and why that order.
//
// Each command's output is checked the way input.go's own `input` helper
// checks `input`'s: not every adb version passes the device shell's exit
// status through, so `settings put` failing on the device -- most often a
// SecurityException -- can still come back as exit 0, and trusting the exit
// code alone would read that as success.
func (t *Device) Rotate(ctx context.Context, to int) error {
	cmds, err := t.rotateCommands(to)
	if err != nil {
		return err
	}
	if !t.info.Ready() {
		return fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}
	if t.info.Emulator {
		return t.tilt(ctx, to)
	}
	for _, args := range cmds {
		out, err := t.adb.Shell(ctx, t.info.Serial, args...)
		if err != nil {
			return fmt.Errorf("rotate %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out))
		}
		if msg := strings.TrimSpace(out); msg != "" && strings.Contains(msg, "Exception") {
			return fmt.Errorf("rotate %s: %s", strings.Join(args, " "), msg)
		}
	}
	return nil
}

// tilt turns an emulator the way a person turns a phone: it sets the tilt
// sensor and leaves auto-rotate on, so an app that can turn does and one
// that locks its orientation stays put, the launcher included. Pinning
// user_rotation instead (rotateCommands) does not hold: Android rewrites it
// to 0 as soon as a portrait-locked app such as the launcher comes to the
// front, and the turn is forgotten. The sensor keeps it.
//
// Upside down (2) is a real tilt too, and most apps do not turn for it, as
// on a phone.
func (t *Device) tilt(ctx context.Context, to int) error {
	out, err := t.adb.Emu(ctx, t.info.Serial, "sensor", "set", "acceleration", gravities[to])
	if err != nil {
		return fmt.Errorf("rotate: set tilt sensor: %w: %s", err, strings.TrimSpace(out))
	}
	if strings.Contains(out, "KO") {
		return fmt.Errorf("rotate: set tilt sensor: %s", strings.TrimSpace(out))
	}
	args := []string{"settings", "put", "system", "accelerometer_rotation", "1"}
	out, err = t.adb.Shell(ctx, t.info.Serial, args...)
	if err == nil && strings.Contains(out, "Exception") {
		err = errors.New(strings.TrimSpace(out))
	}
	if err != nil {
		return fmt.Errorf("rotate %s: %w", strings.Join(args, " "), err)
	}
	_, _ = t.adb.Shell(ctx, t.info.Serial, unforceRotationCommand...)
	return nil
}

// Rotation reports the display's current rotation. It reuses
// ADB.DisplayRotation rather than a second parser: trap 44 already settled
// how to read display 0's rotation out of a dump that also carries a decoy
// display and the device's fixed rotation constants, and there is nothing
// left to get right a second time.
//
// It answers the held rotation, not the display's: a Rotate counts on from
// here, and on an app that locks portrait the display stays at 0 however
// often the emulator is tilted, so counting from the display would ask for
// 90 forever.
func (t *Device) Rotation(ctx context.Context) (int, error) {
	display, err := t.adb.DisplayRotation(ctx, t.info.Serial)
	if err != nil {
		return 0, err
	}
	return t.held(ctx, display), nil
}
