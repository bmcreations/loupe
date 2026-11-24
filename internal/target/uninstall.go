package target

import "context"

// Uninstaller is implemented by targets that can remove an Android package.
// Like Installer it is not part of Target: simulators and iPhones do not
// implement it, and the route answers 400 unsupported for them.
type Uninstaller interface {
	// Uninstall removes pkg and its data. A name that fails the package
	// pattern wraps ErrBadLaunchName, and a package that is not on the
	// device wraps ErrAppNotInstalled. A refusal from the package manager
	// (a device-policy app, say) is an *InstallError whose Code is the
	// DELETE_FAILED_* reason.
	Uninstall(ctx context.Context, pkg string) (*InstallResult, error)
}
