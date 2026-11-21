package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// WarmSnapshot is the quickboot snapshot this server saves once, after a cold
// boot it started, and loads on every boot after that.
//
// Not default_boot. The emulator writes that one on a graceful exit, which
// -no-snapshot-save turns off and which never happens to an emulator nothing
// shuts down, so loupe never had one to load: on api34_emoji_check it was an
// 11-byte snapshot.pb and nothing else, and every open was a 12s cold boot.
// A name of loupe's own is also left alone by Android Studio's quickboot.
const WarmSnapshot = "loupe-warm"

// hasSnapshot reports whether the AVD in dir holds a loadable snapshot by
// that name. snapshot.pb alone is not one: default_boot above was exactly
// that. The RAM image is the part a restore cannot do without.
func hasSnapshot(dir, name string) bool {
	if dir == "" {
		return false
	}
	snap := filepath.Join(dir, "snapshots", name)
	for _, f := range []string{"snapshot.pb", "ram.bin"} {
		if fi, err := os.Stat(filepath.Join(snap, f)); err != nil || fi.Size() == 0 {
			return false
		}
	}
	return true
}

// emulatorArgs is the command line for booting avd on port. With a snapshot
// it loads that one; -no-snapshot-save stays either way, so a session's
// changes never overwrite the state every later boot starts from.
//
// Without window the emulator runs with its window hidden and no audio. Loupe
// watches it through scrcpy, which reads the guest display over adb, so the
// window is a second renderer nobody looks at, and the stream never carries
// audio (launch.go passes audio=false).
//
// Hidden, not -no-window. -no-window starts the qemu-system-<arch>-headless
// binary, and under continuous swipes on a throwaway pixel_6 (android-36) it
// streamed 22-44fps, pts p50 17-33ms, against 53-55fps and p50 16.7ms from
// the windowed binary; Pixel_10 read 27fps headless. -qt-hide-window keeps
// the windowed binary, which macOS lists as a UIElement, so it has no Dock
// tile, and it loaded a loupe-warm saved by the headless one.
//
// -grpc opens the emulator's control service on port+3000, which takes
// injected microphone audio even with -no-audio. -grpc-use-token makes it
// listen on 127.0.0.1 only and want a bearer token, which the emulator writes
// with the port into its pid_<pid>.ini under the AVD running directory.
// Clients read the port from there rather than recomputing it.
func emulatorArgs(avd string, port int, gpu, snapshot string, window bool) []string {
	args := []string{
		"-avd", avd,
		"-port", strconv.Itoa(port),
		"-gpu", gpu,
	}
	if snapshot != "" {
		args = append(args, "-snapshot", snapshot)
	}
	args = append(args, "-grpc", strconv.Itoa(port+3000), "-grpc-use-token")
	if !window {
		args = append(args, "-qt-hide-window", "-no-audio")
	}
	return append(args,
		"-no-snapshot-save",
		"-no-boot-anim",
		"-netdelay", "none",
		"-netspeed", "full",
	)
}

// emulatorLog is where an emulator this server starts writes its output, one
// file per AVD, truncated on each boot. Empty when there is no cache dir.
func emulatorLog(avd string) string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(cache, "loupe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return filepath.Join(dir, "emulator-"+avd+".log")
}

// loadedSnapshot reports whether the emulator's log says it restored name. A
// requested snapshot that no longer fits -- a new system image, another -gpu
// -- is skipped for a cold boot with nothing but a log line to say so, and
// that boot has to save a fresh one.
func loadedSnapshot(log []byte, name string) bool {
	return bytes.Contains(log, []byte("Successfully loaded snapshot '"+name+"'"))
}

// SaveSnapshot saves the running emulator's state under name through the
// emulator console. The console answers in-band: a failure is a KO line and
// adb still exits 0.
func (a ADB) SaveSnapshot(ctx context.Context, serial, name string) error {
	out, err := exec.CommandContext(ctx, a.bin(), "-s", serial, "emu", "avd", "snapshot", "save", name).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("save snapshot %s on %s: %w: %s", name, serial, err, text)
	}
	if strings.Contains(text, "KO") || !strings.Contains(text, "OK") {
		return fmt.Errorf("save snapshot %s on %s: %s", name, serial, text)
	}
	return nil
}

