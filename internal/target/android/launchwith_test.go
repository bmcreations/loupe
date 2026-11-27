package android

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// fakeLaunchDevice is a device for launchInjector: the pid pidof reports,
// whether run-as works, and whether an attached agent shows in the maps.
type fakeLaunchDevice struct {
	pid          int
	debuggable   bool
	loads        bool
	attachOutput string
	// mapped is the pids whose maps show the agent: set by an attach that
	// loads, or by a test standing in for an earlier loupe's attach.
	mapped map[int]bool
	// refusal is what run-as prints for an app that is not debuggable.
	refusal string
	log     []string
}

func (f *fakeLaunchDevice) injector(attached *attachLog) *launchInjector {
	return &launchInjector{
		sh: func(_ context.Context, args ...string) (string, error) {
			cmd := strings.Join(args, " ")
			f.log = append(f.log, "sh "+cmd)
			switch {
			case strings.HasPrefix(cmd, "pidof "):
				if f.pid == 0 {
					return "", errors.New("exit status 1")
				}
				return fmt.Sprintf("%d\n", f.pid), nil
			case strings.HasPrefix(cmd, "run-as ") && !f.debuggable:
				if f.refusal != "" {
					return f.refusal, errors.New("exit status 1")
				}
				return "run-as: package not debuggable: com.example.app\n", errors.New("exit status 1")
			case strings.Contains(cmd, "grep -c -F"):
				if f.mapped[f.pid] {
					return "1\n", nil
				}
				return "0\n", errors.New("exit status 1")
			case strings.HasPrefix(cmd, "am attach-agent "):
				if f.loads && f.attachOutput == "" {
					if f.mapped == nil {
						f.mapped = map[int]bool{}
					}
					f.mapped[f.pid] = true
				}
				return f.attachOutput, nil
			}
			return "", nil
		},
		launch: func(_ context.Context, pkg, activity string) (*target.LaunchResult, error) {
			f.log = append(f.log, "launch "+pkg+" "+activity)
			return &target.LaunchResult{Activity: pkg + "/.Main"}, nil
		},
		push: func(_ context.Context, local, remote string) error {
			f.log = append(f.log, "push "+filepath.Base(local)+" "+remote)
			return nil
		},
		reverse: func(_ context.Context, remote string, port int) error {
			f.log = append(f.log, fmt.Sprintf("reverse %s tcp:%d", remote, port))
			return nil
		},
		attached: attached,
		poll:     time.Millisecond,
		pidWait:  20 * time.Millisecond,
		loadWait: 20 * time.Millisecond,
	}
}

func agentExt(t *testing.T) target.LaunchExtension {
	t.Helper()
	dir := t.TempDir()
	so, dex := filepath.Join(dir, "libagent.so"), filepath.Join(dir, "runtime.dex")
	for _, p := range []string{so, dex} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return target.LaunchExtension{
		Name: "ext",
		Agent: &target.LaunchAgent{SO: so, Dex: dex, Options: map[string]string{
			"server": "local:ext-sock", "runtime": "@dex",
		}},
		Reverse: "ext-sock", ReversePort: 18492,
	}
}

// The order docs/extensions.md gives: launch, wait for the process, check
// run-as, reverse, push both files, replace the app's copies, attach with
// the options built here, then see the agent in the process's maps.
func TestLaunchWithAttachesInOrder(t *testing.T) {
	f := &fakeLaunchDevice{pid: 1234, debuggable: true, loads: true}
	res, err := f.injector(&attachLog{}).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{agentExt(t)})
	if err != nil {
		t.Fatal(err)
	}
	const dir = "/data/data/com.example.app/code_cache/loupe-ext"
	want := []string{
		"launch com.example.app ",
		"sh pidof 'com.example.app'",
		"sh run-as 'com.example.app' sh -c 'true'",
		"reverse localabstract:ext-sock tcp:18492",
		"sh run-as 'com.example.app' sh -c " + shellQuote("grep -c -F '/code_cache/loupe-ext/ext.so' /proc/1234/maps"),
		"push libagent.so /data/local/tmp/loupe-ext/ext.so",
		"push runtime.dex /data/local/tmp/loupe-ext/ext.dex",
		"sh run-as 'com.example.app' sh -c " + shellQuote("mkdir -p 'code_cache/loupe-ext' && cd 'code_cache/loupe-ext'"+
			" && rm -f 'ext.so' 'ext.dex' && cp '/data/local/tmp/loupe-ext/ext.so' '/data/local/tmp/loupe-ext/ext.dex' . && chmod 444 'ext.dex'"),
		"sh am attach-agent 'com.example.app' '" + dir + "/ext.so=runtime=" + dir + "/ext.dex,server=local:ext-sock'",
		"sh run-as 'com.example.app' sh -c " + shellQuote("grep -c -F '/code_cache/loupe-ext/ext.so' /proc/1234/maps"),
	}
	if !reflect.DeepEqual(f.log, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.log, "\n"), strings.Join(want, "\n"))
	}
	wantWith := []target.LaunchedExtension{{Name: "ext", PID: 1234, Attached: true}}
	if !reflect.DeepEqual(res.With, wantWith) || res.Activity != "com.example.app/.Main" {
		t.Errorf("result = %+v", res)
	}
}

