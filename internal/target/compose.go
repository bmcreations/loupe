package target

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/composetree"
)

// ComposeInspector is implemented by targets that can read the composable
// tree out of the foreground app. Only Android targets can, and only for a
// debuggable app, so this is not part of Target.
type ComposeInspector interface {
	ComposeDump(ctx context.Context) (*ComposeDump, error)
}

// ComposeDump is a live connection to the compose agent in one app process.
type ComposeDump struct {
	Package string
	// Fetch asks the agent for every window's composables once. The tree is
	// short for a moment after attach; composetree.Stable polls it.
	Fetch func(context.Context) ([][]byte, error)
}

var (
	ErrNotDebuggable = errors.New("app is not debuggable")
	ErrNoCompose     = errors.New("app does not use Compose")
)

const composeDeviceTmp = "/data/local/tmp/loupe-compose"

// composeDevice attaches the compose agent (internal/composetree) to the
// foreground app and keeps one forward per app process. Every device-facing
// step is a field, so the attach logic can be tested without a device, the
// same shape androidApp uses.
type composeDevice struct {
	sh         shellFunc
	foreground func(ctx context.Context) (pkg string, err error)
	push       func(ctx context.Context, data []byte, remote string) error
	forward    func(ctx context.Context, remote string) (port int, err error)
	unforward  func(ctx context.Context, port int) error
	call       func(ctx context.Context, port int, op byte, payload []byte) ([]byte, error)
	jar        func(ctx context.Context, version string) ([]byte, error)

	mu   sync.Mutex
	sess *composeSession
}

// composeSession is an attached app process. A new pid means the app
// restarted and the agent, and the abstract socket named after the pid, went
// with it.
type composeSession struct {
	pkg     string
	pid     int
	port    int
	version string
}

