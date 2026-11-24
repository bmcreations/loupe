package android

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/target/ios"

	"github.com/bmcreations/loupe/internal/target"
)

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
// same either way. A tag or text filter is applied here to what logcat
// prints, never passed to it.
func (t *Device) Logcat(ctx context.Context, q target.LogcatQuery) (*target.Logcat, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", target.ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	if q.Format != "" {
		return nil, fmt.Errorf("%w: logcat has no %s format", ios.ErrLogQueryUnsupported, q.Format)
	}
	pid, pkg, err := t.logcatPID(ctx, q)
	if err != nil {
		return nil, err
	}
	if q.Filtered() && q.Since.IsZero() && !q.Follow {
		b, cut, err := t.filteredDump(ctx, q, pid)
		if err != nil {
			return nil, err
		}
		return &target.Logcat{PID: pid, Package: pkg, Cut: cut, ReadCloser: io.NopCloser(bytes.NewReader(b))}, nil
	}
	rc, err := t.adb.ExecOutStream(ctx, t.info.Serial, logcatArgs(q, pid)...)
	if err != nil {
		return nil, err
	}
	rc = target.FilterLog(rc, target.NewLogFilter(q, parseThreadtime))
	return &target.Logcat{PID: pid, Package: pkg, ReadCloser: rc}, nil
}

const (
	// firstLogWindow is how many of the newest lines a filtered dump reads
	// first. adb and logcat take about 3 s to start on a phone with large
	// buffers whatever the count, and 10000 lines add a fraction of that.
	firstLogWindow = 10000
	// maxLogWindowGrowth bounds each window against the one before, for
	// a read too fast to time, such as an emulator's.
	maxLogWindowGrowth = 64
)

// filteredDump is the last q.Lines lines a tag or text filter keeps from
// the log, for a dump with no since.
//
// The whole buffer can take longer to come back over adb than a dump is
// given: a phone with 16 MiB main and system buffers took 33 s to print its
// 366 MB. So it reads the newest lines first, with -t, and widens
// while those hold fewer than q.Lines matches, sizing each window from how
// fast the last came back so it ends before ctx's deadline. A window that
// comes back with fewer lines than it asked for was the whole buffer. When
// the deadline cuts a wider window, or the next would not fit, the last
// finished window is the answer, which may be nothing, with
// target.LogCutTime.
//
// A pid narrows the read on the device, and that usually brings the whole
// buffer back in time, so with one that is tried first, with half of what
// is left of the deadline, and windows follow only when it does not finish.
func (t *Device) filteredDump(ctx context.Context, q target.LogcatQuery, pid int) ([]byte, string, error) {
	if pid > 0 {
		b, done, err := t.wholeFiltered(ctx, q, pid)
		if done {
			return b, "", err
		}
	}
	n := max(firstLogWindow, q.Lines)
	var best []byte
	for i := 0; ; i++ {
		start := time.Now()
		rc, err := t.adb.ExecOutStream(ctx, t.info.Serial, logcatWindowArgs(n, pid)...)
		if err != nil {
			return nil, "", err
		}
		raw := &countedLog{src: rc, start: start}
		lines, err := target.LastLines(target.FilterLog(raw, target.NewLogFilter(q, parseThreadtime)), q.Lines)
		if err != nil {
			if i > 0 && ctx.Err() != nil {
				return best, target.LogCutTime, nil
			}
			return nil, "", err
		}
		best = target.JoinLines(lines)
		if len(lines) >= q.Lines || raw.lines < n {
			return best, "", nil
		}
		next, ok := nextLogWindow(ctx, n, raw.lines, raw.first, time.Since(start))
		if !ok {
			return best, target.LogCutTime, nil
		}
		n = next
	}
}

// wholeFiltered reads all of pid's lines, with half of what is left of
// ctx. done is false when that half ran out and ctx did not.
func (t *Device) wholeFiltered(ctx context.Context, q target.LogcatQuery, pid int) ([]byte, bool, error) {
	wctx := ctx
	if dl, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		wctx, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(dl)/2))
		defer cancel()
	}
	rc, err := t.adb.ExecOutStream(wctx, t.info.Serial, logcatArgs(q, pid)...)
	if err != nil {
		return nil, true, err
	}
	lines, err := target.LastLines(target.FilterLog(rc, target.NewLogFilter(q, parseThreadtime)), q.Lines)
	if err != nil {
		return nil, wctx.Err() == nil || ctx.Err() != nil, err
	}
	return target.JoinLines(lines), true, nil
}

