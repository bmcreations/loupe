package android

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

func withCameraDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := target.CameraDir
	target.CameraDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { target.CameraDir = old })
	return dir
}

func TestEmulatorCameraArgsOnlyWithAStagedImage(t *testing.T) {
	dir := withCameraDir(t)
	if got := emulatorCameraArgs("Pixel_9", "emulator"); got != nil {
		t.Errorf("nothing staged: %q; want no flag, the AVD's own camera", got)
	}
	path := filepath.Join(dir, "avd-Pixel_9.png")
	if err := target.WriteCameraFile(path, []byte("\x89PNG image")); err != nil {
		t.Fatal(err)
	}
	want := []string{"-camera-back", "imagefile:" + path}
	if got := emulatorCameraArgs("Pixel_9", "emulator"); !reflect.DeepEqual(got, want) {
		t.Errorf("staged: %q; want %q", got, want)
	}
}

// A clear while the AVD ran leaves the blank behind, since a missing file is
// the emulator's magenta error frame. The next boot drops it and the flag.
func TestEmulatorCameraArgsDropsTheBlankLeftByAClear(t *testing.T) {
	dir := withCameraDir(t)
	path := filepath.Join(dir, "avd-Pixel_9.png")
	if err := target.WriteCameraFile(path, cameraBlank); err != nil {
		t.Fatal(err)
	}
	if got := emulatorCameraArgs("Pixel_9", "emulator"); got != nil {
		t.Errorf("blank staged: %q; want no flag", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("blank still there (%v); want it removed", err)
	}
}

func TestBootedWithCamera(t *testing.T) {
	path := "/Users/x/Library/Caches/loupe/camera/avd-Pixel_9.png"
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"qemu-system-aarch64", "-avd", "Pixel_9", "-camera-back", "imagefile:" + path, "-no-boot-anim"}, true},
		{[]string{"qemu-system-aarch64", "-avd", "Pixel_9", "-no-boot-anim"}, false},
		{[]string{"qemu-system-aarch64", "-avd", "Pixel_9", "-camera-back", "virtualscene"}, false},
		// ps splits argv on spaces, so a path with one arrives in pieces.
		{[]string{"qemu", "-camera-back", "imagefile:/Users/A", "B/avd-Pixel_9.png"}, false},
	} {
		if got := bootedWithCamera(tc.args, path); got != tc.want {
			t.Errorf("%q: %v; want %v", tc.args, got, tc.want)
		}
	}
	spaced := "/Users/A B/avd-Pixel_9.png"
	if !bootedWithCamera([]string{"qemu", "-camera-back", "imagefile:/Users/A", "B/avd-Pixel_9.png"}, spaced) {
		t.Error("a path with a space, split by ps: want it matched")
	}
}