func (d *composeDevice) ComposeDump(ctx context.Context) (*ComposeDump, error) {
	pkg, err := d.foreground(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidPackageName(pkg) {
		return nil, fmt.Errorf("compose: unexpected package name %q", pkg)
	}
	out, err := d.sh(ctx, "pidof "+shellQuote(pkg))
	pid, perr := strconv.Atoi(strings.Fields(out + " x")[0])
	if err != nil || perr != nil {
		return nil, fmt.Errorf("compose: %s is not running", pkg)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.sess
	if s == nil || s.pkg != pkg || s.pid != pid {
		if s != nil {
			_ = d.unforward(ctx, s.port)
			d.sess = nil
		}
		if s, err = d.attach(ctx, pkg, pid); err != nil {
			return nil, err
		}
		d.sess = s
	}
	if s.version == "" {
		return nil, fmt.Errorf("%s: %w", pkg, ErrNoCompose)
	}
	return &ComposeDump{Package: pkg, Fetch: func(ctx context.Context) ([][]byte, error) {
		b, err := d.call(ctx, s.port, composetree.OpTree, nil)
		if err != nil {
			d.drop(s)
			return nil, err
		}
		return composetree.Windows(b)
	}}, nil
}

// drop forgets s if it is still the current session, so the next dump
// attaches afresh instead of retrying a forward that has gone away.
func (d *composeDevice) drop(s *composeSession) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == s {
		d.sess = nil
	}
}

func (d *composeDevice) attach(ctx context.Context, pkg string, pid int) (*composeSession, error) {
	out, err := d.sh(ctx, "getprop ro.product.cpu.abi")
	if err != nil {
		return nil, fmt.Errorf("compose: reading the ABI: %w", err)
	}
	abi := strings.TrimSpace(out)
	so, dex, err := composetree.AgentFiles(abi)
	if err != nil {
		return nil, err
	}
	soName := composetree.SOName(abi)
	if err := d.push(ctx, so, composeDeviceTmp+"/"+soName); err != nil {
		return nil, err
	}
	if err := d.push(ctx, dex, composeDeviceTmp+"/"+composetree.DexName); err != nil {
		return nil, err
	}
	// rm before cp: the dex is read-only, so cp can't overwrite one left by
	// an earlier attach. ART refuses a writable dex, hence the chmod.
	if err := d.runAs(ctx, pkg, fmt.Sprintf(
		"mkdir -p files/loupe && cd files/loupe && rm -f %[2]s %[3]s && cp %[1]s/%[2]s %[1]s/%[3]s . && chmod 444 %[3]s",
		composeDeviceTmp, soName, composetree.DexName)); err != nil {
		return nil, err
	}

	dir := "/data/data/" + pkg + "/files/loupe"
	out, err = d.sh(ctx, "am attach-agent "+shellQuote(pkg)+" "+shellQuote(dir+"/"+soName+"="+dir+"/"+composetree.DexName))
	if err != nil || strings.TrimSpace(out) != "" {
		return nil, fmt.Errorf("compose: attach-agent %s: %v %s", pkg, err, strings.TrimSpace(out))
	}

	port, err := d.forward(ctx, "localabstract:loupe-compose-"+strconv.Itoa(pid))
	if err != nil {
		return nil, err
	}
	s := &composeSession{pkg: pkg, pid: pid, port: port}
	if err := d.load(ctx, s); err != nil {
		_ = d.unforward(ctx, port)
		return nil, err
	}
	return s, nil
}

// load asks the agent for the app's Compose version and loads the matching
// inspector. It leaves s.version empty for an app without Compose.
func (d *composeDevice) load(ctx context.Context, s *composeSession) error {
	// The agent opens its socket on its own thread once the Application
	// exists, so the first calls after attach can find nothing listening.
	var hello []byte
	var err error
	for deadline := time.Now().Add(3 * time.Second); ; {
		if hello, err = d.call(ctx, s.port, composetree.OpHello, nil); err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("compose: agent in %s did not answer: %w", s.pkg, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var h struct {
		ComposeVersion string `json:"composeVersion"`
	}
	if err := json.Unmarshal(hello, &h); err != nil {
		return fmt.Errorf("compose: agent hello %q: %w", hello, err)
	}
	if h.ComposeVersion == "" {
		return nil
	}
	jar, err := d.jar(ctx, h.ComposeVersion)
	if err != nil {
		return err
	}
	// The version passed InspectorJar's pattern, so it is safe in a path.
	name := "inspector-" + h.ComposeVersion + ".jar"
	if err := d.push(ctx, jar, composeDeviceTmp+"/"+name); err != nil {
		return err
	}
	if err := d.runAs(ctx, s.pkg, fmt.Sprintf("cd files/loupe && rm -f %[2]s && cp %[1]s/%[2]s .", composeDeviceTmp, name)); err != nil {
		return err
	}
	path := "/data/data/" + s.pkg + "/files/loupe/" + name
	if _, err := d.call(ctx, s.port, composetree.OpLoad, []byte(path)); err != nil {
		return err
	}
	s.version = h.ComposeVersion
	return nil
}

// runAs runs script as pkg, in its data directory. run-as is what limits
// this feature to debuggable apps; its refusal becomes ErrNotDebuggable.
func (d *composeDevice) runAs(ctx context.Context, pkg, script string) error {
	out, err := d.sh(ctx, "run-as "+shellQuote(pkg)+" sh -c "+shellQuote(script))
	if strings.Contains(out, "not debuggable") {
		return fmt.Errorf("%s: %w", pkg, ErrNotDebuggable)
	}
	if err != nil || strings.TrimSpace(out) != "" {
		return fmt.Errorf("compose: run-as %s: %v %s", pkg, err, strings.TrimSpace(out))
	}
	return nil
}

// ComposeDump attaches the compose agent to the foreground app, or reuses
// the attachment from an earlier call while the app process lives.
func (t *ADBTarget) ComposeDump(ctx context.Context) (*ComposeDump, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	t.composeOnce.Do(func() { t.compose = newComposeDevice(t) })
	return t.compose.ComposeDump(ctx)
}

func newComposeDevice(t *ADBTarget) *composeDevice {
	serial := t.info.Serial
	return &composeDevice{
		sh: func(ctx context.Context, args ...string) (string, error) {
			return t.adb.Shell(ctx, serial, args...)
		},
		foreground: func(ctx context.Context) (string, error) {
			app, err := newAndroidApp(t).ForegroundApp(ctx)
			if err != nil {
				return "", err
			}
			return app.Package, nil
		},
		push: func(ctx context.Context, data []byte, remote string) error {
			f, err := os.CreateTemp("", "loupe-compose-*")
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			if _, err := f.Write(data); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			return t.adb.Push(ctx, serial, f.Name(), remote)
		},
		forward: func(ctx context.Context, remote string) (int, error) {
			return t.adb.Forward(ctx, serial, remote)
		},
		unforward: func(ctx context.Context, port int) error {
			return t.adb.RemoveForward(ctx, serial, port)
		},
		call: func(ctx context.Context, port int, op byte, payload []byte) ([]byte, error) {
			var dl net.Dialer
			c, err := dl.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				return nil, err
			}
			defer c.Close()
			if d, ok := ctx.Deadline(); ok {
				c.SetDeadline(d)
			}
			return composetree.Call(c, op, payload)
		},
		jar: func(ctx context.Context, version string) ([]byte, error) {
			cache, err := os.UserCacheDir()
			if err != nil {
				return nil, err
			}
			p, err := composetree.InspectorJar(ctx, filepath.Join(cache, "loupe"), composetree.MavenBase, version)
			if err != nil {
				return nil, err
			}
			return os.ReadFile(p)
		},
	}
}