// nextLogWindow is how many lines the window after one of n can ask for and
// still end within three quarters of what is left of ctx, and false when
// that is no wider. That one printed got lines, its first byte after ttfb
// and its last after took. The start-up is paid again on every window and
// the rest grows with the lines, and -t N can print more than N lines, so
// the next is sized at the rate that one printed them.
func nextLogWindow(ctx context.Context, n, got int, ttfb, took time.Duration) (int, bool) {
	next := n * maxLogWindowGrowth
	if dl, ok := ctx.Deadline(); ok {
		plan := time.Until(dl)*3/4 - ttfb
		if plan <= 0 {
			return 0, false
		}
		perLine := float64(took-ttfb) / float64(got) * max(1, float64(got)/float64(n))
		if fit := float64(plan) / perLine; perLine > 0 && fit < float64(next) {
			next = int(fit)
		}
	}
	return next, next > n
}

// logcatWindowArgs reads the newest n lines, of pid's alone when it is set.
func logcatWindowArgs(n, pid int) []string {
	args := []string{"logcat", "-v", "threadtime", "-t", strconv.Itoa(n)}
	if pid > 0 {
		args = append(args, "--pid="+strconv.Itoa(pid))
	}
	return args
}

// countedLog counts the lines logcat printed before a filter drops any, and
// notes when the first byte came.
type countedLog struct {
	src   io.ReadCloser
	start time.Time
	first time.Duration
	lines int
}

func (c *countedLog) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	if n > 0 && c.first == 0 {
		c.first = time.Since(c.start)
	}
	c.lines += bytes.Count(p[:n], []byte{'\n'})
	return n, err
}

func (c *countedLog) Close() error { return c.src.Close() }

// logcatPID resolves the process the query names to a pid.
func (t *Device) logcatPID(ctx context.Context, q target.LogcatQuery) (int, string, error) {
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
func (t *Device) pidOf(ctx context.Context, pkg string) (int, error) {
	out, err := t.adb.Shell(ctx, t.info.Serial, "pidof", shellQuote(pkg))
	fields := strings.Fields(out)
	if len(fields) == 0 {
		// pidof exits 1 with nothing printed for a name with no process,
		// which is the ordinary "not running" rather than a failure.
		return 0, fmt.Errorf("%w: %s", target.ErrAppNotRunning, pkg)
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
//
// A dump with a tag or text filter and no start time reads -d here only
// for a pid, as filteredDump's first try, since the last N lines may hold no
// match; filteredDump reads the rest in windows. A follow with one starts
// from the last N lines unfiltered and keeps those that match.
func logcatArgs(q target.LogcatQuery, pid int) []string {
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
	case q.Filtered():
		args = append(args, "-d")
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

	return target.StartStream(ctx, "adb exec-out "+strings.Join(args, " "), nil, a.bin(), full...)
}

// parseThreadtime reads a threadtime line,
// "11-24 07:50:01.123  1234  1250 I ReactNativeJS: msg", for LogFilter.
// logcat pads a short tag to eight columns before its colon. It prints every
// line of a multi-line message with a header of its own, so no line
// continues another, and a buffer marker ("--------- beginning of main")
// has no tag.
func parseThreadtime(line []byte) target.LogLine {
	if len(line) < 19 || line[0] < '0' || line[0] > '9' || line[2] != '-' || line[5] != ' ' {
		return target.LogLine{}
	}
	rest := line
	for i := 0; i < 4; i++ { // date, time, pid, tid
		rest = bytes.TrimLeft(rest, " ")
		sp := bytes.IndexByte(rest, ' ')
		if sp < 0 {
			return target.LogLine{}
		}
		rest = rest[sp:]
	}
	rest = bytes.TrimLeft(rest, " ")
	if len(rest) < 2 || rest[1] != ' ' { // the level letter
		return target.LogLine{}
	}
	tag, msg, ok := bytes.Cut(rest[2:], []byte(": "))
	if !ok {
		if tag, ok = bytes.CutSuffix(rest[2:], []byte(":")); !ok {
			return target.LogLine{}
		}
	}
	return target.LogLine{Tags: []string{string(bytes.TrimRight(tag, " "))}, Message: msg}
}
