package target

import (
	"context"
)

// AppInstaller is implemented by the iOS targets: a physical iPhone, through
// devicectl, and a simulator, through simctl. It is not Installer because an
// iOS app is one bundle, not a base and its splits, and none of adb's flags
// apply.
type AppInstaller interface {
	// InstallApp installs the app in archive, a zip on the host: an .ipa
	// (Payload/Name.app/...) or a zipped Name.app. It replaces any installed
	// app with the same bundle ID. A device that refused the app returns an
	// *InstallError; any other error means it was never asked.
	InstallApp(ctx context.Context, archive string) (*AppInstallResult, error)
}

// AppInstallResult is an install that succeeded.
type AppInstallResult struct {
	// BundleID is CFBundleIdentifier from the app's Info.plist.
	BundleID string
	Output   string
}
