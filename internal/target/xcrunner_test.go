package target

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTouchesGoToTheRunnerBelowIOS27Only(t *testing.T) {
	for _, tc := range []struct {
		os   string
		want bool
	}{{"26.6", true}, {"18.0", true}, {"27.0", false}, {"27.2", false}, {"", false}} {
		d := NewIOSDevice(IOSDeviceInfo{UDID: "u", OSVersion: tc.os, Transport: "wired"})
		if got := d.touchesViaRunner(); got != tc.want {
			t.Errorf("iOS %q: touchesViaRunner = %v; want %v", tc.os, got, tc.want)
		}
	}
}

func TestRunnerVerbsAreTouchesNotButtonsOrKeys(t *testing.T) {
	for line, want := range map[string]bool{
		"tap 10 20": true, "swipe 1 2 3 4 300": true, "touch down 1 2": true,
		"touch2 down 1 2 3 4": true, "size": true,
		"button home": false, "keydown 4": false, "route": false, "hwkeyboard on": false,
	} {
		if got := runnerVerb(line); got != want {
			t.Errorf("runnerVerb(%q) = %v; want %v", line, got, want)
		}
	}
}

func TestATouchOverWiFiBelow27NeedsACable(t *testing.T) {
	d := NewIOSDevice(IOSDeviceInfo{UDID: "u", OSVersion: "26.6", Transport: "localNetwork"})
	d.SetXCRunner("/built")
	if _, err := d.do(context.Background(), 0, "tap %d %d", 1, 2); !errors.Is(err, ErrTouchNeedsCable) {
		t.Fatalf("err = %v; want ErrTouchNeedsCable", err)
	}
}

func TestATouchBelow27WithNoRunnerBuiltSaysSo(t *testing.T) {
	d := NewIOSDevice(IOSDeviceInfo{UDID: "u", OSVersion: "26.6", Transport: "wired"})
	if _, err := d.do(context.Background(), 0, "tap %d %d", 1, 2); !errors.Is(err, ErrRunnerNotBuilt) {
		t.Fatalf("err = %v; want ErrRunnerNotBuilt", err)
	}
}

// builtRunnerDir is a products directory holding one .xctestrun.
func builtRunnerDir(t *testing.T) string {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "XCRunner.xctestrun"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestARunnerBuiltAfterStartupIsFoundOnTheNextTouch(t *testing.T) {
	dir := t.TempDir()
	d := NewIOSDevice(IOSDeviceInfo{UDID: "u", OSVersion: "26.6", Transport: "wired"})
	d.SetXCRunner(dir)
	if _, err := d.do(context.Background(), 0, "tap %d %d", 1, 2); !errors.Is(err, ErrRunnerNotBuilt) {
		t.Fatalf("before the build: err = %v; want ErrRunnerNotBuilt", err)
	}

	oldStart := startRunner
	started := make(chan struct{}, 1)
	startRunner = func(udid, dir string) (*exec.Cmd, chan error, error) {
		started <- struct{}{}
		return nil, nil, errors.New("no phone here")
	}
	t.Cleanup(func() { startRunner = oldStart })
	if err := os.WriteFile(filepath.Join(dir, "XCRunner.xctestrun"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := d.do(context.Background(), 0, "tap %d %d", 1, 2); errors.Is(err, ErrRunnerNotBuilt) {
		t.Fatalf("after the build: err = %v; want the runner started", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the touch after the build did not start the runner")
	}
}

// fakeRunner is the runner's line protocol on a pipe: ping and tap answer
// ok, and every line is recorded.
type fakeRunner struct {
	mu    sync.Mutex
	lines []string
	up    chan struct{} // closed once dials should succeed
}

func (f *fakeRunner) dial(ctx context.Context, udid string) (net.Conn, error) {
	select {
	case <-f.up:
	default:
		return nil, errors.New("usbmuxd: connection refused by the device (nothing listening)")
	}
	a, b := net.Pipe()
	go func() {
		r := bufio.NewReader(b)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			f.mu.Lock()
			f.lines = append(f.lines, line)
			f.mu.Unlock()
			fmt.Fprintln(b, "ok")
		}
	}()
	return a, nil
}

func TestATouchBeforeTheRunnerAnswersIsAnErrorNotAFalseOK(t *testing.T) {
	f := &fakeRunner{up: make(chan struct{})}
	started := make(chan struct{}, 1)
	oldDial, oldStart := dialRunner, startRunner
	dialRunner = f.dial
	startRunner = func(udid, dir string) (*exec.Cmd, chan error, error) {
		started <- struct{}{}
		close(f.up) // xcodebuild "installs" it
		return nil, make(chan error), nil
	}
	t.Cleanup(func() { dialRunner, startRunner = oldDial, oldStart })

	d := NewIOSDevice(IOSDeviceInfo{UDID: "u", OSVersion: "26.6", Transport: "wired"})
	d.SetXCRunner(builtRunnerDir(t))
	if _, err := d.do(context.Background(), 0, "tap %d %d", 1, 2); !errors.Is(err, ErrRunnerStarting) {
		t.Fatalf("first tap: err = %v; want ErrRunnerStarting", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first tap did not start the runner")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := d.do(context.Background(), 0, "tap %d %d", 3, 4)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrRunnerStarting) || time.Now().After(deadline) {
			t.Fatalf("tap after start: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.lines[len(f.lines)-1]; got != "tap 3 4" {
		t.Fatalf("runner got %q last; want the tap sent once it answered, not the one refused", got)
	}
	for _, l := range f.lines {
		if l == "tap 1 2" {
			t.Fatal("the tap answered ErrRunnerStarting still reached the runner")
		}
	}
}
