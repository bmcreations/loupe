package android

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// What emulator-5556 (Pixel_10, Android 16) printed for each, captured with
// `adb shell 'am start -W -a android.intent.action.VIEW -d <url>; echo exit=$?'`.
// The first exited 0 and the second 1.
const (
	amViewChrome    = "Starting: Intent { act=android.intent.action.VIEW dat=https://example.com/... }\nStatus: ok\nLaunchState: UNKNOWN (0)\nActivity: com.android.chrome/org.chromium.chrome.browser.ChromeTabbedActivity\nWaitTime: 167\nComplete\n"
	amViewUnhandled = "Starting: Intent { act=android.intent.action.VIEW dat=loupenope://x/... }\nError: Activity not started, unable to resolve Intent { act=android.intent.action.VIEW dat=loupenope://x/... flg=0x10000000 xflg=0x4 }\n"
)

// What api34_emoji_check (Android 14) printed through a spare server when
// Chrome took 12s to draw. am exited 0 and Chrome was in front.
const amViewTimeout = "Starting: Intent { act=android.intent.action.VIEW dat=https://example.com/... }\nWarning: Activity not started, its current task has been brought to the front\nStatus: timeout\nLaunchState: UNKNOWN (-1)\nActivity: com.android.chrome/org.chromium.chrome.browser.document.ChromeLauncherActivity\nWaitTime: 11815\nComplete\n"

// A handler slow to draw was still started, so a timeout that names an
// activity opens the URL rather than failing it.
func TestOpenURLAndroidTimeoutWithAnActivityOpened(t *testing.T) {
	sh := func(ctx context.Context, args ...string) (string, error) { return amViewTimeout, nil }
	res, err := openURLAndroid(context.Background(), sh, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if want := "com.android.chrome/org.chromium.chrome.browser.document.ChromeLauncherActivity"; res.Activity != want || !res.TimedOut {
		t.Errorf("res = %+v, want Activity %q and TimedOut", res, want)
	}
}

// A timeout that names nothing is still a failure.
func TestOpenURLAndroidTimeoutWithoutAnActivityFails(t *testing.T) {
	sh := func(ctx context.Context, args ...string) (string, error) {
		return "Starting: Intent { act=android.intent.action.VIEW }\nStatus: timeout\nComplete\n", nil
	}
	if _, err := openURLAndroid(context.Background(), sh, "https://example.com"); err == nil {
		t.Fatal("no error")
	}
}

func TestOpenURLAndroid(t *testing.T) {
	const u = "https://example.com/a?token=SECRET"
	cmd := "am start -W -a android.intent.action.VIEW -d 'https://example.com/a?token=SECRET'"
	f := &fakeShell{answers: map[string]string{cmd: amViewChrome}}
	res, err := openURLAndroid(context.Background(), f.run, u)
	if err != nil {
		t.Fatal(err)
	}
	if want := "com.android.chrome/org.chromium.chrome.browser.ChromeTabbedActivity"; res.Activity != want {
		t.Errorf("Activity = %q, want %q", res.Activity, want)
	}
	if strings.Join(f.calls, "\n") != cmd {
		t.Errorf("ran %q, want %q", f.calls, cmd)
	}
}

// A scheme nothing claims exits 1 with an Error line, and the error says
// so without the URL's path or query.
func TestOpenURLAndroidUnhandledIsErrNoURLHandler(t *testing.T) {
	const u = "loupenope://x/y?token=SECRET"
	sh := func(ctx context.Context, args ...string) (string, error) {
		return amViewUnhandled, errors.New("exit status 1")
	}
	_, err := openURLAndroid(context.Background(), sh, u)
	if !errors.Is(err, target.ErrNoURLHandler) {
		t.Fatalf("err = %v, want ErrNoURLHandler", err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "/y") {
		t.Errorf("err %q carries the URL's path or query", err)
	}
	// The sentinel once, and the URL cut once: a live run read
	// "...this url: nothing on the device handles this url: am start nosuchscheme://x/…/…".
	if want := "nothing on the device handles this url: am start loupenope://x/…"; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
}

// A URL with no path is cut once, though its redacted form contains it.
func TestOpenURLAndroidRedactsABareURLOnce(t *testing.T) {
	const u = "https://example.com"
	sh := func(ctx context.Context, args ...string) (string, error) {
		return "Status: weird", nil
	}
	_, err := openURLAndroid(context.Background(), sh, u)
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "/…/…") {
		t.Errorf("err %q cut the URL twice", err)
	}
}

// Any other failure keeps its text, with the URL cut to scheme and host.
func TestOpenURLAndroidOtherFailureIsRedacted(t *testing.T) {
	const u = "https://example.com/a?token=SECRET"
	sh := func(ctx context.Context, args ...string) (string, error) {
		return "error: closed " + u, errors.New("exit status 255")
	}
	_, err := openURLAndroid(context.Background(), sh, u)
	if err == nil {
		t.Fatal("no error")
	}
	if errors.Is(err, target.ErrNoURLHandler) {
		t.Errorf("err = %v, read as no handler", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err %q carries the query", err)
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("err %q does not name the host", err)
	}
}

// Nothing reaches the device's shell unless CheckURL passed it.
func TestOpenURLAndroidRejectsBeforeTheShell(t *testing.T) {
	for _, u := range []string{"", "example.com", "https://a b", "x'; reboot", "https://a\nb", "https:"} {
		f := &fakeShell{}
		_, err := openURLAndroid(context.Background(), f.run, u)
		if !errors.Is(err, target.ErrBadURL) {
			t.Errorf("openURL(%q) err = %v, want ErrBadURL", u, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("openURL(%q) ran %q", u, f.calls)
		}
	}
}
