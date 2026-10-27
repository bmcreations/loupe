package target

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exitErr is a real *exec.ExitError, wrapped the way TrackDevices wraps it.
func exitErr(t *testing.T) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", "exit 1").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("sh exit 1 gave %T, not an ExitError", err)
	}
	return errors.Join(errors.New("adb track-devices"), err)
}

func TestADBHealthReportsDownOnceAndRestartsAtMostEveryInterval(t *testing.T) {
	h := &ADBHealth{DownAfter: 3, RestartAfter: 6, RestartEvery: 10 * time.Minute}
	now := time.Unix(0, 0)
	fault := exitErr(t)

	var downs, restarts []int
	for i := 1; i <= 8; i++ {
		v := h.Failed(now, fault)
		if v.Failures != i {
			t.Fatalf("failure %d counted as %d", i, v.Failures)
		}
		if v.BecameDown {
			downs = append(downs, i)
		}
		if v.Restart {
			restarts = append(restarts, i)
		}
		now = now.Add(30 * time.Second)
	}
	if len(downs) != 1 || downs[0] != 3 {
		t.Errorf("reported down on failures %v, want only on 3", downs)
	}
	if len(restarts) != 1 || restarts[0] != 6 {
		t.Errorf("restarted on failures %v, want only on 6", restarts)
	}

	// Ten minutes after the first restart, a server still failing is
	// restarted again.
	if v := h.Failed(now.Add(10*time.Minute), fault); !v.Restart {
		t.Error("no second restart ten minutes after the first")
	}

	if !h.Heard() {
		t.Error("Heard after a reported outage did not report a recovery")
	}
	if h.Heard() {
		t.Error("a second Heard reported a recovery again")
	}
	if v := h.Failed(now, fault); v.Failures != 1 || v.BecameDown {
		t.Errorf("after recovering, the next failure = %+v, want a count of 1 and not down", v)
	}
}

// A binary that cannot start is not the server's fault, and killing the
// server would not fix it.
func TestADBHealthDoesNotRestartForAnAdbThatCannotStart(t *testing.T) {
	h := &ADBHealth{DownAfter: 1, RestartAfter: 1, RestartEvery: time.Minute}
	_, err := exec.Command("/nonexistent/adb").Output()
	v := h.Failed(time.Unix(0, 0), err)
	if !v.BecameDown {
		t.Error("an adb that cannot start is not reported down")
	}
	if v.Restart {
		t.Error("an adb that cannot start triggered a server restart")
	}
}

// Without stderr the 2304 lines in loupe-run.log said only "exit status 1".
func TestTrackDevicesErrorCarriesAdbsReason(t *testing.T) {
	fakeADB := filepath.Join(t.TempDir(), "adb")
	script := "#!/bin/sh\n" +
		"echo \"adb: protocol fault (couldn't read status): Connection reset by peer\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(fakeADB, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := ADB{Path: fakeADB}.TrackDevices(context.Background(), func([]DeviceInfo) {
		t.Error("a failing track-devices delivered a list")
	})
	if err == nil || !strings.Contains(err.Error(), "protocol fault") {
		t.Fatalf("err = %v, want adb's own reason in it", err)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Errorf("err %v does not wrap the ExitError ADBHealth keys restarts on", err)
	}
}

func TestParseLsofListener(t *testing.T) {
	for _, c := range []struct {
		in   string
		pid  int
		comm string
	}{
		{"p4242\ncadb\n", 4242, "adb"},
		{"p4242\ncadb\np5151\ncother\n", 4242, "adb"},
		{"", 0, ""},
	} {
		pid, comm := parseLsofListener(c.in)
		if pid != c.pid || comm != c.comm {
			t.Errorf("parseLsofListener(%q) = %d, %q, want %d, %q", c.in, pid, comm, c.pid, c.comm)
		}
	}
}
