package target

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairAVDTarget(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "a37.avd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ini := filepath.Join(home, "a37.ini")
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(ini, "avd.ini.encoding=UTF-8\npath="+dir+"\ntarget=android-0\n")
	write(filepath.Join(dir, "config.ini"), "image.sysdir.1 = system-images/android-37.0/google_apis_playstore/arm64-v8a/\n")

	got, err := RepairAVDTarget(dir)
	if err != nil || got != "android-37" {
		t.Fatalf("RepairAVDTarget = %q, %v; want android-37", got, err)
	}
	raw, _ := os.ReadFile(ini)
	if !strings.Contains(string(raw), "target=android-37\n") || strings.Contains(string(raw), "android-0") {
		t.Fatalf("ini not repaired:\n%s", raw)
	}
	if got, err := RepairAVDTarget(dir); err != nil || got != "" {
		t.Fatalf("second repair = %q, %v; want no change", got, err)
	}
}
