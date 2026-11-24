package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Logcatter is implemented by targets with a log to read: an Android
// device's logcat, or a simulator's unified log. Synthetic has none, so
// this is deliberately not part of Target.
type Logcatter interface {
	Logcat(ctx context.Context, q LogcatQuery) (*Logcat, error)
}

// LogcatQuery narrows what Logcat returns. At most one of PID, Package and
// Foreground names a process; with none of them every process is read.
type LogcatQuery struct {
	// Since starts the log at a wall-clock time, read by the device's clock.
	// Zero means the last Lines lines instead.
	Since time.Time
	// Lines is how many of the most recent lines to start with when Since is
	// zero. With Since set, it is left to the caller to trim.
	Lines int
	PID   int
	// Package is resolved to its pid with pidof when the log is opened.
	Package string
	// Foreground resolves the app in front, the one /api/app reports.
	Foreground bool
	// Follow keeps the log open and streams new lines until the reader is
	// closed, rather than dumping what is buffered and ending.
	Follow bool
	// Format is "" for the target's own text, or LogFormatNDJSON for one
	// JSON object per line where the target can give one. A target that
	// cannot answers ErrLogQueryUnsupported.
	Format string
}

// Logcat is an open log. PID and Package say which process it was
// narrowed to, if any, so a caller that asked for the foreground app can
// tell which one it got.
type Logcat struct {
	PID     int
	Package string
	io.ReadCloser
}

// ErrAppNotRunning is a package that has no process to filter on. logcat
// filters by pid, and a package that is not running has none.
var ErrAppNotRunning = errors.New("app is not running")

// StartStream runs name with args and returns its stdout as it arrives,
// for ExecOutStream and a simulator's log. label names the command in
// errors. prep, when set, adjusts the command before it starts.
func StartStream(ctx context.Context, label string, prep func(*exec.Cmd), name string, args ...string) (*execStream, error) {
	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, name, args...)
	if prep != nil {
		prep(cmd)
	}
	s := &execStream{cmd: cmd, cancel: cancel, label: label}
	cmd.Stderr = &s.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	s.out = out
	return s, nil
}

type execStream struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	out    io.ReadCloser
	stderr bytes.Buffer
	label  string
	// Noise drops stderr lines that say nothing about a failure.
	Noise  func(line string) bool
	eof    bool
	closed bool
}

func (s *execStream) Read(p []byte) (int, error) {
	n, err := s.out.Read(p)
	if err == io.EOF {
		s.eof = true
	}
	return n, err
}

// Close stops the command and reports how it ended. A stream read to its
// end reports the command's own exit, with what it printed to stderr; one
// closed early, which is how a follow always ends, was stopped on purpose
// and is not an error.
func (s *execStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	defer s.cancel()
	if !s.eof {
		s.cancel()
		s.cmd.Wait()
		return nil
	}
	err := s.cmd.Wait()
	if err == nil {
		return nil
	}
	if msg := s.stderrText(); msg != "" {
		return fmt.Errorf("%s: %w: %s", s.label, err, msg)
	}
	return fmt.Errorf("%s: %w", s.label, err)
}

func (s *execStream) stderrText() string {
	msg := strings.TrimSpace(s.stderr.String())
	if s.Noise == nil {
		return msg
	}
	var kept []string
	for _, line := range strings.Split(msg, "\n") {
		if !s.Noise(line) {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
