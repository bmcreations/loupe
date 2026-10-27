package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeStub creates an executable POSIX-sh stub named name in dir. body is
// the script content, without a shebang.
func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	content := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

// execRealScript runs script through a real POSIX sh, with dir prepended to
// PATH so stub binaries placed there shadow the real settings/getprop/
// dumpsys/pm/pidof/cmd. This is what settingsScript, appScript and
// talkBackSupported's preflight actually run through in production -- a
// fakeShell that fabricates whatever markers it is asked for can never
// notice a missing terminal echo, a mis-ordered pipeline, or a broken
// pipeline, because it never executes the script text at all.
func execRealScript(dir, script string) (string, error) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		return "", fmt.Errorf("real-shell test requires sh on PATH: %w", err)
	}
	cmd := exec.Command(shPath, "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, cmdErr := cmd.CombinedOutput()
	return string(out), cmdErr
}

// runRealScript is execRealScript for callers inside a test.
func runRealScript(t *testing.T, dir, script string) (string, error) {
	t.Helper()
	return execRealScript(dir, script)
}

// item 1: settingsScript's terminal echo must mask pm path's exit status.
// Run the real, unmodified production script through a real shell with a
// pm stub that fails exactly the way a device without TalkBack does.
func TestSettingsScriptRealShellSurvivesFailingPmPath(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "cmd", `echo "Night mode: no"`)
	writeStub(t, dir, "settings", `case "$2 $3" in
  "system font_scale") echo null ;;
  "global window_animation_scale") echo 1.0 ;;
  "global transition_animation_scale") echo 1.0 ;;
  "global animator_duration_scale") echo 1.0 ;;
  "secure enabled_accessibility_services") echo null ;;
  *) echo null ;;
esac`)
	writeStub(t, dir, "getprop", `echo false`)
	writeStub(t, dir, "pm", `exit 1`)

	nonce := "deadbeefcafef00d"
	script := settingsScript(nonce, false)
	out, err := runRealScript(t, dir, script)
	if err != nil {
		t.Fatalf("real sh reported an error even though the terminal echo should mask pm path's exit 1: %v\noutput:\n%s", err, out)
	}
	sec, complete := splitSections(out, nonce, 8)
	if !complete {
		t.Fatalf("script did not complete against a real shell:\n%s", out)
	}
	if got := strings.TrimSpace(sec[7]); got != "" {
		t.Errorf("pm path section = %q, want empty (pm exited 1 and printed nothing)", got)
	}
}

// item 1: output genuinely cut off before the terminal marker (e.g. adb
// dropping the connection mid-script) must be reported as incomplete, not
// silently parsed as whatever sections happened to arrive.
func TestSettingsScriptRealShellTruncatedBeforeTerminalMarker(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "cmd", `echo "Night mode: no"`)
	writeStub(t, dir, "settings", `echo null`)
	writeStub(t, dir, "getprop", `echo false`)
	writeStub(t, dir, "pm", `echo package:/data/app/talkback/base.apk`)

	nonce := "deadbeefcafef00d"
	full, err := runRealScript(t, dir, settingsScript(nonce, false))
	if err != nil {
		t.Fatalf("baseline real-sh run failed: %v", err)
	}
	// Truncate right before the terminal marker line to simulate a
	// connection dropped mid-script -- this is real output from a real
	// script, just cut short the way a dropped adb connection would.
	term := sectionMarkerLine(nonce, 8)
	cut := strings.Index(full, term)
	if cut < 0 {
		t.Fatalf("terminal marker not found in baseline output:\n%s", full)
	}
	truncated := full[:cut]

	_, complete := splitSections(truncated, nonce, 8)
	if complete {
		t.Fatalf("truncated output (cut before the terminal marker) reported complete, want incomplete")
	}
}

// realShellFunc adapts runRealScript to the shellFunc shape ForegroundApp
// and the settings logic call through, joining args back into the one
// script string every caller here actually passes.
func realShellFunc(dir string) shellFunc {
	return func(_ context.Context, args ...string) (string, error) {
		return execRealScript(dir, strings.Join(args, " "))
	}
}

