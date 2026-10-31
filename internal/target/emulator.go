package target

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// EmulatorConfig describes an AVD this server can boot on demand.
type EmulatorConfig struct {
	// AVD is the name as `emulator -list-avds` reports it.
	AVD string
	// DisplayName is what the picker shows. Empty means AVD.
	DisplayName string
	// Desc is what the AVD's config.ini says about it, reported until the
	// emulator is running and getprop can say more.
	Desc Description
	// EmulatorPath is the emulator binary. Empty means "emulator" on PATH,
	// which launchd's PATH does not reach; EmulatorBinary finds it in the SDK.
	EmulatorPath string
	// GPU is the -gpu mode. "host" renders on the host GPU. On an M3 Max,
	// "swiftshader_indirect" matched it under scrolling at the same three to
	// four cores; a cloud vCPU is unmeasured.
	GPU string
	// Port fixes the console port. Zero picks a free one when the AVD boots,
	// which is the only safe choice: a port assigned up front belongs to
	// whatever is already running on it.
	Port int
	// BootBudget bounds how long Open waits for sys.boot_completed.
	BootBudget time.Duration
	// Dir is the AVD's .avd directory, where WarmSnapshot lives. Empty means
	// every boot is cold and nothing is saved.
	Dir string
	// Home is the AVD home directory, where <name>.ini lives. Delete needs it.
	Home string
	// Trace receives one record per boot this target starts. Nil drops them.
	Trace func(map[string]any)
	// Window reports whether a boot should open the emulator's window. It is
	// read at each boot, so a config change reaches the next one without
	// re-registering the target. Nil means headless.
	Window func() bool
	// KeepState reports whether a shutdown should first save the emulator's
	// state into loupe-warm. Read at each shutdown. Nil means never.
	KeepState func() bool
}

func (c EmulatorConfig) withDefaults() EmulatorConfig {
	if c.EmulatorPath == "" {
		c.EmulatorPath = "emulator"
	}
	if c.GPU == "" {
		c.GPU = "host"
	}
	if c.BootBudget == 0 {
		c.BootBudget = 3 * time.Minute
	}
	return c
}

// Emulator is one AVD, running or not. It is listed whether or not it is
// running, boots on Open if it is not, and while it runs it is the adb device
// it booted as: every capability goes through an inner ADBTarget built on
// that serial.
//
// Which serial is decided by the AVD's name, never by its port. The manager
// maps each running emulator to an AVD through ro.boot.qemu.avd_name and calls
// Attach, so an AVD started by Android Studio on any port is the same target
// as one this server booted, and a port reused by a different AVD is not
// mistaken for this one.
type Emulator struct {
	adb     ADB
	cfg     EmulatorConfig
	jar     string
	version string

	// mu is the boot lifecycle. ensureBooted holds it for as long as a boot
	// takes, which can be minutes, so nothing on the listing path takes it.
	mu   sync.Mutex
	proc *exec.Cmd
	// exited is closed when proc exits. A process still running after a boot
	// that ran out of budget is still booting, and a second `emulator -avd`
	// for the same AVD would be refused, so the next Open waits on it instead.
	exited chan struct{}
	port   int

	// attMu guards inner alone, and is only held for an assignment or a read,
	// so Targets and the agent endpoints never wait on a boot.
	attMu sync.Mutex
	inner *ADBTarget
	// attached mirrors inner != nil for BootsOnOpen, which is on the listing
	// path and reads it without a lock.
	attached atomic.Bool
	// bootingAs is "emulator-<port>" from starting an emulator until it exits
	// or attaches. See BootingAs.
	bootingAs atomic.Value

	// boot is what the last boot this server started recorded, readable
	// while that boot holds mu. See bootstatus.go.
	boot bootState

	// pause is whether the VM is stopped for being idle. See emulatorpause.go.
	pause pauseState
	// desc is the running device's last description, which Describe answers
	// with while the emulator is paused and getprop would hang.
	desc describeCache
}

// NewEmulator builds a target for one AVD. It boots nothing until Open.
func NewEmulator(adb ADB, cfg EmulatorConfig, jar, version string) *Emulator {
	e := &Emulator{adb: adb, cfg: cfg.withDefaults(), jar: jar, version: version}
	e.bootingAs.Store("")
	return e
}

func (e *Emulator) ID() string { return "avd:" + e.cfg.AVD }
func (e *Emulator) Kind() Kind { return KindEmulator }
func (e *Emulator) Label() string {
	name := e.cfg.DisplayName
	if name == "" {
		name = e.cfg.AVD
	}
	return name + " (emulator)"
}

