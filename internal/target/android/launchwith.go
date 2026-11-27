package android

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// A launch with extensions on Android is an ordinary Launch, then, once the
// app's process is up, an adb reverse for each extension that asked for one
// and each extension's JVMTI agent attached with `am attach-agent`. The
// agent's files go to fixed paths named after the extension, so no device
// path comes from the extension, and the options string is built here from
// checked keys and values. See docs/extensions.md, "Launch-time injection".

const (
	// launchExtTmp is where the agent files land on the device, before the
	// app copies them into its own data directory.
	launchExtTmp = "/data/local/tmp/loupe-ext"
	// launchExtDir is the app's copy, under its data directory. ART loads
	// an attached agent only from there.
	launchExtDir = "code_cache/loupe-ext"
	// launchPidWait bounds the wait for the app's process after the launch.
	launchPidWait = 10 * time.Second
	// launchLoadWait bounds the wait for an attached agent to show in the
	// process's maps: am attach-agent returns before the app loads it.
	launchLoadWait = 3 * time.Second
	// launchPoll is the pause between looks at pidof and the maps.
	launchPoll = 100 * time.Millisecond
)

// attachLog is the app process each extension's agent was attached to, so a
// second launch of the same process does not load the agent twice.
type attachLog struct {
	// launching is held through a whole launch with extensions, so two at
	// once cannot both find the agent unattached and attach it twice.
	launching sync.Mutex
	mu        sync.Mutex
	pids      map[attachKey]int
}

type attachKey struct{ ext, pkg string }

func (l *attachLog) attachedTo(ext, pkg string, pid int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return pid > 0 && l.pids[attachKey{ext, pkg}] == pid
}

func (l *attachLog) note(ext, pkg string, pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pids == nil {
		l.pids = map[attachKey]int{}
	}
	l.pids[attachKey{ext, pkg}] = pid
}

// launchInjector is LaunchWith over fields, so it can be tested without a
// device, the same shape as composeDevice.
type launchInjector struct {
	sh       shellFunc
	launch   func(ctx context.Context, pkg, activity string) (*target.LaunchResult, error)
	push     func(ctx context.Context, local, remote string) error
	reverse  func(ctx context.Context, remote string, port int) error
	attached *attachLog
	// poll, pidWait and loadWait default to launchPoll, launchPidWait and
	// launchLoadWait.
	poll, pidWait, loadWait time.Duration
}

