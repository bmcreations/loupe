package target

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"unicode/utf8"
)

// URLOpener is implemented by targets that can hand a URL to whatever app
// the device has registered for it: a deep link, a universal link or an
// https page. A physical iPhone is not one: devicectl opens a URL only as
// the payload of launching a named app, and there is no "whatever handles
// this" on that route.
type URLOpener interface {
	// OpenURL opens u. The result's Activity is the component Android
	// reports as started, and empty on a simulator, which does not say.
	OpenURL(ctx context.Context, u string) (*LaunchResult, error)

	// CheckURL is OpenURL's check alone, with no I/O, so a malformed URL
	// is a 400 before arbitration rather than a 409 humanHeld that a retry
	// cannot fix. It wraps ErrBadURL.
	CheckURL(u string) error
}

var (
	// ErrBadURL is a URL that failed CheckURL, so nothing was sent.
	ErrBadURL = errors.New("bad url")
	// ErrNoURLHandler is a well-formed URL that nothing on the device
	// handles, such as a custom scheme whose app is not installed.
	ErrNoURLHandler = errors.New("nothing on the device handles this url")
)

// MaxURLLen bounds a URL. Browsers accept far longer, but a deep link is
// a scheme, a path and a few parameters, and the URL rides an adb command
// line on Android.
const MaxURLLen = 2048

// urlSchemeRE is RFC 3986's scheme.
var urlSchemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// CheckURL requires a scheme and something after it, and refuses
// whitespace, control characters and invalid UTF-8. It requires no host:
// mailto: and myapp:settings have none. The URL is otherwise passed on as
// written, since the app it reaches is the one that parses it.
func CheckURL(u string) error {
	if u == "" {
		return fmt.Errorf("%w: empty", ErrBadURL)
	}
	if len(u) > MaxURLLen {
		return fmt.Errorf("%w: %d bytes, over %d", ErrBadURL, len(u), MaxURLLen)
	}
	if !utf8.ValidString(u) {
		return fmt.Errorf("%w: not UTF-8", ErrBadURL)
	}
	for _, r := range u {
		if r <= 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return fmt.Errorf("%w: contains whitespace or a control character", ErrBadURL)
		}
	}
	scheme, rest, ok := strings.Cut(u, ":")
	if !ok || !urlSchemeRE.MatchString(scheme) {
		return fmt.Errorf("%w: no scheme", ErrBadURL)
	}
	if rest == "" || rest == "//" {
		return fmt.Errorf("%w: nothing after %s:", ErrBadURL, scheme)
	}
	return nil
}

// URLParts is what of a URL may be logged: its scheme and host. A deep
// link's path and query routinely carry tokens, so neither leaves this
// package in an error or the trace. host is empty for a URL without one.
func URLParts(u string) (scheme, host string) {
	scheme, _, _ = strings.Cut(u, ":")
	scheme = strings.ToLower(scheme)
	if p, err := url.Parse(u); err == nil {
		host = p.Hostname()
	}
	return scheme, host
}

// RedactURL is u as an error may name it: scheme and host, with the rest
// elided.
func RedactURL(u string) string {
	scheme, host := URLParts(u)
	if host != "" {
		return scheme + "://" + host + "/…"
	}
	return scheme + ":…"
}

// redactErr rewrites err's text with every copy of u replaced, keeping
// whichever of the sentinels a caller branches on it wraps. The text
// already starts with that sentinel's, so it is wrapped back on in its
// place rather than in front of it.
func redactErr(err error, u string) error {
	if err == nil || !strings.Contains(err.Error(), u) {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), u, RedactURL(u))
	for _, s := range []error{ErrNoURLHandler, ErrDeviceUnreachable} {
		if errors.Is(err, s) {
			return fmt.Errorf("%w%s", s, strings.TrimPrefix(msg, s.Error()))
		}
	}
	return errors.New(msg)
}

// OpenURL starts a VIEW intent for u with `am start -W`, so the reply
// names the activity that took it.
func (t *ADBTarget) OpenURL(ctx context.Context, u string) (*LaunchResult, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	return openURLAndroid(ctx, func(ctx context.Context, args ...string) (string, error) {
		return t.adb.Shell(ctx, t.info.Serial, args...)
	}, u)
}

// CheckURL implements URLOpener.
func (t *ADBTarget) CheckURL(u string) error { return CheckURL(u) }

// openURLAndroid is OpenURL over a shellFunc, so it can be tested without
// a device. u reaches the device's shell only after CheckURL and through
// shellQuote.
//
// A URL nothing handles prints "Error: Activity not started, unable to
// resolve Intent" and exits 1, which parseAm reads as ErrNoURLHandler.
// parseAm gets u itself as the label, not RedactURL(u): redactErr cuts
// every copy of u afterwards, and a label already cut would be cut again,
// since RedactURL(u) can contain u.
func openURLAndroid(ctx context.Context, sh shellFunc, u string) (*LaunchResult, error) {
	if err := CheckURL(u); err != nil {
		return nil, err
	}
	out, err := sh(ctx, "am", "start", "-W", "-a", "android.intent.action.VIEW", "-d", shellQuote(u))
	res, err := parseAm(u, out, err)
	return res, redactErr(err, u)
}

// OpenURL opens u with `simctl openurl`. It gets u as its own exec
// argument, with no shell between.
func (s *Simulator) OpenURL(ctx context.Context, u string) (*LaunchResult, error) {
	if err := CheckURL(u); err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "openurl", s.UDID(), u).CombinedOutput()
	if err != nil {
		if simOpenURLUnhandled(err) {
			return nil, fmt.Errorf("%w: %s", ErrNoURLHandler, RedactURL(u))
		}
		return nil, redactErr(fmt.Errorf("simctl openurl: %w: %s", err, strings.TrimSpace(string(out))), u)
	}
	return &LaunchResult{}, nil
}

// CheckURL implements URLOpener.
func (s *Simulator) CheckURL(u string) error { return CheckURL(u) }

// simOpenURLUnhandled reports whether simctl openurl failed because no app
// claims the URL: it exits 115, LSApplicationWorkspaceErrorDomain's code
// for that, on Xcode 27.
func simOpenURLUnhandled(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 115
}
