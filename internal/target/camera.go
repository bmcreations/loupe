package target

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CameraApplies says when a camera image reaches the app looking through the
// camera.
type CameraApplies string

const (
	// CameraOnOpen: the emulator reads the file each time an app opens the
	// camera, so a preview already showing keeps the old image until the app
	// closes and reopens it.
	CameraOnOpen CameraApplies = "cameraOpen"
	// CameraOnBoot: the AVD is running without loupe's camera flag, or is not
	// running. The next boot through loupe picks the image up.
	CameraOnBoot CameraApplies = "boot"
	// CameraLive: a simulator app launched with the camera shim is running,
	// and sees the change within a quarter of a second.
	CameraLive CameraApplies = "live"
	// CameraOnLaunch: no app on the simulator has the shim yet. The next
	// launch through loupe relaunches the app with it.
	CameraOnLaunch CameraApplies = "launch"
)

// CameraFeeder is a target whose back camera can be pointed at an image.
type CameraFeeder interface {
	// SetCamera makes the back camera see png, an encoded PNG.
	SetCamera(ctx context.Context, png []byte) (CameraApplies, error)
	// ClearCamera gives the back camera back. Nothing staged is not an
	// error.
	ClearCamera(ctx context.Context) (CameraApplies, error)
}

// ErrCameraShim is a camera shim that could not be built or signed.
var ErrCameraShim = errors.New("camera shim")

// LaunchBudgeter is a Launcher whose launch can take longer than an
// ordinary one; 0 means the ordinary budget.
type LaunchBudgeter interface {
	LaunchBudget() time.Duration
}

// ErrNoCameraDir is returned when there is no cache dir to stage an image in.
var ErrNoCameraDir = errors.New("no cache directory to stage a camera image in")

// cameraDir is where staged images live, one per target. A var so a test can
// point it at a temp dir.
var cameraDir = func() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", ErrNoCameraDir
	}
	return filepath.Join(cache, "loupe", "camera"), nil
}

// cameraFile is the staged image's path for a target. The name is the
// target's own, AVD names and UDIDs being safe in a file name already.
func cameraFile(name string) (string, error) {
	dir, err := cameraDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".png"), nil
}

// writeCameraFile replaces the staged image in one rename, so neither the
// emulator nor the shim ever reads half a file. The rename also moves the
// mtime the shim polls.
func writeCameraFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".camera-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// cameraBlank is what an emulator booted with the flag sees once its image
// is cleared: a missing file is the emulator's magenta error frame, so the
// flag's file has to stay until the next boot drops the flag.
var cameraBlank = func() []byte {
	img := image.NewGray(image.Rect(0, 0, 640, 480)) // zero is black
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}()

// emulatorCameraArgs is the -camera-back flag for an AVD's next boot, or nil
// when nothing is staged for it. A blank left by a clear while it ran is
// removed here, so that boot comes up with the AVD's own camera.
func emulatorCameraArgs(avd string) []string {
	path, err := cameraFile("avd-" + avd)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if bytes.Equal(b, cameraBlank) {
		os.Remove(path)
		return nil
	}
	return []string{"-camera-back", "imagefile:" + path}
}

// bootedWithCamera reports whether a running emulator's argv carries the
// flag for path. ps splits argv on spaces, so it is matched rejoined.
func bootedWithCamera(args []string, path string) bool {
	return strings.Contains(strings.Join(args, " "), "-camera-back imagefile:"+path)
}

// runningAVDArgs is the argv of the emulator running avd, or nil.
func runningAVDArgs(ctx context.Context, avd string) []string {
	ps, err := listProcesses(ctx)
	if err != nil {
		return nil
	}
	for _, p := range ps {
		if runsAVD(p.args, avd) {
			return p.args
		}
	}
	return nil
}

// SetCamera implements CameraFeeder. The emulator only takes an image file
// as a boot flag, `-camera-back imagefile:<path>`, and reads the file each
// time the camera opens; the console and the gRPC API have no call that
// swaps it on a running emulator. It trusts the file's extension, so the
// image is always staged as PNG, laid out for the emulator's crop
// (camera_emulator.go).
func (e *Emulator) SetCamera(ctx context.Context, png []byte) (CameraApplies, error) {
	path, err := cameraFile("avd-" + e.cfg.AVD)
	if err != nil {
		return "", err
	}
	staged, err := emulatorCameraImage(png, backCameraOrientation(e.cfg.Dir))
	if err != nil {
		return "", err
	}
	if err := writeCameraFile(path, staged); err != nil {
		return "", err
	}
	if args := runningAVDArgs(ctx, e.cfg.AVD); args != nil && bootedWithCamera(args, path) {
		return CameraOnOpen, nil
	}
	return CameraOnBoot, nil
}

// ClearCamera implements CameraFeeder.
func (e *Emulator) ClearCamera(ctx context.Context) (CameraApplies, error) {
	path, err := cameraFile("avd-" + e.cfg.AVD)
	if err != nil {
		return "", err
	}
	if args := runningAVDArgs(ctx, e.cfg.AVD); args != nil && bootedWithCamera(args, path) {
		if err := writeCameraFile(path, cameraBlank); err != nil {
			return "", err
		}
		return CameraOnOpen, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return CameraOnBoot, nil
}
