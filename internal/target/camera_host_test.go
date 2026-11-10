package target

import (
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const webcamListSample = `List of web cameras connected to the computer:
 Camera 'Insta360 Link 2' can be specified by label as 'webcam0' or by id as '0x1000002e1a4c04' and will use pixel format 'NV12'
 Camera 'FaceTime HD Camera' can be specified by label as 'webcam1' or by id as '1F06D5FF-4C76-410E-9770-C04C467C1317' and will use pixel format 'NV12'
 Camera 'Insta360 Virtual Camera' can be specified by label as 'webcam2' or by id as '4BA85DDE-7965-43F0-BEF3-EF2BD91FB4D3' and will use pixel format 'BGR4'
`

func TestParseWebcamList(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []HostCamera
	}{
		{"three cameras, names with spaces", webcamListSample, []HostCamera{
			{"Insta360 Link 2", "webcam0"},
			{"FaceTime HD Camera", "webcam1"},
			{"Insta360 Virtual Camera", "webcam2"},
		}},
		{"header only", "List of web cameras connected to the computer:\n", nil},
		{"empty", "", nil},
		{"crlf", " Camera 'A B' can be specified by label as 'webcam0' or by id as 'x'\r\n", []HostCamera{{"A B", "webcam0"}}},
		{"quote in the name", " Camera 'Bob's cam' can be specified by label as 'webcam3' or by id as 'x'\n", []HostCamera{{"Bob's cam", "webcam3"}}},
		{"noise skipped", "emulator: INFO: something\n Camera 'X' can be specified by label as 'webcam1' or by id as 'y'\n", []HostCamera{{"X", "webcam1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseWebcamList(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v; want %+v", got, tc.want)
			}
		})
	}
}

func withHostCameras(t *testing.T, cams []HostCamera, err error) {
	t.Helper()
	old := hostCameraLister
	hostCameraLister = func(context.Context, string) ([]HostCamera, error) { return cams, err }
	t.Cleanup(func() { hostCameraLister = old })
}

