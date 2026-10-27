package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/bmcreations/loupe/internal/config"
)

// fakeFlags mirrors the subset of main's flag set that settle inspects. It is
// a separate FlagSet so a test can decide what was "given on the command line"
// without touching the process-wide one.
func fakeFlags(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	def := config.Default()
	fs.Int("max-fps", def.MaxFPS, "")
	fs.Int("bitrate", def.BitRate, "")
	fs.String("addr", def.Addr, "")
	fs.String("adb", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return fs
}

func TestSettleWritesDefaultsOnFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	got, cfg, _, err := settle(fakeFlags(t), path)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
	if cfg != config.Default() {
		t.Errorf("cfg = %+v, want defaults", cfg)
	}

	// The file has to exist afterwards, or a settings UI opens on a path that
	// is not there and has nothing to show.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	var onDisk config.Config
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if onDisk != config.Default() {
		t.Errorf("on disk = %+v, want defaults", onDisk)
	}
}

func TestSettleReportsOnlyExplicitFlagsAsPinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	// -max-fps is given, -bitrate is not. A flag left at its default must not
	// pin: that would make every setting permanently unwritable through the API.
	_, _, pinned, err := settle(fakeFlags(t, "-max-fps", "30"), path)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !pinned["maxFps"] {
		t.Error("maxFps: given explicitly, want pinned")
	}
	if pinned["bitRate"] {
		t.Error("bitRate: not given, want unpinned")
	}
	if pinned["addr"] {
		t.Error("addr: not given, want unpinned")
	}
}

func TestSettleIgnoresFlagsThatAreNotSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	// -adb is a machine detail, not a setting. It has no config field, and
	// naming one anyway would put a key in the response that cannot be written.
	_, _, pinned, err := settle(fakeFlags(t, "-adb", "/usr/bin/adb"), path)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(pinned) != 0 {
		t.Errorf("pinned = %v, want empty", pinned)
	}
}

func TestSettleKeepsGoingWhenTheFileIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A corrupt file must not stop the server booting. Streaming is the point;
	// settings are not worth refusing to start over.
	_, cfg, _, err := settle(fakeFlags(t), path)
	if err == nil {
		t.Error("want an error describing the unreadable file")
	}
	if cfg != config.Default() {
		t.Errorf("cfg = %+v, want defaults", cfg)
	}
}