// item 1 / item 4: appScript scopes PKG resolution to Display #0's own
// section using sed before grep. Run the real pipeline, end to end through
// androidApp.ForegroundApp, against a fixture with Display #0 printed last,
// first, as the only display, and missing, to pin both the scoping and the
// sed-then-grep ordering (mutation 4b: swapping sed and grep breaks the
// scoping, since grep -o 'topResumedActivity=...' would then match anywhere
// in the whole dump before sed ever narrowed it to display #0).
func TestAppScriptRealShellScopesPKGToDisplayZero(t *testing.T) {
	cases := []struct {
		name    string
		dump    string
		wantPKG string
	}{
		{
			name: "display zero last",
			dump: `Display #1 (activities from top to bottom):
    topResumedActivity=ActivityRecord{aaa u0 com.example.other/.OtherActivity t1}
Display #0 (activities from top to bottom):
    topResumedActivity=ActivityRecord{bbb u0 com.example.main/.MainActivity t2}
`,
			wantPKG: "com.example.main",
		},
		{
			name: "display zero first",
			dump: `Display #0 (activities from top to bottom):
    topResumedActivity=ActivityRecord{bbb u0 com.example.main/.MainActivity t2}
Display #1 (activities from top to bottom):
    topResumedActivity=ActivityRecord{aaa u0 com.example.other/.OtherActivity t1}
`,
			wantPKG: "com.example.main",
		},
		{
			name: "display zero only",
			dump: `Display #0 (activities from top to bottom):
    topResumedActivity=ActivityRecord{bbb u0 com.example.main/.MainActivity t2}
`,
			wantPKG: "com.example.main",
		},
		{
			name: "display zero missing",
			dump: `Display #1 (activities from top to bottom):
    topResumedActivity=ActivityRecord{aaa u0 com.example.other/.OtherActivity t1}
`,
			wantPKG: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A marker file records which $PKG value, if any, reached
			// dumpsys package -- this is what proves scoping happened
			// shell-side, not just in Go's own resumedActivity parse.
			marker := filepath.Join(dir, "pkg-seen")
			writeStub(t, dir, "dumpsys", `if [ "$1 $2" = "activity activities" ]; then
  cat <<'DUMPEOF'
`+tc.dump+`DUMPEOF
elif [ "$1" = "package" ]; then
  echo "$2" > `+marker+`
  echo ""
fi`)
			writeStub(t, dir, "pm", `echo ""`)
			writeStub(t, dir, "pidof", `echo ""`)

			out, err := runRealScript(t, dir, appScript("deadbeefcafef00d"))
			if err != nil {
				t.Fatalf("real sh error: %v\noutput:\n%s", err, out)
			}
			gotPKG := ""
			if b, rerr := os.ReadFile(marker); rerr == nil {
				gotPKG = strings.TrimSpace(string(b))
			}
			if gotPKG != tc.wantPKG {
				t.Errorf("$PKG resolved to %q, want %q\nscript output:\n%s", gotPKG, tc.wantPKG, out)
			}
		})
	}
}

// item 1: a failing pm path (TalkBack not installed) run through the real
// preflight script used by talkBackSupported must still report "not
// installed" via a completed script, not an adb-level error -- pm path on
// an absent package legitimately exits nonzero.
func TestTalkBackSupportedRealShellFailingPmPath(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "pm", `exit 1`)

	supported, reason, err := talkBackSupported(context.Background(), realShellFunc(dir))
	if err != nil {
		t.Fatalf("talkBackSupported: %v, want no error (pm path exiting 1 with no output is 'not installed', not unreachable)", err)
	}
	if supported {
		t.Errorf("supported = true, want false")
	}
	if reason == "" {
		t.Errorf("reason is empty, want a not-installed explanation")
	}
}

