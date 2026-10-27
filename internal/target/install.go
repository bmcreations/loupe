package target

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
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

// Install runs `adb install -r` on one APK and `adb install-multiple -r`
// on a base and its splits. A package delivered from an App Bundle, which is
// anything from Play and any module with dynamic features, is a base plus
// config splits, and the base alone is refused with
// INSTALL_FAILED_MISSING_SPLIT.
//
// The paths reach only the host's exec, never the device's shell: adb
// streams each file to the package manager itself.
func (t *ADBTarget) Install(ctx context.Context, apks []string, opts InstallOptions) (*InstallResult, error) {
	if len(apks) == 0 {
		return nil, errors.New("no APK to install")
	}
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	verb := "install"
	if len(apks) > 1 {
		verb = "install-multiple"
	}
	args := []string{"-s", t.info.Serial, verb, "-r"}
	if opts.AllowTest {
		args = append(args, "-t")
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
func parseInstall(out string, runErr error) (*InstallResult, error) {
	out = strings.TrimSpace(out)
	if m := installFailureRE.FindStringSubmatch(out); m != nil {
		return nil, &InstallError{Code: m[1], Reason: strings.TrimSpace(m[2]), Output: out}
	}
	if runErr != nil {
		if msg := lastLine(out); msg != "" {
			return nil, fmt.Errorf("adb install: %w: %s", runErr, msg)
		}
		return nil, fmt.Errorf("adb install: %w", runErr)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "Success" {
			return &InstallResult{Output: out}, nil
		}
	}
	return nil, fmt.Errorf("adb install printed no Success line: %q", out)
}

// IsInstallRefused reports whether err is the package manager refusing an
// APK, as opposed to adb or the device failing before it was asked.
func IsInstallRefused(err error) (*InstallError, bool) {
	var ie *InstallError
	ok := errors.As(err, &ie)
	return ie, ok
}