// AVD is the name ro.boot.qemu.avd_name reports while this AVD runs.
func (e *Emulator) AVD() string { return e.cfg.AVD }

// Serial is the adb serial this AVD is running as, or "" while it is not.
func (e *Emulator) Serial() string {
	if in := e.current(); in != nil {
		return in.Info().Serial
	}
	return ""
}

// BootingAs is the serial an emulator this target started will appear as,
// while it is booting and not yet attached, or "" otherwise. The manager
// needs it because adb lists a booting emulator before it can answer which
// AVD it is.
//
// It takes no lock: ensureBooted holds mu for the whole boot, and the device
// sync that asks cannot wait that long, so the fields it reads are atomic.
func (e *Emulator) BootingAs() string {
	if e.attached.Load() {
		return ""
	}
	return e.bootingAs.Load().(string)
}

// Attach records that this AVD is running as d. A serial or transport that
// changed gets a fresh inner target, since an ADBTarget keeps the DeviceInfo
// it was built with. It returns the inner target now in place.
func (e *Emulator) Attach(d DeviceInfo) *ADBTarget {
	e.attMu.Lock()
	defer e.attMu.Unlock()
	if e.inner != nil && e.inner.Info() == d {
		return e.inner
	}
	e.inner = NewADBTarget(e.adb, d, e.jar, e.version)
	e.attached.Store(true)
	e.resetPause()
	return e.inner
}

// Detach records that this AVD is no longer running, so the next Open boots
// it again.
func (e *Emulator) Detach() {
	e.attMu.Lock()
	defer e.attMu.Unlock()
	e.inner = nil
	e.attached.Store(false)
}

func (e *Emulator) current() *ADBTarget {
	e.attMu.Lock()
	defer e.attMu.Unlock()
	return e.inner
}

// Attached returns the running device behind this AVD, for the endpoints that
// read or drive it. An AVD that is not running can do all of it once booted,
// so the error says to boot it rather than implying it never could.
func (e *Emulator) Attached() (Target, error) {
	if in := e.current(); in != nil {
		return in, nil
	}
	return nil, fmt.Errorf("%s is not running; open it in the viewer to boot it", e.cfg.AVD)
}

// Open boots the AVD if it is not already running, waits for the boot to
// complete, then streams it.
func (e *Emulator) Open(ctx context.Context, opts Options) (Stream, error) {
	// The device ensureBooted attached, not e.current(): a device sync can
	// land between the boot finishing and this line, from a listing taken
	// before the emulator appeared in it, and detach what just booted.
	in, err := e.ensureBooted(ctx)
	if err != nil {
		return nil, err
	}
	return in.Open(ctx, opts)
}

// Boot starts the AVD if it is not running and waits for it, without opening
// a stream. It is what -avd does at startup.
func (e *Emulator) Boot(ctx context.Context) error {
	_, err := e.ensureBooted(ctx)
	// Nothing else reaches it until someone does, so the idle clock starts
	// from the boot rather than from the attach.
	e.MarkUsed()
	return err
}

