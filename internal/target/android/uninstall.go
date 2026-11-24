package android

import (
	"context"
	"fmt"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// Uninstall implements target.Uninstaller with `pm uninstall` on the device's shell.
func (t *Device) Uninstall(ctx context.Context, pkg string) (*target.InstallResult, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", target.ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	return uninstallAndroid(ctx, func(ctx context.Context, args ...string) (string, error) {
		return t.adb.Shell(ctx, t.info.Serial, args...)
	}, pkg)
}

// uninstallAndroid is Uninstall over a shellFunc, so it can be tested without
// a device. pkg reaches the device's shell only after its pattern check and
// through shellQuote, as launchAndroid's does.
func uninstallAndroid(ctx context.Context, sh shellFunc, pkg string) (*target.InstallResult, error) {
	if !ValidPackageName(pkg) {
		return nil, fmt.Errorf("%w: %q is not a package name", target.ErrBadLaunchName, pkg)
	}
	out, err := sh(ctx, "pm", "uninstall", shellQuote(pkg))
	out = strings.TrimSpace(out)
	if m := installFailureRE.FindStringSubmatch(out); m != nil {
		// A package that is not there fails the same way as one that could
		// not be removed, so tell them apart only after a Failure line, and
		// only when the shell answered: a dropped adb must not read as 404.
		if pathOut, pathErr := sh(ctx, "pm", "path", shellQuote(pkg)); pathErr == nil && !strings.Contains(pathOut, "package:") {
			return nil, fmt.Errorf("%w: %s", target.ErrAppNotInstalled, pkg)
		}
		return nil, &target.InstallError{Code: m[1], Reason: strings.TrimSpace(m[2]), Output: out}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "Success" {
			return &target.InstallResult{Output: out}, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("pm uninstall %s: %w: %s", pkg, err, target.LastLine(out))
	}
	return nil, fmt.Errorf("pm uninstall printed no Success line: %q", out)
}
