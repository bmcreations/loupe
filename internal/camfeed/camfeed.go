// Package camfeed passes camera frames to Handset Camera, the Mac camera that
// Handset's camera extension adds. An AVD booted with that camera as its back
// camera sees each frame as it arrives, with no restart; the emulator itself
// only takes a camera at boot.
//
// The frames go through loupe-camfeed (tools/camfeed), a helper loupe starts
// and writes frames to. It is a process of its own because CoreMediaIO hands a
// process the camera list once: a process that was running when the extension
// was installed or updated never sees the camera. So the helper exits when its
// camera goes away and the next frame starts a new one.
package camfeed

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Name is the camera Handset's extension adds, as the Mac and
// `emulator -webcam-list` call it.
const Name = "Handset Camera"

// helperName is the helper's file name, installed next to the loupe binary.
const helperName = "loupe-camfeed"

// ErrNoHelper is a loupe with no loupe-camfeed beside it.
var ErrNoHelper = errors.New("no " + helperName + " next to loupe: build it with make camfeed")

// ErrNoCamera is a Mac without Handset Camera: Handset is not installed, or
// macOS has not been allowed to load its camera extension.
var ErrNoCamera = errors.New(Name + " is not on this Mac: install it from Handset")

// ErrBusy is Handset Camera already fed by another process on this Mac (a
// second loupe, say). The camera's sink serves one feeder, so this loupe's
// frames would be dropped unread; it says so rather than feed into nothing.
var ErrBusy = errors.New(Name + " is being fed by another process on this Mac")

// exitBusy is the status loupe-camfeed exits with when it finds the camera
// already fed by another process. It stays in step with tools/camfeed.
const exitBusy = 5

const (
	// startTimeout bounds how long a new helper may take to say whether it
	// found the camera.
	startTimeout = 10 * time.Second
	// missingBackoff is how long a "missing" answer is believed before the
	// next frame tries again, so a camera that is still registering (about
	// 15 s after it is allowed) is picked up without a helper per frame.
	missingBackoff = 3 * time.Second
	// busyBackoff is how long a "busy" exit is believed before another helper
	// is tried, so a second loupe neither feeds into nothing nor churns
	// helpers while the first still holds the camera.
	busyBackoff = 5 * time.Second
)

// idleTimeout stops a helper no frame has reached for this long, unless a
// frame is held. It holds the camera's input while it runs. A var so a test
// can shorten it.
var idleTimeout = time.Minute

// helperPath finds loupe-camfeed: $LOUPE_CAMFEED, then the file next to the
// running loupe (Handset bundles both in Contents/Helpers), then a
// checkout's .build, where `make camfeed` puts it. A var so a test can point
// it at a fake.
var helperPath = func() (string, error) {
	if p := os.Getenv("LOUPE_CAMFEED"); p != "" {
		return p, nil
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), helperName))
	}
	candidates = append(candidates, filepath.Join(".build", helperName))
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", ErrNoHelper
}

// Feed is the one feed into Handset Camera. Every AVD booted on the camera
// sees the same picture, so the last frame sent wins, from whichever device
// or client sent it.
type Feed struct {
	mu sync.Mutex
	// pending is the newest frame not yet written, or nil.
	pending []byte
	// stopPending asks the helper to show the camera's placeholder.
	stopPending bool
	// held is a still frame from Hold that stays up past idleTimeout, or nil.
	// It is sent again when the helper has exited, as an update of the
	// extension does.
	held []byte
	// running is whether the writer goroutine is alive.
	running bool
	// missingAt is when a helper last answered "missing".
	missingAt time.Time
	// busyAt is when a helper last exited because another process holds the
	// camera.
	busyAt time.Time
	wake   chan struct{}
}

// Default is the feed loupe's AVDs use.
var Default = &Feed{}

// Send queues frame, an encoded PNG or JPEG, replacing any frame not yet
// written. It returns once the frame is queued. It fails when there is no
// helper, or when the last helper found no Handset Camera a moment ago.
func (f *Feed) Send(frame []byte) error {
	if _, err := helperPath(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.missingAt.IsZero() && time.Since(f.missingAt) < missingBackoff {
		return ErrNoCamera
	}
	if !f.busyAt.IsZero() && time.Since(f.busyAt) < busyBackoff {
		return ErrBusy
	}
	f.pending, f.stopPending, f.held = frame, false, nil
	f.kick()
	return nil
}

// Hold is Send for a still image: the camera keeps showing frame until the
// next Send, Hold or Stop, where a sent frame gives way to the placeholder
// once no frame has come for idleTimeout.
func (f *Feed) Hold(frame []byte) error {
	if err := f.Send(frame); err != nil {
		return err
	}
	f.mu.Lock()
	f.held = frame
	f.mu.Unlock()
	return nil
}

// Stop has the camera show its placeholder. Nothing running is not an error.
func (f *Feed) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = nil
	if !f.running {
		return
	}
	f.pending, f.stopPending = nil, true
	f.kick()
}

