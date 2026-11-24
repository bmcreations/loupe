package target

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUninstallAndroid(t *testing.T) {
	f := &fakeShell{answers: map[string]string{
		"pm path 'com.example.app'":      "package:/data/app/base.apk",
		"pm uninstall 'com.example.app'": "Success",
	}}
	res, err := uninstallAndroid(context.Background(), f.run, "com.example.app")
	if err != nil || res.Output != "Success" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if got := strings.Join(f.calls, "|"); got != "pm path 'com.example.app'|pm uninstall 'com.example.app'" {
		t.Errorf("calls = %q; want the name quoted on both", got)
	}
}

func TestUninstallAndroidNotInstalled(t *testing.T) {
	f := &fakeShell{}
	_, err := uninstallAndroid(context.Background(), f.run, "com.example.app")
	if !errors.Is(err, ErrAppNotInstalled) {
		t.Errorf("err = %v; want ErrAppNotInstalled", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %q; uninstall must not run for a missing package", f.calls)
	}
}

func TestUninstallAndroidRefusal(t *testing.T) {
	f := &fakeShell{answers: map[string]string{
		"pm path 'com.example.app'":      "package:/data/app/base.apk",
		"pm uninstall 'com.example.app'": "Failure [DELETE_FAILED_DEVICE_POLICY_MANAGER]",
	}}
	_, err := uninstallAndroid(context.Background(), f.run, "com.example.app")
	ie, ok := IsInstallRefused(err)
	if !ok || ie.Code != "DELETE_FAILED_DEVICE_POLICY_MANAGER" {
		t.Errorf("err = %v; want an InstallError with the DELETE_FAILED code", err)
	}
}

func TestUninstallAndroidNoSuccessLineFails(t *testing.T) {
	f := &fakeShell{answers: map[string]string{"pm path 'com.example.app'": "package:/x"}}
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
		if !errors.Is(err, ErrBadLaunchName) || len(f.calls) != 0 {
			t.Errorf("uninstall(%q): err %v calls %q; want ErrBadLaunchName and no shell", pkg, err, f.calls)
		}
	}
}

var _ Uninstaller = (*ADBTarget)(nil)
