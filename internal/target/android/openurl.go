package android

import (
	"context"
	"fmt"

	"github.com/bmcreations/loupe/internal/target"
)

// OpenURL starts a VIEW intent for u with `am start -W`, so the reply
// names the activity that took it.
func (t *Device) OpenURL(ctx context.Context, u string) (*target.LaunchResult, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", target.ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	return openURLAndroid(ctx, func(ctx context.Context, args ...string) (string, error) {
		return t.adb.Shell(ctx, t.info.Serial, args...)
	}, u)
}

// CheckURL implements URLOpener.
func (t *Device) CheckURL(u string) error { return target.CheckURL(u) }

// openURLAndroid is OpenURL over a shellFunc, so it can be tested without
// a device. u reaches the device's shell only after CheckURL and through
// shellQuote.
//
// A URL nothing handles prints "Error: Activity not started, unable to
// resolve Intent" and exits 1, which parseAm reads as ErrNoURLHandler.
// parseAm gets u itself as the label, not RedactURL(u): RedactErr cuts
// every copy of u afterwards, and a label already cut would be cut again,
// since RedactURL(u) can contain u.
func openURLAndroid(ctx context.Context, sh shellFunc, u string) (*target.LaunchResult, error) {
	if err := target.CheckURL(u); err != nil {
		return nil, err
	}
	out, err := sh(ctx, "am", "start", "-W", "-a", "android.intent.action.VIEW", "-d", shellQuote(u))
	res, err := parseAm(u, out, err)
	return res, target.RedactErr(err, u)
}
