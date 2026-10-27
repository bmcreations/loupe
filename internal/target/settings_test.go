package target

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeShell is a shellFunc double keyed on the exact space-joined command.
// scriptAnswers, when set, answers any call whose single argument is the
// combined settings script (detected by the section marker) with a
// synthesized combined output built from per-section strings, the same
// shape a real device's response to that script would have.
//
// A call that is instead a single-command sentinel-wrapped script (the
// shape talkBackSupported builds, and any script scriptAnswers does not
// match) is answered generically: each embedded command is looked up in
// answers the same way a plain call would be, real ";"-joined-script
// semantics apply -- a command that fails (present in fail) still lets the
// script continue and reports empty output for that section, exactly as a
// real failing command's stdout would be empty -- and the terminal marker
// is always emitted unless breakScript is set, which truncates the output
// before it to simulate adb itself losing the connection mid-script (as
// opposed to a command inside the script merely exiting nonzero).
type fakeShell struct {
	answers       map[string]string
	fail          map[string]error
	calls         []string
	scriptAnswers []string // one per settingsScript() section, in order
	breakScript   bool     // truncate any script response before its terminal marker

	// errAlongsideComplete makes run return this error alongside otherwise
	// complete, well-formed script output -- the shape CombinedOutput takes
	// when a command inside the script exits non-zero but the script's own
	// last command (the terminal marker) still runs and succeeds. Pins that
	// completion is judged by the terminal marker, not by this error.
	errAlongsideComplete error

	// honourCtx makes run return ctx.Err() once ctx is done, instead of
	// ignoring ctx the way every other test here relies on (their expired-ctx
	// scenarios simulate a deadline that lapses server-side, after the fake
	// has already answered). Tests that need to observe what ctx a specific
	// call actually received -- proving rollback runs on a detached context
	// with its own timeout, not the request's -- set this and read
	// ctxSnapshotOfCall, a snapshot taken AT CALL TIME: the actual context
	// object cannot be inspected after the fact, because rollback's own
	// bounded context is canceled by its "defer cancel()" the instant
	// rollback returns, which would read back as "canceled" regardless of
	// whether the parent's cancellation ever reached it.
	honourCtx         bool
	ctxSnapshotOfCall map[string]ctxSnapshot

	// onFail, when set, runs just before a call that fails (is in the fail
	// map) returns its error -- e.g. to cancel a parent context at exactly
	// the moment a real request's deadline might lapse mid-ApplySettings,
	// so a test can prove rollback still completes.
	onFail func(joined string)
}

type ctxSnapshot struct {
	err         error
	hasDeadline bool
	remaining   time.Duration
}

func (f *fakeShell) run(ctx context.Context, args ...string) (string, error) {
	joined := joinArgs(args)
	f.calls = append(f.calls, joined)
	if f.ctxSnapshotOfCall == nil {
		f.ctxSnapshotOfCall = map[string]ctxSnapshot{}
	}
	snap := ctxSnapshot{err: ctx.Err()}
	if dl, ok := ctx.Deadline(); ok {
		snap.hasDeadline = true
		snap.remaining = time.Until(dl)
	}
	f.ctxSnapshotOfCall[joined] = snap
	if f.honourCtx {
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}

	if len(args) == 1 {
		if nonce, ok := extractNonce(args[0]); ok {
			if f.scriptAnswers != nil {
				if f.breakScript {
					return "", errors.New("device unreachable")
				}
				resp, _ := nonceFakeResponse(nonce, f.scriptAnswers)
				return resp, f.errAlongsideComplete
			}
			cmds := scriptCommands(args[0])
			sections := make([]string, len(cmds))
			for i, cmd := range cmds {
				sections[i] = f.answers[cmd]
			}
			if f.breakScript {
				return "", errors.New("device unreachable")
			}
			resp, _ := nonceFakeResponse(nonce, sections)
			return resp, f.errAlongsideComplete
		}
	}

	if err, ok := f.fail[joined]; ok {
		if f.onFail != nil {
			f.onFail(joined)
		}
		return "", err
	}
	if out, ok := f.answers[joined]; ok {
		return out, nil
	}
	return "", nil
}

// scriptCommands extracts the plain shell command embedded after each
// sectionEcho in a script built the way settingsScript/talkBackSupported
// build one: "echo <marker>; <cmd>\n" for every command, followed by a bare
// terminal "echo <marker>\n" with no command. Only the commands are
// returned, in order.
func scriptCommands(script string) []string {
	var cmds []string
	for _, line := range strings.Split(script, "\n") {
		i := strings.Index(line, "; ")
		if i < 0 {
			continue
		}
		marker := line[:i]
		if !strings.HasPrefix(marker, "echo "+settingsSectionMarker) {
			continue
		}
		cmds = append(cmds, line[i+2:])
	}
	return cmds
}

func joinArgs(args []string) string {
	return strings.Join(args, " ")
}

func newFakeSettings(f *fakeShell) *androidSettings {
	return &androidSettings{sh: f.run}
}

func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func countExact(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

func indexOfExact(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

func lastIndexOfPrefix(calls []string, prefix string) int {
	last := -1
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			last = i
		}
	}
	return last
}

