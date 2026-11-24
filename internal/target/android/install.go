package android

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// Install runs `adb install -r` on one APK and `adb install-multiple -r`
// on a base and its splits. A package delivered from an App Bundle, which is
// anything from Play and any module with dynamic features, is a base plus
// config splits, and the base alone is refused with
// INSTALL_FAILED_MISSING_SPLIT.
//
// The paths reach only the host's exec, never the device's shell: adb
// streams each file to the package manager itself.
func (t *Device) Install(ctx context.Context, apks []string, opts target.InstallOptions) (*target.InstallResult, error) {
	if len(apks) == 0 {
		return nil, errors.New("no APK to install")
	}
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", target.ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	verb := "install"
	if len(apks) > 1 {
		verb = "install-multiple"
	}
	args := []string{"-s", t.info.Serial, verb, "-r"}
	if opts.AllowTest {
		args = append(args, "-t")
	}
	if opts.Downgrade {
		args = append(args, "-d")
	}
	out, runErr := exec.CommandContext(ctx, t.adb.bin(), append(args, apks...)...).CombinedOutput()
	return parseInstall(string(out), runErr)
}

// installFailureRE reads "Failure [CODE]" or "Failure [CODE: reason]". adb
// prints it on its own line from older package managers, and after
// "adb: failed to install <path>: " from newer ones.
var installFailureRE = regexp.MustCompile(`Failure \[([A-Z0-9_]+)(?::\s*([^\]]*))?\]`)

// parseInstall decides what an install did from adb's output and exit status.
//
// The exit status alone cannot be trusted in either direction. Older adb
// and some devices exit 0 after printing "Failure [INSTALL_FAILED_...]", so a
// Failure line fails the install whatever the status says. And a zero exit
// with neither Success nor Failure has not shown that anything was
// installed, so that fails too rather than reading as success.
func parseInstall(out string, runErr error) (*target.InstallResult, error) {
	out = strings.TrimSpace(out)
	if m := installFailureRE.FindStringSubmatch(out); m != nil {
		return nil, &target.InstallError{Code: m[1], Reason: strings.TrimSpace(m[2]), Output: out}
	}
	if runErr != nil {
		if msg := target.LastLine(out); msg != "" {
			return nil, fmt.Errorf("adb install: %w: %s", runErr, msg)
		}
		return nil, fmt.Errorf("adb install: %w", runErr)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "Success" {
			return &target.InstallResult{Output: out}, nil
		}
	}
	return nil, fmt.Errorf("adb install printed no Success line: %q", out)
}
