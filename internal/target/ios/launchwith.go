package ios

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// A launch with extensions on a simulator is `simctl launch` with each
// extension's environment as SIMCTL_CHILD_ variables, which simctl hands to
// the app. A running app is terminated first, always: simctl only brings a
// running app to the front, and it would not load what the extension asked
// for. The camera shim rides along when an image is staged, its
// DYLD_INSERT_LIBRARIES joined with the extensions' rather than replaced.

// simctlLaunch runs `xcrun simctl` with env added to loupe's own. A var so
// tests can stand in for simctl.
var simctlLaunch = func(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

// LaunchFacts implements target.LaunchInjector.
func (s *Simulator) LaunchFacts(ctx context.Context) (target.LaunchFacts, error) {
	info := s.Info()
	return target.LaunchFacts{Platform: target.LaunchIOSSimulator, UDID: info.UDID, IOSVersion: info.OSVersion}, nil
}

// LaunchWith implements target.LaunchInjector: it terminates bundle if it is
// running and launches it with the camera shim, when an image is staged,
// and every extension's environment.
func (s *Simulator) LaunchWith(ctx context.Context, bundle, activity string, exts []target.LaunchExtension) (*target.LaunchWithResult, error) {
	if err := s.CheckLaunch(bundle, activity); err != nil {
		return nil, err
	}
	for _, e := range exts {
		if err := e.Validate(target.LaunchIOSSimulator); err != nil {
			return nil, fmt.Errorf("extension %s: %w", e.Name, err)
		}
	}
	file := s.stagedCamera()
	shim := ""
	if file != "" {
		var err error
		if shim, err = loupecamDylib(ctx); err != nil {
			return nil, err
		}
	}
	env, err := simLaunchEnv(shim, file, exts)
	if err != nil {
		return nil, err
	}
	list, err := simAppList(ctx, s)
	if err != nil {
		return nil, err
	}
	running := pidForApp(list, bundle) > 0
	out, err := simctlLaunch(ctx, env, "launch", "--terminate-running-process", s.UDID(), bundle)
	if err != nil {
		return nil, s.launchFailed(ctx, bundle, out, err)
	}
	pid := simctlLaunchPid(string(out))
	if file != "" {
		s.noteShimmed(bundle, pid)
	}
	res := &target.LaunchWithResult{Relaunched: running}
	for _, e := range exts {
		res.With = append(res.With, target.LaunchedExtension{Name: e.Name, PID: pid})
	}
	return res, nil
}

// simLaunchEnv is the SIMCTL_CHILD_ environment for a launch with the camera
// shim (when shim is set) and exts, sorted by key. DYLD_INSERT_LIBRARIES
// lists the shim first and then each extension's libraries in order, each
// once. Any other key two extensions both set is an error: one of them
// would silently lose.
func simLaunchEnv(shim, cameraFile string, exts []target.LaunchExtension) ([]string, error) {
	var inserts []string
	if shim != "" {
		inserts = append(inserts, shim)
	}
	vars := map[string]string{}
	owner := map[string]string{}
	if cameraFile != "" {
		vars[target.ReservedLaunchEnv] = cameraFile
		owner[target.ReservedLaunchEnv] = "loupe's camera"
	}
	for _, e := range exts {
		keys := make([]string, 0, len(e.Env))
		for k := range e.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := e.Env[k]
			if k == target.InsertLibrariesKey {
				for _, lib := range strings.Split(v, ":") {
					if !slices.Contains(inserts, lib) {
						inserts = append(inserts, lib)
					}
				}
				continue
			}
			if by, ok := owner[k]; ok {
				return nil, fmt.Errorf("extension %s: %w: env %s is already set by %s", e.Name, target.ErrBadLaunchSpec, k, by)
			}
			vars[k] = v
			owner[k] = "extension " + e.Name
		}
	}
	if len(inserts) > 0 {
		vars[target.InsertLibrariesKey] = strings.Join(inserts, ":")
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, "SIMCTL_CHILD_"+k+"="+vars[k])
	}
	return env, nil
}

var _ target.LaunchInjector = (*Simulator)(nil)
