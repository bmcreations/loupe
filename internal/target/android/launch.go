package android

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

var (
	// ErrNoLaunchActivity is an installed package with no activity in the
	// launcher category, such as a library or a service-only app, so there
	// is nothing to open without naming an activity.
	ErrNoLaunchActivity = errors.New("app has no launcher activity")
	// ErrNoSuchActivity is a named activity the package does not declare.
	ErrNoSuchActivity = errors.New("no such activity")
)

// activityNameRE is a Java class name, qualified or relative to the
// package with a leading dot. $ is allowed for a nested class.
var activityNameRE = regexp.MustCompile(`^\.?[A-Za-z_][A-Za-z0-9_$]*(\.[A-Za-z_][A-Za-z0-9_$]*)*$`)

// ValidActivityName reports whether s is shaped like an activity class name.
func ValidActivityName(s string) bool { return activityNameRE.MatchString(s) }

// Launch starts pkg's launcher activity, or the named one.
//
// It resolves the launcher activity with `cmd package resolve-activity` and
// starts it with `am start -W -n`, rather than `monkey -p <pkg> -c LAUNCHER
// 1`, because the two-step route says what happened. On emulator-5554 monkey
// printed "No activities found to run" and exited 252 alike for a package
// that was not installed and one with no launcher activity, and names no
// activity when it works. resolve-activity names the component, `pm path`
// tells the two failures apart, and -W makes am wait for the activity and
// print the one that actually came up.
func (t *Device) Launch(ctx context.Context, pkg, activity string) (*target.LaunchResult, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", target.ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	return launchAndroid(ctx, func(ctx context.Context, args ...string) (string, error) {
		return t.adb.Shell(ctx, t.info.Serial, args...)
	}, pkg, activity)
}

// CheckLaunch implements Launcher.
func (t *Device) CheckLaunch(pkg, activity string) error {
	return checkAndroidLaunch(pkg, activity)
}

// checkAndroidLaunch is the pattern check that keeps pkg and activity safe
// to hand to the device's shell.
func checkAndroidLaunch(pkg, activity string) error {
	if !ValidPackageName(pkg) {
		return fmt.Errorf("%w: %q is not a package name", target.ErrBadLaunchName, pkg)
	}
	if activity != "" && !ValidActivityName(activity) {
		return fmt.Errorf("%w: %q is not an activity name", target.ErrBadLaunchName, activity)
	}
	return nil
}

// launchAndroid is Launch over a shellFunc, so it can be tested without a
// device. pkg and activity reach the device's shell only after their
// pattern check and through shellQuote.
func launchAndroid(ctx context.Context, sh shellFunc, pkg, activity string) (*target.LaunchResult, error) {
	if err := checkAndroidLaunch(pkg, activity); err != nil {
		return nil, err
	}
	var component string
	if activity != "" {
		component = pkg + "/" + activity
	} else {
		out, err := sh(ctx, "cmd", "package", "resolve-activity", "--brief",
			"-c", "android.intent.category.LAUNCHER", shellQuote(pkg))
		if err != nil {
			return nil, fmt.Errorf("resolve-activity %s: %w: %s", pkg, err, strings.TrimSpace(out))
		}
		var ok bool
		if component, ok = parseResolveActivity(out); !ok {
			return nil, noActivityReason(ctx, sh, pkg)
		}
	}
	out, err := sh(ctx, "am", "start", "-W", "-n", shellQuote(component))
	return parseAmStart(component, out, err)
}

// parseResolveActivity reads the component from `resolve-activity --brief`,
// which prints the match details and then pkg/class on the last line. For a
// package it cannot resolve it prints "No activity found" and still exits 0.
func parseResolveActivity(out string) (string, bool) {
	line := target.LastLine(strings.TrimSpace(out))
	pkg, cls, ok := strings.Cut(line, "/")
	if !ok || !ValidPackageName(pkg) || !ValidActivityName(cls) {
		return "", false
	}
	return line, true
}

// noActivityReason tells a package that is not installed from one with no
// launcher activity, which resolve-activity answers identically.
func noActivityReason(ctx context.Context, sh shellFunc, pkg string) error {
	out, _ := sh(ctx, "pm", "path", shellQuote(pkg))
	if strings.Contains(out, "package:") {
		return fmt.Errorf("%w: %s", ErrNoLaunchActivity, pkg)
	}
	return fmt.Errorf("%w: %s", target.ErrAppNotInstalled, pkg)
}