// A second launch of a process loupe already attached to reverses again but
// does not load the agent a second time; a new process gets it again.
func TestLaunchWithAttachesOncePerProcess(t *testing.T) {
	f := &fakeLaunchDevice{pid: 1234, debuggable: true, loads: true}
	attached := &attachLog{}
	ext := agentExt(t)
	if _, err := f.injector(attached).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{ext}); err != nil {
		t.Fatal(err)
	}
	f.log = nil
	res, err := f.injector(attached).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{ext})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.log {
		if strings.HasPrefix(c, "push ") || strings.Contains(c, "attach-agent") {
			t.Errorf("second launch ran %q", c)
		}
	}
	if !slicesContains(f.log, "reverse localabstract:ext-sock tcp:18492") {
		t.Errorf("second launch did not reverse: %q", f.log)
	}
	if got := res.With[0].Attached; got != target.AttachedAlready {
		t.Errorf("attached = %v, want %q", got, target.AttachedAlready)
	}

	f.pid, f.log = 5678, nil
	res, err = f.injector(attached).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{ext})
	if err != nil {
		t.Fatal(err)
	}
	if res.With[0].Attached != true || res.With[0].PID != 5678 {
		t.Errorf("new process: with = %+v", res.With)
	}
}

// A loupe that restarted has no record of its attach, but the agent is
// still in the process's maps: that is "already", not a second attach.
func TestLaunchWithSeesAnAgentAnEarlierLoupeAttached(t *testing.T) {
	f := &fakeLaunchDevice{pid: 1234, debuggable: true, loads: true, mapped: map[int]bool{1234: true}}
	res, err := f.injector(&attachLog{}).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{agentExt(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.log {
		if strings.HasPrefix(c, "push ") || strings.Contains(c, "attach-agent") || strings.Contains(c, "rm -f") {
			t.Errorf("ran %q", c)
		}
	}
	if got := res.With[0].Attached; got != target.AttachedAlready {
		t.Errorf("attached = %v, want %q", got, target.AttachedAlready)
	}
}

// run-as refusing the app is ErrNotDebuggable, after the launch and before
// anything is pushed, reversed or attached.
func TestLaunchWithRefusesAnAppThatIsNotDebuggable(t *testing.T) {
	f := &fakeLaunchDevice{pid: 1234, debuggable: false, loads: true}
	_, err := f.injector(&attachLog{}).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{agentExt(t)})
	if !errors.Is(err, ErrNotDebuggable) {
		t.Fatalf("err = %v, want ErrNotDebuggable", err)
	}
	for _, c := range f.log {
		if strings.HasPrefix(c, "push ") || strings.HasPrefix(c, "reverse ") || strings.Contains(c, "attach-agent") {
			t.Errorf("ran %q", c)
		}
	}
}

// run-as refuses an app with a system uid, such as Settings, in other words.
// That is ErrNotDebuggable too, not a bare exit status.
func TestLaunchWithRefusesASystemApp(t *testing.T) {
	f := &fakeLaunchDevice{pid: 1234, debuggable: false, loads: true, refusal: "run-as: package not an application: com.android.settings\n"}
	_, err := f.injector(&attachLog{}).launchWith(context.Background(), "com.android.settings", "", []target.LaunchExtension{agentExt(t)})
	if !errors.Is(err, ErrNotDebuggable) {
		t.Fatalf("err = %v, want ErrNotDebuggable", err)
	}
}

// Two extensions asking for one socket name is an error before anything
// launches: the second reverse would repoint the first one's socket.
func TestLaunchWithRefusesTwoExtensionsOnOneReverse(t *testing.T) {
	f := &fakeLaunchDevice{pid: 1234, debuggable: true, loads: true}
	a := target.LaunchExtension{Name: "one", Reverse: "shared-sock", ReversePort: 9001}
	b := target.LaunchExtension{Name: "two", Reverse: "shared-sock", ReversePort: 9002}
	_, err := f.injector(&attachLog{}).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{a, b})
	if !errors.Is(err, target.ErrBadLaunchSpec) {
		t.Fatalf("err = %v, want ErrBadLaunchSpec", err)
	}
	if len(f.log) != 0 {
		t.Errorf("ran %q before refusing", f.log)
	}
}

