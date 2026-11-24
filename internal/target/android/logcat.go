package android

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
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
	if !q.Follow && (q.Filtered() || !q.Since.IsZero()) {
		b, cut, err := t.newestDump(ctx, q, pid)
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

// newestDump is the last q.Lines lines of a dump with a tag or text
// filter, a since, or both.
//
// The whole buffer, or the whole window since q.Since, can take longer to
// come back over adb than a dump is given: a phone with 16 MiB main and
// system buffers took 33 s to print its 366 MB, and 10 s to print the 91 MB
// of its last hour. So it reads the newest lines first, with -t, and widens
// while those hold fewer than q.Lines lines that the filter keeps and that
// were logged at or after since, sizing each window from how fast the last
// came back so it ends before ctx's deadline. A window that comes back with
// fewer lines than it asked for was the whole buffer, and one that reaches
// back past since holds every line it can give. When the deadline cuts a
// wider window, or the next would not fit, the last finished window is the
// answer, which may be nothing, with target.LogCutTime.
//
// With a since, a window that falls short of it also says how many lines
// reach it, at the rate that window went back. When those fit where a wider
// window would, the next read is every line from since on, with -d -T,
// which logcat clips at since itself: it reads no line before since, and it
// starts sooner, as that phone printed its last minute that way in 1.7 s
// and took 3.1 s over -t 5. That read is the whole answer when it finishes,
// and when the deadline cuts it, the window before it is, with
// target.LogCutTime.
//
// An unfiltered dump keeps every line after since, so its first window is
// q.Lines, and that one read always answers it.
//
// A pid narrows a filtered read on the device, and that usually brings the
// whole buffer, or the whole window since since, back in time, so with one
// that is tried first, with half of what is left of the deadline, and
// windows follow only when it does not finish. They do not end with the
// read from since on again.
func (t *Device) newestDump(ctx context.Context, q target.LogcatQuery, pid int) ([]byte, string, error) {
	triedSince := false
	if pid > 0 && q.Filtered() {
		b, done, err := t.wholeFiltered(ctx, q, pid)
		if done {
			return b, "", err
		}
		triedSince = true
	}
	n := q.Lines
	if q.Filtered() {
		n = max(firstLogWindow, q.Lines)
	}
	var best []byte
	for i := 0; ; i++ {
		start := time.Now()
		rc, err := t.adb.ExecOutStream(ctx, t.info.Serial, logcatWindowArgs(n, pid, !q.Since.IsZero())...)
		if err != nil {
			return nil, "", err
		}
		raw := &countedLog{src: rc, start: start}
		var log io.ReadCloser = raw
		var clip *sinceClip
		if !q.Since.IsZero() {
			clip = newSinceClip(raw, q.Since)
			log = clip
		}
		lines, err := target.LastLines(target.FilterLog(log, target.NewLogFilter(q, parseThreadtime)), q.Lines)
		if err != nil {
			if i > 0 && ctx.Err() != nil {
				return best, target.LogCutTime, nil
			}
			return nil, "", err
		}
		best = target.JoinLines(lines)
		if len(lines) >= q.Lines || raw.lines < n || clip.reachedSince() {
			return best, "", nil
		}
		next := nextLogWindow(ctx, n, raw.lines, raw.first, time.Since(start))
		if need := clip.linesToSince(raw.lines); need > 0 && need <= next && !triedSince {
			b, err := t.sinceFiltered(ctx, q, pid)
			if err != nil && ctx.Err() != nil {
				return best, target.LogCutTime, nil
			}
			return b, "", err
		}
		if next <= n {
			return best, target.LogCutTime, nil
		}
		n = next
	}
}

// sinceFiltered reads every line from q.Since on, of pid's alone when it is
// set, and keeps the last q.Lines that q's filter keeps.
func (t *Device) sinceFiltered(ctx context.Context, q target.LogcatQuery, pid int) ([]byte, error) {
	rc, err := t.adb.ExecOutStream(ctx, t.info.Serial, logcatArgs(q, pid)...)
	if err != nil {
		return nil, err
	}
	lines, err := target.LastLines(target.FilterLog(rc, target.NewLogFilter(q, parseThreadtime)), q.Lines)
	if err != nil {
		return nil, err
	}
	return target.JoinLines(lines), nil
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
// still end within three quarters of what is left of ctx, which may be no
// wider, or 0. That one printed got lines, its first byte after ttfb and its
// last after took. The start-up is paid again on every window and the rest
// grows with the lines, and -t N can print more than N lines, so the next is
// sized at the rate that one printed them.
func nextLogWindow(ctx context.Context, n, got int, ttfb, took time.Duration) int {
	next := n * maxLogWindowGrowth
	if dl, ok := ctx.Deadline(); ok {
		plan := time.Until(dl)*3/4 - ttfb
		if plan <= 0 {
			return 0
		}
		perLine := float64(took-ttfb) / float64(got) * max(1, float64(got)/float64(n))
		if fit := float64(plan) / perLine; perLine > 0 && fit < float64(next) {
			next = int(fit)
		}
	}
	return next
}

// logcatWindowArgs reads the newest n lines, of pid's alone when it is set.
// With stamped, each line's time carries its year and zone, which
// sinceClip reads and takes back out.
func logcatWindowArgs(n, pid int, stamped bool) []string {
	args := []string{"logcat", "-v", "threadtime"}
	if stamped {
		args = append(args, "-v", "year", "-v", "zone")
	}
	args = append(args, "-t", strconv.Itoa(n))
	if pid > 0 {
		args = append(args, "--pid="+strconv.Itoa(pid))
	}
	return args
}

// stampLayout is the time logcat's year and zone modifiers print, where
// threadtime alone prints "01-02 15:04:05.000".
const stampLayout = "2006-01-02 15:04:05.000 -0700"

// sinceClip drops the lines of a read with year and zone that were logged
// before since, and gives back the rest as plain threadtime, so a dump
// that reads newest first says what a dump since since would. A line with
// no time, such as "--------- beginning of main", passes through. It notes
// how far back the read went.
//
// A threadtime line's time is the device's local time with no year, so
// the read asks for both rather than guessing them: each line then names
// its own instant, across a change of year or of daylight saving time.
type sinceClip struct {
	src            io.ReadCloser
	br             *bufio.Reader
	since          time.Time
	pend           []byte
	oldest, newest time.Time
	past           bool // a line logged before since was read
}

func newSinceClip(src io.ReadCloser, since time.Time) *sinceClip {
	return &sinceClip{src: src, br: bufio.NewReaderSize(src, 64<<10), since: since}
}

func (c *sinceClip) Read(p []byte) (int, error) {
	for len(c.pend) == 0 {
		line, err := c.br.ReadBytes('\n')
		if len(line) > 0 {
			c.add(line)
		}
		if err != nil {
			if len(c.pend) > 0 {
				break
			}
			return 0, err
		}
	}
	n := copy(p, c.pend)
	c.pend = c.pend[n:]
	return n, nil
}

// add queues line unless it was logged before since, with its year and
// zone taken out: "2026-11-24 13:47:36.157 -0500  1828 ..." becomes
// "11-24 13:47:36.157  1828 ...".
func (c *sinceClip) add(line []byte) {
	if len(line) <= len(stampLayout) || line[len(stampLayout)] != ' ' {
		c.pend = append(c.pend, line...)
		return
	}
	at, err := time.Parse(stampLayout, string(line[:len(stampLayout)]))
	if err != nil {
		c.pend = append(c.pend, line...)
		return
	}
	if c.oldest.IsZero() || at.Before(c.oldest) {
		c.oldest = at
	}
	if at.After(c.newest) {
		c.newest = at
	}
	if at.Before(c.since) {
		c.past = true
		return
	}
	c.pend = append(c.pend, line[len("2006-"):len("2006-01-02 15:04:05.000")]...)
	c.pend = append(c.pend, line[len(stampLayout):]...)
}

func (c *sinceClip) Close() error { return c.src.Close() }

// reachedSince reports whether the read went back past since. A read with
// -t across buffers is not strictly in time order, so any line before
// since counts, not only the first.
func (c *sinceClip) reachedSince() bool { return c != nil && c.past }

// linesToSince is how many lines a read needs to reach since, at the rate
// this read's got lines went back, or 0 when that cannot be told.
func (c *sinceClip) linesToSince(got int) int {
	if c == nil {
		return 0
	}
	span := c.newest.Sub(c.oldest)
	if span <= 0 {
		return 0
	}
	need := float64(got) * float64(c.newest.Sub(c.since)) / float64(span)
	return int(min(need, math.MaxInt32))
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
// A dump with a tag or text filter, with or without a start time, reads -d
// here only for newestDump: for a pid as its first try, since the last N
// lines may hold no match, and with a start time once a window has shown
// the lines back to it fit. newestDump reads the rest in windows, as it
// does every dump with a start time. A follow with a filter starts from the
// last N lines unfiltered and keeps those that match.
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
