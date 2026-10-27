package target

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrLogQueryUnsupported is a log request this target cannot answer as
// asked: a format it has no form of, or a start time on a log that is live
// only. The handler answers it 422 with the reason.
var ErrLogQueryUnsupported = errors.New("not supported on this target")

// LogFormatNDJSON asks for one JSON object per entry, the unified log's
// own fields, where the target has them.
const LogFormatNDJSON = "ndjson"

var bundleIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,254}$`)

// ValidBundleID reports whether id can be an Apple bundle ID. Unlike an
// Android package it may hold a hyphen. It never reaches a shell: a
// simulator compares it against launchctl's labels on the host.
func ValidBundleID(id string) bool { return bundleIDPattern.MatchString(id) }

// simLogWindows are the --last windows a lines-only dump tries in turn,
// stopping at the first that holds enough lines. log show with no window
// reads the whole store, and a streamed simulator logs fast: on loupe-probe
// 10m was 545k lines in 10.1s, so 1h does not fit a 15s dump on a busy one,
// while a quiet one gets through all three quickly because quiet is fast.
var simLogWindows = []time.Duration{time.Minute, 10 * time.Minute, time.Hour}

// simLogStart runs `log` inside the simulator. A var so tests can stand in
// for simctl.
var simLogStart = func(ctx context.Context, udid string, args []string) (io.ReadCloser, error) {
	full := append([]string{"simctl", "spawn", udid, "log"}, args...)
	s, err := startStream(ctx, "simctl spawn log "+args[0], func(cmd *exec.Cmd) {
		// SIGTERM rather than the default SIGKILL, so simctl can pass the
		// end on to the log process it started inside the simulator.
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 2 * time.Second
	}, "xcrun", full...)
	if err != nil {
		return nil, err
	}
	// log prints this for every run under simctl spawn, success or not, and
	// it would lead every error.
	s.noise = func(line string) bool { return strings.HasPrefix(line, "getpwuid_r did not find a match") }
	return s, nil
}

// Logcat implements Logcatter with the simulator's unified log, through
// `log show` for a dump and `log stream` for a follow. The text form is
// `--style compact`; ?format=ndjson is `--style ndjson`.
//
// A follow is live from the moment it opens: log stream has no backlog,
// and a show stitched in front of it would lose whatever was logged while
// the show ran, so lines and since do not apply to it, and since is
// refused rather than ignored.
func (s *Simulator) Logcat(ctx context.Context, q LogcatQuery) (*Logcat, error) {
	if err := s.requireBooted(); err != nil {
		return nil, err
	}
	if q.Follow && !q.Since.IsZero() {
		return nil, fmt.Errorf("%w: a simulator's follow is live from now; since applies only without follow", ErrLogQueryUnsupported)
	}
	pid, pkg, err := s.logProcess(ctx, q)
	if err != nil {
		return nil, err
	}
	out := &Logcat{PID: pid, Package: pkg}
	switch {
	case q.Follow:
		r, err := simLogStart(ctx, s.UDID(), simLogArgs(q, pid, 0))
		if err != nil {
			return nil, err
		}
		out.ReadCloser = newSimLogFilter(r, q.Format)
	case !q.Since.IsZero():
		// The handler trims a since dump to its lines.
		r, err := simLogStart(ctx, s.UDID(), simLogArgs(q, pid, 0))
		if err != nil {
			return nil, err
		}
		out.ReadCloser = newSimLogFilter(r, q.Format)
	default:
		b, err := simLogTail(ctx, s.UDID(), q, pid)
		if err != nil {
			return nil, err
		}
		out.ReadCloser = io.NopCloser(bytes.NewReader(b))
	}
	return out, nil
}

// logProcess resolves pid, package or foreground to a pid, as Android's
// does: an app with no process is ErrAppNotRunning rather than a log that
// is quietly empty.
func (s *Simulator) logProcess(ctx context.Context, q LogcatQuery) (int, string, error) {
	switch {
	case q.PID > 0:
		return q.PID, "", nil
	case q.Foreground:
		front, err := s.input().do(ctx, 0, "front")
		if err != nil {
			return 0, "", fmt.Errorf("foreground app: %w", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(front))
		if err != nil || pid < 1 {
			return 0, "", fmt.Errorf("foreground app: simcap answered %q", front)
		}
		list, _ := s.launchctlList(ctx)
		return pid, appForPid(list, strconv.Itoa(pid)), nil
	case q.Package != "":
		if !ValidBundleID(q.Package) {
			return 0, "", fmt.Errorf("%q is not a bundle ID", q.Package)
		}
		list, err := s.launchctlList(ctx)
		if err != nil {
			return 0, "", err
		}
		pid := pidForApp(list, q.Package)
		if pid == 0 {
			return 0, "", fmt.Errorf("%w: %s", ErrAppNotRunning, q.Package)
		}
		return pid, q.Package, nil
	}
	return 0, "", nil
}

func (s *Simulator) launchctlList(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "spawn", s.UDID(), "launchctl", "list").Output()
	if err != nil {
		return "", fmt.Errorf("launchctl list: %w", err)
	}
	return string(out), nil
}

// pidForApp is the pid launchctl lists for bundle, or 0 when it has none.
// An app launchd knows and is not running is listed with "-" for its pid.
func pidForApp(list, bundle string) int {
	for _, line := range strings.Split(list, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		label, ok := strings.CutPrefix(f[2], "UIKitApplication:")
		if !ok {
			continue
		}
		if i := strings.IndexByte(label, '['); i >= 0 {
			label = label[:i]
		}
		if label != bundle {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// simLogArgs builds the arguments to `log`. window is the --last of a
// lines-only dump and ignored otherwise.
//
// --start takes "YYYY-MM-DD HH:MM:SS" with an optional zone and no
// fractional seconds, so since is rounded down to the second; a dump with
// since is trimmed to its lines by the handler either way.
func simLogArgs(q LogcatQuery, pid int, window time.Duration) []string {
	style := "compact"
	if q.Format == LogFormatNDJSON {
		style = LogFormatNDJSON
	}
	var args []string
	switch {
	case q.Follow:
		args = []string{"stream", "--style", style}
	case !q.Since.IsZero():
		args = []string{"show", "--style", style, "--start", q.Since.Format("2006-01-02 15:04:05-0700")}
	default:
		args = []string{"show", "--style", style, "--last", fmt.Sprintf("%ds", int(window/time.Second))}
	}
	if pid > 0 {
		args = append(args, "--predicate", fmt.Sprintf("processID == %d", pid))
	}
	return args
}

// simLogTail is the last q.Lines entries, trying each of simLogWindows
// until one holds enough. It gives up widening when the next window,
// guessed from how long this one took, would not finish before ctx's
// deadline, and returns what it has.
//
// It counts lines, not entries, so a compact entry that runs over several
// lines (a dictionary printed whole) can lose its first lines at the cut.
// ndjson is one line per entry.
func simLogTail(ctx context.Context, udid string, q LogcatQuery, pid int) ([]byte, error) {
	var best []byte
	for i, w := range simLogWindows {
		start := time.Now()
		r, err := simLogStart(ctx, udid, simLogArgs(q, pid, w))
		if err != nil {
			return nil, err
		}
		lines, err := lastFiltered(newSimLogFilter(r, q.Format), q.Lines)
		if err != nil {
			if best != nil && ctx.Err() != nil {
				return best, nil
			}
			return nil, err
		}
		best = joinLines(lines)
		if len(lines) >= q.Lines || i+1 == len(simLogWindows) {
			return best, nil
		}
		took := time.Since(start)
		next := time.Duration(float64(took) * float64(simLogWindows[i+1]) / float64(w))
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < next {
			return best, nil
		}
	}
	return best, nil
}

// lastFiltered reads r to the end, keeping its last n lines, and closes it.
func lastFiltered(r io.ReadCloser, n int) ([]string, error) {
	ring := make([]string, 0, n)
	start := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if len(ring) < n {
			ring = append(ring, sc.Text())
			continue
		}
		ring[start] = sc.Text()
		start = (start + 1) % n
	}
	err := sc.Err()
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return append(ring[start:], ring[:start]...), nil
}

func joinLines(lines []string) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// simLogFilter drops what `log` prints around the entries: compact's
// column header, first on every run, empty or not, and ndjson's closing
// {"count":N,"finished":1}.
type simLogFilter struct {
	src    io.ReadCloser
	br     *bufio.Reader
	format string
	first  bool
	pend   []byte
}

func newSimLogFilter(r io.ReadCloser, format string) *simLogFilter {
	return &simLogFilter{src: r, br: bufio.NewReaderSize(r, 64<<10), format: format, first: true}
}

func (f *simLogFilter) Read(p []byte) (int, error) {
	for len(f.pend) == 0 {
		line, err := f.br.ReadBytes('\n')
		if len(line) > 0 && !f.drop(line) {
			f.pend = line
		}
		f.first = false
		if err != nil {
			if len(f.pend) > 0 {
				break
			}
			return 0, err
		}
	}
	n := copy(p, f.pend)
	f.pend = f.pend[n:]
	return n, nil
}

func (f *simLogFilter) drop(line []byte) bool {
	if f.format == LogFormatNDJSON {
		return bytes.HasPrefix(line, []byte(`{"count":`)) && bytes.Contains(line, []byte(`"finished":`))
	}
	return f.first && bytes.HasPrefix(line, []byte("Timestamp "))
}

func (f *simLogFilter) Close() error { return f.src.Close() }
