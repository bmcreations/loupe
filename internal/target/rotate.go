package target

import (
	"context"
	"errors"
)

// Rotator turns the display. It is optional the way Inspector, Screenshotter
// and Framer are: a target that cannot turn its display omits it, and
// /api/input answers 501 rather than guessing.
//
// It pins the rotation -- user_rotation, then auto-rotate off -- because a
// device left on auto-rotate snaps back to the sensor the moment it is
// touched, which would make a rotate call from /api/input undo itself on the
// next tap. See rotateCommands for why that specific order and not the
// reverse.
type Rotator interface {
	// Rotate turns the display to the given quarter-turn count, 0..3,
	// matching Android's Surface.ROTATION_* constants.
	Rotate(ctx context.Context, to int) error
	// Rotation reports how the device is held, the same space Rotate takes:
	// the turn last asked for. On Android that can differ from the display's
	// own rotation (ADB.DisplayRotation, trap 44), which an app that locks
	// its orientation keeps where it wants it.
	Rotation(ctx context.Context) (int, error)
}

// ButtonPresser is implemented by targets whose buttons are not Android
// keycodes. /api/input's "button" action tries it before androidButtons, so
// an iOS simulator presses its own Home rather than being sent keycode 3.
type ButtonPresser interface {
	PressButton(ctx context.Context, name string) error
	// Buttons lists the names PressButton accepts, for an error that says
	// what is available.
	Buttons() []string
}

// ErrUnknownButton wraps a name a ButtonPresser does not have, so the
// handler can answer 400 rather than 500.
var ErrUnknownButton = errors.New("unknown button")

// ContentRotationFor is DeviceFrame.ContentRotation for a device held at
// held whose display is at display, both counter-clockwise quarter turns.
// An app that locks its orientation keeps the display where it wants it,
// so a phone held sideways shows it sideways, the way a real one does: the
// client turns the picture and frame clockwise by the difference. Held and
// display agree for any app that turned, which leaves 0 and today's frame.
func ContentRotationFor(held, display int) int {
	return ((display-held)%4 + 4) % 4
}