func lastIndexOfExact(calls []string, want string) int {
	last := -1
	for i, c := range calls {
		if c == want {
			last = i
		}
	}
	return last
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fullFakeAnswers answers every individual command settingFields' read,
// readRestore and write functions issue, for tests that exercise fields one
// at a time rather than through the combined Settings() script.
func fullFakeAnswers() map[string]string {
	return map[string]string{
		"cmd uimode night":                                   "Night mode: no",
		"settings get system font_scale":                     "1.0",
		"settings get global window_animation_scale":         "1.0",
		"settings get global transition_animation_scale":     "1.0",
		"settings get global animator_duration_scale":        "1.0",
		"getprop debug.layout":                               "false",
		"settings get secure enabled_accessibility_services": "null",
		"pm path com.google.android.marvin.talkback":         "package:/data/app/talkback.apk",
	}
}

// --- darkMode ---------------------------------------------------------

// darkMode's read path is exercised through Settings() rather than a direct
// per-field read function (item 8: the dead read* functions were removed,
// since readRestore* already reads the same state for rollback purposes).
func TestSettingsReadsDarkModeBothWays(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"yes", "Night mode: yes", true},
		{"no", "Night mode: no", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeShell{scriptAnswers: []string{
				c.out, "1.0", "1.0", "1.0", "1.0", "false", "null", "package:/data/app/talkback.apk",
			}}
			settings, err := newFakeSettings(f).Settings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]Setting{}
			for _, s := range settings {
				byID[s.ID] = s
			}
			if byID["darkMode"].Value != c.want {
				t.Fatalf("darkMode = %v, want %v", byID["darkMode"].Value, c.want)
			}
		})
	}
}

func TestWriteDarkMode(t *testing.T) {
	f := &fakeShell{}
	if err := writeDarkMode(context.Background(), f.run, true); err != nil {
		t.Fatal(err)
	}
	if countExact(f.calls, "cmd uimode night yes") != 1 {
		t.Fatalf("calls: %v", f.calls)
	}
}

// --- fontScale ----------------------------------------------------------

func TestSettingsReadsFontScaleIncludingNullAsDefault(t *testing.T) {
	cases := []struct {
		out  string
		want float64
	}{
		{"1.3", 1.3},
		{"null", 1.0},
		{"", 1.0},
	}
	for _, c := range cases {
		f := &fakeShell{scriptAnswers: []string{
			"Night mode: no", c.out, "1.0", "1.0", "1.0", "false", "null", "package:/data/app/talkback.apk",
		}}
		settings, err := newFakeSettings(f).Settings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]Setting{}
		for _, s := range settings {
			byID[s.ID] = s
		}
		if byID["fontScale"].Value != c.want {
			t.Fatalf("out=%q fontScale = %v, want %v", c.out, byID["fontScale"].Value, c.want)
		}
	}
}

func TestWriteFontScale(t *testing.T) {
	f := &fakeShell{}
	if err := writeFontScale(context.Background(), f.run, 1.25); err != nil {
		t.Fatal(err)
	}
	if countExact(f.calls, "settings put system font_scale 1.25") != 1 {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestValidateRangeUsesTheFieldsOwnBounds(t *testing.T) {
	f, _ := (&androidSettings{}).fieldByID("fontScale")
	cases := []struct {
		v    float64
		want bool
	}{
		{0.85, true},
		{0.849, false},
		{2.0, true},
		{2.001, false},
		{1.0, true},
	}
	for _, c := range cases {
		err := f.validate(f, c.v)
		if (err == nil) != c.want {
			t.Fatalf("validate(%v): err=%v, want ok=%v", c.v, err, c.want)
		}
	}
}

// --- reduceMotion ---------------------------------------------------------

func TestSettingsReadsReduceMotion(t *testing.T) {
	cases := []struct {
		name                         string
		window, transition, animator string
		want                         bool
	}{
		{"all zero is on", "0", "0", "0", true},
		{"any nonzero is off", "0", "1.0", "0", false},
		{"null defaults to 1, off", "null", "null", "null", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeShell{scriptAnswers: []string{
				"Night mode: no", "1.0", c.window, c.transition, c.animator, "false", "null", "package:/data/app/talkback.apk",
			}}
			settings, err := newFakeSettings(f).Settings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]Setting{}
			for _, s := range settings {
				byID[s.ID] = s
			}
			if byID["reduceMotion"].Value != c.want {
				t.Fatalf("reduceMotion = %v, want %v", byID["reduceMotion"].Value, c.want)
			}
		})
	}
}

func TestWriteReduceMotionOnSetsAllThreeKeysToZero(t *testing.T) {
	f := &fakeShell{}
	if err := writeReduceMotion(context.Background(), f.run, true); err != nil {
		t.Fatal(err)
	}
	for _, key := range animationScaleKeys {
		if countExact(f.calls, "settings put global "+key+" 0") != 1 {
			t.Fatalf("missing write for %s: %v", key, f.calls)
		}
	}
}

func TestWriteReduceMotionOffSetsAllThreeKeysToOne(t *testing.T) {
	f := &fakeShell{}
	if err := writeReduceMotion(context.Background(), f.run, false); err != nil {
		t.Fatal(err)
	}
	for _, key := range animationScaleKeys {
		if countExact(f.calls, "settings put global "+key+" 1") != 1 {
			t.Fatalf("missing write for %s: %v", key, f.calls)
		}
	}
}

// --- layoutBounds ---------------------------------------------------------

func TestSettingsReadsLayoutBounds(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"", false},
	}
	for _, c := range cases {
		f := &fakeShell{scriptAnswers: []string{
			"Night mode: no", "1.0", "1.0", "1.0", "1.0", c.out, "null", "package:/data/app/talkback.apk",
		}}
		settings, err := newFakeSettings(f).Settings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]Setting{}
		for _, s := range settings {
			byID[s.ID] = s
		}
		if byID["layoutBounds"].Value != c.want {
			t.Fatalf("out=%q layoutBounds = %v, want %v", c.out, byID["layoutBounds"].Value, c.want)
		}
	}
}

