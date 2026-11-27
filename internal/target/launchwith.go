package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LaunchInjector is a Launcher that can start an app with what extensions
// ask a launch to carry: environment variables on an iOS simulator, a JVMTI
// agent and an adb reverse on Android. It is its own interface, not a wider
// Launch, so every other Launcher refuses a launch with extensions by not
// implementing it. See docs/extensions.md, "Launch-time injection".
type LaunchInjector interface {
	Launcher
	// LaunchFacts is what an extension is told about the target before it
	// answers: the platform and the facts that pick its build.
	LaunchFacts(ctx context.Context) (LaunchFacts, error)
	// LaunchWith launches app the way Launch does, carrying exts, which
	// have passed Validate for the platform LaunchFacts named.
	LaunchWith(ctx context.Context, app, activity string, exts []LaunchExtension) (*LaunchWithResult, error)
}

// The platforms a LaunchFacts names.
const (
	LaunchIOSSimulator = "ios-simulator"
	LaunchAndroid      = "android"
)

// LaunchFacts describes a target to an extension. UDID and IOSVersion are
// set for an iOS simulator, ABI and SDK for Android.
type LaunchFacts struct {
	Platform   string
	UDID       string
	IOSVersion string
	ABI        string
	SDK        int
}

// LaunchExtension is one extension's part of a launch.
type LaunchExtension struct {
	// Name is the extension's registered name. Android names the agent's
	// files on the device after it, so the device paths never come from the
	// extension.
	Name string
	// Env is an iOS simulator's launch environment, without the
	// SIMCTL_CHILD_ prefix.
	Env map[string]string
	// Agent is a JVMTI agent to attach to an Android app after it starts.
	Agent *LaunchAgent
	// Reverse is the localabstract socket name an Android launch reverses
	// to ReversePort on this Mac. ReversePort is the port of the
	// extension's registered URL, never one the extension answered with.
	Reverse     string
	ReversePort int
}

// LaunchAgent is a JVMTI agent: the .so and the dex on this Mac, and the
// options loupe passes to Agent_OnAttach. An option value may name the
// dex's device path as "@dex".
type LaunchAgent struct {
	SO      string
	Dex     string
	Options map[string]string
}

// LaunchWithResult is a launch with extensions. Relaunched says the app was
// running and was started again, which an iOS simulator always does so the
// app picks up its environment.
type LaunchWithResult struct {
	LaunchResult
	Relaunched bool
	With       []LaunchedExtension
}

// LaunchedExtension is what one extension's part of a launch did. Attached
// is true when loupe attached the agent, "already" when it had attached it
// to this process before, and nil when the extension asked for no agent.
type LaunchedExtension struct {
	Name     string `json:"name"`
	PID      int    `json:"pid,omitempty"`
	Attached any    `json:"attached,omitempty"`
}

// AttachedAlready is LaunchedExtension.Attached for an agent loupe had
// attached to the same process before.
const AttachedAlready = "already"

// ErrBadLaunchSpec is an extension's launch spec that fails a check, so
// nothing was launched.
var ErrBadLaunchSpec = errors.New("bad launch spec")

