package target

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Describer reports what a target is, for the device drawer. It is optional:
// a target that cannot say omits the fields rather than guessing them.
type Describer interface {
	Describe(ctx context.Context) (Description, error)
}

// Description is what a target says about itself. Every field is optional --
// an AVD that has not booted yet has none of them -- so a caller reports what
// it has and omits the rest rather than guessing.
type Description struct {
	Form      string `json:"form,omitempty"` // phone, foldable, tablet, pattern
	OS        string `json:"os,omitempty"`   // android, ios, synthetic
	OSVersion string `json:"osVersion,omitempty"`
	APILevel  int    `json:"apiLevel,omitempty"`
	// Transport is how the device reaches this Mac: usb, wifi, emulator or
	// simulator. Omitted for synthetic, and for anything that has not
	// described itself.
	Transport string `json:"transport,omitempty"`
}

// parseGetprop reads the output of `getprop`, one `[key]: [value]` pair per
// line. A `#` line is a comment -- testdata fixtures use it to record what
// was redacted from a real capture -- and is skipped, along with anything
// that does not match the pattern, rather than failing the whole parse: a
// getprop dump runs to hundreds of lines and only a handful are ever read.
func parseGetprop(raw []byte) map[string]string {
	props := make(map[string]string)
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	// getprop lines are short, but a value can legitimately embed a bare `]`
	// (ro.build.fingerprint does, when a build id itself carries one), so the
	// default 64KB scanner buffer is generous rather than load-bearing here.
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "[") {
			continue
		}
		// Split on the first "]: [" and the last "]", so a value containing
		// "]" is kept whole rather than truncated at its first occurrence.
		sep := strings.Index(line, "]: [")
		if sep < 0 || !strings.HasSuffix(line, "]") {
			continue
		}
		key := line[1:sep]
		value := line[sep+4 : len(line)-1]
		props[key] = value
	}
	return props
}

// describeFrom turns parsed getprop output into a Description. A dump
// missing ro.build.version.release or ro.build.version.sdk entirely is
// treated as incomplete -- the second return is false -- rather than
// producing a Description with those fields silently blank: a half-parsed
// getprop capture (a device still booting, a shell that returned early) must
// not be cached or reported as if it were a real answer. Once both keys are
// present, an unparsable sdk value is a narrower failure and only omits
// APILevel, since an APILevel of 0 and an absent one are indistinguishable in
// the JSON either way (the field carries `omitempty`).
func describeFrom(props map[string]string) (Description, bool) {
	release, hasRelease := props["ro.build.version.release"]
	sdkStr, hasSDK := props["ro.build.version.sdk"]
	if !hasRelease || !hasSDK {
		return Description{}, false
	}
	d := Description{
		OS:        "android",
		OSVersion: release,
		Form:      formFrom(props["ro.build.characteristics"]),
	}
	if sdk, err := strconv.Atoi(sdkStr); err == nil {
		d.APILevel = sdk
	}
	return d, true
}

// formFrom reads ro.build.characteristics, a comma-separated list such as
// "emulator,tablet" or "nosdcard,notablet". Elements are matched exactly --
// a substring match would call "notablet" a tablet -- against the set
// Android actually uses:
//
//   - "tablet" is reported as "tablet".
//   - "tv", "watch" and "automotive" are reported as "" (omitted): calling a
//     television a phone is a guess, not a description, and the plan is
//     explicit that a value this code cannot support is left out rather than
//     guessed.
//   - Everything else -- "default", "nosdcard", "emulator", and an empty or
//     absent value -- is phone-like and reported as "phone".
func formFrom(characteristics string) string {
	elems := make(map[string]bool)
	for _, c := range strings.Split(characteristics, ",") {
		elems[strings.TrimSpace(c)] = true
	}
	if elems["tv"] || elems["watch"] || elems["automotive"] {
		return ""
	}
	if elems["tablet"] {
		return "tablet"
	}
	return "phone"
}

// describeCache holds a target's Description once it succeeds. A device does
// not change OS version or form under a running server, so there is nothing
// to invalidate; a failure is not cached, so a target that answered late (an
// AVD still booting, for instance) is asked again on the next call rather
// than being stuck reporting nothing for the server's whole life.
//
// A cached value lives for as long as the in-process target does, not for as
// long as the underlying device does. If a physical serial is reused by a
// different device, or an emulator is restarted on the same port from a
// different system image, the cache still reports whatever it read the first
// time. This is the same shape as the known gap in the target list itself
// (CLAUDE.md: "The target list is a snapshot of startup") one layer down, and
// is accepted for the same reason: nothing here re-enumerates a target that
// is already registered.
type describeCache struct {
	mu   sync.Mutex
	desc Description
	have bool
}

func (c *describeCache) get() (Description, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.desc, c.have
}

func (c *describeCache) set(d Description) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.desc, c.have = d, true
}

// Describe reports the device's OS and form by reading getprop once and
// caching the result for the target's life.
func (t *ADBTarget) Describe(ctx context.Context) (Description, error) {
	if d, ok := t.describeCache.get(); ok {
		return d, nil
	}
	if !t.info.Ready() {
		return Description{}, fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}
	out, err := t.adb.Shell(ctx, t.info.Serial, "getprop")
	if err != nil {
		return Description{}, err
	}
	d, ok := describeFrom(parseGetprop([]byte(out)))
	if !ok {
		return Description{}, fmt.Errorf("incomplete getprop output from %s", t.info.Serial)
	}
	d.Transport = t.info.transport()
	t.describeCache.set(d)
	return d, nil
}