// kick wakes the writer, starting it if need be. f.mu is held.
func (f *Feed) kick() {
	if f.wake == nil {
		f.wake = make(chan struct{}, 1)
	}
	select {
	case f.wake <- struct{}{}:
	default:
	}
	if !f.running {
		f.running = true
		go f.run()
	}
}

// helper is one running loupe-camfeed.
type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// exited is closed when the process has exited, after waitErr is set.
	exited chan struct{}
	// waitErr is the process's exit, read only after exited is closed.
	waitErr error
}

// done reports whether the process has exited.
func (h *helper) done() bool {
	select {
	case <-h.exited:
		return true
	default:
		return false
	}
}

// busy reports whether the helper has exited because another process holds the
// camera. It is read only after the helper has exited.
func (h *helper) busy() bool {
	var ee *exec.ExitError
	return errors.As(h.waitErr, &ee) && ee.ExitCode() == exitBusy
}

// noteExit records an exited helper's reason so Send can report it.
func (f *Feed) noteExit(h *helper) {
	if h.busy() {
		f.mu.Lock()
		f.busyAt = time.Now()
		f.mu.Unlock()
		log.Printf("camfeed: %v", ErrBusy)
	}
}

// run writes queued frames to a helper, starting one when there is none,
// until no frame has come for idleTimeout and none is held.
func (f *Feed) run() {
	var h *helper
	defer func() {
		if h != nil {
			h.close()
		}
	}()
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-f.wake:
		case <-idle.C:
			f.mu.Lock()
			if f.pending == nil && !f.stopPending {
				if f.held == nil {
					f.running = false
					f.mu.Unlock()
					return
				}
				if h == nil || h.done() {
					f.pending = f.held
				}
			}
			f.mu.Unlock()
		}
		idle.Reset(idleTimeout)

		f.mu.Lock()
		frame, stop := f.pending, f.stopPending
		f.pending, f.stopPending = nil, false
		f.mu.Unlock()
		if frame == nil && !stop {
			continue
		}
		if h != nil {
			select {
			case <-h.exited:
				f.noteExit(h)
				h = nil
			default:
			}
		}
		if h == nil {
			if stop {
				continue
			}
			var err error
			h, err = startHelper()
			if errors.Is(err, ErrNoCamera) {
				f.mu.Lock()
				f.missingAt = time.Now()
				f.mu.Unlock()
				continue
			}
			if err != nil {
				log.Printf("camfeed: %v", err)
				continue
			}
			f.mu.Lock()
			f.missingAt = time.Time{}
			f.mu.Unlock()
		}
		if err := h.write(frame); err != nil {
			// The helper exited: the camera went away with an update of the
			// extension, or another process holds it (noteExit). The next
			// frame starts another, unless Send is backing off.
			log.Printf("camfeed: helper gone: %v", err)
			h.close()
			f.noteExit(h)
			h = nil
		}
	}
}

// startHelper starts loupe-camfeed and waits for its first line: "ready", or
// "missing" when the Mac has no Handset Camera.
func startHelper() (*helper, error) {
	path, err := helperPath()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(), "LOUPE_CAMFEED_NAME="+CameraName())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = logWriter{}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", path, err)
	}
	h := &helper{cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		first := true
		for sc.Scan() {
			if first {
				lines <- sc.Text()
				first = false
				continue
			}
			log.Printf("camfeed: %s", sc.Text())
		}
		if first {
			close(lines)
		}
	}()
	go func() { h.waitErr = cmd.Wait(); close(h.exited) }()

	select {
	case line, ok := <-lines:
		switch {
		case ok && line == "ready":
			return h, nil
		case ok && line == "missing":
			h.close()
			return nil, ErrNoCamera
		default:
			h.close()
			return nil, fmt.Errorf("%s said %q, not ready", path, line)
		}
	case <-time.After(startTimeout):
		h.close()
		return nil, fmt.Errorf("%s did not answer in %v", path, startTimeout)
	}
}

// write sends one frame, or a stop for a nil frame.
func (h *helper) write(frame []byte) error {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(frame)))
	if _, err := h.stdin.Write(n[:]); err != nil {
		return err
	}
	_, err := h.stdin.Write(frame)
	return err
}

// close ends the helper: closing stdin has it stop the camera's input and
// exit; one that does not within a second is killed.
func (h *helper) close() {
	h.stdin.Close()
	select {
	case <-h.exited:
	case <-time.After(time.Second):
		h.cmd.Process.Kill()
		<-h.exited
	}
}

// CameraName is the camera the helper feeds and AVDs are routed to: Name,
// unless $LOUPE_CAMFEED_NAME names another extension's camera for a test.
func CameraName() string {
	if n := os.Getenv("LOUPE_CAMFEED_NAME"); n != "" {
		return n
	}
	return Name
}

// logWriter passes the helper's stderr to loupe's log, a line at a time.
type logWriter struct{}

func (logWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line != "" {
			log.Printf("camfeed: %s", line)
		}
	}
	return len(b), nil
}
