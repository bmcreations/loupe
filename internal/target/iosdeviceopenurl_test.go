package target

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPhoneOpenURLLaunchesSafariWithThePayload(t *testing.T) {
	var gotDevice, gotURL string
	old := devicectlOpenURL
	devicectlOpenURL = func(_ context.Context, device, u string) ([]byte, error) {
		gotDevice, gotURL = device, u
		return []byte("Launched application with com.apple.mobilesafari bundle identifier.\n"), nil
	}
	t.Cleanup(func() { devicectlOpenURL = old })

	d := NewIOSDevice(IOSDeviceInfo{Identifier: "CORE-ID", UDID: "UDID", Name: "phone"})
	res, err := d.OpenURL(context.Background(), "https://example.com/a?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	if gotDevice != "CORE-ID" || gotURL != "https://example.com/a?token=secret" {
		t.Errorf("devicectl got device %q url %q", gotDevice, gotURL)
	}
	if res.Activity != safariBundle {
		t.Errorf("Activity = %q, want %q", res.Activity, safariBundle)
	}
}

func TestPhoneCheckURLRefusesSchemesSafariCannotOpen(t *testing.T) {
	d := NewIOSDevice(IOSDeviceInfo{UDID: "UDID"})
	for _, u := range []string{"http://example.com", "HTTPS://example.com/x"} {
		if err := d.CheckURL(u); err != nil {
			t.Errorf("CheckURL(%q) = %v, want nil", u, err)
		}
	}
	for _, u := range []string{"prefs:root=General", "myapp://settings", "mailto:a@b.c", "https://", ""} {
		if err := d.CheckURL(u); !errors.Is(err, ErrBadURL) {
			t.Errorf("CheckURL(%q) = %v, want ErrBadURL", u, err)
		}
	}
}

func TestPhoneOpenURLFailureKeepsThePathOutOfTheError(t *testing.T) {
	old := devicectlOpenURL
	devicectlOpenURL = func(_ context.Context, _, u string) ([]byte, error) {
		return []byte("ERROR: The application failed to launch. " + u + "\n       NSLocalizedFailureReason = The device is locked."), errors.New("exit status 1")
	}
	t.Cleanup(func() { devicectlOpenURL = old })

	d := NewIOSDevice(IOSDeviceInfo{UDID: "UDID"})
	_, err := d.OpenURL(context.Background(), "https://example.com/a?token=secret")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error carries the URL's query: %v", err)
	}
	if !strings.Contains(err.Error(), "The device is locked.") {
		t.Errorf("error lost devicectl's reason: %v", err)
	}
}
