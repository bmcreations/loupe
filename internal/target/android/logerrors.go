package android

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// logErrorArgs is the logcat dump the error scan reads: the two tags the
// runtimes print their errors under, ReactNativeJS at E and above and
// flutter at I and above, since Flutter prints a framework error at info.
// The time is epoch micros, which a log error's id is made of, and uid
// names the app after its process is gone. Every argument is a constant or
// digits, so nothing needs quoting on any adb.
func logErrorArgs(since time.Time) []string {
	args := []string{"logcat", "-d", "-v", "threadtime", "-v", "epoch", "-v", "usec", "-v", "uid"}
	if !since.IsZero() {
		ms := since.UnixMilli()
		args = append(args, "-T", fmt.Sprintf("%d.%03d", ms/1000, ms%1000))
	}
	return append(args, "-s", "ReactNativeJS:E", "flutter:I")
}

// logcatEpochRE is a line of that dump,
// "   1795527034.063098 10236  5994  5994 E flutter : message": time, uid,
// pid, tid. A uid whose name is five letters or fewer, root or shell, comes
// as its name. The tag is padded to eight, and an empty message line loses
// the space after ':'.
var logcatEpochRE = regexp.MustCompile(`^\s*(\d+)\.(\d{6})\s+(\w+)\s+(\d+)\s+(\d+)\s+([VDIWEFA])\s+(ReactNativeJS|flutter)\s*:(?: (.*))?$`)

// logcatEntry is a message of the dump and the uid logcat printed for it.
type logcatEntry struct {
	target.LogEntry
	UID int
}

// parseLogErrorDump turns the dump into log entries. logcat prints each line
// of a message with the message's own header, so consecutive lines with the
// same time, uid, process, thread, level and tag are one message.
func parseLogErrorDump(out []byte) []logcatEntry {
	var (
		entries []logcatEntry
		key     string
		texts   []string
		raws    []string
		cur     logcatEntry
	)
	flush := func() {
		if len(texts) == 0 {
			return
		}
		cur.Text = strings.Join(texts, "\n")
		cur.Raw = strings.Join(raws, "\n")
		entries = append(entries, cur)
		texts, raws, key = nil, nil, ""
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		m := logcatEpochRE.FindStringSubmatch(line)
		if m == nil {
			// "--------- beginning of main" and anything else logcat says.
			flush()
			continue
		}
		k := strings.Join(m[1:8], " ")
		if k != key {
			flush()
			sec, _ := strconv.ParseInt(m[1], 10, 64)
			usec, _ := strconv.ParseInt(m[2], 10, 64)
			uid, _ := strconv.Atoi(m[3]) // 0 for a name, which no app has
			pid, _ := strconv.Atoi(m[4])
			cur = logcatEntry{LogEntry: target.LogEntry{
				Runtime: target.LogFlutter,
				Time:    time.Unix(sec, usec*1000).UTC(),
				PID:     pid,
				Error:   m[6] == "E" || m[6] == "F",
			}, UID: uid}
			if m[7] == "ReactNativeJS" {
				cur.Runtime = target.LogReactNative
			}
			key = k
		}
		texts = append(texts, m[8])
		raws = append(raws, line)
	}
	flush()
	return entries
}

// logErrors scans the device log for JS and Dart errors since since. The
// log is the main buffer, which held the last 22 minutes on an API 37
// emulator running two apps (256 KiB), so an error older than that is gone.
func (t *Device) logErrors(ctx context.Context, since time.Time) ([]target.LogError, error) {
	out, err := t.adb.ExecOut(ctx, t.info.Serial, logErrorArgs(since)...)
	if err != nil {
		return nil, fmt.Errorf("logcat: %w", err)
	}
	parsed := parseLogErrorDump(out)
	entries := make([]target.LogEntry, len(parsed))
	// An error's time and pid are its first entry's, which says its uid.
	type at struct {
		micros int64
		pid    int
	}
	uids := map[at]int{}
	for i, e := range parsed {
		entries[i] = e.LogEntry
		uids[at{e.Time.UnixMicro(), e.PID}] = e.UID
	}
	errs := target.ScanLogErrors(entries)
	if len(errs) == 0 {
		return nil, nil
	}
	var pkgs map[int]string
	if out, err := t.adb.ExecOut(ctx, t.info.Serial, "pm", "list", "packages", "-U"); err == nil {
		pkgs = parsePackageUIDs(out)
	}
	// The process a pid names, for as long as it lives.
	names := map[int]string{}
	if ps, err := t.adb.ExecOut(ctx, t.info.Serial, "ps", "-A", "-o", "PID,NAME"); err == nil {
		names = parsePSNames(ps)
	}
	for i := range errs {
		name := names[errs[i].PID]
		pkg, _, _ := strings.Cut(name, ":")
		if !ValidPackageName(pkg) {
			pkg = ""
		}
		switch uidPkg := pkgs[uids[at{errs[i].Time.UnixMicro(), errs[i].PID}]]; {
		case uidPkg == "":
			// A uid that several packages share, or none: ps's name is
			// all there is.
			errs[i].Process, errs[i].BundleID = name, pkg
		case uidPkg == pkg:
			errs[i].Process, errs[i].BundleID = name, pkg
		default:
			// The app has died and its pid is another process's now, or
			// it has none.
			errs[i].BundleID = uidPkg
		}
	}
	return errs, nil
}

// parsePackageUIDs reads `pm list packages -U`, a "package:<name> uid:<n>"
// line a package, into the package each uid names. A uid that packages
// share names none of them.
func parsePackageUIDs(out []byte) map[int]string {
	pkgs := map[int]string{}
	shared := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		name, uid, ok := strings.Cut(strings.TrimSpace(line), " uid:")
		name, found := strings.CutPrefix(name, "package:")
		if !ok || !found || !ValidPackageName(name) {
			continue
		}
		n, err := strconv.Atoi(uid)
		if err != nil {
			continue
		}
		if _, dup := pkgs[n]; dup {
			shared[n] = true
		}
		pkgs[n] = name
	}
	for n := range shared {
		delete(pkgs, n)
	}
	return pkgs
}

// parsePSNames reads `ps -A -o PID,NAME`: a header, then a pid and a name a
// line.
func parsePSNames(out []byte) map[int]string {
	names := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			names[pid] = f[1]
		}
	}
	return names
}

// logErrorReports is logErrors as crash rows.
func (t *Device) logErrorReports(ctx context.Context, since time.Time) ([]target.CrashReport, error) {
	errs, err := t.logErrors(ctx, since)
	if err != nil {
		return nil, err
	}
	rows := make([]target.CrashReport, len(errs))
	for i, e := range errs {
		rows[i] = e.Report()
	}
	return rows, nil
}

// logErrorText is one log error's lines as logcat printed them, found again
// by the time its id names.
func (t *Device) logErrorText(ctx context.Context, id string, at time.Time) ([]byte, error) {
	errs, err := t.logErrors(ctx, at.Add(-time.Millisecond))
	if err != nil {
		return nil, err
	}
	for _, e := range errs {
		if e.ID() == id {
			return []byte(e.Text + "\n"), nil
		}
	}
	return nil, target.ErrCrashNotFound
}
