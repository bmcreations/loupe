package target

import (
	"context"
	"fmt"
	"strings"
	"time"
)

var _ Rotator = (*Simulator)(nil)

// simOrientations maps Rotator's quarter-turn count (Android's
// Surface.ROTATION_*: counter-clockwise from natural) to CoreDevice's
// DeviceOrientation names. Landscape left is the device turned
// counter-clockwise, so 1 is landscapeLeft and 3 is landscapeRight.
var simOrientations = [4]string{"portrait", "landscapeLeft", "portraitUpsideDown", "landscapeRight"}

func simOrientationName(to int) (string, error) {
	if to < 0 || to > 3 {
		return "", fmt.Errorf("rotation %d out of range [0,3]", to)
	}
	return simOrientations[to], nil
}

// simRotationFromName is the reverse. faceUp, faceDown and unknown (a fresh
// simulator reports unknown) are not turns, so they read as portrait.
func simRotationFromName(name string) int {
	for i, n := range simOrientations {
		if n == name {
			return i
		}
	}
	return 0
}

// simOrientOpenBudget is added to an `orient` command's budget. simcap opens
// CoreDevice's orientation-control capability on the first `orient` a helper
// gets and waits up to 10s for it (orientation.swift), before the call's own
// 5s. With only simInputBudget, the first Rotate after loupe or the helper
// started failed with "no reply in 5s" while the open was still going. A var
// so tests can shrink it.
var simOrientOpenBudget = 10 * time.Second

// Rotate implements Rotator through simcap's `orient set`, which calls
// CoreDevice's orientation-control capability, the one Device Hub's rotate
// buttons use. The video surface does not change size: the content is drawn
// turned inside the same portrait framebuffer (see docs/ios.md).
func (s *Simulator) Rotate(ctx context.Context, to int) error {
	name, err := simOrientationName(to)
	if err != nil {
		return err
	}
	if err := s.requireBooted(); err != nil {
		return err
	}
	_, err = s.input().do(ctx, simOrientOpenBudget, "orient set %s", name)
	return err
}

// Rotation implements Rotator through `orient get`, whose reply is
// "NAME locked|unlocked".
func (s *Simulator) Rotation(ctx context.Context) (int, error) {
	if err := s.requireBooted(); err != nil {
		return 0, err
	}
	out, err := s.input().do(ctx, simOrientOpenBudget, "orient get")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return 0, fmt.Errorf("orient get: empty reply")
	}
	return simRotationFromName(f[0]), nil
}

// simContentRotation turns a Rotator quarter-turn count (counter-clockwise)
// into DeviceFrame.ContentRotation (clockwise): a simulator turned
// counter-clockwise has its content drawn turned clockwise inside the
// portrait picture, so the client turns the picture back the other way.
func simContentRotation(to int) int {
	return ((4-to)%4 + 4) % 4
}

// contentRotation is the frame's ContentRotation, read from `orient get`.
// A shut-down simulator, or one whose simcap cannot answer, reads as upright:
// a frame drawn upright is what every client got before simulators turned.
// This starts the input process on a single-panel simulator too; any Rotate
// would have.
func (s *Simulator) contentRotation(ctx context.Context) int {
	if s.requireBooted() != nil {
		return 0
	}
	// A CoreDevice lookup can hang (see framePanel), and an upright frame is
	// better than none.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	to, err := s.Rotation(ctx)
	if err != nil {
		return 0
	}
	return simContentRotation(to)
}
