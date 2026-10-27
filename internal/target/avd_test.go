package target

import (
	"os"
	"path/filepath"
	"testing"
)

// The android CLI creates AVDs under ANDROID_USER_HOME and ignores
// ANDROID_AVD_HOME, so the two homes part exactly when that variable is set.
func TestCLIAVDHomeIgnoresAVDHomeVariable(t *testing.T) {
	t.Setenv("ANDROID_USER_HOME", "/u")
	t.Setenv("ANDROID_AVD_HOME", "")
	if AVDHome() != "/u/avd" || CLIAVDHome() != "/u/avd" {
		t.Fatalf("unset: AVDHome %q, CLIAVDHome %q", AVDHome(), CLIAVDHome())
	}
	t.Setenv("ANDROID_AVD_HOME", "/elsewhere")
	if AVDHome() != "/elsewhere" || CLIAVDHome() != "/u/avd" {
		t.Fatalf("set: AVDHome %q, CLIAVDHome %q", AVDHome(), CLIAVDHome())
	}
}

// TestScanAVDsReadsBothConfigSpellings runs the scan over copies of the two
// AVDs on the development machine: Pixel_10, written by Android Studio as
// `key=value`, and acmechat_blockstore, written by avdmanager as
// `key = value`. `avdmanager list avd` fails to load the first of them, which
// is the reason the scan reads files at all.
func TestScanAVDsReadsBothConfigSpellings(t *testing.T) {
	got, err := ScanAVDs(filepath.Join("testdata", "avd"))
	if err != nil {
		t.Fatal(err)
	}
	want := []AVD{
		{Name: "Pixel_10", DisplayName: "Pixel 10", Device: "pixel_10", APILevel: 37},
		{Name: "acmechat_blockstore", DisplayName: "acmechat blockstore", Device: "pixel_7", APILevel: 36},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d AVDs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		got[i].Dir = ""
		if got[i] != want[i] {
			t.Errorf("AVD %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestScanAVDsSkipsAnINIWithNoDirectory covers deleted.ini, which points at a
// directory that does not exist. That is what a half-deleted AVD leaves, and
// the emulator cannot boot it either.
func TestScanAVDsSkipsAnINIWithNoDirectory(t *testing.T) {
	got, _ := ScanAVDs(filepath.Join("testdata", "avd"))
	for _, a := range got {
		if a.Name == "deleted" {
			t.Errorf("listed %+v, whose directory does not exist", a)
		}
	}
}

// TestScanAVDsListsAnAVDWithAnUnreadableConfig: the AVD may still boot, and
// leaving it out would hide it with nothing to say why.
func TestScanAVDsListsAnAVDWithAnUnreadableConfig(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "bare.avd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bare.ini"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ScanAVDs(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "bare" || got[0].DisplayName != "bare" {
		t.Fatalf("got %+v, want one AVD named bare", got)
	}
}

func TestAVDDescriptionNeedsNoBoot(t *testing.T) {
	for _, tc := range []struct {
		device string
		form   string
	}{
		{"pixel_10", "phone"},
		{"pixel_tablet", "tablet"},
		{"tv_1080p", ""},
		{"wearos_large_round", ""},
		{"automotive_1024p_landscape", ""},
	} {
		d := AVD{Device: tc.device, APILevel: 36}.Description()
		if d.Form != tc.form || d.OS != "android" || d.APILevel != 36 || d.OSVersion != "" {
			t.Errorf("%s: %+v, want form %q, os android, api 36, no osVersion", tc.device, d, tc.form)
		}
	}
}

func TestAVDWithAHingeIsFoldable(t *testing.T) {
	d := AVD{Device: "pixel_9_pro_fold", APILevel: 36, Hinge: true}.Description()
	if d.Form != "foldable" || d.Transport != "emulator" {
		t.Errorf("%+v, want form foldable, transport emulator", d)
	}
	// A hinge on something that is not a handset, such as a TV profile,
	// stays without a form rather than becoming a foldable.
	if d := (AVD{Device: "tv_1080p", Hinge: true}).Description(); d.Form != "" {
		t.Errorf("tv with hinge: form %q, want none", d.Form)
	}
}

func TestAPILevelFrom(t *testing.T) {
	for _, tc := range []struct {
		target, sysdir string
		want           int
	}{
		{"android-37.0", "", 37},
		{"android-36", "", 36},
		{"", "system-images/android-35/google_apis/arm64-v8a/", 35},
		{"android-Baklava", "", 0},
	} {
		if got := apiLevelFrom(tc.target, tc.sysdir); got != tc.want {
			t.Errorf("apiLevelFrom(%q, %q) = %d, want %d", tc.target, tc.sysdir, got, tc.want)
		}
	}
}