// item 3: a component naming an inner class (Svc$Inner) reaching the shell
// unquoted would have "$Inner" expanded away by a real shell, since $ is
// live in an unquoted argument -- silently truncating the restored service
// list. Round both readRestoreTalkBack's restore function and writeTalkBack
// through a real sh and require the value that actually reaches `settings
// put` is byte-identical to what was read, proving shellQuote is what makes
// it survive.
func TestTalkBackRollbackQuotesAnInnerClassServiceThroughRealShell(t *testing.T) {
	dir := t.TempDir()
	listOut := filepath.Join(dir, "list.out")
	enabledOut := filepath.Join(dir, "enabled.out")
	rawList := "com.other/com.other.Svc:com.x/com.x.Svc$Inner"

	writeStub(t, dir, "settings", `case "$1 $2 $3" in
  "get secure enabled_accessibility_services") echo '`+rawList+`' ;;
  "get secure accessibility_enabled") echo 1 ;;
  "put secure enabled_accessibility_services") printf '%s' "$4" > "`+listOut+`" ;;
  "put secure accessibility_enabled") printf '%s' "$4" > "`+enabledOut+`" ;;
  *) echo null ;;
esac`)

	sh := realShellFunc(dir)
	restore, err := readRestoreTalkBack(context.Background(), sh)
	if err != nil {
		t.Fatalf("readRestoreTalkBack: %v", err)
	}
	if err := restore(context.Background(), sh); err != nil {
		t.Fatalf("restore: %v", err)
	}

	gotList, err := os.ReadFile(listOut)
	if err != nil {
		t.Fatalf("enabled_accessibility_services was never written by the real shell: %v", err)
	}
	if string(gotList) != rawList {
		t.Fatalf("enabled_accessibility_services rollback = %q, want byte-identical %q (a real shell expanded $Inner, so the value reached it unquoted)", gotList, rawList)
	}

	gotEnabled, err := os.ReadFile(enabledOut)
	if err != nil {
		t.Fatalf("accessibility_enabled was never written by the real shell: %v", err)
	}
	if string(gotEnabled) != "1" {
		t.Fatalf("accessibility_enabled rollback = %q, want %q", gotEnabled, "1")
	}
}

// item 3, forward direction: writeTalkBack's own `settings put` for the
// enabled_accessibility_services list must survive the same inner-class
// component when adding it to an existing list.
func TestWriteTalkBackQuotesAnInnerClassServiceThroughRealShell(t *testing.T) {
	dir := t.TempDir()
	listOut := filepath.Join(dir, "list.out")
	existing := "com.other/com.other.Svc:com.y/com.y.Svc$Inner"

	writeStub(t, dir, "settings", `case "$1 $2 $3" in
  "get secure enabled_accessibility_services") echo '`+existing+`' ;;
  "put secure enabled_accessibility_services") printf '%s' "$4" > "`+listOut+`" ;;
  "put secure accessibility_enabled") exit 0 ;;
  *) echo null ;;
esac`)

	sh := realShellFunc(dir)
	if err := writeTalkBack(context.Background(), sh, true); err != nil {
		t.Fatalf("writeTalkBack: %v", err)
	}

	got, err := os.ReadFile(listOut)
	if err != nil {
		t.Fatalf("enabled_accessibility_services was never written: %v", err)
	}
	want := withService(existing, talkBackComponent)
	if string(got) != want {
		t.Fatalf("enabled_accessibility_services write = %q, want byte-identical %q", got, want)
	}
}

// item 2 (round 3): accessibility_enabled's own rollback write is a second,
// separate shellQuote call from the list's -- "1" alone, the only value the
// forward write ever sends, has no shell metacharacters, so a test using it
// cannot distinguish shellQuote(rawEnabled) from rawEnabled unquoted. Reading
// the value back as "1$X" (a value only reachable by round-tripping whatever
// the device itself reports, never by this package's own forward writes)
// forces a real shell to prove the rollback quotes it.
func TestTalkBackAccessibilityEnabledRollbackQuotesAMetacharacterValue(t *testing.T) {
	dir := t.TempDir()
	enabledOut := filepath.Join(dir, "enabled.out")
	raw := "1$X"

	writeStub(t, dir, "settings", `case "$1 $2 $3" in
  "get secure enabled_accessibility_services") echo null ;;
  "get secure accessibility_enabled") echo '`+raw+`' ;;
  "put secure accessibility_enabled") printf '%s' "$4" > "`+enabledOut+`" ;;
  *) : ;;
esac`)

	sh := realShellFunc(dir)
	restore, err := readRestoreTalkBack(context.Background(), sh)
	if err != nil {
		t.Fatalf("readRestoreTalkBack: %v", err)
	}
	if err := restore(context.Background(), sh); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got, err := os.ReadFile(enabledOut)
	if err != nil {
		t.Fatalf("accessibility_enabled was never written by the real shell: %v", err)
	}
	if string(got) != raw {
		t.Fatalf("accessibility_enabled rollback = %q, want byte-identical %q (a real shell expanded $X, so the value reached it unquoted)", got, raw)
	}
}