// Emu runs one emulator console command and returns its reply. Like
// SaveSnapshot's, the verdict is in the reply, OK or KO, not adb's exit.
func (a ADB) Emu(ctx context.Context, serial string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, a.bin(), append([]string{"-s", serial, "emu"}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("adb emu %s on %s: %w: %s", strings.Join(args, " "), serial, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// StateSaver saves a running virtual device's state so its next boot resumes
// it. Optional, as Stopper is: only an Emulator has a snapshot to save into.
type StateSaver interface {
	SaveState(ctx context.Context) (bool, error)
}

// scrcpyGone is how long SaveState waits for a scrcpy server in the guest to
// exit after its session ended. A snapshot with one inside restores it with
// its socket gone, which is why keepWarm saves before scrcpy starts.
var scrcpyGone = 3 * time.Second

// SaveState saves the emulator into loupe-warm when KeepState is on, so a
// shutdown that follows keeps what changed since the boot, installs
// included. It reports whether it saved. Off, not running, or with no AVD
// directory, it saves nothing and returns false, nil.
//
// A paused emulator saves as it is: `emu avd snapshot save` goes to the
// console, which answers while the VM is stopped (1.0s on loupe_keep, and a
// file written before the pause was there after the next warm boot). It is
// not resumed first, and nothing is read through adb shell, which would hang.
func (e *Emulator) SaveState(ctx context.Context) (bool, error) {
	if e.cfg.KeepState == nil || !e.cfg.KeepState() || e.cfg.Dir == "" {
		return false, nil
	}
	if e.current() == nil {
		return false, nil
	}
	err := e.saveWarm(ctx, false)
	return err == nil, err
}

// WarmSaver saves a running emulator's state as the one every later boot
// starts from, whatever KeepState says.
type WarmSaver interface {
	SaveWarm(ctx context.Context) error
}

// StartStater reports when a start state was last saved on purpose.
type StartStater interface {
	StartSavedAt() time.Time
}

// ErrStreamLingering is SaveWarm refusing to save with a scrcpy server still
// in the guest: every later boot would restore it with its socket gone.
var ErrStreamLingering = errors.New("a scrcpy server is still in the guest")

// startMarker in an AVD's directory holds the time, RFC 3339, of the last
// SaveWarm. It describes loupe-warm, so whatever replaces or removes that
// snapshot removes it too: a reset, or a cold boot's fresh save.
const startMarker = "loupe-start"

// SaveWarm saves the running emulator over loupe-warm because someone asked
// for it, so it is stricter than SaveState: it refuses with no AVD
// directory, when the emulator is not running, and while a scrcpy server is
// still in the guest after scrcpyGone, instead of saving anyway. It leaves
// startMarker behind.
func (e *Emulator) SaveWarm(ctx context.Context) error {
	if e.cfg.Dir == "" {
		return fmt.Errorf("save %s: no AVD directory", e.cfg.AVD)
	}
	if e.current() == nil {
		return fmt.Errorf("save %s: not running", e.cfg.AVD)
	}
	if err := e.saveWarm(ctx, true); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(e.cfg.Dir, startMarker), []byte(stamp), 0o644); err != nil {
		e.traceWith("startMarker", "", time.Now(), err, nil)
	}
	return nil
}

// StartSavedAt is the time startMarker holds, or zero when there is none or
// it does not parse.
func (e *Emulator) StartSavedAt() time.Time {
	if e.cfg.Dir == "" {
		return time.Time{}
	}
	b, err := os.ReadFile(filepath.Join(e.cfg.Dir, startMarker))
	if err != nil {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}
	}
	return at
}

// clearStartMarker removes startMarker, for whatever replaces loupe-warm.
func (e *Emulator) clearStartMarker() error {
	if e.cfg.Dir == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(e.cfg.Dir, startMarker)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// saveWarm saves loupe-warm from the running emulator, waiting for scrcpy to
// leave the guest first unless it is paused. Strict, a server still there
// after the wait refuses the save; otherwise it saves anyway and the trace
// carries lingered.
func (e *Emulator) saveWarm(ctx context.Context, strict bool) error {
	in := e.current()
	if in == nil {
		return fmt.Errorf("save %s: not running", e.cfg.AVD)
	}
	serial := in.Info().Serial
	started := time.Now()
	lingered := false
	if !e.pause.paused.Load() {
		lingered = e.awaitScrcpyExit(ctx, serial)
	}
	waited := time.Since(started)
	extra := map[string]any{
		"snapshot": WarmSnapshot,
		"waitMs":   waited.Milliseconds(),
		"lingered": lingered,
		"explicit": strict,
	}
	if strict && lingered {
		err := fmt.Errorf("save %s: %w", e.cfg.AVD, ErrStreamLingering)
		e.traceWith("save", serial, started, err, extra)
		return err
	}
	err := e.adb.SaveSnapshot(ctx, serial, WarmSnapshot)
	e.traceWith("save", serial, started, err, extra)
	return err
}

// awaitScrcpyExit waits, up to scrcpyGone, for no scrcpy server of loupe's
// pinned version to be left in the guest. Matching on the version leaves out
// another tool's server: a ws-scrcpy on this Mac attached its own
// "Server 1.19-ws8" to a fresh emulator, and the wait ran out on it at every
// save. It gives up quietly, reporting true when one was still there: a
// save with one inside is worse than none only in theory, and the shutdown
// it precedes was asked for regardless. The save's trace carries it as
// lingered.
func (e *Emulator) awaitScrcpyExit(ctx context.Context, serial string) bool {
	deadline := time.Now().Add(scrcpyGone)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		out, err := e.adb.Shell(ctx, serial, "pgrep", "-f", shellQuote(scrcpyPattern(e.version)))
		if err != nil || strings.TrimSpace(out) == "" {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
	return true
}

// scrcpyPattern is the pgrep -f pattern for loupe's scrcpy server at version.
// adb shell runs pgrep under `sh -c`, whose command line holds the pattern
// itself; "[S]erver" matches the server's "Server" but not the literal text
// in that sh, which a plain pattern found on every call.
func scrcpyPattern(version string) string {
	return "com.genymobile.scrcpy.[S]erver " + regexp.QuoteMeta(version)
}
