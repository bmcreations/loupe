package target

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// AppInfo is implemented by targets that can report what is on screen right
// now. Synthetic cannot, so this is deliberately not part of Target.
type AppInfo interface {
	ForegroundApp(ctx context.Context) (*App, error)
}

// App describes the foreground activity. Label and an icon are left out of
// v1 -- the spec records the icon as a follow-up and the client shows a
// letter tile instead. VersionName, VersionCode, PID and APKPath are filled
// on a best-effort basis: only the resumed activity is required to build an
// App at all.
type App struct {
	Package     string `json:"package"`
	Activity    string `json:"activity"`
	VersionName string `json:"versionName,omitempty"`
	VersionCode int    `json:"versionCode,omitempty"`
	PID         int    `json:"pid,omitempty"`
	APKPath     string `json:"apkPath,omitempty"`
}

// ForegroundApp reads the resumed activity off the device, then fills in
// whatever else it can. Only the first call is required to succeed; a
// version, pid or path that cannot be read is left zero rather than failing
// the whole request, because a caller after "what is on screen" still gets an
// answer from a device where `pm path` is slow or `pidof` is missing.
func (t *ADBTarget) ForegroundApp(ctx context.Context) (*App, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	return newAndroidApp(t).ForegroundApp(ctx)
}

// androidApp is the pure logic behind ADBTarget's AppInfo, kept separate so
// it can be tested with a fake shellFunc instead of a device, the same shape
// androidSettings uses.
type androidApp struct{ sh shellFunc }

func newAndroidApp(t *ADBTarget) *androidApp {
	return &androidApp{sh: func(ctx context.Context, args ...string) (string, error) {
		return t.adb.Shell(ctx, t.info.Serial, args...)
	}}
}

// appScript is the single command ForegroundApp issues (trap 33: a separate
// adb shell per field is a separate loopback connection per field, and this
// endpoint historically issued four). Section 0 is the full activities dump,
// which Go parses authoritatively -- including resumedActivity's display-0
// scoping (trap 44) -- for the Package and Activity fields this endpoint
// promises.
//
// The follow-up commands need a package name to target, and there is no way
// to hand one from Go back into the same shell invocation, so PKG is
// resolved shell-side instead. It is scoped to display #0's own section with
// `sed -n "/^Display #0 /,/^Display #/p"` before the grep, the same display
// trap 44 required Go's own parse to scope to -- verified against
// emulator-5554's toybox sed, which supports this address-range form. That
// still leaves it weaker than the Go-side parse in one way: if display #0's
// header itself is missing or malformed, the sed range prints nothing and
// PKG resolves empty rather than falling back to an unscoped match the way
// resumedActivity does, so a device with no identifiable display #0 section
// gets no version/pid/apk instead of possibly-wrong ones. That is treated as
// the correct trade-off, not a limitation to fix: an unscoped shell-side
// match is exactly the bug this guards against.
//
// A PKG that resolves empty guards into a no-op branch rather than reaching
// `dumpsys package ""`, `pidof ""` or `pm path ""` -- `dumpsys package` with
// no argument dumps every installed package, measured at 3.4MB on
// emulator-5554, which is not a reasonable cost for a request whose Package
// and Activity fields are already fully answered by section 0.
func appScript(nonce string) string {
	var b strings.Builder
	b.WriteString(sectionEcho(nonce, 0))
	b.WriteString("; dumpsys activity activities\n")
	b.WriteString(`PKG=$(dumpsys activity activities | sed -n '/^Display #0 /,/^Display #/p' | grep -o 'topResumedActivity=ActivityRecord{[^}]*}' | head -1 | sed -E 's#.* u[0-9]+ ([^/]+)/.*#\1#')`)
	b.WriteString("\n")
	b.WriteString(`if [ -n "$PKG" ]; then` + "\n")
	b.WriteString("  " + sectionEcho(nonce, 1))
	b.WriteString(`; dumpsys package "$PKG"`)
	b.WriteString("\n  " + sectionEcho(nonce, 2))
	b.WriteString(`; pidof "$PKG"`)
	b.WriteString("\n  " + sectionEcho(nonce, 3))
	b.WriteString(`; pm path "$PKG"`)
	b.WriteString("\nelse\n  ")
	b.WriteString(sectionEcho(nonce, 1))
	b.WriteString("\n  ")
	b.WriteString(sectionEcho(nonce, 2))
	b.WriteString("\n  ")
	b.WriteString(sectionEcho(nonce, 3))
	b.WriteString("\nfi\n")
	b.WriteString(sectionEcho(nonce, 4))
	b.WriteString("\n")
	return b.String()
}

