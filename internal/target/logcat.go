package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
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

// packageNameRE is Android's own rule for a package name: two or more
// dot-separated segments, each starting with a letter. It is checked before
// the name reaches pidof so a request cannot carry anything else to the
// device's shell, on top of the quoting.
var packageNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)

// ValidPackageName reports whether s is shaped like an Android package name.
func ValidPackageName(s string) bool { return packageNameRE.MatchString(s) }

// Logcat opens the device log through exec-out, which keeps stdout apart
// from stderr and passes the bytes through untranslated.
//
// No value from the request reaches the device's shell as text: the pid and
// the time are formatted here as digits, and a package name goes to pidof
// only after ValidPackageName and through shellQuote. adb 35 escapes each
// exec-out argument itself and older clients do not, and digits read the
// same either way.
func (t *ADBTarget) Logcat(ctx context.Context, q LogcatQuery) (*Logcat, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	if q.Format != "" {
		return nil, fmt.Errorf("%w: logcat has no %s format", ErrLogQueryUnsupported, q.Format)
	}
	pid, pkg, err := t.logcatPID(ctx, q)
	if err != nil {
		return nil, err
	}
	rc, err := t.adb.ExecOutStream(ctx, t.info.Serial, logcatArgs(q, pid)...)
	if err != nil {
		return nil, err
	}
	return &Logcat{PID: pid, Package: pkg, ReadCloser: rc}, nil
}

// logcatPID resolves the process the query names to a pid.
func (t *ADBTarget) logcatPID(ctx context.Context, q LogcatQuery) (int, string, error) {
	switch {
	case q.PID > 0:
		return q.PID, "", nil
	case q.Foreground:
		app, err := t.ForegroundApp(ctx)
		if err != nil {
			return 0, "", err
		}
		// ForegroundApp leaves PID zero when pidof printed more than one,
		// so that case asks again and takes the first.
		if app.PID > 0 {
			return app.PID, app.Package, nil
		}
		pid, err := t.pidOf(ctx, app.Package)
		return pid, app.Package, err
	case q.Package != "":
		if !ValidPackageName(q.Package) {
			return 0, "", fmt.Errorf("%q is not a package name", q.Package)
		}
		pid, err := t.pidOf(ctx, q.Package)
		return pid, q.Package, err
	}
	return 0, "", nil
}

// pidOf is the first pid pidof prints for pkg. A package's main process is
// named exactly the package, so its other processes (pkg:remote) do not
// match; more than one line means the same name twice, and the first is
// kept.
func (t *ADBTarget) pidOf(ctx context.Context, pkg string) (int, error) {
	out, err := t.adb.Shell(ctx, t.info.Serial, "pidof", shellQuote(pkg))
	fields := strings.Fields(out)
	if len(fields) == 0 {
		// pidof exits 1 with nothing printed for a name with no process,
		// which is the ordinary "not running" rather than a failure.
		return 0, fmt.Errorf("%w: %s", ErrAppNotRunning, pkg)
	}
	pid, perr := strconv.Atoi(fields[0])
	if perr != nil {
		if err != nil {
			return 0, fmt.Errorf("pidof %s: %w: %s", pkg, err, strings.TrimSpace(out))
		}
		return 0, fmt.Errorf("pidof %s: %q", pkg, strings.TrimSpace(out))
	}
	return pid, nil
}

// logcatArgs builds the logcat command line. threadtime is logcat's default
// format, named so a device configured otherwise cannot change what a
// caller parses.
//
// -t N implies -d and -T does not, so a dump with a start time needs -d
// said outright, and a follow with a line count uses -T N, the one form of
// "start N lines back" that keeps reading.
func logcatArgs(q LogcatQuery, pid int) []string {
	args := []string{"logcat", "-v", "threadtime"}
	switch {
	case !q.Since.IsZero():
		if !q.Follow {
			args = append(args, "-d")
		}
		// logcat's epoch form, seconds.millis, needs no quoting on any
		// adb, which its "MM-DD hh:mm:ss.mmm" form would.
		ms := q.Since.UnixMilli()
		args = append(args, "-T", fmt.Sprintf("%d.%03d", ms/1000, ms%1000))
	case q.Follow:
		args = append(args, "-T", strconv.Itoa(q.Lines))
	default:
		args = append(args, "-t", strconv.Itoa(q.Lines))
	}
	if pid > 0 {
		args = append(args, "--pid="+strconv.Itoa(pid))
	}
	return args
}

// ExecOutStream is ExecOut for output that does not end on its own: it
// returns stdout as it arrives. Closing the reader stops the command, and
// ctx ending stops it too.
func (a ADB) ExecOutStream(ctx context.Context, serial string, args ...string) (io.ReadCloser, error) {
	full := []string{}
	if serial != "" {
		full = append(full, "-s", serial)
	}
	full = append(full, "exec-out")
	full = append(full, args...)

	return startStream(ctx, "adb exec-out "+strings.Join(args, " "), nil, a.bin(), full...)
}

// startStream runs name with args and returns its stdout as it arrives,
// for ExecOutStream and a simulator's log. label names the command in
// errors. prep, when set, adjusts the command before it starts.
func startStream(ctx context.Context, label string, prep func(*exec.Cmd), name string, args ...string) (*execStream, error) {
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
	// noise drops stderr lines that say nothing about a failure.
	noise  func(line string) bool
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
	if s.noise == nil {
		return msg
	}
	var kept []string
	for _, line := range strings.Split(msg, "\n") {
		if !s.noise(line) {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
