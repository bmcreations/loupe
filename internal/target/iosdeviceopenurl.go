package target

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// safariBundle opens a phone's web links. devicectl hands a URL to an app
// only as the payload of launching that app by name, so a phone has no
// "whatever handles this" route the way simctl and am start do. Safari takes
// any http or https URL; a universal link opens in Safari as a page, not in
// the app that claims it.
const safariBundle = "com.apple.mobilesafari"

// CheckURL implements URLOpener. On a phone that is CheckURL plus an http or
// https scheme: Safari is the only app the URL can be sent to, and it shows
// "the address is invalid" for anything else (tried with prefs: on iOS 27).
func (d *IOSDevice) CheckURL(u string) error {
	return checkPhoneURL(u)
}

func checkPhoneURL(u string) error {
	if err := CheckURL(u); err != nil {
		return err
	}
	scheme, _ := URLParts(u)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w: a phone opens only http and https URLs, in Safari; %s: has no route", ErrBadURL, scheme)
	}
	return nil
}

// OpenURL implements URLOpener by launching Safari with u as its payload,
// `devicectl device process launch --payload-url`. Like Launch, it needs
// Developer Mode and fails on a locked phone. The result's Activity is
// Safari's bundle ID.
func (d *IOSDevice) OpenURL(ctx context.Context, u string) (*LaunchResult, error) {
	if err := checkPhoneURL(u); err != nil {
		return nil, err
	}
	out, err := devicectlOpenURL(ctx, d.device(), u)
	if err != nil {
		return nil, redactErr(fmt.Errorf("devicectl launch %s with a payload: %w: %s", safariBundle, err, strings.TrimSpace(string(out))), u)
	}
	return &LaunchResult{Activity: safariBundle}, nil
}

// devicectlOpenURL runs the launch. It is a variable so tests run without a
// phone. u is its own exec argument, never a shell word.
var devicectlOpenURL = func(ctx context.Context, device, u string) ([]byte, error) {
	return exec.CommandContext(ctx, "xcrun", "devicectl", "device", "process", "launch",
		"--device", device, "--payload-url", u, safariBundle).CombinedOutput()
}

var _ URLOpener = (*IOSDevice)(nil)