func (a *androidApp) ForegroundApp(ctx context.Context) (*App, error) {
	nonce, nerr := newNonce()
	if nerr != nil {
		return nil, fmt.Errorf("dumpsys activity activities: %w", nerr)
	}
	out, shErr := a.sh(ctx, appScript(nonce))
	sec, complete := splitSections(out, nonce, 4)
	if !complete {
		if shErr != nil {
			return nil, fmt.Errorf("dumpsys activity activities: %w", shErr)
		}
		return nil, fmt.Errorf("dumpsys activity activities: script did not complete")
	}
	dump, pdump, pidOut, pathOut := sec[0], sec[1], sec[2], sec[3]

	pkg, activity, ok := resumedActivity(dump)
	if !ok {
		return nil, fmt.Errorf("dumpsys activity activities: no resumed activity found")
	}
	app := &App{Package: pkg, Activity: activity}

	// version, pid and apkPath all come from the shell-side $PKG, which can
	// disagree with the Go-parsed pkg above (see appScript). packageVersion
	// matching a "Package [pkg]" block naming the same pkg is the one check
	// available without another round trip, so it gates all three together:
	// a version block that does not name pkg means $PKG named a different
	// app, and pid/apkPath came from that same wrong app.
	if name, code, ok := packageVersion(pdump, pkg); ok {
		app.VersionName = name
		app.VersionCode = code
		if pid, perr := strconv.Atoi(strings.TrimSpace(pidOut)); perr == nil {
			app.PID = pid
		}
		if apk := apkPath(pathOut); apk != "" {
			app.APKPath = apk
		}
	}

	return app, nil
}

// resumedActivityRE keys on the "topResumedActivity=" prefix, which appears
// exactly once per display. `dumpsys activity activities` also prints
// mLastPausedActivity and a per-task ActivityRecord{...} line for every
// backgrounded app, and their ordering relative to topResumedActivity is not
// guaranteed (trap 39's shape: a decoy that can sort first), so a parser
// matching the first bare ActivityRecord{...} would sometimes report a paused
// app instead.
var resumedActivityRE = regexp.MustCompile(`topResumedActivity=ActivityRecord\{\S+ u\d+ (\S+)/(\S+) t\d+\}`)

// displayHeaderRE finds each "Display #<n> (activities from top to
// bottom):" section header, which is what scopes a topResumedActivity match
// to the display it belongs to.
var displayHeaderRE = regexp.MustCompile(`(?m)^Display #(\d+) `)

// resumedActivity reads the package and activity of the top resumed activity
// out of `dumpsys activity activities`. A leading-dot activity (uiautomator's
// and dumpsys's shorthand for "relative to this package") is expanded to the
// full class name.
//
// A multi-display dump (trap 44's shape) prints one topResumedActivity per
// display, and the sections are not guaranteed to print in display order, so
// this scopes the match to display #0's own section -- the display scrcpy
// streams by default -- rather than taking whichever match sorts first in
// the raw text. If the dump carries no identifiable per-display sections at
// all, there is nothing to scope by, so it falls back to matching anywhere
// in the dump.
func resumedActivity(dump string) (pkg, activity string, ok bool) {
	if section, ok := displayZeroSection(dump); ok {
		if pkg, activity, ok := findResumedActivity(section); ok {
			return pkg, activity, true
		}
	}
	return findResumedActivity(dump)
}

// displayZeroSection returns the text from "Display #0 ..." up to the next
// "Display #" header (or the end of the dump), and whether a display #0
// header was found at all.
func displayZeroSection(dump string) (string, bool) {
	locs := displayHeaderRE.FindAllStringSubmatchIndex(dump, -1)
	for i, loc := range locs {
		id := dump[loc[2]:loc[3]]
		if id != "0" {
			continue
		}
		start := loc[0]
		end := len(dump)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		return dump[start:end], true
	}
	return "", false
}

func findResumedActivity(dump string) (pkg, activity string, ok bool) {
	m := resumedActivityRE.FindStringSubmatch(dump)
	if m == nil {
		return "", "", false
	}
	pkg = m[1]
	activity = m[2]
	if strings.HasPrefix(activity, ".") {
		activity = pkg + activity
	}
	return pkg, activity, true
}

var (
	packageBlockRE = regexp.MustCompile(`(?m)^\s*Package \[[^\]]*\]`)
	versionCodeRE  = regexp.MustCompile(`versionCode=(\d+)`)
	versionNameRE  = regexp.MustCompile(`versionName=(\S+)`)
)

// packageVersion reads versionName and versionCode out of the "Package
// [pkg]" block in `dumpsys package <pkg>` that names pkg specifically --
// not merely the first "Package [...]" block, which can belong to a
// different package than the one asked for if the shell-side $PKG that
// produced this dump (see appScript) disagreed with pkg. An updated system
// app also prints a second block under "Hidden system packages:" carrying
// the factory version it shipped with (trap 39's shape again), so this
// stops at the next "Package [" line of any name rather than scanning the
// whole dump for the fields.
func packageVersion(dump, pkg string) (name string, code int, ok bool) {
	header := regexp.MustCompile(`(?m)^\s*Package \[` + regexp.QuoteMeta(pkg) + `\]`)
	loc := header.FindStringIndex(dump)
	if loc == nil {
		return "", 0, false
	}
	start := loc[1]
	end := len(dump)
	if next := packageBlockRE.FindStringIndex(dump[start:]); next != nil {
		end = start + next[0]
	}
	block := dump[start:end]

	cm := versionCodeRE.FindStringSubmatch(block)
	nm := versionNameRE.FindStringSubmatch(block)
	if cm == nil || nm == nil {
		return "", 0, false
	}
	code, err := strconv.Atoi(cm[1])
	if err != nil {
		return "", 0, false
	}
	return nm[1], code, true
}

// apkPath strips the "package:" prefix `pm path` puts on every line, and
// keeps only the first. A split APK prints one line per split; the first is
// always the base.
func apkPath(out string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return strings.TrimPrefix(strings.TrimSpace(first), "package:")
}