func (e *Emulator) ensureBooted(ctx context.Context) (*ADBTarget, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if in := e.current(); in != nil && in.Info().Ready() {
		// A paused emulator answers no getprop, so WaitForBoot and the scrcpy
		// push after it would hang until something resumed it.
		if err := e.wake(ctx, in.Info().Serial); err != nil {
			return nil, err
		}
		return in, e.adb.WaitForBoot(ctx, in.Info().Serial, e.bootBudget(ctx))
	}

	// Running, but not mapped yet: the manager maps on its own schedule, and
	// Open can arrive between an emulator appearing and that happening.
	if d, ok := e.findRunning(ctx); ok {
		if err := e.wake(ctx, d.Serial); err != nil {
			return nil, err
		}
		if err := e.adb.WaitForBoot(ctx, d.Serial, e.bootBudget(ctx)); err != nil {
			return nil, err
		}
		return e.Attach(d), nil
	}

	// Still booting from an earlier Open that ran out of budget. Starting a
	// second emulator for the same AVD would be refused, so wait on this one.
	if e.proc != nil && !closed(e.exited) {
		return e.awaitBoot(ctx)
	}

	// Running, and adb does not have it. A second emulator for the same AVD
	// would be refused, and the error that surfaces then is about -gpu.
	if pid := runningAVDPid(ctx, e.cfg.AVD); pid != 0 {
		if _, err := e.adb.Devices(ctx); err != nil {
			return nil, fmt.Errorf("%s is already running (pid %d) but adb cannot reach it: %w", e.cfg.AVD, pid, err)
		}
		return nil, fmt.Errorf("%s is already running (pid %d) but adb does not list it as ready; wait for it to finish booting", e.cfg.AVD, pid)
	}

	port := e.cfg.Port
	if port == 0 {
		p, err := reservePort(ctx, e.adb)
		if err != nil {
			return nil, err
		}
		defer releasePort(p)
		port = p
	}

	snapshot, wipe := bootFrom(e.cfg.Dir)
	window := e.cfg.Window != nil && e.cfg.Window()
	args := emulatorArgs(e.cfg.AVD, port, e.cfg.GPU, snapshot, window)
	if wipe {
		args = append(args, "-wipe-data")
	}
	args = append(args, emulatorCameraArgs(e.cfg.AVD)...)
	cmd := exec.Command(e.cfg.EmulatorPath, args...)
	// The emulator's output is the only place it says whether the snapshot
	// loaded. A file, not a pipe: the emulator outlives this process, and a
	// pipe with no reader would kill it on its next write.
	logPath := emulatorLog(e.cfg.AVD)
	if logPath != "" {
		if f, err := os.Create(logPath); err == nil {
			cmd.Stdout, cmd.Stderr = f, f
			defer f.Close()
		} else {
			logPath = ""
		}
	}
	// Its own session, so the supervisor restarting this process on every
	// build does not take the emulator down with it. The next process finds
	// it running and attaches by name.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start emulator %s with %s: %w", e.cfg.AVD, e.cfg.EmulatorPath, err)
	}
	exited := make(chan struct{})
	e.bootingAs.Store("emulator-" + strconv.Itoa(port))
	e.boot.started.Store(started.UnixNano())
	e.boot.pid.Store(int64(cmd.Process.Pid))
	e.boot.logPath.Store(logPath)
	e.boot.alive.Store(true)
	go func() {
		_ = cmd.Wait()
		e.boot.alive.Store(false)
		e.bootingAs.Store("")
		close(exited)
	}()
	e.proc, e.exited, e.port = cmd, exited, port
	in, err := e.awaitBoot(ctx)
	if err != nil {
		return nil, err
	}
	if wipe {
		// Only once the wiped device is up: a boot that failed has not wiped
		// it for certain, and the next one tries again.
		_ = os.Remove(filepath.Join(e.cfg.Dir, wipeMarker))
	}
	e.keepWarm(ctx, in.Info().Serial, snapshot, logPath, started)
	return in, nil
}

// keepWarm saves WarmSnapshot after a boot that did not load it, and traces
// how the boot went either way. It runs before anything is streamed: a
// snapshot taken with a scrcpy server in the guest would restore that server
// with its socket gone. The save pauses the device for about 2s, which lands
// on the cold boot that is already slow, never on a warm one. A failed save
// leaves the boot as it was; the next one is cold and tries again.
func (e *Emulator) keepWarm(ctx context.Context, serial, snapshot, logPath string, started time.Time) {
	rec := map[string]any{
		"kind":   "emulator",
		"action": "boot",
		"avd":    e.cfg.AVD,
		"serial": serial,
		"tookMs": time.Since(started).Milliseconds(),
	}
	loaded := false
	if snapshot != "" {
		// With no log to read, a requested snapshot is taken to have loaded,
		// so a missing cache dir costs a cold boot rather than a save on
		// every boot.
		loaded = true
		if logPath != "" {
			if b, err := os.ReadFile(logPath); err == nil {
				loaded = loadedSnapshot(b, snapshot)
			}
		}
	}
	switch {
	case loaded:
		rec["snapshot"] = "loaded"
	case e.cfg.Dir == "":
		rec["snapshot"] = "cold"
	default:
		t0 := time.Now()
		if err := e.adb.SaveSnapshot(ctx, serial, WarmSnapshot); err != nil {
			rec["snapshot"] = "saveFailed"
			rec["error"] = err.Error()
		} else {
			rec["snapshot"] = "saved"
		}
		rec["saveMs"] = time.Since(t0).Milliseconds()
	}
	if e.cfg.Trace != nil {
		e.cfg.Trace(rec)
	}
}

