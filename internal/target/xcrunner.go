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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bmcreations/loupe/internal/usbmux"
)

// Below iOS 27 a phone takes CoreDevice's HID button presses but drops its
// touches: simcap answers `ok` and nothing lands. XCUITest's synthesized
// touches do land, so a phone below 27 on a cable takes its touches from
// tools/xcrunner, a UI test that never returns, installed and started by
// `xcodebuild test-without-building` and serving simcap's line protocol on
// the phone's own 127.0.0.1.
//
// The Mac reaches that port through usbmuxd, the same path Xcode uses, so
// it needs a cable and nothing installed (internal/usbmux). The CoreDevice
// tunnel was not taken because whether a listener on the phone answers
// through it has not been tried, and the runner dialing out to the Mac
// would need the Local Network permission and the Mac's address.
//
// xcodebuild runs in its own process group, as an emulator does, so a
// server restarted by a build finds the runner still listening and attaches
// to it rather than installing it again.

// ErrRunnerStarting is a touch sent while the phone's XCUITest runner is
// not answering yet. Nothing was sent; the caller may retry. The first touch
// starts it, and an install and launch takes several seconds.
var ErrRunnerStarting = errors.New("the iPhone's touch runner is starting; try again in a few seconds")

// ErrRunnerNotBuilt is a touch on a phone below iOS 27 with no runner built.
var ErrRunnerNotBuilt = errors.New("touch on an iPhone below iOS 27 needs the XCUITest runner: make xcrunner TEAM=<team id>")

// ErrTouchNeedsCable is a touch on a phone below iOS 27 over Wi-Fi. The
// runner is reached through usbmuxd, which is USB only.
var ErrTouchNeedsCable = errors.New("touch on an iPhone below iOS 27 works only over a cable")

// xcRunnerPort is the runner's port on the phone, passed to it as
// TEST_RUNNER_LOUPE_PORT.
const xcRunnerPort = 7110

var (
	// xcRunnerStartWait is how long a started runner gets to answer before
	// it is given up as failed and the next touch starts it again.
	xcRunnerStartWait = 3 * time.Minute
	// xcRunnerBudget is what one command gets before a swipe's duration
	// is added. An XCUI tap waits for the app to idle, 1-2s on a busy one.
	xcRunnerBudget = 15 * time.Second
	// dialRunner opens the runner's port. A var so tests need no phone.
	dialRunner = func(ctx context.Context, udid string) (net.Conn, error) {
		return usbmux.Connect(ctx, udid, xcRunnerPort)
	}
	// startRunner starts xcodebuild. A var so tests need no phone.
	startRunner = startXCRunner
)

// xcRunner is one phone's runner: the xcodebuild that hosts it, while this
// server started it, and the connection to it.
type xcRunner struct {
	udid string
	dir  string // the built products, holding one .xctestrun

	mu       sync.Mutex
	conn     net.Conn
	r        *bufio.Reader
	starting bool
	started  time.Time
	cmd      *exec.Cmd
	lastErr  error
}

// do sends one line and returns the text after `ok`. With no connection it
// starts the runner, or attaches to one already listening, in the
// background and answers ErrRunnerStarting, never a false ok.
func (x *xcRunner) do(ctx context.Context, budget time.Duration, format string, a ...any) (string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.conn == nil {
		return "", x.startLocked()
	}
	line := fmt.Sprintf(format, a...)
	deadline := time.Now().Add(xcRunnerBudget + budget)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = x.conn.SetDeadline(deadline)
	if _, err := fmt.Fprintf(x.conn, "%s\n", line); err != nil {
		x.dropLocked()
		return "", fmt.Errorf("%w: %v", ErrRunnerStarting, err)
	}
	reply, err := x.r.ReadString('\n')
	if err != nil {
		// The line went out; whether it was applied is unknown.
		x.dropLocked()
		return "", fmt.Errorf("%w: the touch runner did not answer: %v", ErrPhoneDeliveryUnknown, err)
	}
	reply = strings.TrimRight(reply, "\r\n")
	if rest, ok := strings.CutPrefix(reply, "ok"); ok {
		return strings.TrimSpace(rest), nil
	}
	if why, ok := strings.CutPrefix(reply, "err "); ok {
		return "", fmt.Errorf("touch runner: %s", why)
	}
	return "", fmt.Errorf("touch runner: unexpected reply %q", reply)
}