// An extension that asks only for a reverse needs no run-as.
func TestLaunchWithReverseOnly(t *testing.T) {
	f := &fakeLaunchDevice{pid: 1234, debuggable: false}
	ext := target.LaunchExtension{Name: "ext", Reverse: "ext-sock", ReversePort: 9}
	res, err := f.injector(&attachLog{}).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{ext})
	if err != nil {
		t.Fatal(err)
	}
	if !slicesContains(f.log, "reverse localabstract:ext-sock tcp:9") {
		t.Errorf("no reverse: %q", f.log)
	}
	if res.With[0].Attached != nil {
		t.Errorf("attached = %v, want nil", res.With[0].Attached)
	}
}

// An agent that does not show in the maps was not loaded, which
// am attach-agent does not say, and is not recorded as attached.
func TestLaunchWithSeesAFailedLoad(t *testing.T) {
	f := &fakeLaunchDevice{pid: 1234, debuggable: true, loads: false}
	attached := &attachLog{}
	if _, err := f.injector(attached).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{agentExt(t)}); err == nil || !strings.Contains(err.Error(), "did not load") {
		t.Fatalf("err = %v, want a failed load", err)
	}
	if attached.attachedTo("ext", "com.example.app", 1234) {
		t.Error("a failed load was recorded as attached")
	}
	f.loads, f.attachOutput = true, "Error: package not found\n"
	if _, err := f.injector(attached).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{agentExt(t)}); err == nil || !strings.Contains(err.Error(), "attach-agent") {
		t.Errorf("err = %v, want attach-agent's own output", err)
	}
}

func TestLaunchWithNeedsTheProcess(t *testing.T) {
	f := &fakeLaunchDevice{pid: 0, debuggable: true, loads: true}
	_, err := f.injector(&attachLog{}).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{agentExt(t)})
	if err == nil || !strings.Contains(err.Error(), "did not start a process") {
		t.Fatalf("err = %v", err)
	}
}

// The checks run again here, before anything reaches the device, since
// these values go into its shell.
func TestLaunchWithChecksEachSpecFirst(t *testing.T) {
	bad := agentExt(t)
	bad.Agent.Options["server"] = "x'; reboot; '"
	for _, ext := range []target.LaunchExtension{
		bad,
		{Name: "ext", Reverse: "x y", ReversePort: 1},
		{Name: "ext", Env: map[string]string{"A": "b"}},
		{Name: "ext"},
	} {
		f := &fakeLaunchDevice{pid: 1234, debuggable: true, loads: true}
		_, err := f.injector(&attachLog{}).launchWith(context.Background(), "com.example.app", "", []target.LaunchExtension{ext})
		if !errors.Is(err, target.ErrBadLaunchSpec) {
			t.Errorf("%+v: err = %v", ext, err)
		}
		if len(f.log) != 0 {
			t.Errorf("%+v: ran %q", ext, f.log)
		}
	}
}

func TestAgentOptionsAreSortedWithTheDexResolved(t *testing.T) {
	got := agentOptions(map[string]string{"z": "1", "a": "@dex", "m": "pre@dex"}, "/d/x.dex")
	if want := "a=/d/x.dex,m=pre/d/x.dex,z=1"; got != want {
		t.Errorf("options = %q, want %q", got, want)
	}
	if got := agentOptions(nil, "/d/x.dex"); got != "" {
		t.Errorf("no options = %q", got)
	}
}

func TestAndroidLaunchFacts(t *testing.T) {
	f, err := launchFacts(context.Background(), func(context.Context, ...string) (string, error) {
		return "arm64-v8a\n36\n", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (target.LaunchFacts{Platform: target.LaunchAndroid, ABI: "arm64-v8a", SDK: 36}); f != want {
		t.Errorf("facts = %+v, want %+v", f, want)
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
