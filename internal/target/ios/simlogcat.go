package ios

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bmcreations/loupe/internal/target"
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
	s, err := target.StartStream(ctx, "simctl spawn log "+args[0], func(cmd *exec.Cmd) {
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
	s.Noise = func(line string) bool { return strings.HasPrefix(line, "getpwuid_r did not find a match") }
	return s, nil
}

// Logcat implements Logcatter with the simulator's unified log, through
// `log show` for a dump and `log stream` for a follow. The text form is
// `--style compact`; ?format=ndjson is `--style ndjson`. A tag or text is
// matched here against what `log` prints, never put in its predicate.
//
// A follow is live from the moment it opens: log stream has no backlog,
// and a show stitched in front of it would lose whatever was logged while
// the show ran, so lines and since do not apply to it, and since is
// refused rather than ignored.
func (s *Simulator) Logcat(ctx context.Context, q target.LogcatQuery) (*target.Logcat, error) {
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
	out := &target.Logcat{PID: pid, Package: pkg}
	switch {
	case q.Follow:
		r, err := simLogStart(ctx, s.UDID(), simLogArgs(q, pid, 0))
		if err != nil {
			return nil, err
		}
		out.ReadCloser = simLogReader(r, q)
	case !q.Since.IsZero():
		// The handler trims a since dump to its lines.
		r, err := simLogStart(ctx, s.UDID(), simLogArgs(q, pid, 0))
		if err != nil {
			return nil, err
		}
		out.ReadCloser = simLogReader(r, q)
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
func (s *Simulator) logProcess(ctx context.Context, q target.LogcatQuery) (int, string, error) {
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
			return 0, "", fmt.Errorf("%w: %s", target.ErrAppNotRunning, q.Package)
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
//
// --info brings in the Info lines show and stream both leave out by
// default; React Native logs console.log at Info. Debug stays out: the store
// keeps almost none of it, so a dump would gain next to nothing, and on one
// React Native dev build's pid over 20s it took a follow from 1,788 entries
// to 4,591, every added one from Apple's frameworks.
func simLogArgs(q target.LogcatQuery, pid int, window time.Duration) []string {
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
	args = append(args, "--info")
	if pid > 0 {
		args = append(args, "--predicate", fmt.Sprintf("processID == %d", pid))
	}
	return args
}

// simLogTail is the last q.Lines entries, trying each of simLogWindows
// until one holds enough. With a tag or text, it counts the lines that
// match, so a sparse tag can widen to the last window. It gives up widening when the next window,
// guessed from how long this one took, would not finish before ctx's
// deadline, and returns what it has.
//
// It counts lines, not entries, so a compact entry that runs over several
// lines (a dictionary printed whole) can lose its first lines at the cut.
// ndjson is one line per entry.
func simLogTail(ctx context.Context, udid string, q target.LogcatQuery, pid int) ([]byte, error) {
	var best []byte
	for i, w := range simLogWindows {
		start := time.Now()
		r, err := simLogStart(ctx, udid, simLogArgs(q, pid, w))
		if err != nil {
			return nil, err
		}
		lines, err := lastFiltered(simLogReader(r, q), q.Lines)
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

// simLogReader is what `log` printed, less what simLogFilter drops and
// what q's tag and text do not match.
func simLogReader(r io.ReadCloser, q target.LogcatQuery) io.ReadCloser {
	parse := parseCompact
	if q.Format == LogFormatNDJSON {
		parse = parseNDJSON
	}
	return target.FilterLog(newSimLogFilter(r, q.Format), target.NewLogFilter(q, parse))
}

// parseCompact reads a `log --style compact` line for LogFilter:
//
//	2026-11-24 07:45:56.764 Df locationd[33406:2f24689] [com.apple.locationd:QA] msg
//	2026-11-24 07:45:59.978 F  backboardd[33403:29d7a27] (IOSurface) msg
//	2026-11-24 07:47:45.253 Df launchd_sim[37327:2f34f37] msg
//
// After the process comes the entry's [subsystem:category]. With no
// subsystem, its sender's image comes in parentheses, or nothing does when
// the sender is the process's own binary, and then the process is the tag,
// as ndjson's senderImagePath gives it. A category may hold spaces, and
// launchd's subsystems hold brackets and spaces of their own, but no
// subsystem seen holds a colon, so the first colon ends it. A bracket or
// parenthesis that does not close as one of those starts the process's own
// message. A line with no timestamp continues the entry above.
func parseCompact(line []byte) target.LogLine {
	if !compactStamp(line) {
		return target.LogLine{Continues: true, Message: line}
	}
	proc, rest, ok := afterCompactProcess(line[len("2026-11-24 07:45:56.764"):])
	if !ok {
		return target.LogLine{}
	}
	switch {
	case bytes.HasPrefix(rest, []byte("[")):
		sub, after, ok := bytes.Cut(rest[1:], []byte(":"))
		if !ok || bytes.Contains(sub, []byte("] ")) {
			break
		}
		cat, msg, ok := cutToken(after, ']')
		if !ok {
			break
		}
		return target.LogLine{Tags: subsystemTags(string(sub), string(cat)), Message: msg}
	case bytes.HasPrefix(rest, []byte("(")):
		sender, msg, ok := cutToken(rest[1:], ')')
		if !ok {
			break
		}
		return target.LogLine{Tags: []string{string(sender)}, Message: msg}
	}
	return target.LogLine{Tags: []string{string(proc)}, Message: rest}
}

// compactStamp reports whether line starts "YYYY-MM-DD hh:mm:ss.mmm ".
func compactStamp(line []byte) bool {
	const shape = "0000-00-00 00:00:00.000 "
	if len(line) < len(shape) {
		return false
	}
	for i := 0; i < len(shape); i++ {
		if shape[i] == '0' {
			if line[i] < '0' || line[i] > '9' {
				return false
			}
		} else if line[i] != shape[i] {
			return false
		}
	}
	return true
}

// afterCompactProcess reads the type and "Process[pid:tid] " that follow a
// compact timestamp, into the process's name and what comes after. A
// process name may hold spaces, so it ends at the first bracket that holds
// pid:tid.
func afterCompactProcess(b []byte) (proc, rest []byte, ok bool) {
	b = bytes.TrimLeft(b, " ")
	sp := bytes.IndexByte(b, ' ')
	if sp < 0 {
		return nil, nil, false
	}
	b = bytes.TrimLeft(b[sp:], " ")
	for i := 0; ; {
		open := bytes.IndexByte(b[i:], '[')
		if open < 0 {
			return nil, nil, false
		}
		open += i
		end := bytes.IndexByte(b[open+1:], ']')
		if end < 0 {
			return nil, nil, false
		}
		end += open + 1
		if pidTID(b[open+1 : end]) {
			return b[:open], bytes.TrimPrefix(b[end+1:], []byte(" ")), true
		}
		i = open + 1
	}
}

func pidTID(b []byte) bool {
	pid, tid, ok := bytes.Cut(b, []byte(":"))
	if !ok || len(pid) == 0 || len(tid) == 0 {
		return false
	}
	for _, c := range pid {
		if c < '0' || c > '9' {
			return false
		}
	}
	for _, c := range tid {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// cutToken splits b at the first close followed by a space or the end of
// the line, into the token before it and the message after.
func cutToken(b []byte, close byte) (token, msg []byte, ok bool) {
	for i := 0; i < len(b); i++ {
		if b[i] != close {
			continue
		}
		if i+1 == len(b) {
			return b[:i], nil, true
		}
		if b[i+1] == ' ' {
			return b[:i], b[i+2:], true
		}
	}
	return nil, nil, false
}

func subsystemTags(sub, cat string) []string {
	if cat == "" {
		return []string{sub}
	}
	return []string{sub, sub + ":" + cat}
}

// parseNDJSON reads a `log --style ndjson` entry for LogFilter, with the
// same tags parseCompact gives the entry: its subsystem and
// subsystem:category, or with no subsystem the base name of its sender's
// image. Text is matched against eventMessage alone, so a key or another
// field's value does not keep the line.
func parseNDJSON(line []byte) target.LogLine {
	var e struct {
		Subsystem       string `json:"subsystem"`
		Category        string `json:"category"`
		SenderImagePath string `json:"senderImagePath"`
		EventMessage    string `json:"eventMessage"`
	}
	if json.Unmarshal(line, &e) != nil {
		return target.LogLine{}
	}
	l := target.LogLine{Message: []byte(e.EventMessage)}
	switch {
	case e.Subsystem != "":
		l.Tags = subsystemTags(e.Subsystem, e.Category)
	case e.SenderImagePath != "":
		l.Tags = []string{path.Base(e.SenderImagePath)}
	}
	return l
}
