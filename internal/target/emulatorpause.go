package target

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// An emulator nobody is watching or driving still runs its guest: on
// loupe_pause (pixel_6, API 34, headless) qemu idled at 14-25% CPU with the
// Home Screen in front. `adb emu avd stop` pauses the VM and `avd start`
// resumes it, and both go to the emulator console rather than adbd, so they
// answer while the guest is stopped. Paused, qemu read about 1% CPU and
// macOS compressed the guest's RAM within two minutes; a screencap answered
// 1.1-1.2s after a resume.
//
// While paused, anything through adbd (getprop, screencap, input, the
// scrcpy push) hangs until the guest runs again, and adb still lists the
// device as `device`. So every path that reaches adbd wakes the emulator
// first, through running() or ensureBooted, and the listing path never does:
// Describe answers from what it already knows.

// pauseState is the part of an Emulator that pausing owns.
type pauseState struct {
	// wakeMu serialises a pause against a wake, so two requests that find it
	// paused send one `avd start` and a pause cannot land between a wake and
	// the call it was for being marked used.
	wakeMu sync.Mutex
	paused atomic.Bool
	// known is false until the console has been asked, after each attach:
	// an emulator a previous server process paused is still paused, and
	// nothing in adb's listing says so.
	known atomic.Bool
	// lastUsed is unix nanoseconds of the last call that reached the device,
	// or of the attach, so a fresh attach is not paused at once.
	lastUsed atomic.Int64
	// inUse counts calls that hold the device without a session, a logcat
	// follow being the one that lasts.
	inUse atomic.Int32
	// stopping is set under wakeMu while StopIfIdle kills the emulator, so a
	// call that arrives during the kill fails rather than resuming a VM that
	// is going away. Stop itself cannot run under wakeMu: a boot holds e.mu
	// and then takes wakeMu, and Stop takes e.mu.
	stopping atomic.Bool
}

// MarkUsed restarts the idle clock. The manager calls it while a session is
// open, since a stream reaches the device without going through running().
func (e *Emulator) MarkUsed() { e.pause.lastUsed.Store(time.Now().UnixNano()) }

// resetPause forgets what was known about a previous attachment.
func (e *Emulator) resetPause() {
	e.pause.known.Store(false)
	e.pause.paused.Store(false)
	e.MarkUsed()
}

// Paused reports whether this server paused the emulator, or found it
// paused. It never runs adb, so the listing can read it.
func (e *Emulator) Paused() bool { return e.pause.paused.Load() }

// IdleFor is how long since a call last reached the device.
func (e *Emulator) IdleFor(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, e.pause.lastUsed.Load()))
}

// InUse reports a call holding the device outside a session.
func (e *Emulator) InUse() bool { return e.pause.inUse.Load() > 0 }

// learnPaused asks the console once per attach whether the VM is stopped.
// The caller holds wakeMu.
func (e *Emulator) learnPaused(ctx context.Context, serial string) {
	if e.pause.known.Load() {
		return
	}
	e.askPaused(ctx, serial)
}

// askPaused reads from the console whether the VM is stopped. A failed read
// keeps what was known, and leaves known false if nothing was. The caller
// holds wakeMu.
func (e *Emulator) askPaused(ctx context.Context, serial string) {
	out, err := e.adb.Emu(ctx, serial, "avd", "status")
	if err != nil {
		return // asked again next time
	}
	e.pause.paused.Store(strings.Contains(out, "is stopped"))
	e.pause.known.Store(true)
}

// pausedNow is Paused, asking the console first if this attachment has not
// been asked. Describe uses it, so a listing after a restart does not run
// getprop on an emulator a previous process paused.
func (e *Emulator) pausedNow(ctx context.Context, serial string) bool {
	if !e.pause.known.Load() {
		e.pause.wakeMu.Lock()
		e.learnPaused(ctx, serial)
		e.pause.wakeMu.Unlock()
	}
	return e.pause.paused.Load()
}

// wake resumes the emulator if it is paused and marks it used. Every call
// that reaches adbd goes through it.
//
// Unless it already reads paused, it asks the console every time rather
// than trusting what it knew: `adb emu avd stop` from a terminal, or
// anything else outside this server, stops the VM without telling it, and
// every adbd call then hangs until something sends `avd start`. The read is
// one console round trip, about 10ms.
func (e *Emulator) wake(ctx context.Context, serial string) error {
	e.MarkUsed()
	e.pause.wakeMu.Lock()
	defer e.pause.wakeMu.Unlock()
	if e.pause.stopping.Load() {
		return fmt.Errorf("%s is shutting down after going unused; open it again to boot it", e.cfg.AVD)
	}
	wasPaused := e.pause.paused.Load()
	if !wasPaused {
		e.askPaused(ctx, serial)
	}
	if !e.pause.paused.Load() {
		return nil
	}
	started := time.Now()
	out, err := e.adb.Emu(ctx, serial, "avd", "start")
	if err == nil && strings.Contains(out, "already running") {
		// Resumed by something else while this server read it paused.
		// Treating the KO as an error left paused set, and every open of
		// Pixel_7a failed with a 502 without reaching adbd.
		e.traceWith("resume", serial, started, nil, map[string]any{"already": true})
		e.pause.paused.Store(false)
		e.pause.known.Store(true)
		e.MarkUsed()
		return nil
	}
	if err == nil && !strings.Contains(out, "OK") {
		err = fmt.Errorf("adb emu avd start on %s: %s", serial, strings.TrimSpace(out))
	}
	var extra map[string]any
	if !wasPaused {
		// Stopped by something other than this server, or before it
		// attached.
		extra = map[string]any{"found": true}
	}
	e.traceWith("resume", serial, started, err, extra)
	if err != nil {
		return fmt.Errorf("resume %s: %w", e.cfg.AVD, err)
	}
	e.pause.paused.Store(false)
	e.MarkUsed()
	return nil
}