// item 1 (round 3): end-to-end ForegroundApp through a real shell, so a
// missing terminal echo in appScript (which makes every /api/app return 502
// in production) is caught the way TestAppScriptRealShellScopesPKGToDisplayZero
// cannot -- that test reads appScript's output via a marker file and never
// calls splitSections or ForegroundApp at all.
func TestForegroundAppRealShellEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "dumpsys", `if [ "$1 $2" = "activity activities" ]; then
  cat <<'DUMPEOF'
Display #0 (activities from top to bottom):
    topResumedActivity=ActivityRecord{bbb u0 com.example.main/.MainActivity t2}
DUMPEOF
elif [ "$1" = "package" ]; then
  printf '  Package [com.example.main] (abc):\n    versionCode=7 minSdk=21\n    versionName=1.2.3\n'
fi`)
	writeStub(t, dir, "pm", `echo package:/data/app/x/base.apk`)
	writeStub(t, dir, "pidof", `echo 4242`)

	app, err := (&androidApp{sh: realShellFunc(dir)}).ForegroundApp(context.Background())
	if err != nil {
		t.Fatalf("ForegroundApp: %v", err)
	}
	if app.Package != "com.example.main" || app.VersionName != "1.2.3" || app.PID != 4242 || app.APKPath == "" {
		t.Fatalf("app = %+v", app)
	}
}

// item 1 (round 5): a dump with no Display #0 section at all takes
// appScript's else branch, where $PKG resolves empty and the follow-up
// commands are skipped entirely. Go's own resumedActivity still falls back
// to an unscoped match (see displayZeroSection), so Package/Activity must
// still come back correctly with no error -- only VersionName/PID/APKPath
// stay empty, the same as an unreachable pm path.
//
// This is what TestAppScriptRealShellScopesPKGToDisplayZero's "display zero
// missing" case cannot catch: that case only inspects the marker file
// dumpsys package writes, and never calls splitSections or ForegroundApp, so
// it cannot see the else branch's own section echoes going missing (9i) or
// the terminal echo being pulled out of the unconditional tail into the then
// branch alone (9k) -- both leave "display zero missing" a real device would
// need to hit a script that never completes, while this test's real sh
// invocation does complete and must parse.
func TestForegroundAppRealShellNoDisplayZero(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "dumpsys", `if [ "$1 $2" = "activity activities" ]; then
  cat <<'DUMPEOF'
Display #1 (activities from top to bottom):
    topResumedActivity=ActivityRecord{aaa u0 com.example.other/.OtherActivity t1}
DUMPEOF
fi`)
	writeStub(t, dir, "pm", `echo package:/x`)
	writeStub(t, dir, "pidof", `echo 1`)

	app, err := (&androidApp{sh: realShellFunc(dir)}).ForegroundApp(context.Background())
	if err != nil {
		t.Fatalf("ForegroundApp: %v, want success (Go's own parse still finds the unscoped activity)", err)
	}
	if app.Package != "com.example.other" || app.Activity != "com.example.other.OtherActivity" {
		t.Fatalf("app = %+v, want package com.example.other activity com.example.other.OtherActivity", app)
	}
	if app.VersionName != "" || app.PID != 0 || app.APKPath != "" {
		t.Fatalf("app = %+v, want VersionName/PID/APKPath left empty since $PKG never resolved", app)
	}
}

// item 1 (round 3): a failing pm path must not fail the whole request --
// Package/Activity/VersionName/PID are answered from dumpsys and pidof
// regardless, and only APKPath is left empty.
func TestForegroundAppRealShellFailingPmPath(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "dumpsys", `if [ "$1 $2" = "activity activities" ]; then
  cat <<'DUMPEOF'
Display #0 (activities from top to bottom):
    topResumedActivity=ActivityRecord{bbb u0 com.example.main/.MainActivity t2}
DUMPEOF
elif [ "$1" = "package" ]; then
  printf '  Package [com.example.main] (abc):\n    versionCode=7 minSdk=21\n    versionName=1.2.3\n'
fi`)
	writeStub(t, dir, "pm", `exit 1`)
	writeStub(t, dir, "pidof", `echo 4242`)

	app, err := (&androidApp{sh: realShellFunc(dir)}).ForegroundApp(context.Background())
	if err != nil {
		t.Fatalf("ForegroundApp: %v, want success (pm path failing must not fail the whole request)", err)
	}
	if app.Package != "com.example.main" || app.VersionName != "1.2.3" || app.PID != 4242 {
		t.Fatalf("app = %+v", app)
	}
	if app.APKPath != "" {
		t.Fatalf("APKPath = %q, want empty since pm path failed", app.APKPath)
	}
}

