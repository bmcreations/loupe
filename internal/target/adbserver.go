package target

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ADBHealth decides, from how track-devices keeps ending, when adb counts as
// unreachable and when to restart its server.
//
// On 2026-09-27 the adb server had been up since the Thursday before and
// accepted every connection only to reset it: `adb devices` printed
// "protocol fault (couldn't read status): Connection reset by peer", and
// track-devices failed 2304 times in loupe-run.log, each one only a line on
// stderr. The viewer sat on "Connecting" throughout. Killing the server and
// running `adb start-server` fixed it within 30 seconds, and the person who
// could do that was at the Mac, not at the phone this is used from.
//
// So loupe restarts the server itself, after RestartAfter failures in a row
// and at most once per RestartEvery. That drops every adb client for the
// second or two it takes, Android Studio's logcat and debugger among them,
// which reconnect on their own. A server that has failed six times running is
// already giving them nothing.
//
// Only a failure where adb ran and exited counts towards a restart. One where
// the binary could not start at all is not the server's fault, and a new
// server will not fix it.
type ADBHealth struct {
	// DownAfter is how many consecutive failures report adb unreachable.
	DownAfter int
	// RestartAfter is how many consecutive failures restart the server.
	RestartAfter int
	// RestartEvery is the least time between two restarts, so a server that
	// a restart does not fix is not killed every 30 seconds.
	RestartEvery time.Duration

	failures    int
	down        bool
	lastRestart time.Time
}

// NewADBHealth returns the thresholds loupe runs with. With track-devices'
// backoff of 1s doubling to 30s, adb is reported unreachable about 3s into a
// wedge and restarted about 30s in.
func NewADBHealth() *ADBHealth {
	return &ADBHealth{DownAfter: 3, RestartAfter: 6, RestartEvery: 10 * time.Minute}
}

// ADBVerdict is what one failure means.
type ADBVerdict struct {
	// Failures is how many track-devices runs in a row have ended without a
	// device list.
	Failures int
	// BecameDown is true on the failure that crosses DownAfter, and only
	// that one, so the caller reports it once.
	BecameDown bool
	// Restart is true when the caller should restart the adb server now.
	Restart bool
}

// Failed records a track-devices run that ended without delivering a list.
func (h *ADBHealth) Failed(now time.Time, err error) ADBVerdict {
	h.failures++
	v := ADBVerdict{Failures: h.failures}
	if !h.down && h.failures >= h.DownAfter {
		h.down = true
		v.BecameDown = true
	}
	var exit *exec.ExitError
	if h.failures >= h.RestartAfter && errors.As(err, &exit) &&
		(h.lastRestart.IsZero() || now.Sub(h.lastRestart) >= h.RestartEvery) {
		h.lastRestart = now
		v.Restart = true
	}
	return v
}

// Heard records a device list arriving. It returns true when that ends a run
// of failures long enough to have been reported.
func (h *ADBHealth) Heard() bool {
	recovered := h.down
	h.failures = 0
	h.down = false
	return recovered
}

// serverPort is the adb server's port, which adb itself reads from the same
// variable.
func serverPort() int {
	if p, err := strconv.Atoi(os.Getenv("ANDROID_ADB_SERVER_PORT")); err == nil && p > 0 {
		return p
	}
	return 5037
}

// RestartServer replaces the adb server. It asks politely first, then, if an
// adb process is still listening on the server port, signals it: a server
// that resets every connection resets `kill-server`'s too. Only a process
// named adb is signalled. It returns the pid it had to signal, or 0.
func (a ADB) RestartServer(ctx context.Context) (int, error) {
	kctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = exec.CommandContext(kctx, a.bin(), "kill-server").Run()
	cancel()

	killed := 0
	if pid, comm := serverListener(ctx, serverPort()); pid != 0 {
		if comm != "adb" {
			return 0, fmt.Errorf("port %d is held by %s (pid %d), not adb; left alone", serverPort(), comm, pid)
		}
		killed = pid
		_ = syscall.Kill(pid, syscall.SIGTERM)
		if !waitGone(ctx, pid, 2*time.Second) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			waitGone(ctx, pid, 2*time.Second)
		}
	}

	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(sctx, a.bin(), "start-server").CombinedOutput(); err != nil {
		if msg := lastLine(string(out)); msg != "" {
			return killed, fmt.Errorf("adb start-server: %w: %s", err, msg)
		}
		return killed, fmt.Errorf("adb start-server: %w", err)
	}
	return killed, nil
}

// serverListener names the process listening on the adb server port, or
// returns 0 if nothing is.
func serverListener(ctx context.Context, port int) (int, string) {
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(lctx, "lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc").Output()
	if err != nil {
		return 0, ""
	}
	return parseLsofListener(string(out))
}

// parseLsofListener reads the first process from lsof's -Fpc output: a "p"
// line with the pid, then a "c" line with the command name.
func parseLsofListener(out string) (int, string) {
	pid, comm := 0, ""
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			if pid != 0 {
				return pid, comm
			}
			pid, _ = strconv.Atoi(line[1:])
		case 'c':
			comm = line[1:]
		}
	}
	return pid, comm
}

func waitGone(ctx context.Context, pid int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}

// lastLine is the last non-empty line of s, trimmed.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
