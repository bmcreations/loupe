package target

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// appFakeShell answers the single combined appScript() call with a
// nonce-marked response, the same shape settings_test.go's fakeShell uses
// for settingsScript(). Any other call is recorded but unanswered, so a test
// can assert app.go never falls back to per-field shell calls (item 11).
type appFakeShell struct {
	sections []string // dump, package dump, pidof, pm path
	calls    []string

	// errAlongsideComplete makes run return this error alongside otherwise
	// complete, well-formed script output. See the identical field on
	// fakeShell in settings_test.go for why: completion is judged by the
	// terminal marker, not by this error.
	errAlongsideComplete error
}

func (f *appFakeShell) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, joinArgs(args))
	if len(args) != 1 {
		return "", nil
	}
	nonce, ok := extractNonce(args[0])
	if !ok {
		return "", nil
	}
	resp, _ := nonceFakeResponse(nonce, f.sections)
	return resp, f.errAlongsideComplete
}

// extractNonce pulls the per-call nonce out of a script built with
// sectionEcho, by finding the first marker line's nonce field. Both
// app_test.go and settings_test.go build fakes this way, so this is the one
// place either needs to parse a script rather than merely answer it.
func extractNonce(script string) (string, bool) {
	i := strings.Index(script, settingsSectionMarker)
	if i < 0 {
		return "", false
	}
	rest := script[i+len(settingsSectionMarker):]
	j := strings.Index(rest, ":")
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// nonceFakeResponse builds the section-marked output splitSections expects:
// one marker + body per section, plus the terminal marker at index
// len(sections).
func nonceFakeResponse(nonce string, sections []string) (string, bool) {
	var b strings.Builder
	for i, s := range sections {
		b.WriteString(sectionMarkerLine(nonce, i))
		b.WriteString("\n")
		if s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	b.WriteString(sectionMarkerLine(nonce, len(sections)))
	b.WriteString("\n")
	return b.String(), true
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return string(b)
}

func TestResumedActivityReadsTheTopResumedRecord(t *testing.T) {
	dump := readTestdata(t, "dumpsys-activities.txt")
	pkg, activity, ok := resumedActivity(dump)
	if !ok {
		t.Fatalf("resumedActivity: ok = false, want true")
	}
	if pkg != "com.android.settings" {
		t.Errorf("pkg = %q, want com.android.settings", pkg)
	}
	if activity != "com.android.settings.homepage.SettingsHomepageActivity" {
		t.Errorf("activity = %q, want com.android.settings.homepage.SettingsHomepageActivity", activity)
	}
}

// A paused record from another task prints before the resumed one in this
// fixture (trap 39's shape: the dump carries a decoy that happens to sort
// first). The parser must key on "topResumedActivity=" rather than the first
// ActivityRecord{...} it finds.
func TestResumedActivityIgnoresAPausedRecordThatPrintsFirst(t *testing.T) {
	dump := readTestdata(t, "dumpsys-activities-paused-first.txt")
	pkg, activity, ok := resumedActivity(dump)
	if !ok {
		t.Fatalf("resumedActivity: ok = false, want true")
	}
	if pkg != "com.example.resumed" {
		t.Errorf("pkg = %q, want com.example.resumed (not the paused one)", pkg)
	}
	if activity != "com.example.resumed.HomeActivity" {
		t.Errorf("activity = %q, want com.example.resumed.HomeActivity", activity)
	}
}

// item 15 / trap 44: a non-zero display's section prints first, and its
// topResumedActivity must not win just because it appears earlier in the
// dump. Display #0 is what scrcpy streams by default.
func TestResumedActivityPrefersDisplayZeroWhenAnotherDisplayPrintsFirst(t *testing.T) {
	dump := readTestdata(t, "dumpsys-activities-two-displays.txt")
	pkg, activity, ok := resumedActivity(dump)
	if !ok {
		t.Fatalf("resumedActivity: ok = false, want true")
	}
	if pkg != "com.example.main" {
		t.Errorf("pkg = %q, want com.example.main (display #0, not display #1)", pkg)
	}
	if activity != "com.example.main.MainActivity" {
		t.Errorf("activity = %q, want com.example.main.MainActivity", activity)
	}
}

func TestResumedActivityReportsNotOKWithNoMatch(t *testing.T) {
	_, _, ok := resumedActivity("nothing useful here")
	if ok {
		t.Fatalf("resumedActivity: ok = true, want false")
	}
}

func TestPackageVersionReadsTheBlockNamingPkg(t *testing.T) {
	dump := readTestdata(t, "dumpsys-package.txt")
	name, code, ok := packageVersion(dump, "com.android.settings")
	if !ok {
		t.Fatalf("packageVersion: ok = false, want true")
	}
	if name != "17" {
		t.Errorf("name = %q, want 17", name)
	}
	if code != 37 {
		t.Errorf("code = %d, want 37", code)
	}
}

// An updated system app prints a second Package [ block under "Hidden system
// packages:" carrying the factory version (trap 39's shape again). The first
// block naming pkg, not the hidden one, is the version actually running.
func TestPackageVersionIgnoresTheHiddenSystemPackageBlock(t *testing.T) {
	dump := readTestdata(t, "dumpsys-package-hidden-decoy.txt")
	name, code, ok := packageVersion(dump, "com.example.app")
	if !ok {
		t.Fatalf("packageVersion: ok = false, want true")
	}
	if name != "1.2.3" {
		t.Errorf("name = %q, want 1.2.3 (the first block, not the hidden factory one)", name)
	}
	if code != 5 {
		t.Errorf("code = %d, want 5", code)
	}
}

func TestPackageVersionReportsNotOKWithNoMatch(t *testing.T) {
	_, _, ok := packageVersion("nothing useful here", "com.example.app")
	if ok {
		t.Fatalf("packageVersion: ok = true, want false")
	}
}

// item 4: a dump whose only Package [ block names a different package than
// pkg must not be read as pkg's version.
func TestPackageVersionRejectsABlockNamingADifferentPackage(t *testing.T) {
	dump := readTestdata(t, "dumpsys-package.txt")
	_, _, ok := packageVersion(dump, "com.other.app")
	if ok {
		t.Fatalf("packageVersion: ok = true, want false (block names a different package)")
	}
}

func TestAPKPathStripsThePackagePrefix(t *testing.T) {
	got := apkPath("package:/system_ext/priv-app/SettingsGoogle/SettingsGoogle.apk\n")
	want := "/system_ext/priv-app/SettingsGoogle/SettingsGoogle.apk"
	if got != want {
		t.Errorf("apkPath = %q, want %q", got, want)
	}
}

// A split APK reports one line per split; only the first is the base.
func TestAPKPathTakesOnlyTheFirstLine(t *testing.T) {
	got := apkPath("package:/data/app/com.example/base.apk\npackage:/data/app/com.example/split_config.apk\n")
	want := "/data/app/com.example/base.apk"
	if got != want {
		t.Errorf("apkPath = %q, want %q", got, want)
	}
}

const appDumpFixture = `ACTIVITY MANAGER ACTIVITIES (dumpsys activity activities)
Display #0 (activities from top to bottom):
  * Task{e3489ae #2394 type=standard A=1000:com.example.app.root U=0 visible=true}
    topResumedActivity=ActivityRecord{72528425 u0 com.example.app/.MainActivity t2394}
    * Hist  #0: ActivityRecord{72528425 u0 com.example.app/.MainActivity t2394}
`

const appPackageDumpFixture = `Packages:
  Package [com.example.app] (abc123):
    versionCode=42 minSdk=21 targetSdk=34
    versionName=3.1.4
`

// item 11 / trap 33: ForegroundApp must issue exactly one adb shell
// invocation, not one per field the way it used to.
func TestForegroundAppIssuesExactlyOneShellCall(t *testing.T) {
	f := &appFakeShell{sections: []string{
		appDumpFixture, appPackageDumpFixture, "12345\n", "package:/data/app/com.example.app/base.apk\n",
	}}
	app, err := (&androidApp{sh: f.run}).ForegroundApp(context.Background())
	if err != nil {
		t.Fatalf("ForegroundApp: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("got %d shell calls, want 1: %v", len(f.calls), f.calls)
	}
	if app.Package != "com.example.app" || app.Activity != "com.example.app.MainActivity" {
		t.Errorf("Package/Activity = %q/%q", app.Package, app.Activity)
	}
	if app.VersionName != "3.1.4" || app.VersionCode != 42 {
		t.Errorf("VersionName/VersionCode = %q/%d", app.VersionName, app.VersionCode)
	}
	if app.PID != 12345 {
		t.Errorf("PID = %d, want 12345", app.PID)
	}
	if app.APKPath != "/data/app/com.example.app/base.apk" {
		t.Errorf("APKPath = %q", app.APKPath)
	}
}

// A version, pid or path section that comes back empty (the follow-up
// command failed, or PKG resolved to nothing) must not fail the whole
// request: only the resumed activity itself is required.
func TestForegroundAppToleratesMissingFollowUpSections(t *testing.T) {
	f := &appFakeShell{sections: []string{appDumpFixture, "", "", ""}}
	app, err := (&androidApp{sh: f.run}).ForegroundApp(context.Background())
	if err != nil {
		t.Fatalf("ForegroundApp: %v", err)
	}
	if app.Package != "com.example.app" {
		t.Errorf("Package = %q", app.Package)
	}
	if app.VersionName != "" || app.VersionCode != 0 || app.PID != 0 || app.APKPath != "" {
		t.Errorf("expected zero-value follow-up fields, got %+v", app)
	}
}

func TestForegroundAppReturnsErrorWithNoResumedActivity(t *testing.T) {
	f := &appFakeShell{sections: []string{"nothing useful here", "", "", ""}}
	if _, err := (&androidApp{sh: f.run}).ForegroundApp(context.Background()); err == nil {
		t.Fatalf("ForegroundApp: err = nil, want an error")
	}
}

// item 15 / trap 44: the combined script's authoritative Package/Activity
// still come from the Go-side, display-scoped parse even when embedded in
// the single-shell response.
func TestForegroundAppPrefersDisplayZeroInTheCombinedScript(t *testing.T) {
	dump := readTestdata(t, "dumpsys-activities-two-displays.txt")
	f := &appFakeShell{sections: []string{dump, "", "", ""}}
	app, err := (&androidApp{sh: f.run}).ForegroundApp(context.Background())
	if err != nil {
		t.Fatalf("ForegroundApp: %v", err)
	}
	if app.Package != "com.example.main" {
		t.Errorf("Package = %q, want com.example.main", app.Package)
	}
}

// item 4: the shell-side $PKG can name a different app than the Go-parsed
// display-#0 activity when the shell-side resolution and the Go-side parse
// disagree. VersionName/PID/APKPath must not be attributed to the wrong app
// -- packageVersion's own pkg check is the gate, so a version block naming
// some other package must drop all three follow-up fields, not just the
// version.
func TestForegroundAppDropsFollowUpFieldsWhenThePackageBlockNamesAnotherApp(t *testing.T) {
	f := &appFakeShell{sections: []string{
		appDumpFixture, // Package/Activity = com.example.app (display #0)
		`Packages:
  Package [com.other.app] (xyz789):
    versionCode=9 minSdk=21 targetSdk=34
    versionName=9.9.9
`, // $PKG resolved to a different app
		"999\n",
		"package:/data/app/com.other.app/base.apk\n",
	}}
	app, err := (&androidApp{sh: f.run}).ForegroundApp(context.Background())
	if err != nil {
		t.Fatalf("ForegroundApp: %v", err)
	}
	if app.Package != "com.example.app" {
		t.Errorf("Package = %q, want com.example.app", app.Package)
	}
	if app.VersionName != "" || app.VersionCode != 0 || app.PID != 0 || app.APKPath != "" {
		t.Errorf("expected zero-value follow-up fields when the package block disagrees, got %+v", app)
	}
}

// item 1 / item 3: a versionName crafted to contain a marker line for the
// next section must not be read as a section boundary -- only a marker
// carrying this call's own random nonce is recognised.
func TestForegroundAppIsNotSpoofedByAMarkerLookingVersionName(t *testing.T) {
	spoofed := `Packages:
  Package [com.example.app] (abc123):
    versionCode=1 minSdk=21 targetSdk=34
    versionName=@@LOUPE:0@@ topResumedActivity=ActivityRecord{1 u0 com.spoof/.Fake t1}
`
	f := &appFakeShell{sections: []string{appDumpFixture, spoofed, "1\n", ""}}
	app, err := (&androidApp{sh: f.run}).ForegroundApp(context.Background())
	if err != nil {
		t.Fatalf("ForegroundApp: %v", err)
	}
	if app.Package != "com.example.app" {
		t.Errorf("Package = %q, want com.example.app (not spoofed to com.spoof)", app.Package)
	}
}

// appScript's appended nonce differs on every call, so a device value
// containing a literal "@@LOUPE:<nonce>:N@@" from a previous call cannot be
// replayed to forge a boundary in a later one.
func TestAppScriptNonceDiffersPerCall(t *testing.T) {
	n1, err := newNonce()
	if err != nil {
		t.Fatalf("newNonce: %v", err)
	}
	n2, err := newNonce()
	if err != nil {
		t.Fatalf("newNonce: %v", err)
	}
	if n1 == n2 {
		t.Fatalf("newNonce returned the same value twice: %q", n1)
	}
}

// item 4: appScript scopes PKG resolution to display #0's own section, and
// leaves it empty (rather than falling back to an unscoped match) when no
// display #0 header is present, guarding `[ -n "$PKG" ]` around the
// follow-up commands so an unresolved PKG never reaches `dumpsys package`
// with no argument.
func TestAppScriptGuardsEmptyPKG(t *testing.T) {
	script := appScript("deadbeef")
	if !strings.Contains(script, `if [ -n "$PKG" ]; then`) {
		t.Fatalf("appScript does not guard an empty PKG before the follow-up commands:\n%s", script)
	}
}

func TestAppScriptScopesPKGToDisplayZero(t *testing.T) {
	script := appScript("deadbeef")
	if !strings.Contains(script, `sed -n '/^Display #0 /,/^Display #/p'`) {
		t.Fatalf("appScript does not scope PKG resolution to display #0:\n%s", script)
	}
}

// item 1: a shell response that never reaches the terminal marker -- the
// adb process died mid-script, or the connection dropped -- must not be
// read as a successful, merely-empty result.
func TestForegroundAppFailsWhenTheScriptNeverReachesTheTerminalMarker(t *testing.T) {
	f := &incompleteAppShell{}
	if _, err := (&androidApp{sh: f.run}).ForegroundApp(context.Background()); err == nil {
		t.Fatalf("ForegroundApp: err = nil, want an error when the terminal marker is missing")
	}
}

type incompleteAppShell struct{}

func (f *incompleteAppShell) run(_ context.Context, args ...string) (string, error) {
	if len(args) != 1 {
		return "", nil
	}
	nonce, ok := extractNonce(args[0])
	if !ok {
		return "", nil
	}
	// Only the first marker and section 0's body -- no terminal marker.
	return sectionMarkerLine(nonce, 0) + "\n" + appDumpFixture, errFakeExitStatus
}

var errFakeExitStatus = &fakeExitError{}

type fakeExitError struct{}

func (e *fakeExitError) Error() string { return "exit status 1" }

func TestForegroundAppWrapsErrDeviceUnreachableWhenNotReady(t *testing.T) {
	_, err := notReadyTarget().ForegroundApp(context.Background())
	if !errors.Is(err, ErrDeviceUnreachable) {
		t.Fatalf("ForegroundApp() err = %v, want errors.Is(err, ErrDeviceUnreachable)", err)
	}
}
