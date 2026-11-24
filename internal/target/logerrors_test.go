package target

import (
	"strings"
	"testing"
	"time"
)

var logT0 = time.Date(2026, 11, 24, 12, 49, 44, 382140000, time.UTC)

func rnEntry(sec int, text string) LogEntry {
	return LogEntry{Runtime: LogReactNative, Time: logT0.Add(time.Duration(sec) * time.Second), PID: 100, Error: true, Text: text}
}

func dartEntry(ms int, pid int, text string) LogEntry {
	return LogEntry{Runtime: LogFlutter, Time: logT0.Add(time.Duration(ms) * time.Millisecond), PID: pid, Text: text}
}

func TestScanLogErrorsReadsReactNativeErrors(t *testing.T) {
	got := ScanLogErrors([]LogEntry{
		// What ExceptionsManager logs for an Error thrown from an onPress,
		// as both platforms showed it.
		rnEntry(0, "[Error: sample-js: test crash from the Crash button]"),
		rnEntry(1, "[TypeError: undefined is not a function]"),
		rnEntry(2, "[Invariant Violation: Text strings must be rendered within a <Text> component.]"),
		rnEntry(3, "[Error: first line\nsecond line]"),
		rnEntry(4, "[Error]"),
	})
	want := []string{
		"Error: sample-js: test crash from the Crash button",
		"TypeError: undefined is not a function",
		"Invariant Violation: Text strings must be rendered within a <Text> component.",
		"Error: first line",
		"Error",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d errors, want %d: %+v", len(got), len(want), got)
	}
	for i, e := range got {
		if e.Kind != KindJSError || e.Summary != want[i] || e.PID != 100 {
			t.Errorf("[%d] = %+v, want jsError %q", i, e, want[i])
		}
	}
	if got[3].Text != "[Error: first line\nsecond line]" {
		t.Errorf("text = %q", got[3].Text)
	}
}

// An Error with properties of its own prints them after it in braces: React
// adds componentStack to every error thrown while rendering, and an app sets
// code or a custom name. The first two are what a simulator logged for a
// render throw and an onPress throw with a code; the unified log cut the
// first one off at about 1 KiB, so its closing brace never came.
func TestScanLogErrorsReadsErrorsWithProperties(t *testing.T) {
	render := "{ [Error: sample-js: thrown during render]\n" +
		`  componentStack: '\n    at Boom (http://localhost:8081/App.bundle//&platform=ios&dev=true:122:20)\n    at RCTView (<anonymous>)\n    at View (http://localhost:8081/index.bundle//&platform=ios&dev=true&lazy=true&minify=false&inlineSourceMap=false&modulesOnly=false&runModule=true&excludeSource=true&sourcePaths=url-server&app=com.example.app&transform.routerRoot=app&transform.engine=hermes<…>`
	got := ScanLogErrors([]LogEntry{
		rnEntry(0, render),
		rnEntry(1, "{ [Error: sample-js: thrown with a code] code: 'E_SAMPLE' }"),
		rnEntry(2, "{ [ValidationFailure: bad input] name: 'ValidationFailure' }"),
		rnEntry(3, "{ [Error: items[0] is missing] code: 'E_MISSING' }"),
		rnEntry(4, "{ [Error] 'retry-after': 30 }"),
		rnEntry(5, "{ [TypeError: first line\nsecond line]\n  componentStack: '\\n    at Row',\n  code: 'E_ROW' }"),
	})
	want := []string{
		"Error: sample-js: thrown during render",
		"Error: sample-js: thrown with a code",
		"ValidationFailure: bad input",
		"Error: items[0] is missing",
		"Error",
		"TypeError: first line",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d errors, want %d: %+v", len(got), len(want), got)
	}
	for i, e := range got {
		if e.Kind != KindJSError || e.Summary != want[i] {
			t.Errorf("[%d] = %+v, want jsError %q", i, e, want[i])
		}
	}
	if got[0].Text != render {
		t.Errorf("text = %q", got[0].Text)
	}
}