func TestWriteLayoutBoundsSetsPropertyThenNudges(t *testing.T) {
	f := &fakeShell{}
	if err := writeLayoutBounds(context.Background(), f.run, true); err != nil {
		t.Fatal(err)
	}
	setIdx := indexOfExact(f.calls, "setprop debug.layout true")
	callIdx := indexOfExact(f.calls, "service call activity 1599295570")
	if setIdx < 0 || callIdx < 0 || callIdx < setIdx {
		t.Fatalf("wrong order: %v", f.calls)
	}
}

// --- service list helpers -------------------------------------------------

func TestHasServiceWithoutServiceKeepsOthers(t *testing.T) {
	list := "com.other/.Svc:" + talkBackComponent
	if !hasService(list, talkBackComponent) {
		t.Fatal("want present")
	}
	next := withoutService(list, talkBackComponent)
	if next != "com.other/.Svc" {
		t.Fatalf("got %q", next)
	}
}

func TestWithServiceIsIdempotent(t *testing.T) {
	list := talkBackComponent
	next := withService(list, talkBackComponent)
	if next != talkBackComponent {
		t.Fatalf("got %q, want no duplicate", next)
	}
}

func TestWithServiceOnNullList(t *testing.T) {
	next := withService("null", talkBackComponent)
	if next != talkBackComponent {
		t.Fatalf("got %q", next)
	}
}

func TestWithoutServiceOnlyEntryLeavesEmpty(t *testing.T) {
	next := withoutService(talkBackComponent, talkBackComponent)
	if next != "" {
		t.Fatalf("got %q, want empty", next)
	}
}

// item 7: a short "pkg/.Class" form names the same service as the
// fully-qualified one, and the two must not be treated as distinct entries.
func TestExpandComponentMatchesShortAndFullForms(t *testing.T) {
	if !hasService(talkBackShortComponent, talkBackComponent) {
		t.Fatal("full lookup should find the short form already in the list")
	}
	if !hasService(talkBackComponent, talkBackShortComponent) {
		t.Fatal("short lookup should find the full form already in the list")
	}
}

func TestWithServiceDoesNotDuplicateTheShortForm(t *testing.T) {
	next := withService(talkBackShortComponent, talkBackComponent)
	if next != talkBackShortComponent {
		t.Fatalf("got %q, want the short form left alone with no duplicate added", next)
	}
}

func TestWithoutServiceRemovesTheShortForm(t *testing.T) {
	next := withoutService(talkBackShortComponent, talkBackComponent)
	if next != "" {
		t.Fatalf("got %q, want the short form removed", next)
	}
}

// --- talkBack ---------------------------------------------------------

func TestSettingsReadsTalkBackOnAmongOthers(t *testing.T) {
	f := &fakeShell{scriptAnswers: []string{
		"Night mode: no", "1.0", "1.0", "1.0", "1.0", "false",
		"com.other/.Svc:" + talkBackComponent, "package:/data/app/talkback.apk",
	}}
	settings, err := newFakeSettings(f).Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Setting{}
	for _, s := range settings {
		byID[s.ID] = s
	}
	tb := byID["talkBack"]
	if !tb.Supported || tb.Value != true {
		t.Fatalf("value=%v supported=%v", tb.Value, tb.Supported)
	}
}

// item 9 of this round: TalkBack is unsupported only when it is genuinely
// absent (pm path exits nonzero, or exits zero without a "package:" line).
// A generic adb failure on the enabled_accessibility_services read must
// surface as an error, not a false "not installed".
func TestSettingsReadsTalkBackUnsupportedWhenNotInstalled(t *testing.T) {
	f := &fakeShell{scriptAnswers: []string{
		"Night mode: no", "1.0", "1.0", "1.0", "1.0", "false", "null", "",
	}}
	settings, err := newFakeSettings(f).Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Setting{}
	for _, s := range settings {
		byID[s.ID] = s
	}
	tb := byID["talkBack"]
	if tb.Supported {
		t.Fatal("want unsupported")
	}
	if tb.Reason == "" {
		t.Fatal("want a reason")
	}
}

// item 6: `pm path` exiting 0 with output that does not start with
// "package:" (an ambiguous or empty success) must not be read as installed.
func TestTalkBackSupportedRequiresThePackagePrefix(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		failErr error
		want    bool
	}{
		{"package prefix", "package:/data/app/talkback.apk\n", nil, true},
		{"exit 0 but no prefix", "\n", nil, false},
		{"exit 0 unexpected text", "no such thing\n", nil, false},
		{"nonzero exit", "", errors.New("exit status 1"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeShell{answers: map[string]string{
				"pm path " + talkBackPackage: c.out,
			}}
			if c.failErr != nil {
				f.fail = map[string]error{"pm path " + talkBackPackage: c.failErr}
			}
			supported, _, err := talkBackSupported(context.Background(), f.run)
			if err != nil {
				t.Fatalf("talkBackSupported: unexpected error: %v", err)
			}
			if supported != c.want {
				t.Fatalf("supported = %v, want %v", supported, c.want)
			}
		})
	}
}