var (
	launchEnvKeyRE    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	launchOptKeyRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	launchOptValueRE  = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,200}$`)
	launchReverseRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	launchExtensionRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

const (
	maxLaunchEnvKeys     = 16
	maxLaunchEnvValueLen = 1024
)

// InsertLibrariesKey is dyld's insert list, the one key whose value is a
// list of files: loupe checks each and joins it with the camera shim's.
const InsertLibrariesKey = "DYLD_INSERT_LIBRARIES"

// ReservedLaunchEnv is a key loupe sets itself and an extension may not.
const ReservedLaunchEnv = "LOUPE_CAMERA_FILE"

// Validate checks e for platform: every rule in docs/extensions.md,
// "Launch-time injection", and that it asks for something the platform
// does. A spec with nothing for the platform is an error, never a plain
// launch. Errors wrap ErrBadLaunchSpec.
func (e LaunchExtension) Validate(platform string) error {
	if !launchExtensionRE.MatchString(e.Name) {
		return fmt.Errorf("%w: extension name %q", ErrBadLaunchSpec, e.Name)
	}
	switch platform {
	case LaunchIOSSimulator:
		if e.Agent != nil || e.Reverse != "" {
			return fmt.Errorf("%w: an iOS simulator takes env, not an agent or a reverse", ErrBadLaunchSpec)
		}
		if len(e.Env) == 0 {
			return fmt.Errorf("%w: no env for an iOS simulator", ErrBadLaunchSpec)
		}
		return checkLaunchEnv(e.Env)
	case LaunchAndroid:
		if len(e.Env) != 0 {
			return fmt.Errorf("%w: Android takes an agent or a reverse, not env", ErrBadLaunchSpec)
		}
		if e.Agent == nil && e.Reverse == "" {
			return fmt.Errorf("%w: no agent or reverse for Android", ErrBadLaunchSpec)
		}
		if e.Agent != nil {
			if err := e.Agent.check(); err != nil {
				return err
			}
		}
		if e.Reverse != "" {
			if !launchReverseRE.MatchString(e.Reverse) {
				return fmt.Errorf("%w: reverse %q must match %s", ErrBadLaunchSpec, e.Reverse, launchReverseRE)
			}
			if e.ReversePort < 1 || e.ReversePort > 65535 {
				return fmt.Errorf("%w: reverse port %d", ErrBadLaunchSpec, e.ReversePort)
			}
		}
		return nil
	}
	return fmt.Errorf("%w: unknown platform %q", ErrBadLaunchSpec, platform)
}

func checkLaunchEnv(env map[string]string) error {
	if len(env) > maxLaunchEnvKeys {
		return fmt.Errorf("%w: %d env keys, at most %d", ErrBadLaunchSpec, len(env), maxLaunchEnvKeys)
	}
	for k, v := range env {
		if !launchEnvKeyRE.MatchString(k) {
			return fmt.Errorf("%w: env key %q must match %s", ErrBadLaunchSpec, k, launchEnvKeyRE)
		}
		if k == ReservedLaunchEnv {
			return fmt.Errorf("%w: env key %s is loupe's own", ErrBadLaunchSpec, k)
		}
		if len(v) > maxLaunchEnvValueLen {
			return fmt.Errorf("%w: env %s is %d bytes, at most %d", ErrBadLaunchSpec, k, len(v), maxLaunchEnvValueLen)
		}
		if strings.ContainsAny(v, "\x00\n\r") {
			return fmt.Errorf("%w: env %s holds a NUL or a newline", ErrBadLaunchSpec, k)
		}
		if k == InsertLibrariesKey {
			for _, p := range strings.Split(v, ":") {
				if err := checkMacFile(k, p); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *LaunchAgent) check() error {
	if err := checkMacFile("agent so", a.SO); err != nil {
		return err
	}
	if err := checkMacFile("agent dex", a.Dex); err != nil {
		return err
	}
	for k, v := range a.Options {
		if !launchOptKeyRE.MatchString(k) {
			return fmt.Errorf("%w: option key %q must match %s", ErrBadLaunchSpec, k, launchOptKeyRE)
		}
		if !launchOptValueRE.MatchString(v) {
			return fmt.Errorf("%w: option %s's value %q must match %s", ErrBadLaunchSpec, k, v, launchOptValueRE)
		}
	}
	return nil
}

// checkMacFile requires p to be an absolute path to a regular file on this
// Mac. It follows a symlink, the way dyld and adb push would.
func checkMacFile(what, p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("%w: %s %q is not a clean absolute path", ErrBadLaunchSpec, what, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", ErrBadLaunchSpec, what, p, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s %s is not a regular file", ErrBadLaunchSpec, what, p)
	}
	return nil
}