// A function with properties prints in braces the same way, and an object
// that holds an Error is not one.
func TestScanLogErrorsSkipsObjectsThatAreNotErrors(t *testing.T) {
	got := ScanLogErrors([]LogEntry{
		rnEntry(0, "{ [Function: handler] displayName: 'Row' }"),
		rnEntry(1, "{ [Function] a: 1 }"),
		rnEntry(2, "{ error: [Error: x] }"),
		rnEntry(3, "{ a: 1, b: 2 }"),
		rnEntry(4, "{ [Error: x] and more after it }"),
		rnEntry(5, "{ [Note: not an error] }"),
	})
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

// The bar is no false positives: React's warnings go to console.error as
// strings, and an app logs whatever it likes there.
func TestScanLogErrorsSkipsReactNativeLogsThatAreNotErrors(t *testing.T) {
	warn := rnEntry(0, "[Error: logged at warn level]")
	warn.Error = false
	got := ScanLogErrors([]LogEntry{
		warn,
		rnEntry(1, `Warning: Each child in a list should have a unique "key" prop.`),
		rnEntry(2, "Failed [Error: x]"),
		rnEntry(3, "[object Object]"),
		rnEntry(4, `["a", "b"]`),
		rnEntry(5, "[1, 2, 3]"),
		rnEntry(6, "[Error: x] and more after it"),
		rnEntry(7, "boom"),
		rnEntry(8, "[Note: not an error name]"),
		rnEntry(9, ""),
	})
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestScanLogErrorsReadsAnUnhandledDartException(t *testing.T) {
	text := "[ERROR:flutter/runtime/dart_vm_initializer.cc(40)] Unhandled Exception: Bad state: sample-dart: test crash from the Crash button\n" +
		"#0      _Header.build.<anonymous closure> (package:sample/main.dart:157:15)\n" +
		"#1      new Future.<anonymous closure> (dart:async/future.dart:260:40)\n"
	got := ScanLogErrors([]LogEntry{dartEntry(0, 200, text)})
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	e := got[0]
	if e.Kind != KindDartError || e.Summary != "Unhandled Exception: Bad state: sample-dart: test crash from the Crash button" || e.Text != text || !e.Time.Equal(logT0) {
		t.Errorf("got %+v", e)
	}
}

// A framework error is printed a line at a time, so it comes as one entry
// per line, from its header to its footer.
func TestScanLogErrorsJoinsAFlutterFrameworkErrorsLines(t *testing.T) {
	lines := []string{
		"══╡ EXCEPTION CAUGHT BY WIDGETS LIBRARY ╞═══════════════════════════════════════════════════════════",
		"The following StateError was thrown building Builder(dirty):",
		"Bad state: sample-dart: thrown in build",
		"",
		"The relevant error-causing widget was:",
		"  Builder",
		"",
		"When the exception was thrown, this was the stack:",
		"#0      _Header.build (package:sample/main.dart:157:15)",
		"════════════════════════════════════════════════════════════════════════════════════════════════════",
	}
	var entries []LogEntry
	for i, l := range lines {
		entries = append(entries, dartEntry(i, 200, l))
		if i == 3 {
			// Another process's line in the middle is not part of it.
			entries = append(entries, dartEntry(i, 300, "flutter: hello from another app"))
		}
	}
	entries = append(entries, dartEntry(20, 200, "flutter: after the error"))
	got := ScanLogErrors(entries)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	e := got[0]
	if e.Kind != KindDartError || e.Summary != "EXCEPTION CAUGHT BY WIDGETS LIBRARY: Bad state: sample-dart: thrown in build" {
		t.Errorf("got %+v", e)
	}
	if e.Text != strings.Join(lines, "\n") || !e.Time.Equal(logT0) || e.PID != 200 {
		t.Errorf("text = %q, time %v", e.Text, e.Time)
	}
}

// After the first framework error of a run, Flutter prints only a one-line
// summary for each one after it.
func TestScanLogErrorsReadsAnotherExceptionWasThrown(t *testing.T) {
	got := ScanLogErrors([]LogEntry{dartEntry(0, 200, "Another exception was thrown: A RenderFlex overflowed by 42 pixels on the right.")})
	if len(got) != 1 || got[0].Kind != KindDartError || got[0].Summary != "Another exception was thrown: A RenderFlex overflowed by 42 pixels on the right." {
		t.Errorf("got %+v", got)
	}
}

// Lines the log stamped alike arrive as one entry, the header with them.
func TestScanLogErrorsReadsAFrameworkErrorInOneEntry(t *testing.T) {
	got := ScanLogErrors([]LogEntry{
		dartEntry(0, 200, "══╡ EXCEPTION CAUGHT BY GESTURE ╞═══════\nThe following StateError was thrown while handling a gesture:\nBad state: x"),
		dartEntry(1, 200, "#0      main (package:sample/main.dart:1:1)\n════════════════════════"),
		dartEntry(2, 200, "Another exception was thrown: Bad state: y"),
	})
	if len(got) != 2 || got[0].Summary != "EXCEPTION CAUGHT BY GESTURE: Bad state: x" || strings.Count(got[0].Text, "\n") != 4 || got[1].Summary != "Another exception was thrown: Bad state: y" {
		t.Errorf("got %+v", got)
	}
}

// A block the log cut off before its footer is still an error.
func TestScanLogErrorsKeepsAFrameworkErrorWithNoFooter(t *testing.T) {
	got := ScanLogErrors([]LogEntry{
		dartEntry(0, 200, "══╡ EXCEPTION CAUGHT BY RENDERING LIBRARY ╞═════════════════"),
		dartEntry(1, 200, "The following assertion was thrown during layout:"),
		dartEntry(2, 200, "A RenderFlex overflowed by 42 pixels on the right."),
	})
	if len(got) != 1 || got[0].Summary != "EXCEPTION CAUGHT BY RENDERING LIBRARY: A RenderFlex overflowed by 42 pixels on the right." {
		t.Errorf("got %+v", got)
	}
}

func TestScanLogErrorsSkipsOrdinaryFlutterOutput(t *testing.T) {
	got := ScanLogErrors([]LogEntry{
		dartEntry(0, 200, "flutter: tapped the button"),
		dartEntry(1, 200, "Unhandled Exception: printed by the app itself"),
		dartEntry(2, 200, "[ERROR:flutter/shell/common/shell.cc(117)] Dart Error: something the engine says"),
		dartEntry(3, 200, "Reloaded 1 of 512 libraries in 210ms."),
		dartEntry(4, 200, "════════════════"),
		dartEntry(5, 200, "W/flutter: [WARNING:flutter/fml/platform.cc] a warning"),
	})
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestLogErrorIDNamesItsKindAndMicros(t *testing.T) {
	e := LogError{Kind: KindDartError, Time: logT0}
	if e.ID() != "dartError-1795524584382140" {
		t.Fatalf("id = %q", e.ID())
	}
	at, ok := LogErrorTime(e.ID())
	if !ok || !at.Equal(logT0) {
		t.Errorf("LogErrorTime = %v, %v", at, ok)
	}
	for _, good := range []string{"jsError-1795524584382140", "dartError-1795524584382140"} {
		if !ValidCrashID(good) || !LogErrorIDRE.MatchString(good) {
			t.Errorf("%q refused", good)
		}
	}
	for _, bad := range []string{"jsError-", "jsError-12", "jserror-1795524584382140", "nativeError-1795524584382140", "jsError-1795524584382140;ls"} {
		if ValidCrashID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLogErrorReportIsARow(t *testing.T) {
	e := LogError{Kind: KindJSError, Time: logT0, PID: 100, Process: "com.example.app", BundleID: "com.example.app", Summary: "Error: x"}
	r := e.Report()
	if r.ID != e.ID() || r.Kind != KindJSError || !r.Time.Equal(logT0) || r.PID != 100 || r.Process != "com.example.app" || r.BundleID != "com.example.app" || r.Summary != "Error: x" {
		t.Errorf("report = %+v", r)
	}
}

func TestMergeCrashReportsNewestFirstWithSinceAndLimit(t *testing.T) {
	at := func(s int) time.Time { return logT0.Add(time.Duration(s) * time.Second) }
	crashes := []CrashReport{{ID: "c3", Time: at(3)}, {ID: "c1", Time: at(1)}}
	logged := []CrashReport{{ID: "j2", Time: at(2)}, {ID: "j4", Time: at(4)}, {ID: "j0", Time: at(0)}}
	ids := func(l []CrashReport) string {
		var s []string
		for _, r := range l {
			s = append(s, r.ID)
		}
		return strings.Join(s, ",")
	}
	if got := ids(MergeCrashReports(crashes, logged, CrashQuery{})); got != "j4,c3,j2,c1,j0" {
		t.Errorf("all = %s", got)
	}
	if got := ids(MergeCrashReports(crashes, logged, CrashQuery{Since: at(2)})); got != "j4,c3,j2" {
		t.Errorf("since = %s", got)
	}
	if got := ids(MergeCrashReports(crashes, logged, CrashQuery{Limit: 2})); got != "j4,c3" {
		t.Errorf("limit = %s", got)
	}
}