func TestWriteTalkBackOnKeepsOthers(t *testing.T) {
	f := &fakeShell{answers: map[string]string{
		"settings get secure enabled_accessibility_services": "com.other/.Svc",
	}}
	if err := writeTalkBack(context.Background(), f.run, true); err != nil {
		t.Fatal(err)
	}
	want := "settings put secure enabled_accessibility_services " + shellQuote("com.other/.Svc:"+talkBackComponent)
	if countExact(f.calls, want) != 1 {
		t.Fatalf("calls: %v, want %q", f.calls, want)
	}
	if countExact(f.calls, "settings put secure accessibility_enabled 1") != 1 {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestWriteTalkBackOffRemovesOnlyTalkBack(t *testing.T) {
	f := &fakeShell{answers: map[string]string{
		"settings get secure enabled_accessibility_services": "com.other/.Svc:" + talkBackComponent,
	}}
	if err := writeTalkBack(context.Background(), f.run, false); err != nil {
		t.Fatal(err)
	}
	want := "settings put secure enabled_accessibility_services " + shellQuote("com.other/.Svc")
	if countExact(f.calls, want) != 1 {
		t.Fatalf("calls: %v, want %q", f.calls, want)
	}
}

// item 1: writeTalkBack must shell-quote the list it writes back, since it
// can contain another accessibility service's component name, itself
// arbitrary data.
func TestWriteTalkBackQuotesTheServiceListItWrites(t *testing.T) {
	dangerous := "com.evil/Svc$(rm -rf /); echo pwned"
	f := &fakeShell{answers: map[string]string{
		"settings get secure enabled_accessibility_services": dangerous,
	}}
	if err := writeTalkBack(context.Background(), f.run, true); err != nil {
		t.Fatal(err)
	}
	next := dangerous + ":" + talkBackComponent
	want := "settings put secure enabled_accessibility_services " + shellQuote(next)
	if countExact(f.calls, want) != 1 {
		t.Fatalf("calls: %v, want exactly one call: %q", f.calls, want)
	}
	// The raw unquoted string must never appear as its own command.
	for _, c := range f.calls {
		if c == "settings put secure enabled_accessibility_services "+next {
			t.Fatalf("unquoted list reached the shell: %q", c)
		}
	}
}

// --- Settings() via the combined script -----------------------------------

func TestSettingsReportsAllFiveFields(t *testing.T) {
	f := &fakeShell{scriptAnswers: []string{
		"Night mode: yes", // darkMode
		"1.2",             // fontScale
		"0", "0", "0",     // reduceMotion (all zero -> on)
		"true",                           // layoutBounds
		talkBackComponent,                // enabled_accessibility_services
		"package:/data/app/talkback.apk", // pm path
	}}
	settings, err := newFakeSettings(f).Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 5 {
		t.Fatalf("got %d settings", len(settings))
	}
	byID := map[string]Setting{}
	for _, s := range settings {
		byID[s.ID] = s
	}
	if byID["darkMode"].Value != true {
		t.Fatalf("darkMode: %+v", byID["darkMode"])
	}
	if byID["fontScale"].Value != 1.2 {
		t.Fatalf("fontScale: %+v", byID["fontScale"])
	}
	if byID["reduceMotion"].Value != true {
		t.Fatalf("reduceMotion: %+v", byID["reduceMotion"])
	}
	if byID["layoutBounds"].Value != true {
		t.Fatalf("layoutBounds: %+v", byID["layoutBounds"])
	}
	if byID["talkBack"].Value != true || !byID["talkBack"].Supported {
		t.Fatalf("talkBack: %+v", byID["talkBack"])
	}
}

// item 11 / trap 33: Settings must issue exactly one adb shell invocation,
// not one per field.
func TestSettingsIssuesExactlyOneShellCall(t *testing.T) {
	f := &fakeShell{scriptAnswers: []string{
		"Night mode: no", "1.0", "1.0", "1.0", "1.0", "false", "null", "",
	}}
	if _, err := newFakeSettings(f).Settings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("got %d shell calls, want 1: %v", len(f.calls), f.calls)
	}
}

// --- ApplySettings: validation before any command --------------------------

func TestApplySettingsRejectsUnknownID(t *testing.T) {
	f := &fakeShell{answers: fullFakeAnswers()}
	_, _, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{"bogus": true})
	if !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("err: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestApplySettingsRejectsOutOfRangeValue(t *testing.T) {
	f := &fakeShell{answers: fullFakeAnswers()}
	_, _, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{"fontScale": 3.0})
	if !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("err: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestApplySettingsRejectsWrongType(t *testing.T) {
	f := &fakeShell{answers: fullFakeAnswers()}
	_, _, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{"darkMode": "yes"})
	if !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("err: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls: %v", f.calls)
	}
}

// item 5: an unsupported talkBack write is rejected before anything runs,
// the same as an unknown id or an out-of-range value.
func TestApplySettingsRejectsTalkBackWhenUnsupported(t *testing.T) {
	f := &fakeShell{answers: map[string]string{
		"pm path com.google.android.marvin.talkback": "",
	}, fail: map[string]error{
		"pm path com.google.android.marvin.talkback": errors.New("not found"),
	}}
	_, _, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{"talkBack": true})
	if !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("err: %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "settings put secure enabled_accessibility_services") ||
			strings.HasPrefix(c, "settings put secure accessibility_enabled") {
			t.Fatalf("a write reached the shell before rejection: %v", f.calls)
		}
	}
}

// --- ApplySettings: rollback ------------------------------------------------