// item 1 (round 3): output cut off before appScript's terminal marker --
// simulating a connection dropped mid-script the way
// TestSettingsScriptRealShellTruncatedBeforeTerminalMarker does for
// settingsScript -- must fail ForegroundApp rather than parsing whatever
// sections happened to arrive.
func TestForegroundAppRealShellTruncatedBeforeTerminalMarker(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "dumpsys", `if [ "$1 $2" = "activity activities" ]; then
  cat <<'DUMPEOF'
Display #0 (activities from top to bottom):
    topResumedActivity=ActivityRecord{bbb u0 com.example.main/.MainActivity t2}
DUMPEOF
elif [ "$1" = "package" ]; then
  printf '  Package [com.example.main] (abc):\n    versionCode=7 minSdk=21\n    versionName=1.2.3\n'
fi`)
	writeStub(t, dir, "pm", `echo package:/data/app/x/base.apk`)
	writeStub(t, dir, "pidof", `echo 4242`)

	nonce := "cafebabedeadbeef"
	full, err := runRealScript(t, dir, appScript(nonce))
	if err != nil {
		t.Fatalf("baseline real-sh run failed: %v", err)
	}
	term := sectionMarkerLine(nonce, 4)
	cut := strings.Index(full, term)
	if cut < 0 {
		t.Fatalf("terminal marker not found in baseline output:\n%s", full)
	}
	truncated := full[:cut]

	sec, complete := splitSections(truncated, nonce, 4)
	if complete {
		t.Fatalf("truncated app output (cut before the terminal marker) reported complete: %v", sec)
	}
}

// item 3 (round 3): a correct marker line embedded as a substring inside a
// longer line -- not as the whole line -- must not be treated as a section
// boundary. splitSections compares a trimmed line for exact equality against
// the expected marker; a mutation to substring/Contains matching would let
// "junk " + marker reopen the section early.
func TestSplitSectionsRejectsAMarkerEmbeddedInALongerLine(t *testing.T) {
	nonce := "0123456789abcdef"
	out := sectionMarkerLine(nonce, 0) + "\n" +
		"junk " + sectionMarkerLine(nonce, 1) + "\n" +
		"real body\n" +
		sectionMarkerLine(nonce, 1) + "\n"
	sec, complete := splitSections(out, nonce, 1)
	if !complete {
		t.Fatalf("complete = false, want true")
	}
	want := "junk " + sectionMarkerLine(nonce, 1) + "\nreal body"
	if sec[0] != want {
		t.Fatalf("sections[0] = %q, want %q (a marker embedded in a longer line must not be treated as a boundary)", sec[0], want)
	}
}

// item 4 (round 3): the marker, not the shell's exit status, is what proves
// a script completed. A fake that hands back complete output alongside a
// non-nil error (a stray printf to stderr, a warning exit code from a
// command the script's later steps recovered from) must still succeed in
// Settings, the talkBack preflight and ForegroundApp alike.
func TestMarkerNotExitStatusGovernsCompletion(t *testing.T) {
	t.Run("Settings", func(t *testing.T) {
		f := &fakeShell{answers: fullFakeAnswers(), errAlongsideComplete: errors.New("stray warning")}
		if _, err := newFakeSettings(f).Settings(context.Background()); err != nil {
			t.Fatalf("Settings: %v, want success (complete output with a non-nil shell error must still parse)", err)
		}
	})
	t.Run("talkBackSupported", func(t *testing.T) {
		f := &fakeShell{answers: map[string]string{"pm path " + talkBackPackage: "package:/data/app/talkback/base.apk"}}
		wrapped := func(ctx context.Context, args ...string) (string, error) {
			out, _ := f.run(ctx, args...)
			return out, errors.New("stray warning")
		}
		supported, _, err := talkBackSupported(context.Background(), wrapped)
		if err != nil {
			t.Fatalf("talkBackSupported: %v, want success (a complete, prefix-matching pm path answer alongside a non-nil error must still report supported)", err)
		}
		if !supported {
			t.Errorf("supported = false, want true")
		}
	})
	t.Run("ForegroundApp", func(t *testing.T) {
		f := &appFakeShell{
			sections:             []string{appDumpFixture, "", "", ""},
			errAlongsideComplete: errors.New("stray warning"),
		}
		app, err := (&androidApp{sh: f.run}).ForegroundApp(context.Background())
		if err != nil {
			t.Fatalf("ForegroundApp: %v, want success (complete output with a non-nil shell error must still parse)", err)
		}
		if app.Package == "" {
			t.Fatalf("app = %+v", app)
		}
	})
}
