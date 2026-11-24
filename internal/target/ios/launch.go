package ios

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// appContainerMissing reports whether get_app_container failed because the
// app is not installed, which it signals with exit status 2 (ENOENT). A
// simulator that is not booted, a bad UDID or a missing xcrun exit with
// something else.
func appContainerMissing(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 2
}

// bundleIDRE is the characters Apple allows in a bundle ID. simctl gets it
// as its own exec argument with no shell between, so this is a sanity check
// on the request rather than a quoting guard.
var bundleIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

// Launch opens an installed app on the simulator by bundle ID, booting
// nothing: a simulator that is not running fails in simctl. iOS has no
// activities, so naming one is an error rather than ignored.
func (s *Simulator) Launch(ctx context.Context, bundle, activity string) (*target.LaunchResult, error) {
	if err := s.CheckLaunch(bundle, activity); err != nil {
		return nil, err
	}
	handled, out, err := s.cameraLaunch(ctx, bundle)
	if !handled {
		out, err = exec.CommandContext(ctx, "xcrun", "simctl", "launch", s.UDID(), bundle).CombinedOutput()
	}
	if errors.Is(err, target.ErrCameraShim) {
		return nil, err
	}
	if err != nil {
		// simctl's error for a missing app is FBSOpenApplicationService code
		// 4, "The request to open ... failed", which names no cause. Asking
		// for the app's container does: on loupe-probe it exits 2 with
		// ENOENT for a bundle that is not installed and 0 for Settings.
		// Any other failure of that check says nothing about the app, so the
		// launch's own error stands.
		if appContainerMissing(exec.CommandContext(ctx, "xcrun", "simctl", "get_app_container", s.UDID(), bundle).Run()) && ctx.Err() == nil {
			return nil, fmt.Errorf("%w: %s", target.ErrAppNotInstalled, bundle)
		}
		return nil, fmt.Errorf("simctl launch %s: %w: %s", bundle, err, strings.TrimSpace(string(out)))
	}
	return &target.LaunchResult{}, nil
}

// CheckLaunch implements Launcher.
func (s *Simulator) CheckLaunch(bundle, activity string) error {
	if activity != "" {
		return fmt.Errorf("%w: a simulator app has no activities to name", target.ErrBadLaunchName)
	}
	if !bundleIDRE.MatchString(bundle) {
		return fmt.Errorf("%w: %q is not a bundle ID", target.ErrBadLaunchName, bundle)
	}
	return nil
}
