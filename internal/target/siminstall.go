package target

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// InstallApp implements AppInstaller with `simctl install`. A simulator
// takes a build for the simulator: an .ipa from App Store Connect or an
// ad hoc export is built for a phone, and simctl refuses it.
func (s *Simulator) InstallApp(ctx context.Context, archive string) (*AppInstallResult, error) {
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
	out, err := simctlInstall(ctx, s.UDID(), app)
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("simctl install: %w", ctx.Err())
		}
		if _, isExit := err.(*exec.ExitError); isExit {
			return nil, &InstallError{Code: "simctlInstall", Reason: simctlReason(text), Output: text}
		}
		return nil, fmt.Errorf("simctl install: %w", err)
	}
	return &AppInstallResult{BundleID: bundle, Output: text}, nil
}

// simctlInstall is a var so tests do not run simctl.
var simctlInstall = func(ctx context.Context, udid, app string) ([]byte, error) {
	return exec.CommandContext(ctx, "xcrun", "simctl", "install", udid, app).CombinedOutput()
}

// simctlReason is the first line of simctl's error, "An error was
// encountered processing the command (domain=..., code=...):", followed by
// the line that says what, which is the useful one.
func simctlReason(out string) string {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "An error was encountered") && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0])
	}
	return ""
}
