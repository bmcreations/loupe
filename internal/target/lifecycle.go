package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Stopper shuts a running virtual device down. A device that is not
// running is already stopped and returns nil.
type Stopper interface {
	Stop(ctx context.Context) error
}

// Deleter removes a virtual device for good, stopping it first.
type Deleter interface {
	Delete(ctx context.Context) error
}

// Starter boots a virtual device without opening a stream, and returns once
// it is up. A device already running returns nil.
type Starter interface {
	Boot(ctx context.Context) error
}

// Resetter throws away a virtual device's saved state, stopping it first
// without saving, so the next boot is clean.
type Resetter interface {
	ResetState(ctx context.Context) error
}

var (
	defaultKillPid = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
	defaultSimctl  = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...).CombinedOutput()
	}
	findAVDPid = runningAVDPid
	killPid    = defaultKillPid
	simctl     = defaultSimctl
	stopPoll   = 200 * time.Millisecond
)

var avdNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Stop asks the emulator's console to exit, which works for one this
// server did not start, and falls back to SIGTERM on its pid when adb
// cannot reach it (the wedged-adb case). It returns once the process is
// gone and adb has dropped the serial, so track-devices cannot attach it
// again on the way down.
func (e *Emulator) Stop(ctx context.Context) error {
	serial := e.Serial()
	if serial == "" {
		serial = e.BootingAs()
	}
	consoleOK := false
	if serial != "" {
		out, err := e.adb.Emu(ctx, serial, "kill")
		consoleOK = err == nil && strings.Contains(out, "OK")
	}
	if !consoleOK {
		if pid := findAVDPid(ctx, e.cfg.AVD); pid != 0 {
			if err := killPid(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("stop %s: kill %d: %w", e.cfg.AVD, pid, err)
			}
		} else if p := e.ownProc(); p != nil {
			_ = p.Kill()
		}
	}
	for {
		if findAVDPid(ctx, e.cfg.AVD) == 0 && (serial == "" || !e.adbHas(ctx, serial)) {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stop %s: still running: %w", e.cfg.AVD, ctx.Err())
		case <-time.After(stopPoll):
		}
	}
	e.mu.Lock()
	// A boot that held mu before it started its process was invisible to
	// the kill above. If the emulator is up now, it is that boot's.
	if findAVDPid(ctx, e.cfg.AVD) != 0 {
		e.mu.Unlock()
		return fmt.Errorf("stop %s: booted while shutting down; try again", e.cfg.AVD)
	}
	e.proc, e.exited, e.port = nil, nil, 0
	e.mu.Unlock()
	e.Detach()
	return nil
}

// ownProc returns the emulator this server started, if no boot holds e.mu.
// A boot keeps e.mu for its whole length, and Stop must not wait on it.
func (e *Emulator) ownProc() *os.Process {
	if !e.mu.TryLock() {
		return nil // a boot is running; the pid path above is the route
	}
	defer e.mu.Unlock()
	if e.proc == nil {
		return nil
	}
	return e.proc.Process
}

func (e *Emulator) adbHas(ctx context.Context, serial string) bool {
	return exec.CommandContext(ctx, e.adb.bin(), "-s", serial, "get-state").Run() == nil
}

// Delete stops the emulator and removes its AVD.
func (e *Emulator) Delete(ctx context.Context) error {
	if err := e.Stop(ctx); err != nil {
		return err
	}
	return DeleteAVD(e.cfg.Home, e.cfg.AVD)
}

// ResetState stops the emulator without saving, removes loupe-warm and leaves
// wipeMarker, so the next boot is a cold one with -wipe-data and keepWarm
// saves a fresh snapshot from it. Removing the snapshot alone is not a reset:
// a cold boot reads the AVD's disk as the last session left it, and on
// loupe_keep a file written to /sdcard/Download survived one. Once a kept
// state has been saved over loupe-warm there is no clean snapshot left to go
// back to, so clean means the system image's own data, as Android Studio's
// Wipe Data does.
func (e *Emulator) ResetState(ctx context.Context) error {
	if err := e.Stop(ctx); err != nil {
		return err
	}
	if e.cfg.Dir == "" {
		return fmt.Errorf("reset %s: no AVD directory", e.cfg.AVD)
	}
	if err := os.RemoveAll(filepath.Join(e.cfg.Dir, "snapshots", WarmSnapshot)); err != nil {
		return fmt.Errorf("reset %s: %w", e.cfg.AVD, err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.Dir, wipeMarker), nil, 0o644); err != nil {
		return fmt.Errorf("reset %s: %w", e.cfg.AVD, err)
	}
	return nil
}

// wipeMarker in an AVD's directory says its next boot wipes its data.
const wipeMarker = "loupe-wipe"

// bootFrom says what a boot of the AVD in dir starts from: loupe-warm when it
// is there, nothing for a cold boot, and a wipe after a reset, which never
// loads a snapshot.
func bootFrom(dir string) (snapshot string, wipe bool) {
	if wipePending(dir) {
		return "", true
	}
	if hasSnapshot(dir, WarmSnapshot) {
		return WarmSnapshot, false
	}
	return "", false
}

func wipePending(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, wipeMarker))
	return err == nil
}

// DeleteAVD removes <home>/<name>.ini and the directory its path= names,
// which is not always <home>/<name>.avd.
func DeleteAVD(home, name string) error {
	if !avdNamePattern.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("delete AVD: bad name %q", name)
	}
	if home == "" {
		return errors.New("delete AVD: no AVD home")
	}
	ini := filepath.Join(home, name+".ini")
	dir := filepath.Join(home, name+".avd")
	if kv, err := readINI(ini); err == nil && kv["path"] != "" {
		dir = kv["path"]
	}
	clean := filepath.Clean(dir)
	if clean == filepath.Clean(home) || clean == "/" || clean == "." {
		return fmt.Errorf("delete AVD %s: refusing to remove %s", name, clean)
	}
	if !strings.HasSuffix(filepath.Base(clean), ".avd") {
		return fmt.Errorf("delete AVD %s: %s is not an .avd directory", name, clean)
	}
	if err := os.RemoveAll(clean); err != nil {
		return fmt.Errorf("delete AVD %s: %w", name, err)
	}
	if err := os.Remove(ini); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete AVD %s: %w", name, err)
	}
	return nil
}

// Stop shuts the simulator down. One already shut down is done.
func (s *Simulator) Stop(ctx context.Context) error {
	udid := s.UDID()
	if out, err := simctl(ctx, "shutdown", udid); err != nil &&
		!strings.Contains(string(out), "current state: Shutdown") {
		return fmt.Errorf("simctl shutdown: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// Through input(), not s.in: a first /api/input sets s.in inside
	// inputOnce, and reading it bare races that write. An unstarted input
	// process closes as a no-op.
	s.input().close()
	s.booted.Store(false)
	info := s.Info()
	info.State = "Shutdown"
	s.Update(info)
	return nil
}

// Delete shuts the simulator down and deletes it.
func (s *Simulator) Delete(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	if out, err := simctl(ctx, "delete", s.UDID()); err != nil {
		return fmt.Errorf("simctl delete: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
