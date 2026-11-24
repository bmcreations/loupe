package target

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// URLOpener is implemented by targets that can hand a URL to whatever app
// the device has registered for it: a deep link, a universal link or an
// https page. A physical iPhone is a narrower one: devicectl opens a URL
// only as the payload of launching a named app, so it opens http and https
// in Safari and refuses other schemes (iosdeviceopenurl.go).
type URLOpener interface {
	// OpenURL opens u. The result's Activity is the component Android
	// reports as started; on a simulator it is the bundle ID of the app
	// that took a universal link, and empty otherwise, since simctl does
	// not say.
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

// RedactErr rewrites err's text with every copy of u replaced, keeping
// whichever of the sentinels a caller branches on it wraps. The text
// already starts with that sentinel's, so it is wrapped back on in its
// place rather than in front of it.
func RedactErr(err error, u string) error {
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
