package target

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func TestCameraEnvCalls(t *testing.T) {
	const lib = "/c/loupecam-0123456789ab.dylib"
	const file = "/c/sim-UDID.png"
	set := [][]string{
		{"setenv", "LOUPE_CAMERA_FILE", file},
		{"setenv", "DYLD_INSERT_LIBRARIES", lib},
	}
	unset := [][]string{
		{"unsetenv", "DYLD_INSERT_LIBRARIES"},
		{"unsetenv", "LOUPE_CAMERA_FILE"},
	}
	for _, tc := range []struct {
		name            string
		staged, current string
		want            [][]string
	}{
		{"staged, nothing set", file, "", set},
		{"staged, already ours", file, lib, set},
		{"staged, an older shim of ours", file, "/c/loupecam-ffffffffffff.dylib", set},
		{"staged, someone else's insert is left alone", file, "/x/other.dylib", nil},
		{"cleared, ours is removed", "", lib, unset},
		{"cleared, nothing set", "", "", nil},
		{"cleared, someone else's insert is left alone", "", "/x/other.dylib", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cameraEnvCalls(tc.staged, lib, tc.current)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cameraEnvCalls(%q, lib, %q) = %q, want %q", tc.staged, tc.current, got, tc.want)
			}
		})
	}
}

// An app started from its icon loads the shim through the simulator's
// launchd and writes the marker, without passing through Launch.
func TestShimMarkerCountsAsLive(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sim-UDID.png")
	if markerAlive(file) {
		t.Fatal("no marker should not be live")
	}
	if err := os.WriteFile(file+".loaded", []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !markerAlive(file) {
		t.Error("a marker naming a running pid should be live")
	}
	if err := os.WriteFile(file+".loaded", []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if markerAlive(file) {
		t.Error("a marker naming a gone pid should not be live")
	}
}