// LaunchFacts implements target.LaunchInjector.
func (t *Device) LaunchFacts(ctx context.Context) (target.LaunchFacts, error) {
	if !t.info.Ready() {
		return target.LaunchFacts{}, fmt.Errorf("%w: device %s is in state %q", target.ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	return launchFacts(ctx, func(ctx context.Context, args ...string) (string, error) {
		return t.adb.Shell(ctx, t.info.Serial, args...)
	})
}

func launchFacts(ctx context.Context, sh shellFunc) (target.LaunchFacts, error) {
	out, err := sh(ctx, "getprop ro.product.cpu.abi; getprop ro.build.version.sdk")
	if err != nil {
		return target.LaunchFacts{}, fmt.Errorf("reading the ABI and SDK: %w: %s", err, strings.TrimSpace(out))
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return target.LaunchFacts{}, fmt.Errorf("reading the ABI and SDK: unexpected %q", strings.TrimSpace(out))
	}
	sdk, err := strconv.Atoi(f[1])
	if err != nil {
		return target.LaunchFacts{}, fmt.Errorf("reading the SDK: unexpected %q", f[1])
	}
	return target.LaunchFacts{Platform: target.LaunchAndroid, ABI: f[0], SDK: sdk}, nil
}

// LaunchWith implements target.LaunchInjector.
func (t *Device) LaunchWith(ctx context.Context, pkg, activity string, exts []target.LaunchExtension) (*target.LaunchWithResult, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", target.ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	serial := t.info.Serial
	sh := func(ctx context.Context, args ...string) (string, error) {
		return t.adb.Shell(ctx, serial, args...)
	}
	d := &launchInjector{
		sh: sh,
		launch: func(ctx context.Context, pkg, activity string) (*target.LaunchResult, error) {
			return launchAndroid(ctx, sh, pkg, activity)
		},
		push: func(ctx context.Context, local, remote string) error {
			return t.adb.Push(ctx, serial, local, remote)
		},
		reverse: func(ctx context.Context, remote string, port int) error {
			return t.adb.Reverse(ctx, serial, remote, port)
		},
		attached: &t.launchAgents,
	}
	return d.launchWith(ctx, pkg, activity, exts)
}

func (d *launchInjector) launchWith(ctx context.Context, pkg, activity string, exts []target.LaunchExtension) (*target.LaunchWithResult, error) {
	if err := checkAndroidLaunch(pkg, activity); err != nil {
		return nil, err
	}
	d.attached.launching.Lock()
	defer d.attached.launching.Unlock()
	agents := false
	for _, e := range exts {
		// Checked again here, not only by the caller, because these values
		// reach the device's shell.
		if err := e.Validate(target.LaunchAndroid); err != nil {
			return nil, fmt.Errorf("extension %s: %w", e.Name, err)
		}
		agents = agents || e.Agent != nil
	}
	lr, err := d.launch(ctx, pkg, activity)
	if err != nil {
		return nil, err
	}
	pid, err := d.waitPid(ctx, pkg)
	if err != nil {
		return nil, err
	}
	if agents {
		// The app is up by now: a 400 here leaves it running, without the
		// agents.
		if _, err := runAsPkg(ctx, d.sh, pkg, "true"); err != nil {
			if errors.Is(err, ErrNotDebuggable) {
				return nil, err
			}
			return nil, fmt.Errorf("run-as %s: %w", pkg, err)
		}
	}
	res := &target.LaunchWithResult{LaunchResult: *lr}
	for _, e := range exts {
		le := target.LaunchedExtension{Name: e.Name, PID: pid}
		// Every launch: an emulator's reverses go with an adb restart or a
		// reboot, and nothing else would put this one back.
		if e.Reverse != "" {
			if err := d.reverse(ctx, "localabstract:"+e.Reverse, e.ReversePort); err != nil {
				return nil, fmt.Errorf("extension %s: %w", e.Name, err)
			}
		}
		if e.Agent != nil {
			if d.attached.attachedTo(e.Name, pkg, pid) {
				le.Attached = target.AttachedAlready
			} else {
				if err := d.attach(ctx, pkg, pid, e); err != nil {
					return nil, fmt.Errorf("extension %s: %w", e.Name, err)
				}
				d.attached.note(e.Name, pkg, pid)
				le.Attached = true
			}
		}
		res.With = append(res.With, le)
	}
	return res, nil
}

// waitPid waits for pkg's process, which an `am start -W` launch has
// usually started already.
func (d *launchInjector) waitPid(ctx context.Context, pkg string) (int, error) {
	wait := cmp.Or(d.pidWait, launchPidWait)
	deadline := time.Now().Add(wait)
	for {
		out, err := d.sh(ctx, "pidof "+shellQuote(pkg))
		if f := strings.Fields(out); err == nil && len(f) > 0 {
			if pid, err := strconv.Atoi(f[0]); err == nil && pid > 0 {
				return pid, nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return 0, fmt.Errorf("%s did not start a process within %s", pkg, wait)
		}
		d.sleep(ctx)
	}
}

func (d *launchInjector) sleep(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(cmp.Or(d.poll, launchPoll)):
	}
}

// attach copies e's agent into pkg's data directory and attaches it to pid.
// The copies are removed before they are written: a cp over a .so the
// process has loaded rewrote it in place and crashed the app, and the dex
// is read-only, so cp could not overwrite it anyway. ART refuses a writable
// dex, hence its chmod.
func (d *launchInjector) attach(ctx context.Context, pkg string, pid int, e target.LaunchExtension) error {
	so, dex := e.Name+".so", e.Name+".dex"
	if err := d.push(ctx, e.Agent.SO, launchExtTmp+"/"+so); err != nil {
		return err
	}
	if err := d.push(ctx, e.Agent.Dex, launchExtTmp+"/"+dex); err != nil {
		return err
	}
	q := shellQuote
	script := "mkdir -p " + q(launchExtDir) + " && cd " + q(launchExtDir) +
		" && rm -f " + q(so) + " " + q(dex) +
		" && cp " + q(launchExtTmp+"/"+so) + " " + q(launchExtTmp+"/"+dex) + " ." +
		" && chmod 444 " + q(dex)
	if out, err := runAsPkg(ctx, d.sh, pkg, script); err != nil || strings.TrimSpace(out) != "" {
		return fmt.Errorf("copying the agent into %s: %v %s", pkg, err, strings.TrimSpace(out))
	}
	dir := "/data/data/" + pkg + "/" + launchExtDir
	arg := dir + "/" + so
	if opts := agentOptions(e.Agent.Options, dir+"/"+dex); opts != "" {
		arg += "=" + opts
	}
	out, err := d.sh(ctx, "am attach-agent "+q(pkg)+" "+q(arg))
	if err != nil || strings.TrimSpace(out) != "" {
		return fmt.Errorf("attach-agent %s: %v %s", pkg, err, strings.TrimSpace(out))
	}
	return d.waitLoaded(ctx, pkg, pid, so)
}

// waitLoaded waits for so to show in pid's maps. am attach-agent hands the
// path to the app and returns, and an agent that fails to load says so only
// in logcat, so without this a broken agent would be reported attached.
func (d *launchInjector) waitLoaded(ctx context.Context, pkg string, pid int, so string) error {
	script := "grep -c -F " + shellQuote("/"+launchExtDir+"/"+so) + " /proc/" + strconv.Itoa(pid) + "/maps"
	wait := cmp.Or(d.loadWait, launchLoadWait)
	deadline := time.Now().Add(wait)
	for {
		out, _ := runAsPkg(ctx, d.sh, pkg, script)
		if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && n > 0 {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the agent did not load into %s (pid %d) within %s; logcat says why", pkg, pid, wait)
		}
		d.sleep(ctx)
	}
}

// agentOptions is the options string Agent_OnAttach gets: key=value pairs
// joined by commas, sorted by key, with "@dex" in a value standing for the
// dex's device path. Validate has kept commas, "=" and anything a shell
// reads out of both.
func agentOptions(opts map[string]string, dexPath string) string {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strings.ReplaceAll(opts[k], "@dex", dexPath))
	}
	return strings.Join(parts, ",")
}

var (
	_ target.LaunchInjector = (*Device)(nil)
	_ target.LaunchInjector = (*Emulator)(nil)
)