func stageHost(t *testing.T, dir, avd, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "avd-"+avd+".host"), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEmulatorCameraArgsHostCamera(t *testing.T) {
	cams := []HostCamera{{"Insta360 Link 2", "webcam0"}, {"FaceTime HD Camera", "webcam1"}}
	for _, tc := range []struct {
		name   string
		staged string
		cams   []HostCamera
		err    error
		want   []string
	}{
		{"resolved to its current label", "FaceTime HD Camera", cams, nil, []string{"-camera-back", "webcam1"}},
		{"label shifted since staging", "Insta360 Link 2", cams[1:], nil, nil},
		{"missing camera boots with the AVD's own", "Gone Cam", cams, nil, nil},
		{"empty list", "Insta360 Link 2", nil, nil, nil},
		{"lister fails", "Insta360 Link 2", nil, errors.New("no emulator"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := withCameraDir(t)
			withHostCameras(t, tc.cams, tc.err)
			stageHost(t, dir, "Pixel_9", tc.staged)
			if got := emulatorCameraArgs("Pixel_9", "emulator"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
	t.Run("label follows the camera", func(t *testing.T) {
		dir := withCameraDir(t)
		withHostCameras(t, []HostCamera{{"FaceTime HD Camera", "webcam0"}}, nil)
		stageHost(t, dir, "Pixel_9", "FaceTime HD Camera")
		if got := emulatorCameraArgs("Pixel_9", "emulator"); !reflect.DeepEqual(got, []string{"-camera-back", "webcam0"}) {
			t.Errorf("got %q; want webcam0", got)
		}
	})
}

func TestBootedWithHostCamera(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		label string
		want  bool
	}{
		{[]string{"qemu", "-avd", "P", "-camera-back", "webcam1", "-no-boot-anim"}, "webcam1", true},
		{[]string{"qemu", "-camera-back", "webcam1"}, "webcam0", false},
		{[]string{"qemu", "-camera-back", "webcam10"}, "webcam1", false},
		{[]string{"qemu", "-camera-back", "imagefile:/x/avd-P.png"}, "webcam1", false},
		{[]string{"qemu", "-avd", "P"}, "webcam1", false},
		{nil, "webcam1", false},
	} {
		if got := bootedWithHostCamera(tc.args, tc.label); got != tc.want {
			t.Errorf("%q %s: %v; want %v", tc.args, tc.label, got, tc.want)
		}
	}
}

func newTestEmulator() *Emulator {
	return NewEmulator(ADB{}, EmulatorConfig{AVD: "Pixel_9"}, "", "")
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// Only one source is staged: an image drops a Mac camera and a Mac camera
// drops an image, and a clear drops either.
func TestCameraSourcesAreExclusive(t *testing.T) {
	dir := withCameraDir(t)
	withHostCameras(t, []HostCamera{{"FaceTime HD Camera", "webcam1"}}, nil)
	withProcesses(t, nil)
	e := newTestEmulator()
	img, host := filepath.Join(dir, "avd-Pixel_9.png"), filepath.Join(dir, "avd-Pixel_9.host")
	ctx := context.Background()

	applies, err := e.SetHostCamera(ctx, "FaceTime HD Camera")
	if err != nil || applies != CameraOnBoot {
		t.Fatalf("SetHostCamera: %v, %v; want boot", applies, err)
	}
	if b, _ := os.ReadFile(host); string(b) != "FaceTime HD Camera" || exists(img) {
		t.Fatalf("host %q, image there %v; want only the host camera", b, exists(img))
	}

	if _, err := e.SetCamera(ctx, encodePNG(t, image.NewRGBA(image.Rect(0, 0, 8, 8)))); err != nil {
		t.Fatal(err)
	}
	if exists(host) || !exists(img) {
		t.Fatalf("after an image: host %v, image %v; want only the image", exists(host), exists(img))
	}

	if _, err := e.SetHostCamera(ctx, "FaceTime HD Camera"); err != nil {
		t.Fatal(err)
	}
	if !exists(host) || exists(img) {
		t.Fatalf("after a host camera: host %v, image %v; want only the host camera", exists(host), exists(img))
	}

	if applies, err := e.ClearCamera(ctx); err != nil || applies != CameraOnBoot {
		t.Fatalf("ClearCamera: %v, %v; want boot", applies, err)
	}
	if exists(host) || exists(img) {
		t.Errorf("after a clear: host %v, image %v; want neither", exists(host), exists(img))
	}
	if _, err := e.ClearCamera(ctx); err != nil {
		t.Errorf("clearing nothing: %v; want no error", err)
	}
}

func TestSetHostCameraUnknownName(t *testing.T) {
	dir := withCameraDir(t)
	withHostCameras(t, []HostCamera{{"FaceTime HD Camera", "webcam1"}}, nil)
	_, err := newTestEmulator().SetHostCamera(context.Background(), "Nope")
	if !errors.Is(err, ErrNoHostCamera) {
		t.Errorf("err %v; want ErrNoHostCamera", err)
	}
	if exists(filepath.Join(dir, "avd-Pixel_9.host")) {
		t.Error("an unknown camera was staged")
	}
}

func TestSetHostCameraIsLiveWhenAlreadyBootedWithIt(t *testing.T) {
	withCameraDir(t)
	withHostCameras(t, []HostCamera{{"FaceTime HD Camera", "webcam1"}}, nil)
	withProcesses(t, []process{{args: []string{"qemu-system-aarch64", "-avd", "Pixel_9", "-camera-back", "webcam1"}}})
	applies, err := newTestEmulator().SetHostCamera(context.Background(), "FaceTime HD Camera")
	if err != nil || applies != CameraLive {
		t.Errorf("%v, %v; want live", applies, err)
	}
	// The same camera under another label than the one it booted with.
	withHostCameras(t, []HostCamera{{"FaceTime HD Camera", "webcam0"}}, nil)
	applies, err = newTestEmulator().SetHostCamera(context.Background(), "FaceTime HD Camera")
	if err != nil || applies != CameraOnBoot {
		t.Errorf("%v, %v; want boot", applies, err)
	}
}

// Switching from an image the running emulator reads to a Mac camera leaves
// the image blank rather than missing, and the next boot drops it.
func TestSetHostCameraBlanksARunningImage(t *testing.T) {
	dir := withCameraDir(t)
	withHostCameras(t, []HostCamera{{"FaceTime HD Camera", "webcam1"}}, nil)
	img := filepath.Join(dir, "avd-Pixel_9.png")
	if err := writeCameraFile(img, []byte("\x89PNG image")); err != nil {
		t.Fatal(err)
	}
	withProcesses(t, []process{{args: []string{"qemu-system-aarch64", "-avd", "Pixel_9", "-camera-back", "imagefile:" + img}}})
	if applies, err := newTestEmulator().SetHostCamera(context.Background(), "FaceTime HD Camera"); err != nil || applies != CameraOnBoot {
		t.Fatalf("%v, %v; want boot", applies, err)
	}
	if b, _ := os.ReadFile(img); string(b) != string(cameraBlank) {
		t.Errorf("image is %d bytes; want the blank", len(b))
	}
	if got := emulatorCameraArgs("Pixel_9", "emulator"); !reflect.DeepEqual(got, []string{"-camera-back", "webcam1"}) {
		t.Errorf("boot args %q; want webcam1", got)
	}
	if exists(img) {
		t.Error("blank survived the boot")
	}
}

func withProcesses(t *testing.T, ps []process) {
	t.Helper()
	restore := listProcesses
	listProcesses = func(context.Context) ([]process, error) { return ps, nil }
	t.Cleanup(func() { listProcesses = restore })
}
