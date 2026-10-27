package target

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeComposeDevice answers composeDevice's shell commands and agent calls
// the way a debuggable x86_64 emulator running com.example does, unless a
// test overrides a piece.
type fakeComposeDevice struct {
	pids     []string // pidof answers, one per call; the last repeats
	runAs    string   // run-as output; empty means success
	version  string   // composeVersion the agent reports
	cmds     []string
	ops      []string
	pushed   []string
	forwards []string
	removed  []int
	jars     int
	nextPort int
}

func (f *fakeComposeDevice) device() *composeDevice {
	if f.nextPort == 0 {
		f.nextPort = 5000
	}
	return &composeDevice{
		sh: func(ctx context.Context, args ...string) (string, error) {
			cmd := strings.Join(args, " ")
			f.cmds = append(f.cmds, cmd)
			switch {
			case strings.HasPrefix(cmd, "pidof "):
				p := f.pids[0]
				if len(f.pids) > 1 {
					f.pids = f.pids[1:]
				}
				return p + "\n", nil
			case strings.HasPrefix(cmd, "getprop "):
				return "x86_64\n", nil
			case strings.HasPrefix(cmd, "run-as "):
				if f.runAs != "" {
					return f.runAs, errors.New("exit status 1")
				}
				return "", nil
			case strings.HasPrefix(cmd, "am attach-agent "):
				return "", nil
			}
			return "", fmt.Errorf("unexpected command %q", cmd)
		},
		foreground: func(ctx context.Context) (string, error) { return "com.example", nil },
		push: func(ctx context.Context, data []byte, remote string) error {
			f.pushed = append(f.pushed, remote)
			return nil
		},
		forward: func(ctx context.Context, remote string) (int, error) {
			f.forwards = append(f.forwards, remote)
			f.nextPort++
			return f.nextPort, nil
		},
		unforward: func(ctx context.Context, port int) error {
			f.removed = append(f.removed, port)
			return nil
		},
		call: func(ctx context.Context, port int, op byte, payload []byte) ([]byte, error) {
			f.ops = append(f.ops, fmt.Sprintf("%c@%d %s", op, port, payload))
			switch op {
			case 'H':
				return []byte(`{"pid":1,"composeVersion":"` + f.version + `"}`), nil
			case 'L':
				return nil, nil
			case 'C':
				return []byte{0, 0, 0, 0}, nil
			}
			return nil, fmt.Errorf("op %c", op)
		},
		jar: func(ctx context.Context, version string) ([]byte, error) {
			f.jars++
			return []byte("jar " + version), nil
		},
	}
}

func (f *fakeComposeDevice) count(prefix string) int {
	n := 0
	for _, c := range f.cmds {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestComposeDumpAttachesAndLoads(t *testing.T) {
	f := &fakeComposeDevice{pids: []string{"100"}, version: "1.12.1"}
	d, err := f.device().ComposeDump(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Package != "com.example" {
		t.Fatalf("package = %q", d.Package)
	}
	if got := f.forwards; len(got) != 1 || got[0] != "localabstract:loupe-compose-100" {
		t.Fatalf("forwards = %v", got)
	}
	wantL := "L@5001 /data/data/com.example/files/loupe/inspector-1.12.1.jar"
	if len(f.ops) != 2 || f.ops[0] != "H@5001 " || f.ops[1] != wantL {
		t.Fatalf("ops = %q, want H then %q", f.ops, wantL)
	}
	windows, err := d.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 0 {
		t.Fatalf("windows = %d", len(windows))
	}
}

func TestComposeDumpNotDebuggable(t *testing.T) {
	f := &fakeComposeDevice{pids: []string{"100"}, version: "1.12.1", runAs: "run-as: package not debuggable: com.example\n"}
	_, err := f.device().ComposeDump(context.Background())
	if !errors.Is(err, ErrNotDebuggable) {
		t.Fatalf("err = %v, want ErrNotDebuggable", err)
	}
	if !strings.Contains(err.Error(), "com.example") {
		t.Fatalf("err = %v, want the package named", err)
	}
	if f.count("am attach-agent") != 0 {
		t.Fatal("attached to an app that run-as refused")
	}
}

func TestComposeDumpNoCompose(t *testing.T) {
	f := &fakeComposeDevice{pids: []string{"100"}, version: ""}
	_, err := f.device().ComposeDump(context.Background())
	if !errors.Is(err, ErrNoCompose) {
		t.Fatalf("err = %v, want ErrNoCompose", err)
	}
	if f.jars != 0 {
		t.Fatalf("fetched the inspector %d times for an app with no Compose", f.jars)
	}
}

func TestComposeDumpReattachesWhenThePidChanges(t *testing.T) {
	f := &fakeComposeDevice{pids: []string{"100", "200", "200"}, version: "1.12.1"}
	d := f.device()
	for i := 0; i < 3; i++ {
		if _, err := d.ComposeDump(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count("am attach-agent"); n != 2 {
		t.Fatalf("%d attaches, want 2 (pid 100, pid 200, then reuse)", n)
	}
	if len(f.removed) != 1 || f.removed[0] != 5001 {
		t.Fatalf("removed forwards = %v, want [5001]", f.removed)
	}
	if f.jars != 2 {
		t.Fatalf("%d jar fetches, want one per attach", f.jars)
	}
}

func TestComposeDumpQuotesThePackage(t *testing.T) {
	f := &fakeComposeDevice{pids: []string{"100"}, version: "1.12.1"}
	if _, err := f.device().ComposeDump(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.cmds {
		if i := unquotedIndex(c, "com.example"); i >= 0 {
			t.Errorf("%q has the package outside quotes at %d", c, i)
		}
	}
}

// unquotedIndex is the first index of sub in s that starts outside single
// quotes, as the device's sh would read s, or -1.
func unquotedIndex(s, sub string) int {
	in := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			in = !in
			continue
		}
		if !in && strings.HasPrefix(s[i:], sub) {
			return i
		}
	}
	return -1
}
