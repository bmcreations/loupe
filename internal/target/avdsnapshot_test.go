package target

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEmulatorArgsLoadTheWarmSnapshotOnlyWhenThereIsOne(t *testing.T) {
	cold := emulatorArgs("api34", 5556, "host", "", true)
	want := []string{"-avd", "api34", "-port", "5556", "-gpu", "host",
		"-no-snapshot-save", "-no-boot-anim", "-netdelay", "none", "-netspeed", "full"}
	if !reflect.DeepEqual(cold, want) {
		t.Errorf("cold:\n got %q\nwant %q", cold, want)
	}
	warm := emulatorArgs("api34", 5556, "host", WarmSnapshot, true)
	want = []string{"-avd", "api34", "-port", "5556", "-gpu", "host",
		"-snapshot", "loupe-warm",
		"-no-snapshot-save", "-no-boot-anim", "-netdelay", "none", "-netspeed", "full"}
	if !reflect.DeepEqual(warm, want) {
		t.Errorf("warm:\n got %q\nwant %q", warm, want)
	}
}

func TestEmulatorArgsHideTheWindowWithoutOne(t *testing.T) {
	got := emulatorArgs("api34", 5556, "host", WarmSnapshot, false)
	want := []string{"-avd", "api34", "-port", "5556", "-gpu", "host",
		"-snapshot", "loupe-warm", "-qt-hide-window", "-no-audio",
		"-no-snapshot-save", "-no-boot-anim", "-netdelay", "none", "-netspeed", "full"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("headless:\n got %q\nwant %q", got, want)
	}
}

// default_boot on api34_emoji_check was an 11-byte snapshot.pb and nothing
// else. Asking the emulator to load that is a cold boot that says it is not.
func TestHasSnapshotNeedsTheRAMImage(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, "snapshots", WarmSnapshot)
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	if hasSnapshot(dir, WarmSnapshot) {
		t.Fatal("empty directory read as a snapshot")
	}
	if err := os.WriteFile(filepath.Join(snap, "snapshot.pb"), make([]byte, 11), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasSnapshot(dir, WarmSnapshot) {
		t.Fatal("snapshot.pb alone read as a snapshot")
	}
	if err := os.WriteFile(filepath.Join(snap, "ram.bin"), []byte{1}, 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasSnapshot(dir, WarmSnapshot) {
		t.Fatal("snapshot.pb and ram.bin not read as a snapshot")
	}
	if hasSnapshot("", WarmSnapshot) {
		t.Fatal("no AVD dir read as a snapshot")
	}
}

func TestLoadedSnapshotReadsTheEmulatorsOwnLine(t *testing.T) {
	// Lines from emulator 36 booting api34_emoji_check with -snapshot loupe-warm.
	log := []byte("INFO         | Loading snapshot 'loupe-warm'...\n" +
		"INFO         | Successfully loaded snapshot 'loupe-warm' using 608 ms\n")
	if !loadedSnapshot(log, WarmSnapshot) {
		t.Error("a successful load was missed")
	}
	if loadedSnapshot([]byte("INFO         | Loading snapshot 'loupe-warm'...\n"), WarmSnapshot) {
		t.Error("an attempted load read as a successful one")
	}
	if loadedSnapshot(log, "default_boot") {
		t.Error("another snapshot's load read as this one")
	}
}
