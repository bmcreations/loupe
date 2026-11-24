package target

import (
	"context"
	"errors"
)

// Launcher is implemented by targets that can open an installed app.
// Synthetic cannot, so this is deliberately not part of Target.
type Launcher interface {
	// Launch opens app: an Android package or an iOS bundle ID. activity
	// names an Android activity to start instead of the package's launcher
	// one, either qualified or starting with "."; empty means the launcher
	// activity.
	Launch(ctx context.Context, app, activity string) (*LaunchResult, error)

	// CheckLaunch is Launch's name check alone, with no I/O, so a request
	// naming nothing launchable is a 400 before arbitration rather than a
	// 409 humanHeld that a retry cannot fix. It wraps ErrBadLaunchName.
	CheckLaunch(app, activity string) error
}

// LaunchResult says what came to the front. Activity is the component the
// device reports as started, which for a trampoline launcher activity is
// not the one asked for; it is empty where the target cannot tell.
type LaunchResult struct {
	Activity string
	// TimedOut is am's "Status: timeout": the activity was started and
	// had not drawn when am stopped waiting.
	TimedOut bool
}

var (
	// ErrAppNotInstalled is a launch of a package the device does not have.
	ErrAppNotInstalled = errors.New("app is not installed")
	// ErrBadLaunchName is a package, bundle ID or activity that failed its
	// pattern check, so nothing was sent to the target.
	ErrBadLaunchName = errors.New("bad launch name")
)