func TestApplySettingsSucceedsWithNoFailures(t *testing.T) {
	f := &fakeShell{answers: fullFakeAnswers()}
	failed, notRestored, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{
		"darkMode": true, "fontScale": 1.2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 0 || len(notRestored) != 0 {
		t.Fatalf("failed=%v notRestored=%v", failed, notRestored)
	}
}

// item 4: the rewritten rollback test asserts the exact rollback commands
// and their order, not just a write count.
func TestApplySettingsRollsBackOnFailureAndNamesTheRest(t *testing.T) {
	f := &fakeShell{
		answers: fullFakeAnswers(),
		fail: map[string]error{
			"settings put global animator_duration_scale 0": errors.New("device offline"),
		},
	}
	changes := map[string]any{
		"darkMode": true, "fontScale": 1.2, "reduceMotion": true, "layoutBounds": true,
	}
	failed, notRestored, err := newFakeSettings(f).ApplySettings(context.Background(), changes)
	if err == nil {
		t.Fatal("want an error")
	}
	if len(notRestored) != 0 {
		t.Fatalf("notRestored: %v", notRestored)
	}
	wantFailed := []string{"reduceMotion", "layoutBounds"}
	if !equalSlices(failed, wantFailed) {
		t.Fatalf("failed = %v, want %v", failed, wantFailed)
	}

	// darkMode was read as "no" and requested as true (yes): forward writes
	// "yes", rollback must write "no" back, after the forward writes.
	forwardDark := indexOfExact(f.calls, "cmd uimode night yes")
	rollbackDark := lastIndexOfExact(f.calls, "cmd uimode night "+shellQuote("no"))
	if forwardDark < 0 || rollbackDark < 0 || rollbackDark < forwardDark {
		t.Fatalf("darkMode rollback missing or out of order: %v", f.calls)
	}

	// fontScale was read as "1.0" and requested as 1.2: rollback must put
	// 1.0 back, after the forward write of 1.2.
	forwardFont := indexOfExact(f.calls, "settings put system font_scale 1.2")
	rollbackFont := lastIndexOfExact(f.calls, "settings put system font_scale "+shellQuote("1.0"))
	if forwardFont < 0 {
		t.Fatalf("forward fontScale write missing: %v", f.calls)
	}
	if rollbackFont < 0 || rollbackFont == forwardFont {
		t.Fatalf("fontScale rollback missing: %v", f.calls)
	}
	if countExact(f.calls, "settings put system font_scale "+shellQuote("1.0")) != 1 {
		t.Fatalf("calls: %v", f.calls)
	}

	// Rollback runs in reverse of application order: fontScale was applied
	// after darkMode, so fontScale's rollback must come before darkMode's.
	if rollbackFont > rollbackDark {
		t.Fatalf("rollback ran out of order: darkMode@%d font@%d", rollbackDark, rollbackFont)
	}

	// layoutBounds was never read or written at all -- it comes after the
	// failing field in table order.
	if countCalls(f.calls, "setprop debug.layout") != 0 || countCalls(f.calls, "getprop debug.layout") != 0 {
		t.Fatalf("layoutBounds should never have run: %v", f.calls)
	}
}

// item 4: a read failure (not a write failure) must still trigger rollback
// of whatever was already applied, and must not attempt to roll back the
// field whose own read failed.
func TestApplySettingsRollsBackOnReadFailure(t *testing.T) {
	f := &fakeShell{
		answers: fullFakeAnswers(),
		fail: map[string]error{
			"settings get global window_animation_scale": errors.New("device offline"),
		},
	}
	changes := map[string]any{"darkMode": true, "reduceMotion": true}
	failed, notRestored, err := newFakeSettings(f).ApplySettings(context.Background(), changes)
	if err == nil {
		t.Fatal("want an error")
	}
	if len(notRestored) != 0 {
		t.Fatalf("notRestored: %v", notRestored)
	}
	wantFailed := []string{"reduceMotion"}
	if !equalSlices(failed, wantFailed) {
		t.Fatalf("failed = %v, want %v", failed, wantFailed)
	}
	if countExact(f.calls, "cmd uimode night yes") != 1 {
		t.Fatalf("forward darkMode write missing: %v", f.calls)
	}
	if countExact(f.calls, "cmd uimode night "+shellQuote("no")) != 1 {
		t.Fatalf("darkMode rollback missing: %v", f.calls)
	}
	if countCalls(f.calls, "settings put global window_animation_scale") != 0 {
		t.Fatalf("reduceMotion write should never have run: %v", f.calls)
	}
}

// item 3: a field that fails partway through its own multi-command write is
// rolled back too, restoring the keys it did manage to change.
func TestApplySettingsRollsBackAPartiallyWrittenField(t *testing.T) {
	f := &fakeShell{
		answers: fullFakeAnswers(),
		fail: map[string]error{
			"settings put global transition_animation_scale 0": errors.New("device offline"),
		},
	}
	failed, notRestored, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{"reduceMotion": true})
	if err == nil {
		t.Fatal("want an error")
	}
	if len(notRestored) != 0 {
		t.Fatalf("notRestored: %v", notRestored)
	}
	if !equalSlices(failed, []string{"reduceMotion"}) {
		t.Fatalf("failed = %v", failed)
	}
	if countExact(f.calls, "settings put global window_animation_scale 0") != 1 {
		t.Fatalf("forward write missing: %v", f.calls)
	}
	if countExact(f.calls, "settings put global window_animation_scale "+shellQuote("1.0")) != 1 {
		t.Fatalf("partial key was not rolled back: %v", f.calls)
	}
}

func TestApplySettingsRollsBackPartialLayoutBoundsWrite(t *testing.T) {
	f := &fakeShell{
		answers: fullFakeAnswers(),
		fail: map[string]error{
			"service call activity 1599295570": errors.New("device offline"),
		},
	}
	failed, notRestored, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{"layoutBounds": true})
	if err == nil {
		t.Fatal("want an error")
	}
	if len(notRestored) != 0 {
		t.Fatalf("notRestored: %v", notRestored)
	}
	if !equalSlices(failed, []string{"layoutBounds"}) {
		t.Fatalf("failed = %v", failed)
	}
	if countExact(f.calls, "setprop debug.layout true") != 1 {
		t.Fatalf("forward setprop missing: %v", f.calls)
	}
	if countExact(f.calls, "setprop debug.layout "+shellQuote("false")) != 1 {
		t.Fatalf("setprop was not rolled back: %v", f.calls)
	}
}

