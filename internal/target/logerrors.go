package target

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A JavaScript error in a React Native dev build and an uncaught Dart error
// in Flutter leave the process running, so they write no crash report. They
// are found in the device log instead, by what each runtime prints for one,
// and listed with the crashes under their own kinds.
const (
	// KindJSError is an Error React Native reported: ExceptionsManager
	// passes a thrown Error to console.error, which logs "[Name: message]"
	// at error level, ReactNativeJS's E on Android and com.facebook.react.log
	// javascript on iOS.
	KindJSError = "jsError"
	// KindDartError is a Dart exception Flutter reported: one no handler
	// caught, which the engine logs as "Unhandled Exception", or one the
	// framework caught, which it prints between EXCEPTION CAUGHT BY and a
	// footer, then as "Another exception was thrown" for each after it.
	KindDartError = "dartError"
)

// LogErrorIDRE is a log error's id: its kind and the unix microseconds of
// its first line, "jsError-1795524584382140".
var LogErrorIDRE = regexp.MustCompile(`^(jsError|dartError)-\d{13,17}$`)

// LogErrorTime is the time a log error's id names.
func LogErrorTime(id string) (time.Time, bool) {
	if !LogErrorIDRE.MatchString(id) {
		return time.Time{}, false
	}
	us, err := strconv.ParseInt(id[strings.LastIndexByte(id, '-')+1:], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMicro(us).UTC(), true
}

// LogRuntime is the runtime a log entry came from, which decides the rules
// it is read by.
type LogRuntime int

const (
	LogReactNative LogRuntime = iota + 1
	LogFlutter
)

// LogEntry is one message of a device log: what one log call wrote, its
// lines joined with "\n".
type LogEntry struct {
	Runtime LogRuntime
	Time    time.Time
	PID     int
	Process string
	// BundleID is the app's, where the log names it.
	BundleID string
	// Error is whether the log gave the entry error level: E or F on
	// Android, Error or Fault in the unified log. Only React Native's rule
	// reads it: Flutter prints its errors at info level.
	Error bool
	Text  string
	// Raw is the entry as the log printed it, the lines a report shows.
	Raw string
}

// LogError is one error a scan found.
type LogError struct {
	Kind     string
	Time     time.Time
	PID      int
	Process  string
	BundleID string
	Summary  string
	// Text is every line of it, as the log printed them.
	Text string
}

func (e LogError) ID() string { return e.Kind + "-" + strconv.FormatInt(e.Time.UnixMicro(), 10) }

// Report is the error as a row of a crash listing.
func (e LogError) Report() CrashReport {
	return CrashReport{ID: e.ID(), Kind: e.Kind, Time: e.Time, Process: e.Process, BundleID: e.BundleID, PID: e.PID, Summary: e.Summary}
}

// jsErrorRE is a whole message that is one Error as React Native's console
// prints it: "[" Error.prototype.toString "]". The name must be an error's,
// ending in Error or Exception, or invariant's "Invariant Violation", so an
// app's own bracketed text and an array or object dump do not match.
var jsErrorRE = regexp.MustCompile(`(?s)^\[((?:[A-Za-z_$][\w$]*)?(?:Error|Exception)|Invariant Violation)(?:: (.*))?\]$`)

// jsErrorPropsRE is an Error with properties of its own, which the console
// prints in braces after it: "{ [Error: message] code: 'E_X' }", or with each
// property on its own line once they run past 60 characters. React gives
// every error thrown while rendering a componentStack. Only an Error or a
// function can open a braced value, so any name but Function is an error's;
// an app's own error names need no Error suffix here. The match stops at the
// first property's key, since the log can cut a long message off before the
// closing brace. A key is an identifier or a quoted string.
var jsErrorPropsRE = regexp.MustCompile(`(?s)^\{ \[([^\]\n:]+?)(?:: (.*?))?\](?: |\n  )(?:[A-Za-z_]\w*|'(?:[^'\\\n]|\\.)*'): `)

// dartUnhandledRE is the engine's line for an exception nothing caught,
// "[ERROR:flutter/runtime/dart_vm_initializer.cc(40)] Unhandled Exception:
// Bad state: ...". The file is left open: older engines logged it from
// lib/ui/ui_dart_state.cc.
var dartUnhandledRE = regexp.MustCompile(`^\[ERROR:flutter/[^\]]+\] (Unhandled Exception: .*)$`)

// dartCaughtRE is the header of an error the framework caught, "══╡
// EXCEPTION CAUGHT BY WIDGETS LIBRARY ╞═══".
var dartCaughtRE = regexp.MustCompile(`^══╡ (EXCEPTION CAUGHT BY .+?) ╞═`)

const dartAnotherPrefix = "Another exception was thrown: "

// maxDartBlockLines ends a framework error whose footer never comes, so a
// lost footer cannot swallow the rest of the log.
const maxDartBlockLines = 400

const maxSummary = 300

// ScanLogErrors finds the errors in entries, which are in the order the log
// printed them. A framework error's lines are joined into one error per
// process; any other match is one entry. The errors come back oldest first.
func ScanLogErrors(entries []LogEntry) []LogError {
	var out []LogError
	// block is a framework error being read: its messages, which the
	// summary comes from, and the lines as the log printed them.
	type block struct {
		err   LogError
		texts []string
		raws  []string
	}
	open := map[int]*block{}
	finish := func(b *block) {
		b.err.Text = strings.Join(b.raws, "\n")
		b.err.Summary = clipSummary(dartBlockSummary(b.err.Summary, b.texts))
		out = append(out, b.err)
	}
	// add puts an entry's lines on b and reports whether one of them was
	// the footer, or the block has run too long to wait for it.
	add := func(b *block, e LogEntry, raw string) bool {
		texts := strings.Split(e.Text, "\n")
		raws := strings.Split(raw, "\n")
		aligned := len(raws) == len(texts)
		if !aligned {
			// A raw form that is not a line per message line goes in whole.
			b.raws = append(b.raws, raw)
		}
		for i, l := range texts {
			b.texts = append(b.texts, l)
			if aligned {
				b.raws = append(b.raws, raws[i])
			}
			if (len(b.texts) > 1 && isDartFooter(l)) || len(b.texts) >= maxDartBlockLines {
				return true
			}
		}
		return false
	}
	for _, e := range entries {
		raw := e.Raw
		if raw == "" {
			raw = e.Text
		}
		switch e.Runtime {
		case LogReactNative:
			if !e.Error {
				continue
			}
			m := jsErrorRE.FindStringSubmatch(e.Text)
			if m == nil {
				if m = jsErrorPropsRE.FindStringSubmatch(e.Text); m == nil || m[1] == "Function" {
					continue
				}
			}
			summary := m[1]
			if msg, _, _ := strings.Cut(m[2], "\n"); msg != "" {
				summary += ": " + msg
			}
			out = append(out, logError(KindJSError, e, clipSummary(summary), raw))
		case LogFlutter:
			if b := open[e.PID]; b != nil {
				if add(b, e, raw) {
					finish(b)
					delete(open, e.PID)
				}
				continue
			}
			first, _, _ := strings.Cut(e.Text, "\n")
			switch {
			case dartUnhandledRE.MatchString(first):
				out = append(out, logError(KindDartError, e, clipSummary(dartUnhandledRE.FindStringSubmatch(first)[1]), raw))
			case dartCaughtRE.MatchString(first):
				// The header can come alone or with the rest of the block
				// in the same entry, when the log stamped its lines alike.
				b := &block{err: logError(KindDartError, e, dartCaughtRE.FindStringSubmatch(first)[1], "")}
				if add(b, e, raw) {
					finish(b)
					continue
				}
				open[e.PID] = b
			case strings.HasPrefix(first, dartAnotherPrefix):
				out = append(out, logError(KindDartError, e, clipSummary(first), raw))
			}
		}
	}
	for _, b := range open {
		finish(b)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func logError(kind string, e LogEntry, summary, text string) LogError {
	return LogError{Kind: kind, Time: e.Time, PID: e.PID, Process: e.Process, BundleID: e.BundleID, Summary: summary, Text: text}
}

// isDartFooter is the line of ═ that closes a framework error.
func isDartFooter(line string) bool {
	line = strings.TrimSpace(line)
	return len(line) >= len("═════") && strings.Trim(line, "═") == ""
}

// dartBlockSummary is the header's "EXCEPTION CAUGHT BY ..." and the error's
// first line, the one after "The following ... was thrown ...:".
func dartBlockSummary(header string, texts []string) string {
	for i, l := range texts {
		if !strings.HasPrefix(l, "The following ") {
			continue
		}
		for _, next := range texts[i+1:] {
			if next = strings.TrimSpace(next); next != "" && !isDartFooter(next) {
				return header + ": " + next
			}
		}
		break
	}
	return header
}

// clipSummary cuts s to maxSummary bytes at a character boundary.
func clipSummary(s string) string {
	if len(s) <= maxSummary {
		return s
	}
	cut := maxSummary
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// fatalJSWindow is how long after a fatal JS error the crash report for
// the same death can be written.
const fatalJSWindow = 5 * time.Second

// MergeCrashReports puts crash reports and log errors into one listing,
// newest first. Since applies to both. Limit applies to each source on its
// own, so up to Limit crash reports and Limit log errors come back: an app
// that logs an error in a loop cannot push a crash out of the listing.
//
// A jsError is dropped when a crash report for the same pid comes at most
// fatalJSWindow after it. In a release build a fatal JS error is logged
// and then ends the process, and the crash report stands for both.
func MergeCrashReports(crashes, logged []CrashReport, q CrashQuery) []CrashReport {
	newest := func(l []CrashReport, keep func(CrashReport) bool) []CrashReport {
		var out []CrashReport
		for _, r := range l {
			if (q.Since.IsZero() || !r.Time.Before(q.Since)) && keep(r) {
				out = append(out, r)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
		if q.Limit > 0 && len(out) > q.Limit {
			out = out[:q.Limit]
		}
		return out
	}
	covered := func(e CrashReport) bool {
		if e.Kind != KindJSError || e.PID == 0 {
			return false
		}
		for _, c := range crashes {
			if c.PID == e.PID && c.Kind != "anr" && !c.Time.Before(e.Time) && c.Time.Sub(e.Time) <= fatalJSWindow {
				return true
			}
		}
		return false
	}
	all := newest(crashes, func(CrashReport) bool { return true })
	all = append(all, newest(logged, func(e CrashReport) bool { return !covered(e) })...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.After(all[j].Time) })
	return all
}
