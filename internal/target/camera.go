package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/camfeed"
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

// hostCameraFile is the staged Mac camera's path for an AVD: a file holding
// the camera's name. Only one of it and the staged image exists.
func hostCameraFile(avd string) (string, error) {
	dir, err := cameraDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "avd-"+avd+".host"), nil
}

// stagedHostCamera is the Mac camera name staged for an AVD, or "".
func stagedHostCamera(avd string) string {
	path, err := hostCameraFile(avd)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// hostCameraLister lists the Mac's cameras with the emulator at emulatorPath.
// A var so a test does not run the emulator.
var hostCameraLister = func(ctx context.Context, emulatorPath string) ([]HostCamera, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, emulatorPath, "-webcam-list").Output()
	if err != nil {
		return nil, fmt.Errorf("%s -webcam-list: %w", emulatorPath, err)
	}
	return parseWebcamList(string(out)), nil
}

// webcamLine matches a line of `emulator -webcam-list`:
//
//	Camera 'Insta360 Link 2' can be specified by label as 'webcam0' or by id as '0x1000...' and will use pixel format 'NV12'
//
// The name is greedy, so a name with a quote in it still ends at the label.
var webcamLine = regexp.MustCompile(`^\s*Camera '(.+)' can be specified by label as '([^']+)'`)

// parseWebcamList reads the cameras out of -webcam-list's output. Lines of
// any other shape, the header included, are skipped.
func parseWebcamList(out string) []HostCamera {
	var cams []HostCamera
	for _, line := range strings.Split(out, "\n") {
		if m := webcamLine.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			cams = append(cams, HostCamera{Name: m[1], Label: m[2]})
		}
	}
	return cams
}

// findHostCamera is the first camera called name.
func findHostCamera(cams []HostCamera, name string) (HostCamera, bool) {
	for _, c := range cams {
		if c.Name == name {
			return c, true
		}
	}
	return HostCamera{}, false
}

// emulatorCameraArgs is the -camera-back flag for an AVD's next boot, or nil
// when nothing is staged for it. A staged Mac camera is looked up by name at
// this boot, since labels shift; one that is not there leaves the AVD its own
// camera, with the reason logged. A blank left by a clear while it ran is
// removed here, so that boot comes up with the AVD's own camera.
func emulatorCameraArgs(avd, emulatorPath string) []string {
	if name := stagedHostCamera(avd); name != "" {
		// A blank image left by switching sources while the AVD ran.
		if img, err := cameraFile("avd-" + avd); err == nil {
			os.Remove(img)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cams, err := hostCameraLister(ctx, emulatorPath)
		if err != nil {
			log.Printf("emulator %s: host camera %q not used: %v", avd, name, err)
			return nil
		}
		cam, ok := findHostCamera(cams, name)
		if !ok {
			log.Printf("emulator %s: host camera %q not used: not connected (found %d cameras)", avd, name, len(cams))
			return nil
		}
		return []string{"-camera-back", cam.Label}
	}
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

// bootedWithHostCamera reports whether a running emulator's argv has
// -camera-back followed by label.
func bootedWithHostCamera(args []string, label string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-camera-back" && args[i+1] == label {
			return true
		}
	}
	return false
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
	// One source at a time: an image replaces a staged Mac camera.
	if err := e.removeHostCamera(); err != nil {
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

// ClearCamera implements CameraFeeder. It clears either kind of source.
func (e *Emulator) ClearCamera(ctx context.Context) (CameraApplies, error) {
	path, err := cameraFile("avd-" + e.cfg.AVD)
	if err != nil {
		return "", err
	}
	e.stopHandsetFeed()
	if err := e.removeHostCamera(); err != nil {
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

// removeHostCamera unstages a Mac camera. Nothing staged is not an error.
func (e *Emulator) removeHostCamera() error {
	hp, err := hostCameraFile(e.cfg.AVD)
	if err != nil {
		return err
	}
	if err := os.Remove(hp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// HostCameras implements HostCameraFeeder.
func (e *Emulator) HostCameras(ctx context.Context) ([]HostCamera, error) {
	return hostCameraLister(ctx, e.cfg.EmulatorPath)
}

// SetHostCamera implements HostCameraFeeder. The emulator takes a Mac camera
// only as a boot flag, `-camera-back webcamN`, so the name is staged and
// resolved to its label at the next boot. It applies live only when the
// running emulator already has the flag for this camera's current label;
// otherwise the boot config changes and the next boot is cold
// (CameraOnBoot).
func (e *Emulator) SetHostCamera(ctx context.Context, name string) (CameraApplies, error) {
	cams, err := e.HostCameras(ctx)
	if err != nil {
		return "", err
	}
	cam, ok := findHostCamera(cams, name)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNoHostCamera, name)
	}
	if name != camfeed.CameraName() {
		e.stopHandsetFeed()
	}
	hp, err := hostCameraFile(e.cfg.AVD)
	if err != nil {
		return "", err
	}
	img, err := cameraFile("avd-" + e.cfg.AVD)
	if err != nil {
		return "", err
	}
	args := runningAVDArgs(ctx, e.cfg.AVD)
	// An image the running emulator reads stays, blanked, so it does not
	// turn magenta; emulatorCameraArgs drops it at the next boot.
	if args != nil && bootedWithCamera(args, img) {
		if err := writeCameraFile(img, cameraBlank); err != nil {
			return "", err
		}
	} else if err := os.Remove(img); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(hp), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(hp, []byte(name), 0o644); err != nil {
		return "", err
	}
	handsetLiveMu.Lock()
	delete(handsetLives, e.cfg.AVD)
	handsetLiveMu.Unlock()
	if bootedWithHostCamera(args, cam.Label) {
		return CameraLive, nil
	}
	return CameraOnBoot, nil
}
