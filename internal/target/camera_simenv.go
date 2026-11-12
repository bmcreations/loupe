package target

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A staged camera also goes into the simulator's own launchd environment, so
// an app started any way at all, from its icon, by `simctl launch` or by
// another tool, loads the shim, not only one loupe launches. launchd hands
// the variables to every process it starts after they are set; the shim does
// nothing outside an installed app (loupecam.m's LCInstall). A reboot of the
// simulator clears them, so they are set again whenever loupe sees it booted,
// which includes loupe's own start.

// simLaunchctl runs launchctl inside the simulator. Tests replace it.
var simLaunchctl = func(ctx context.Context, udid string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "xcrun", append([]string{"simctl", "spawn", udid, "launchctl"}, args...)...).Output()
}

// isLoupecam reports whether an insert value is one of loupe's shims.
func isLoupecam(insert string) bool {
	return strings.HasPrefix(filepath.Base(insert), "loupecam-") && strings.HasSuffix(insert, ".dylib")
}

// cameraEnvCalls is the launchctl calls that bring the simulator's
// environment in line with what is staged: the shim and the file while an
// image is staged, neither once it is cleared. An insert that is not
// loupe's is someone else's, and is left alone either way.
func cameraEnvCalls(staged, lib, current string) [][]string {
	if current != "" && !isLoupecam(current) {
		return nil
	}
	if staged != "" {
		// The file first: an app that starts between the two calls then
		// finds it when the shim loads.
		return [][]string{
			{"setenv", "LOUPE_CAMERA_FILE", staged},
			{"setenv", "DYLD_INSERT_LIBRARIES", lib},
		}
	}
	if current == "" {
		return nil
	}
	return [][]string{
		{"unsetenv", "DYLD_INSERT_LIBRARIES"},
		{"unsetenv", "LOUPE_CAMERA_FILE"},
	}
}

// syncCameraEnv sets or clears the shim in the simulator's launchd to match
// the staged image. It needs the simulator booted.
func (s *Simulator) syncCameraEnv(ctx context.Context) error {
	udid := s.UDID()
	out, err := simLaunchctl(ctx, udid, "getenv", "DYLD_INSERT_LIBRARIES")
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(out))
	staged := s.stagedCamera()
	lib := ""
	if staged != "" {
		if lib, err = loupecamDylib(ctx); err != nil {
			return err
		}
	}
	calls := cameraEnvCalls(staged, lib, current)
	for _, c := range calls {
		if _, err := simLaunchctl(ctx, udid, c...); err != nil {
			return err
		}
	}
	if len(calls) > 0 {
		s.traceCamera(map[string]any{"kind": "camera-env", "target": s.ID(), "staged": staged != "", "calls": len(calls)})
	}
	return nil
}

// syncCameraEnvSoon runs syncCameraEnv off the caller's path, for the
// listing, which must not wait on simctl.
func (s *Simulator) syncCameraEnvSoon() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.syncCameraEnv(ctx); err != nil {
			s.traceCamera(map[string]any{"kind": "camera-env", "target": s.ID(), "error": err.Error()})
		}
	}()
}

// markerAlive reports whether the shim's marker next to file names a
// running process: an app that loaded the shim without Launch, from its
// icon say, still sees the image.
func markerAlive(file string) bool {
	b, err := os.ReadFile(file + ".loaded")
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && alive(pid)
}

// traceCamera records m when the server gave the simulator a trace.
func (s *Simulator) traceCamera(m map[string]any) {
	if s.trace != nil {
		s.trace(m)
	}
}

// syncCameraEnvBooted syncs the environment of a booted simulator now. A
// shut-down one has no launchd to set; its boot syncs it. A failure only
// costs launches outside loupe the camera, so it is traced, not returned.
func (s *Simulator) syncCameraEnvBooted(ctx context.Context) {
	if !s.booted.Load() {
		return
	}
	if err := s.syncCameraEnv(ctx); err != nil {
		s.traceCamera(map[string]any{"kind": "camera-env", "target": s.ID(), "error": err.Error()})
	}
}
