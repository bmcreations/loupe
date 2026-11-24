package android

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

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
// same either way.
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
	rc, err := t.adb.ExecOutStream(ctx, t.info.Serial, logcatArgs(q, pid)...)
	if err != nil {
		return nil, err
	}
	return &target.Logcat{PID: pid, Package: pkg, ReadCloser: rc}, nil
}

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
