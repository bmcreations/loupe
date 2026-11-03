package target

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InstallApp implements AppInstaller with `devicectl device install app`,
// over a cable or Wi-Fi. The app must be signed for this phone: a
// development or ad hoc profile that lists it. A refusal, such as a bundle
// with no profile, is an *InstallError naming CoreDevice's code.
func (d *IOSDevice) InstallApp(ctx context.Context, archive string) (*AppInstallResult, error) {
	dir, err := os.MkdirTemp("", "loupe-ipa-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	app, err := UnpackIOSApp(archive, dir)
	if err != nil {
		return nil, err
	}
	bundle, err := appBundleID(ctx, app)
	if err != nil {
		return nil, err
	}
	js := filepath.Join(dir, "result.json")
	out, runErr := devicectlInstall(ctx, d.device(), app, js)
	if runErr == nil {
		return &AppInstallResult{BundleID: bundle, Output: strings.TrimSpace(string(out))}, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("devicectl install: %w", ctx.Err())
	}
	b, _ := os.ReadFile(js)
	if ie, ok := parseDevicectlInstall(b); ok {
		return nil, ie
	}
	return nil, fmt.Errorf("devicectl install: %v: %s", runErr, strings.TrimSpace(string(out)))
}

// devicectlInstall runs the install, writing devicectl's JSON to js. A var
// so tests do not run devicectl.
var devicectlInstall = func(ctx context.Context, device, app, js string) ([]byte, error) {
	return exec.CommandContext(ctx, "xcrun", "devicectl", "device", "install", "app",
		"--device", device, app, "-j", js, "-q").CombinedOutput()
}
