package target

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Boot states reported by BootStatus.State.
const (
	BootStateUp      = "up"      // attached and adb says it is ready
	BootStateBooting = "booting" // an emulator process for this AVD is alive but not ready
	BootStateStopped = "stopped" // no emulator process for this AVD
)

// BootStatus is a snapshot of an AVD's boot, for a caller polling a boot it
// started with `wait=0` or one that timed out. It reads no lock a boot holds.
type BootStatus struct {
	AVD string `json:"avd"`
	// State is "up", "booting" or "stopped".
	State string `json:"state"`
	// ProcessAlive is whether an emulator process for this AVD exists, one
	// this server started or not.
	ProcessAlive bool `json:"processAlive"`
	// PID is that process, or 0.
	PID int `json:"pid,omitempty"`
	// Serial is the adb serial it appears (or will appear) as.
	Serial string `json:"serial,omitempty"`
	// ADBState is adb's word for the serial ("device", "offline",
	// "unauthorized"), or "absent" when adb does not list it. Empty when
	// there is no serial yet.
	ADBState string `json:"adbState,omitempty"`
	// BootCompleted is sys.boot_completed. Read only while adb says the
	// device is ready and it is not paused, since getprop hangs on a
	// paused VM.
	BootCompleted bool `json:"bootCompleted"`
	// ElapsedMs is how long this server's boot has run, 0 when it started
	// none.
	ElapsedMs int64 `json:"elapsedMs,omitempty"`
	// LogPath is where the emulator's stdout and stderr go.
	LogPath string `json:"logPath,omitempty"`
}

// BootReporter is a target that can say how its boot is going.
type BootReporter interface {
	BootStatus(ctx context.Context) BootStatus
}

const statusProbe = 3 * time.Second

// BootStatus reports on the emulator process and adb's view of it.
func (e *Emulator) BootStatus(ctx context.Context) BootStatus {
	ctx, cancel := context.WithTimeout(ctx, statusProbe)
	defer cancel()
	st := BootStatus{AVD: e.cfg.AVD, State: BootStateStopped, LogPath: e.bootLogPath()}
	if started := e.boot.started.Load(); started != 0 && e.boot.alive.Load() {
		st.ElapsedMs = time.Since(time.Unix(0, started)).Milliseconds()
	}
	st.PID = int(e.boot.pid.Load())
	st.ProcessAlive = e.boot.alive.Load()
	if !st.ProcessAlive {
		// Started by someone else, or by a previous server process.
		if pid := runningAVDPid(ctx, e.cfg.AVD); pid != 0 {
			st.PID, st.ProcessAlive = pid, true
		} else {
			st.PID = 0
		}
	}
	st.Serial = e.Serial()
	if st.Serial == "" {
		st.Serial = e.BootingAs()
	}
	if st.Serial != "" {
		st.ADBState = "absent"
		if devices, err := e.adb.Devices(ctx); err == nil {
			for _, d := range devices {
				if d.Serial == st.Serial {
					st.ADBState = d.State
				}
			}
		}
	}
	if st.ADBState == "device" && !e.Paused() {
		out, err := e.adb.Shell(ctx, st.Serial, "getprop", "sys.boot_completed")
		st.BootCompleted = err == nil && strings.TrimSpace(out) == "1"
	}
	in := e.current()
	switch {
	case in != nil && in.Info().Ready() && st.ProcessAlive:
		st.State = BootStateUp
	case st.ProcessAlive:
		st.State = BootStateBooting
	}
	return st
}

// bootState is what a boot records without a lock, so BootStatus can read it
// while ensureBooted holds mu for the whole boot.
type bootState struct {
	started atomic.Int64
	pid     atomic.Int64
	alive   atomic.Bool
	logPath atomic.Value // string
}

// bootBudget is how long WaitForBoot may poll: what is left of ctx's deadline
// when it has one (POST /api/devices/boot?wait= sets it), else the
// configured budget.
func (e *Emulator) bootBudget(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 {
			return d
		}
	}
	return e.cfg.BootBudget
}

func (e *Emulator) bootLogPath() string {
	p, _ := e.boot.logPath.Load().(string)
	return p
}

// bootFailure adds to a boot error what a caller needs to act on it: whether
// the emulator process is still running and being tracked, adb's view of it,
// and the log with its last lines.
func (e *Emulator) bootFailure(err error, serial string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%v", err)
	if e.boot.alive.Load() {
		fmt.Fprintf(&b, "; the emulator (pid %d) is still running and loupe still tracks it: poll GET /api/devices/boot?target=%s, or POST /api/devices/shutdown to stop it", e.boot.pid.Load(), e.ID())
	} else {
		b.WriteString("; the emulator process has exited")
	}
	if p := e.bootLogPath(); p != "" {
		fmt.Fprintf(&b, "; emulator log: %s", p)
		if tail := logTail(p, 5, 800); tail != "" {
			fmt.Fprintf(&b, " (last lines: %s)", tail)
		}
	}
	return fmt.Errorf("%s", b.String())
}

// logTail is the last n non-empty lines of the file at path, joined with
// " | " and cut to at most max bytes from the end.
func logTail(path string, n, max int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	size := int64(16 << 10)
	if fi.Size() < size {
		size = fi.Size()
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, fi.Size()-size); err != nil && size > 0 {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(buf), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, " | ")
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}
