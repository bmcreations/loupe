package android

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

func TestUninstallAndroid(t *testing.T) {
	f := &fakeShell{answers: map[string]string{"pm uninstall 'com.example.app'": "Success"}}
	res, err := uninstallAndroid(context.Background(), f.run, "com.example.app")
	if err != nil || res.Output != "Success" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if got := strings.Join(f.calls, "|"); got != "pm uninstall 'com.example.app'" {
		t.Errorf("calls = %q; want the name quoted", got)
	}
}

func TestUninstallAndroidNotInstalled(t *testing.T) {
	f := &fakeShell{answers: map[string]string{"pm uninstall 'com.example.app'": "Failure [DELETE_FAILED_INTERNAL_ERROR]"}}
	_, err := uninstallAndroid(context.Background(), f.run, "com.example.app")
	if !errors.Is(err, target.ErrAppNotInstalled) {
		t.Errorf("err = %v; want target.ErrAppNotInstalled", err)
	}
}

// A shell that drops during the follow-up lookup must not read as "not
// installed": the package may well be there.
func TestUninstallAndroidFailedLookupIsNotNotInstalled(t *testing.T) {
	f := &fakeShell{
		answers: map[string]string{"pm uninstall 'com.example.app'": "Failure [DELETE_FAILED_INTERNAL_ERROR]"},
		fail:    map[string]error{"pm path 'com.example.app'": errors.New("adb: device offline")},
	}
	_, err := uninstallAndroid(context.Background(), f.run, "com.example.app")
	if err == nil || errors.Is(err, target.ErrAppNotInstalled) {
		t.Errorf("err = %v; want the refusal, not target.ErrAppNotInstalled", err)
	}
}

func TestUninstallAndroidRefusal(t *testing.T) {
	f := &fakeShell{answers: map[string]string{
		"pm path 'com.example.app'":      "package:/data/app/base.apk",
		"pm uninstall 'com.example.app'": "Failure [DELETE_FAILED_DEVICE_POLICY_MANAGER]",
	}}
	_, err := uninstallAndroid(context.Background(), f.run, "com.example.app")
	ie, ok := target.IsInstallRefused(err)
	if !ok || ie.Code != "DELETE_FAILED_DEVICE_POLICY_MANAGER" {
		t.Errorf("err = %v; want an target.InstallError with the DELETE_FAILED code", err)
	}
}

func TestUninstallAndroidNoSuccessLineFails(t *testing.T) {
	f := &fakeShell{}
	if _, err := uninstallAndroid(context.Background(), f.run, "com.example.app"); err == nil {
		t.Error("an empty answer read as success")
	}
}

// Nothing a caller sends reaches the device's shell unless it passed its
// pattern check.
func TestUninstallAndroidRejectsNamesBeforeTheShell(t *testing.T) {
	for _, pkg := range []string{"com.x;rm -rf /", "com.x && reboot", "$(reboot)", "", "settings", "com.x'"} {
		f := &fakeShell{}
		_, err := uninstallAndroid(context.Background(), f.run, pkg)
		if !errors.Is(err, target.ErrBadLaunchName) || len(f.calls) != 0 {
			t.Errorf("uninstall(%q): err %v calls %q; want target.ErrBadLaunchName and no shell", pkg, err, f.calls)
		}
	}
}

var _ target.Uninstaller = (*Device)(nil)