// parseAmStart decides whether `am start -W` started anything. A component
// that does not exist prints "Error type 3" and an "Error:" line, and exits
// 1 on emulator-5554; older devices exit 0 after printing it, so an Error
// line fails the launch whatever the status says. An app already in front
// prints "Warning: Activity not started, intent has been delivered to
// currently running top-most instance" with "Status: ok", which is success:
// the app is in front, as asked.
//
// "Status: timeout" with an Activity line is success too: the intent
// reached that activity and only its first draw was slow. Through a spare
// server on api34_emoji_check, Chrome answered that way to two of three
// VIEW intents for https://example.com after 12-24s, and was in front.
func parseAmStart(component, out string, runErr error) (*target.LaunchResult, error) {
	res, err := parseAm(component, out, runErr)
	if err != nil {
		return nil, err
	}
	if res.Activity == "" {
		res.Activity = component
	}
	return res, nil
}

// parseAm is parseAmStart without the fallback to the component asked for,
// which a VIEW intent does not have. label names what was started in an
// error, and is all of the request an error carries.
func parseAm(label, out string, runErr error) (*target.LaunchResult, error) {
	out = strings.TrimSpace(out)
	var res target.LaunchResult
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Error:"):
			msg := strings.TrimSpace(strings.TrimPrefix(line, "Error:"))
			if strings.HasSuffix(msg, "does not exist.") {
				return nil, fmt.Errorf("%w: am start %s: %s", ErrNoSuchActivity, label, msg)
			}
			if strings.Contains(msg, "unable to resolve Intent") {
				// The line goes on to print the intent, data URI and all,
				// so it is not kept.
				return nil, fmt.Errorf("%w: am start %s", target.ErrNoURLHandler, label)
			}
			return nil, fmt.Errorf("am start %s: %s", label, msg)
		case strings.HasPrefix(line, "Activity:"):
			res.Activity = strings.TrimSpace(strings.TrimPrefix(line, "Activity:"))
		}
	}
	if runErr != nil {
		return nil, fmt.Errorf("am start %s: %w: %s", label, runErr, target.LastLine(out))
	}
	if strings.Contains(out, "Status: ok") {
		return &res, nil
	}
	if strings.Contains(out, "Status: timeout") && res.Activity != "" {
		res.TimedOut = true
		return &res, nil
	}
	return nil, fmt.Errorf("am start %s: %q", label, out)
}

// AAPT2Binary is aapt2 from the newest build-tools in the SDK, or "" when
// there is none. launchd's PATH does not reach the SDK, for the reason
// SDKRoot gives, so PATH is not consulted. Versions are compared as
// numbers, so 37.0.0 beats 4.0.0 and 36.1.0 beats 36.0.0.
func AAPT2Binary(sdk string) string {
	if sdk == "" {
		return ""
	}
	dirs, err := os.ReadDir(filepath.Join(sdk, "build-tools"))
	if err != nil {
		return ""
	}
	best, bestVer := "", []int(nil)
	for _, d := range dirs {
		ver, ok := buildToolsVersion(d.Name())
		if !ok {
			continue
		}
		p := filepath.Join(sdk, "build-tools", d.Name(), "aapt2")
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if bestVer == nil || versionLess(bestVer, ver) {
			best, bestVer = p, ver
		}
	}
	return best
}

// buildToolsVersion reads a build-tools directory name such as 36.1.0. A
// preview such as 37.0.0-rc1 is skipped rather than ranked.
func buildToolsVersion(name string) ([]int, bool) {
	parts := strings.Split(name, ".")
	ver := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		ver[i] = n
	}
	return ver, true
}

func versionLess(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// APKPackage reads the package name out of an APK with `aapt2 dump
// packagename`. A config split carries its base's package name, so any one
// APK of an install-multiple answers for the whole set.
func APKPackage(ctx context.Context, aapt2, apk string) (string, error) {
	if aapt2 == "" {
		return "", errors.New("aapt2 not found: no Android SDK build-tools")
	}
	out, err := exec.CommandContext(ctx, aapt2, "dump", "packagename", apk).CombinedOutput()
	name := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("aapt2 dump packagename: %w: %s", err, target.LastLine(name))
	}
	if !ValidPackageName(name) {
		return "", fmt.Errorf("aapt2 dump packagename printed %q", name)
	}
	return name, nil
}