// PauseIfIdle pauses the emulator when nothing has reached it for idle and
// nothing holds it. It reports whether it paused. The caller decides that
// the emulator is one to pause at all: headless, with no session.
func (e *Emulator) PauseIfIdle(ctx context.Context, idle time.Duration) (bool, error) {
	in := e.current()
	if in == nil {
		return false, nil
	}
	serial := in.Info().Serial
	e.pause.wakeMu.Lock()
	defer e.pause.wakeMu.Unlock()
	// Checked under wakeMu, which a wake holds until it has marked the
	// emulator used, so a call that woke it is never paused straight after.
	if e.InUse() || e.IdleFor(time.Now()) < idle || e.BootingAs() != "" {
		return false, nil
	}
	e.learnPaused(ctx, serial)
	if e.pause.paused.Load() {
		return false, nil
	}
	started := time.Now()
	out, err := e.adb.Emu(ctx, serial, "avd", "stop")
	if err == nil && strings.Contains(out, "already stopped") {
		// Stopped by something else, or by an earlier stop that reported a
		// failure and paused it anyway. Believing paused was false left
		// Pixel_7a stopped with every wake skipping `avd start`, and every
		// open waited on adbd for frames that never came.
		e.traceWith("pause", serial, started, nil, map[string]any{"already": true})
		e.pause.paused.Store(true)
		e.pause.known.Store(true)
		return false, nil
	}
	if err == nil && !strings.Contains(out, "OK") {
		err = fmt.Errorf("adb emu avd stop on %s: %s", serial, strings.TrimSpace(out))
	}
	e.trace("pause", serial, started, err)
	if err != nil {
		// The stop may have landed anyway (a timeout after the console
		// acted), so the next wake asks the console instead of trusting
		// paused.
		e.pause.known.Store(false)
		return false, err
	}
	e.pause.paused.Store(true)
	e.pause.known.Store(true)
	return true, nil
}

// StopIfIdle shuts the emulator down when nothing has reached it for idle
// and nothing holds it, paused or not: `emu kill` goes to the console, which
// answers while the VM is stopped. It reports whether it stopped. The caller
// decides that the emulator is one to stop at all, as for PauseIfIdle, and
// holds the reopen guard so no open boots it during the kill.
func (e *Emulator) StopIfIdle(ctx context.Context, idle time.Duration) (bool, error) {
	in := e.current()
	if in == nil {
		return false, nil
	}
	serial := in.Info().Serial
	e.pause.wakeMu.Lock()
	idleFor := e.IdleFor(time.Now())
	if e.InUse() || idleFor < idle || e.BootingAs() != "" {
		e.pause.wakeMu.Unlock()
		return false, nil
	}
	wasPaused := e.pause.paused.Load()
	e.pause.stopping.Store(true)
	e.pause.wakeMu.Unlock()
	defer e.pause.stopping.Store(false)

	started := time.Now()
	saved, _ := e.SaveState(ctx)
	err := e.Stop(ctx)
	e.traceWith("shutdown", serial, started, err, map[string]any{
		"idleMs": idleFor.Milliseconds(),
		"paused": wasPaused,
		"saved":  saved,
	})
	if err != nil {
		return false, err
	}
	e.pause.paused.Store(false)
	e.pause.known.Store(false)
	return true, nil
}

func (e *Emulator) trace(action, serial string, started time.Time, err error) {
	e.traceWith(action, serial, started, err, nil)
}

func (e *Emulator) traceWith(action, serial string, started time.Time, err error, extra map[string]any) {
	if e.cfg.Trace == nil {
		return
	}
	rec := map[string]any{
		"kind":   "emulator",
		"action": action,
		"avd":    e.cfg.AVD,
		"serial": serial,
		"tookMs": time.Since(started).Milliseconds(),
	}
	for k, v := range extra {
		rec[k] = v
	}
	if err != nil {
		rec["error"] = err.Error()
	}
	e.cfg.Trace(rec)
}

// hold keeps the emulator from being paused until the returned func runs,
// which also restarts the idle clock.
func (e *Emulator) hold() func() {
	e.pause.inUse.Add(1)
	return func() {
		e.MarkUsed()
		e.pause.inUse.Add(-1)
	}
}

// holdLogcat keeps the emulator from being paused until the log is closed,
// since a follow can outlast any idle window while it reads.
func (e *Emulator) holdLogcat(l *Logcat) *Logcat {
	l.ReadCloser = &releaseOnClose{ReadCloser: l.ReadCloser, release: e.hold()}
	return l
}

type releaseOnClose struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (r *releaseOnClose) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)
	return err
}

// ConsoleAVDName is the AVD name from the emulator console. It answers
// while the VM is paused, when getprop would hang, so it is the fallback
// for naming an emulator AVDName could not.
func (a ADB) ConsoleAVDName(ctx context.Context, serial string) (string, error) {
	out, err := a.Emu(ctx, serial, "avd", "name")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line != "OK" && !strings.HasPrefix(line, "KO") {
			return line, nil
		}
	}
	return "", fmt.Errorf("console on %s reports no avd name", serial)
}

// nameEmulator reads an emulator's AVD name through getprop, then through
// the console when getprop fails or hangs past resolveTimeout.
func nameEmulator(ctx context.Context, adb ADB, serial string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	name, err := adb.AVDName(rctx, serial)
	cancel()
	if err == nil {
		return name, nil
	}
	cctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	if cname, cerr := adb.ConsoleAVDName(cctx, serial); cerr == nil {
		return cname, nil
	}
	return "", err
}