// awaitBoot waits for the emulator this target started, then checks that the
// serial on its port is this AVD before attaching. Something else can take
// the port between choosing it and the emulator binding it, and streaming that
// under this AVD's name is the bug choosing ports at boot exists to prevent.
func (e *Emulator) awaitBoot(ctx context.Context) (*ADBTarget, error) {
	serial := "emulator-" + strconv.Itoa(e.port)
	if err := e.adb.WaitForBoot(ctx, serial, e.bootBudget(ctx)); err != nil {
		if closed(e.exited) {
			return nil, e.bootFailure(fmt.Errorf("emulator %s exited while booting: %w (check that -gpu %s is supported on this host)", e.cfg.AVD, err, e.cfg.GPU), serial)
		}
		return nil, e.bootFailure(err, serial)
	}
	name, err := e.adb.AVDName(ctx, serial)
	if err != nil {
		return nil, err
	}
	if name != e.cfg.AVD {
		return nil, fmt.Errorf("%s is running %s, not %s", serial, name, e.cfg.AVD)
	}
	return e.Attach(DeviceInfo{Serial: serial, State: "device", Model: e.cfg.AVD, Emulator: true, AVD: name}), nil
}

// findRunning looks for a ready emulator already running this AVD.
func (e *Emulator) findRunning(ctx context.Context) (DeviceInfo, bool) {
	devices, err := e.adb.Devices(ctx)
	if err != nil {
		return DeviceInfo{}, false
	}
	for _, d := range devices {
		if !d.Emulator || !d.Ready() {
			continue
		}
		// nameEmulator falls back to the console, which answers for an
		// emulator a previous server process left paused.
		if name, err := nameEmulator(ctx, e.adb, d.Serial); err == nil && name == e.cfg.AVD {
			return d, true
		}
	}
	return DeviceInfo{}, false
}

// Describe reports the running device's own getprop answer, or what the AVD's
// config says while it is not running. It never boots anything and, while
// the AVD is not running, never runs adb.
func (e *Emulator) Describe(ctx context.Context) (Description, error) {
	if in := e.current(); in != nil {
		// The listing never wakes a paused emulator, and getprop would hang
		// on one, so it answers with what the device said before it paused.
		if e.pausedNow(ctx, in.Info().Serial) {
			if d, ok := e.desc.get(); ok {
				return d, nil
			}
			if e.cfg.Desc != (Description{}) {
				return e.cfg.Desc, nil
			}
			return Description{}, fmt.Errorf("emulator %s is paused", e.cfg.AVD)
		}
		d, err := in.Describe(ctx)
		// getprop never says foldable, so the config does, or the AVD would
		// turn from a foldable into a phone as it boots.
		if err == nil && e.cfg.Desc.Form == "foldable" {
			d.Form = "foldable"
		}
		if err == nil {
			e.desc.set(d)
		}
		return d, err
	}
	if e.cfg.Desc == (Description{}) {
		return Description{}, fmt.Errorf("emulator %s is not running and its config says nothing about it", e.cfg.AVD)
	}
	return e.cfg.Desc, nil
}

// Shutdown stops an emulator this target started. One it attached to after
// someone else started it is left alone. Nothing calls this yet: an emulator
// keeps running across server restarts until someone asks for it to stop.
func (e *Emulator) Shutdown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.proc != nil && e.proc.Process != nil {
		_ = e.proc.Process.Kill()
	}
}

func closed(ch chan struct{}) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Console ports adb discovers emulators on by itself: 16 instances from 5554.
const (
	firstConsolePort = 5554
	lastConsolePort  = 5584
)

// ports holds the console ports reserved by boots still in progress. Between
// choosing a port and the emulator binding it, nothing on the machine shows it
// as taken, so two AVDs opened at once would otherwise both choose the first
// free one.
var ports = struct {
	sync.Mutex
	reserved map[int]bool
}{reserved: map[int]bool{}}

// reservePort chooses a free console port: even, in adb's discovery range,
// not a serial adb already lists, and with nothing listening on it or on the
// adb port after it.
func reservePort(ctx context.Context, adb ADB) (int, error) {
	listed := map[string]bool{}
	if devices, err := adb.Devices(ctx); err == nil {
		for _, d := range devices {
			listed[d.Serial] = true
		}
	}
	ports.Lock()
	defer ports.Unlock()
	for p := firstConsolePort; p <= lastConsolePort; p += 2 {
		if ports.reserved[p] || listed["emulator-"+strconv.Itoa(p)] || listening(p) || listening(p+1) {
			continue
		}
		ports.reserved[p] = true
		return p, nil
	}
	return 0, fmt.Errorf("no free emulator console port between %d and %d", firstConsolePort, lastConsolePort)
}

func releasePort(p int) {
	ports.Lock()
	defer ports.Unlock()
	delete(ports.reserved, p)
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