// startLocked begins attaching in the background and returns the error the
// caller gets meanwhile. Called with mu held.
func (x *xcRunner) startLocked() error {
	if !x.starting {
		x.starting, x.started = true, time.Now()
		go x.attach()
	}
	err := fmt.Errorf("%w (%ds in)", ErrRunnerStarting, int(time.Since(x.started).Seconds()))
	if x.lastErr != nil {
		err = fmt.Errorf("%w; the last start failed: %v", err, x.lastErr)
	}
	return err
}

// attach dials the runner, and starts xcodebuild when nothing answers, then
// keeps dialing until it does, the xcodebuild exits, or xcRunnerStartWait
// passes.
func (x *xcRunner) attach() {
	conn, err := x.dial()
	var exited chan error
	if err != nil {
		var cmd *exec.Cmd
		cmd, exited, err = startRunner(x.udid, x.dir)
		if err == nil {
			x.mu.Lock()
			x.cmd = cmd
			x.mu.Unlock()
			conn, err = x.waitForListener(exited)
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.starting = false
	if err != nil {
		x.lastErr = err
		if x.cmd != nil {
			killGroup(x.cmd)
			x.cmd = nil
		}
		return
	}
	x.lastErr = nil
	x.conn, x.r = conn, bufio.NewReader(conn)
}

func (x *xcRunner) dial() (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := dialRunner(ctx, x.udid)
	if err != nil {
		return nil, err
	}
	// A listener is not enough: the runner answers ping only once it has
	// measured the screen.
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintln(c, "ping"); err == nil {
		if reply, err := bufio.NewReader(c).ReadString('\n'); err == nil && strings.HasPrefix(reply, "ok") {
			_ = c.SetDeadline(time.Time{})
			return c, nil
		}
	}
	c.Close()
	return nil, errors.New("the runner's port answered without ok to ping")
}

func (x *xcRunner) waitForListener(exited chan error) (net.Conn, error) {
	give := time.After(xcRunnerStartWait)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("xcodebuild exited before the runner listened (%v); see %s", err, xcRunnerLog(x.udid))
		case <-give:
			return nil, fmt.Errorf("the runner did not listen within %s; see %s", xcRunnerStartWait, xcRunnerLog(x.udid))
		case <-tick.C:
			if c, err := x.dial(); err == nil {
				return c, nil
			}
		}
	}
}

// dropLocked forgets the connection, so the next command attaches again.
func (x *xcRunner) dropLocked() {
	if x.conn != nil {
		x.conn.Close()
		x.conn, x.r = nil, nil
	}
}

// close drops the connection and stops an xcodebuild this server started.
func (x *xcRunner) close() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.dropLocked()
	if x.cmd != nil {
		killGroup(x.cmd)
		x.cmd = nil
	}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

func xcRunnerLog(udid string) string {
	dir, _ := os.UserCacheDir()
	return filepath.Join(dir, "loupe", "xcrunner-"+udid+".log")
}

// xcRunnerBuilt reports whether dir holds a built runner: one .xctestrun,
// which is what startXCRunner needs.
func xcRunnerBuilt(dir string) bool {
	runs, _ := filepath.Glob(filepath.Join(dir, "*.xctestrun"))
	return len(runs) == 1
}

// startXCRunner runs `xcodebuild test-without-building` against the
// .xctestrun in dir, in its own process group, logging to xcRunnerLog. The
// returned channel receives xcodebuild's exit.
func startXCRunner(udid, dir string) (*exec.Cmd, chan error, error) {
	runs, _ := filepath.Glob(filepath.Join(dir, "*.xctestrun"))
	if len(runs) != 1 {
		return nil, nil, fmt.Errorf("%s holds %d .xctestrun files, not one; run make xcrunner TEAM=<team id>", dir, len(runs))
	}
	logPath := xcRunnerLog(udid)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, nil, err
	}
	log, err := os.Create(logPath)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command("xcrun", "xcodebuild", "test-without-building",
		"-xctestrun", runs[0], "-destination", "id="+udid)
	cmd.Env = append(os.Environ(), "TEST_RUNNER_LOUPE_PORT="+strconv.Itoa(xcRunnerPort))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, nil, fmt.Errorf("start xcodebuild: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); log.Close() }()
	return cmd, exited, nil
}

// runnerVerb reports whether a simcap input line is a touch, which goes to
// the runner on a phone below iOS 27. Buttons and keys stay on CoreDevice,
// where they land.
func runnerVerb(line string) bool {
	verb, _, _ := strings.Cut(line, " ")
	switch verb {
	case "tap", "swipe", "touch", "touch2", "size":
		return true
	}
	return false
}
