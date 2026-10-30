package target

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// loupecamSource is the camera shim, compiled for the simulator on first use.
// See its header for what it fakes.
//
//go:embed loupecam/loupecam.m
var loupecamSource []byte

var (
	loupecamMu    sync.Mutex
	loupecamPaths = map[string]string{}
)

// loupecamDylib builds the shim for the simulator.
func loupecamDylib(ctx context.Context) (string, error) {
	return loupecamBuild(ctx, "iphonesimulator")
}

// loupecamBuild builds the shim once per source and SDK, iphonesimulator or
// iphoneos, into the cache dir next to the staged images, and returns its
// path. It needs Xcode, which a simulator or a phone needs anyway.
func loupecamBuild(ctx context.Context, sdk string) (string, error) {
	loupecamMu.Lock()
	defer loupecamMu.Unlock()
	if p := loupecamPaths[sdk]; p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	ver, err := exec.CommandContext(ctx, "xcrun", "--sdk", sdk, "--show-sdk-version").Output()
	if err != nil {
		return "", fmt.Errorf("%w: no %s SDK: %v", ErrCameraShim, sdk, err)
	}
	sum := sha256.Sum256(append(append([]byte(sdk), loupecamSource...), ver...))
	dir, err := cameraDir()
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "loupecam-"+hex.EncodeToString(sum[:6])+".dylib")
	if _, err := os.Stat(out); err == nil {
		loupecamPaths[sdk] = out
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	src := filepath.Join(dir, "loupecam.m")
	if err := os.WriteFile(src, loupecamSource, 0o644); err != nil {
		return "", err
	}
	tmp := out + ".tmp"
	args := []string{"--sdk", sdk, "clang", "-dynamiclib", "-arch", "arm64"}
	if sdk == "iphonesimulator" {
		args = append(args, "-arch", "x86_64", "-mios-simulator-version-min=15.0")
	} else {
		args = append(args, "-mios-version-min=15.0")
	}
	args = append(args,
		"-fno-objc-arc", "-fobjc-exceptions", "-O2",
		"-framework", "Foundation", "-framework", "AVFoundation", "-framework", "CoreMedia",
		"-framework", "CoreVideo", "-framework", "CoreImage", "-framework", "ImageIO",
		"-framework", "QuartzCore", "-framework", "CoreGraphics",
		"-o", tmp, src)
	if b, err := exec.CommandContext(ctx, "xcrun", args...).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("%w: build: %v: %s", ErrCameraShim, err, strings.TrimSpace(string(b)))
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	loupecamPaths[sdk] = out
	return out, nil
}

// SetCamera implements CameraFeeder. A simulator has no camera at all, so
// the image goes to the camera shim, which an app gets when loupe launches
// it while an image is staged. The shim polls the file, so apps that have it
// see a new image without a relaunch.
func (s *Simulator) SetCamera(ctx context.Context, png []byte) (CameraApplies, error) {
	// Built now, so a missing Xcode fails here rather than at the launch.
	if _, err := loupecamDylib(ctx); err != nil {
		return "", err
	}
	path, err := cameraFile("sim-" + s.UDID())
	if err != nil {
		return "", err
	}
	if err := writeCameraFile(path, png); err != nil {
		return "", err
	}
	if s.anyShimmed() {
		return CameraLive, nil
	}
	return CameraOnLaunch, nil
}

// ClearCamera implements CameraFeeder. Apps running with the shim keep it
// and see black, a covered lens; the next launch of each is a plain one.
func (s *Simulator) ClearCamera(ctx context.Context) (CameraApplies, error) {
	path, err := cameraFile("sim-" + s.UDID())
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if s.anyShimmed() {
		return CameraLive, nil
	}
	return CameraOnLaunch, nil
}

// stagedCamera is the simulator's staged image, or "" when there is none.
func (s *Simulator) stagedCamera() string {
	path, err := cameraFile("sim-" + s.UDID())
	if err != nil {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// anyShimmed reports whether an app this server launched with the shim is
// still running, forgetting the ones that are not.
func (s *Simulator) anyShimmed() bool {
	s.shimMu.Lock()
	defer s.shimMu.Unlock()
	for b, pid := range s.shimmed {
		if !alive(pid) {
			delete(s.shimmed, b)
		}
	}
	return len(s.shimmed) > 0
}

func (s *Simulator) shimmedPid(bundle string) int {
	s.shimMu.Lock()
	defer s.shimMu.Unlock()
	return s.shimmed[bundle]
}

func (s *Simulator) noteShimmed(bundle string, pid int) {
	s.shimMu.Lock()
	defer s.shimMu.Unlock()
	if s.shimmed == nil {
		s.shimmed = map[string]int{}
	}
	s.shimmed[bundle] = pid
}

// alive reports whether pid is running. A simulator's apps are processes on
// this Mac, so signal 0 answers for them.
func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// simctlLaunchPid reads the pid out of `simctl launch`'s "bundle: pid".
var simctlLaunchPidRE = regexp.MustCompile(`:\s*(\d+)\s*$`)

func simctlLaunchPid(out string) int {
	m := simctlLaunchPidRE.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return 0
	}
	pid, _ := strconv.Atoi(m[1])
	return pid
}

// cameraLaunch is how Launch starts bundle while an image is staged: with
// the shim and the file in its environment, and relaunched if it is running
// without them, since simctl only brings a running app to the front. It
// reports false when there is nothing staged or the app already has the
// shim, and the launch should be a plain one.
func (s *Simulator) cameraLaunch(ctx context.Context, bundle string) (handled bool, out []byte, err error) {
	file := s.stagedCamera()
	if file == "" || alive(s.shimmedPid(bundle)) {
		return false, nil, nil
	}
	lib, err := loupecamDylib(ctx)
	if err != nil {
		return true, nil, err
	}
	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "launch", "--terminate-running-process", s.UDID(), bundle)
	cmd.Env = append(os.Environ(),
		"SIMCTL_CHILD_DYLD_INSERT_LIBRARIES="+lib,
		"SIMCTL_CHILD_LOUPE_CAMERA_FILE="+file,
	)
	out, err = cmd.CombinedOutput()
	if err == nil {
		s.noteShimmed(bundle, simctlLaunchPid(string(out)))
	}
	return true, out, err
}
