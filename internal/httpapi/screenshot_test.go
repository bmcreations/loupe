package httpapi

import (
	"strings"
	"testing"
	"time"
)

// A target id is whatever adb reports: an emulator's is emulator-5554, and a
// device's is a serial that can carry a colon on a network target. A share
// sheet and a browser save both take this straight to the filesystem.
func TestScreenshotNameIsSafeAndSaysWhichDevice(t *testing.T) {
	at := time.Date(2026, 9, 21, 22, 3, 8, 0, time.UTC)

	cases := map[string]string{
		"emulator-5554":     "loupe-emulator-5554-20260921-220308.png",
		"ACME00000000001":   "loupe-ACME00000000001-20260921-220308.png",
		"192.168.1.20:5555": "loupe-192-168-1-20-5555-20260921-220308.png",
		"../../etc/passwd":  "loupe-------etc-passwd-20260921-220308.png",
	}
	for id, want := range cases {
		if got := screenshotName(id, at); got != want {
			t.Errorf("screenshotName(%q) = %q, want %q", id, got, want)
		}
	}
}

// The name goes into a Content-Disposition header, so a separator that survives
// would let a target id break out of the quoted string.
func TestScreenshotNameCannotEscapeTheHeader(t *testing.T) {
	got := screenshotName(`a"; attachment; filename="b`, time.Now())
	for _, bad := range []string{`"`, ";", " ", "\r", "\n", "/"} {
		if strings.Contains(got, bad) {
			t.Errorf("screenshotName kept %q in %q", bad, got)
		}
	}
}
