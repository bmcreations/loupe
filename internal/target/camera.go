package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	// running. The next boot through loupe picks the image up. Turning a
	// camera source on, off or to another kind changes the boot config
	// (-camera-back, hw.camera.back), and the emulator refuses a snapshot
	// saved under another camera ("different AVD configuration"), so that
	// boot cannot load loupe-warm and is a cold one. A new image at the same
	// path does not count.
	CameraOnBoot CameraApplies = "boot"
	// CameraLive: a simulator app launched with the camera shim is running,
	// and sees the change within a quarter of a second. On an AVD, the
	// running emulator already has -camera-back for that Mac camera.
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

// HostCamera is a camera on the Mac running loupe, as the emulator lists it.
type HostCamera struct {
	// Name is what the Mac calls it, "FaceTime HD Camera".
	Name string `json:"name"`
	// Label is what the emulator takes in -camera-back, "webcam1". Labels
	// shift as cameras come and go, so it is looked up on each boot.
	Label string `json:"label"`
}

// HostCameraFeeder is a CameraFeeder that can also take one of the Mac's
// cameras as its back camera. ClearCamera clears either kind of source.
type HostCameraFeeder interface {
	CameraFeeder
	// SetHostCamera stages the Mac camera called name. The name must be one
	// HostCameras lists now.
	SetHostCamera(ctx context.Context, name string) (CameraApplies, error)
	// HostCameras lists the Mac's cameras.
	HostCameras(ctx context.Context) ([]HostCamera, error)
}

// ErrNoHostCamera is a Mac camera name the emulator does not list.
var ErrNoHostCamera = errors.New("no such host camera")

// ErrCameraShim is a camera shim that could not be built or signed.
var ErrCameraShim = errors.New("camera shim")

// LaunchBudgeter is a Launcher whose launch can take longer than an
// ordinary one; 0 means the ordinary budget.
type LaunchBudgeter interface {
	LaunchBudget() time.Duration
}

// ErrNoCameraDir is returned when there is no cache dir to stage an image in.
var ErrNoCameraDir = errors.New("no cache directory to stage a camera image in")

// CameraDir is where staged images live, one per target. A var so a test can
// point it at a temp dir.
var CameraDir = func() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", ErrNoCameraDir
	}
	return filepath.Join(cache, "loupe", "camera"), nil
}

// CameraFile is the staged image's path for a target. The name is the
// target's own, AVD names and UDIDs being safe in a file name already.
func CameraFile(name string) (string, error) {
	dir, err := CameraDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".png"), nil
}

// WriteCameraFile replaces the staged image in one rename, so neither the
// emulator nor the shim ever reads half a file. The rename also moves the
// mtime the shim polls.
func WriteCameraFile(path string, b []byte) error {
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
