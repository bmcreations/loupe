package target

import (
	"context"
	"fmt"
	"strings"
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
	_, err = s.input().do(ctx, 0, "orient set %s", name)
	return err
}

// Rotation implements Rotator through `orient get`, whose reply is
// "NAME locked|unlocked".
func (s *Simulator) Rotation(ctx context.Context) (int, error) {
	if err := s.requireBooted(); err != nil {
		return 0, err
	}
	out, err := s.input().do(ctx, 0, "orient get")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return 0, fmt.Errorf("orient get: empty reply")
	}
	return simRotationFromName(f[0]), nil
}
