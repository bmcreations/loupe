package target

import (
	"context"
	"errors"
	"fmt"
)

// Installer is implemented by targets that can install an Android package.
// Synthetic and simulators cannot, so this is deliberately not part of
// Target, the same way Inspector is not.
type Installer interface {
	// Install installs the APKs at the given host paths as one package,
	// replacing any installed version: a lone APK, or a base and its
	// splits. A device that ran the install and refused the package returns
	// an *InstallError; any other error means the install never got as far
	// as the package manager.
	Install(ctx context.Context, apks []string, opts InstallOptions) (*InstallResult, error)
}

// InstallOptions are the adb install flags a caller may ask for.
type InstallOptions struct {
	// AllowTest passes -t. Android Studio's Run marks the APK it builds
	// android:testOnly, and the package manager refuses one without it
	// (INSTALL_FAILED_TEST_ONLY); `gradlew assembleDebug` does not.
	AllowTest bool
	// Downgrade passes -d, which lets a lower versionCode replace the
	// installed one. The package manager honours it only for a debuggable
	// app; a release build is still refused with
	// INSTALL_FAILED_VERSION_DOWNGRADE and has to be uninstalled first.
	Downgrade bool
}

// InstallResult is what adb printed for an install that succeeded.
type InstallResult struct {
	Output string
}

// InstallError is an install the package manager refused. Code is the
// INSTALL_FAILED_* or INSTALL_PARSE_FAILED_* reason out of "Failure [...]",
// which is what a caller branches on; Output is everything adb printed.
type InstallError struct {
	Code   string
	Reason string
	Output string
}

func (e *InstallError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("install failed: %s: %s", e.Code, e.Reason)
	}
	return "install failed: " + e.Code
}

// IsInstallRefused reports whether err is the package manager refusing an
// APK, as opposed to adb or the device failing before it was asked.
func IsInstallRefused(err error) (*InstallError, bool) {
	var ie *InstallError
	ok := errors.As(err, &ie)
	return ie, ok
}