func TestApplySettingsRollsBackPartialTalkBackWrite(t *testing.T) {
	f := &fakeShell{
		answers: map[string]string{
			"settings get secure enabled_accessibility_services": "null",
			"settings get secure accessibility_enabled":          "null",
			"pm path com.google.android.marvin.talkback":         "package:/data/app/talkback.apk",
		},
		fail: map[string]error{
			"settings put secure accessibility_enabled 1": errors.New("device offline"),
		},
	}
	failed, notRestored, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{"talkBack": true})
	if err == nil {
		t.Fatal("want an error")
	}
	if len(notRestored) != 0 {
		t.Fatalf("notRestored: %v", notRestored)
	}
	if !equalSlices(failed, []string{"talkBack"}) {
		t.Fatalf("failed = %v", failed)
	}
	want := "settings put secure enabled_accessibility_services " + shellQuote(talkBackComponent)
	if countExact(f.calls, want) != 1 {
		t.Fatalf("forward list write missing: %v", f.calls)
	}
	if countCalls(f.calls, "settings delete secure enabled_accessibility_services") != 1 {
		t.Fatalf("list was not rolled back to its prior (null) state: %v", f.calls)
	}
}

// item 2: rollback must run to completion even when the request context has
// already expired by the time a write fails.
func TestApplySettingsRollsBackAfterTheRequestContextHasExpired(t *testing.T) {
	f := &fakeShell{
		answers: fullFakeAnswers(),
		fail: map[string]error{
			"settings put system font_scale 1.2": errors.New("device offline"),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done() // guarantee it's already expired

	failed, notRestored, err := newFakeSettings(f).ApplySettings(ctx, map[string]any{
		"darkMode": true, "fontScale": 1.2,
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if len(notRestored) != 0 {
		t.Fatalf("notRestored: %v", notRestored)
	}
	if !equalSlices(failed, []string{"fontScale"}) {
		t.Fatalf("failed = %v", failed)
	}
	// The rollback of darkMode must still have run, despite the expired ctx.
	if countExact(f.calls, "cmd uimode night "+shellQuote("no")) != 1 {
		t.Fatalf("rollback did not run against an expired context: %v", f.calls)
	}
}

// item 2: a rollback write that itself fails is reported in notRestored
// rather than silently dropped.
func TestApplySettingsReportsNotRestoredWhenARollbackWriteFails(t *testing.T) {
	f := &fakeShell{
		answers: fullFakeAnswers(),
		fail: map[string]error{
			"settings put system font_scale 1.2":   errors.New("device offline"),
			"cmd uimode night " + shellQuote("no"): errors.New("device offline, still"),
		},
	}
	failed, notRestored, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{
		"darkMode": true, "fontScale": 1.2,
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if !equalSlices(failed, []string{"fontScale"}) {
		t.Fatalf("failed = %v", failed)
	}
	if !equalSlices(notRestored, []string{"darkMode"}) {
		t.Fatalf("notRestored = %v, want [darkMode]", notRestored)
	}
}

// item 8: rollback restores the exact raw prior string, not a bool-rounded
// value -- an "auto" dark mode and a 0.5 animation scale survive a rollback
// unchanged, and an unset font_scale restores via delete.
func TestApplySettingsRollbackRestoresRawValuesNotBoolRoundedOnes(t *testing.T) {
	f := &fakeShell{
		answers: map[string]string{
			"cmd uimode night":                               "Night mode: auto",
			"settings get system font_scale":                 "null",
			"settings get global window_animation_scale":     "0.5",
			"settings get global transition_animation_scale": "0.5",
			"settings get global animator_duration_scale":    "0.5",
		},
		fail: map[string]error{
			"settings put global animator_duration_scale 0": errors.New("device offline"),
		},
	}
	_, notRestored, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{
		"darkMode": true, "fontScale": 1.4, "reduceMotion": true,
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if len(notRestored) != 0 {
		t.Fatalf("notRestored: %v", notRestored)
	}
	if countExact(f.calls, "cmd uimode night "+shellQuote("auto")) != 1 {
		t.Fatalf("darkMode should roll back to auto, not no: %v", f.calls)
	}
	if countCalls(f.calls, "settings delete system font_scale") != 1 {
		t.Fatalf("fontScale should roll back via delete, not a put: %v", f.calls)
	}
	for _, key := range []string{"window_animation_scale", "transition_animation_scale"} {
		if countExact(f.calls, "settings put global "+key+" "+shellQuote("0.5")) != 1 {
			t.Fatalf("%s should roll back to 0.5: %v", key, f.calls)
		}
	}
}

// item 5: ApplySettings is serialized per ADBTarget via settingsMu. A fake
// adb binary sleeps 50ms and logs a start/end timestamp pair (keyed by its
// own subprocess PID, so two concurrent invocations are distinguishable)
// around every command it "runs". Two ApplySettings calls fired at the same
// ADBTarget from separate goroutines must show no overlap between their
// command intervals: the mutex must hold for the whole call, not just one
// command within it.
func TestADBTargetSerializesApplySettings(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/log.txt"
	script := "#!/bin/sh\n" +
		"echo \"$(date +%s%N) start $$\" >> " + shellQuote(logPath) + "\n" +
		"sleep 0.05\n" +
		"echo \"$(date +%s%N) end $$\" >> " + shellQuote(logPath) + "\n" +
		"echo 'Night mode: no'\n" +
		"exit 0\n"
	fakeAdb := dir + "/adb"
	if err := os.WriteFile(fakeAdb, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	target := NewADBTarget(ADB{Path: fakeAdb}, DeviceInfo{Serial: "fake-serial", State: "device"}, "", "")

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = target.ApplySettings(context.Background(), map[string]any{"darkMode": true})
		}()
	}
	wg.Wait()

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	type interval struct{ start, end int64 }
	var intervals []interval
	open := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("malformed log line: %q", line)
		}
		ts, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			t.Fatalf("parse timestamp: %v", err)
		}
		pid := fields[2]
		switch fields[1] {
		case "start":
			open[pid] = ts
		case "end":
			s, ok := open[pid]
			if !ok {
				t.Fatalf("end with no start for pid %s", pid)
			}
			intervals = append(intervals, interval{s, ts})
			delete(open, pid)
		}
	}
	if len(open) != 0 {
		t.Fatalf("%d commands never logged an end: %v", len(open), open)
	}
	if len(intervals) < 2 {
		t.Fatalf("expected at least 2 commands, got %d", len(intervals))
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start < intervals[j].start })
	for i := 1; i < len(intervals); i++ {
		if intervals[i].start < intervals[i-1].end {
			t.Fatalf("commands overlapped: %+v then %+v -- settingsMu did not serialize ApplySettings", intervals[i-1], intervals[i])
		}
	}
}

// item 6: prove -- not just assume -- that rollback runs on a context
// detached from the request (context.WithoutCancel) and bounded by its own
// timeout (rollbackTimeout), rather than the request's. honourCtx makes the
// fake actually fail a call made on an already-done context, and onFail
// cancels the parent the instant the triggering write fails, mid-ApplySettings,
// so darkMode's forward write completes first and its rollback runs after
// the parent is already canceled.
func TestApplySettingsRollbackRunsOnADetachedContextWithItsOwnTimeout(t *testing.T) {
	f := &fakeShell{
		answers:   fullFakeAnswers(),
		honourCtx: true,
		fail: map[string]error{
			"settings put system font_scale 1.2": errors.New("device offline"),
		},
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onFail = func(joined string) {
		if joined == "settings put system font_scale 1.2" {
			cancel() // simulate the request's own deadline lapsing right here
		}
	}

	failed, notRestored, err := newFakeSettings(f).ApplySettings(parent, map[string]any{
		"darkMode": true, "fontScale": 1.2,
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if !equalSlices(failed, []string{"fontScale"}) {
		t.Fatalf("failed = %v", failed)
	}
	if len(notRestored) != 0 {
		t.Fatalf("notRestored = %v -- rollback did not complete despite the parent being canceled", notRestored)
	}

	rollbackCmd := "cmd uimode night " + shellQuote("no")
	snap, ok := f.ctxSnapshotOfCall[rollbackCmd]
	if !ok {
		t.Fatalf("rollback command never ran: %v", f.calls)
	}
	t.Logf("calls: %v", f.calls)
	t.Logf("parent.Err() = %v", parent.Err())
	// The snapshot is taken at call time, inside run(), before rollback's own
	// bounded context has had a chance to be canceled by its "defer cancel()"
	// -- which fires the instant rollback() returns, and would read back as
	// "canceled" regardless of whether the parent's cancellation ever reached
	// it. Reading rollbackCtx.Err() *after* ApplySettings returns proves
	// nothing either way, which is why the fake records this at the time.
	if snap.err != nil {
		t.Fatalf("rollback ran on a context that had already seen the parent's cancellation (%v) -- it is not detached", snap.err)
	}
	if !snap.hasDeadline {
		t.Fatal("rollback ran on a context with no deadline -- it has no timeout of its own")
	}
	if snap.remaining <= 0 || snap.remaining > rollbackTimeout {
		t.Fatalf("rollback deadline remaining %v is not within (0, rollbackTimeout] of call time", snap.remaining)
	}
}

// splitSections must recognise a marker line even when the transport ran it
// through a pty and appended a trailing CR to every line -- an older adb
// path does this, and without trimming it every GET would read as
// incomplete (every marker line would fail to match) and 502.
func TestSplitSectionsTrimsATrailingCROnEveryLine(t *testing.T) {
	nonce := "abc123"
	var b strings.Builder
	b.WriteString(sectionMarkerLine(nonce, 0) + "\r\n")
	b.WriteString("first section body\r\n")
	b.WriteString(sectionMarkerLine(nonce, 1) + "\r\n")
	b.WriteString(sectionMarkerLine(nonce, 2) + "\r\n")

	sections, complete := splitSections(b.String(), nonce, 2)
	if !complete {
		t.Fatalf("complete = false, want true (CRLF should not hide the terminal marker)")
	}
	if len(sections) != 2 || strings.TrimSpace(sections[0]) != "first section body" {
		t.Fatalf("sections = %#v", sections)
	}
}

func TestSplitSectionsWithoutCRStillWorks(t *testing.T) {
	nonce := "def456"
	out := sectionMarkerLine(nonce, 0) + "\n" +
		"body\n" +
		sectionMarkerLine(nonce, 1) + "\n"
	sections, complete := splitSections(out, nonce, 1)
	if !complete || sections[0] != "body" {
		t.Fatalf("sections = %#v complete = %v", sections, complete)
	}
}

// TestSplitSectionsRejectsAWholeLineMarkerWithTheWrongNonce reproduces the
// exact spoof the doc comment above newNonce describes: a device value
// (e.g. an app's versionName, or an entry in a services list) that is an
// entire line matching the marker shape but under a different nonce than
// this call's. Without a nonce check, a whole-line match alone would accept
// it as the next boundary and corrupt the section split.
func TestSplitSectionsRejectsAWholeLineMarkerWithTheWrongNonce(t *testing.T) {
	nonce := "aaaaaaaaaaaaaaaa"
	forged := "bbbbbbbbbbbbbbbb"
	out := sectionMarkerLine(nonce, 0) + "\n" +
		sectionMarkerLine(forged, 1) + "\n" + // forged boundary, wrong nonce
		"real body\n" +
		sectionMarkerLine(nonce, 1) + "\n"
	sections, complete := splitSections(out, nonce, 1)
	if !complete {
		t.Fatalf("complete = false, want true")
	}
	want := sectionMarkerLine(forged, 1) + "\nreal body"
	if sections[0] != want {
		t.Fatalf("sections[0] = %q, want %q (forged marker under a different nonce must read as body)", sections[0], want)
	}
}

// TestSplitSectionsRejectsARepeatedIndex covers the other half of the spoof:
// a device value that repeats an index this call has already consumed (real
// or forged) must not reopen that section or advance past the terminal
// marker a second time.
func TestSplitSectionsRejectsARepeatedIndex(t *testing.T) {
	nonce := "cccccccccccccccc"
	out := sectionMarkerLine(nonce, 0) + "\n" +
		"first body\n" +
		sectionMarkerLine(nonce, 0) + "\n" + // repeat of an already-consumed index
		"injected body\n" +
		sectionMarkerLine(nonce, 1) + "\n"
	sections, complete := splitSections(out, nonce, 1)
	if !complete {
		t.Fatalf("complete = false, want true")
	}
	want := "first body\n" + sectionMarkerLine(nonce, 0) + "\ninjected body"
	if sections[0] != want {
		t.Fatalf("sections[0] = %q, want %q (a repeated index must not reopen the section)", sections[0], want)
	}
}

// TestSplitSectionsRejectsAMarkerWithTrailingText covers a third spoof
// shape: a device value whose line starts with a valid marker for the next
// expected index but does not end there -- e.g. a services list entry that
// happens to be "<marker> plus garbage". A HasPrefix (or any prefix-style)
// match would treat this as the real boundary and drop everything after the
// marker text on that line into the boundary itself, corrupting the split.
// Only exact whole-line equality is correct.
func TestSplitSectionsRejectsAMarkerWithTrailingText(t *testing.T) {
	nonce := "dddddddddddddddd"
	out := sectionMarkerLine(nonce, 0) + "\n" +
		sectionMarkerLine(nonce, 1) + " trailing junk\n" + // marker as a prefix, not the whole line
		"real body\n" +
		sectionMarkerLine(nonce, 1) + "\n"
	sections, complete := splitSections(out, nonce, 1)
	if !complete {
		t.Fatalf("complete = false, want true")
	}
	want := sectionMarkerLine(nonce, 1) + " trailing junk\nreal body"
	if sections[0] != want {
		t.Fatalf("sections[0] = %q, want %q (a marker followed by trailing text must read as body, not as the boundary)", sections[0], want)
	}
}

// TestNewNonceHasSubstantialEntropy pins the 8-byte/16-hex-character nonce
// newNonce's doc comment promises. A 1-byte nonce would still compile and
// pass every other test here, since none of them assert length -- only 256
// possible values stands between a forged line and a correct guess.
func TestNewNonceHasSubstantialEntropy(t *testing.T) {
	nonce, err := newNonce()
	if err != nil {
		t.Fatalf("newNonce: %v", err)
	}
	if len(nonce) < 16 {
		t.Fatalf("len(nonce) = %d, want at least 16 hex characters (8 bytes)", len(nonce))
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		t.Fatalf("nonce %q is not valid hex: %v", nonce, err)
	}
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		n, err := newNonce()
		if err != nil {
			t.Fatalf("newNonce: %v", err)
		}
		if seen[n] {
			t.Fatalf("newNonce produced a repeat across 100 calls: %q", n)
		}
		seen[n] = true
	}
}

// notReadyTarget builds an ADBTarget whose DeviceInfo.Ready() is false, so
// Settings, ApplySettings and ForegroundApp all take their pre-flight
// rejection path without ever touching adb.
func notReadyTarget() *ADBTarget {
	return NewADBTarget(ADB{}, DeviceInfo{Serial: "offline-1", State: "offline"}, "", "")
}

func TestSettingsWrapsErrDeviceUnreachableWhenNotReady(t *testing.T) {
	_, err := notReadyTarget().Settings(context.Background())
	if !errors.Is(err, ErrDeviceUnreachable) {
		t.Fatalf("Settings() err = %v, want errors.Is(err, ErrDeviceUnreachable)", err)
	}
}

// TestApplySettingsTalkBackPreflightAdbFailureIsDeviceUnreachableNotInvalid
// pins the distinction talkBackSupported's doc comment draws: a script that
// never completes (adb itself failing to reach the device) must not be
// reported the same way as a script that completed and found `pm path`
// answering without "package:" (TalkBack genuinely absent). The first is a
// 502 the caller should retry; the second is a 400 it should not.
func TestApplySettingsTalkBackPreflightAdbFailureIsDeviceUnreachableNotInvalid(t *testing.T) {
	f := &fakeShell{breakScript: true}
	_, _, err := newFakeSettings(f).ApplySettings(context.Background(), map[string]any{"talkBack": true})
	if !errors.Is(err, ErrDeviceUnreachable) {
		t.Fatalf("ApplySettings() err = %v, want errors.Is(err, ErrDeviceUnreachable)", err)
	}
	if errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("ApplySettings() err = %v, must not also be ErrInvalidSetting", err)
	}
}

func TestApplySettingsWrapsErrDeviceUnreachableWhenNotReady(t *testing.T) {
	_, _, err := notReadyTarget().ApplySettings(context.Background(), map[string]any{"darkMode": true})
	if !errors.Is(err, ErrDeviceUnreachable) {
		t.Fatalf("ApplySettings() err = %v, want errors.Is(err, ErrDeviceUnreachable)", err)
	}
}
