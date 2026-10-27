package target

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
)

// pngMagic is the PNG signature. screencap writes its errors to stdout and
// still exits zero on some devices, so the bytes are checked rather than
// trusted.
var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// Screenshotter is implemented by targets that can produce one still frame
// without a stream. Synthetic cannot, so this is deliberately not part of
// Target, the same way Inspector is not.
type Screenshotter interface {
	Screenshot(ctx context.Context) ([]byte, error)
}

// Screenshot captures the device screen as a PNG.
//
// This deliberately does not read the video path. A frame off the canvas is
// whatever the encoder produced -- MaxSize has already downscaled it, the
// codec has already quantised it, and a viewer has to be attached for one to
// exist at all. screencap reads the framebuffer, so it answers colour and
// layout questions at the device's own resolution with no session open, which
// is the gap it exists to fill.
//
// It costs a device round trip in the low hundreds of milliseconds and a few
// megabytes of PNG, so it is a still to look at rather than a way to sample
// frames.
func (t *ADBTarget) Screenshot(ctx context.Context) ([]byte, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}

	// exec-out, not shell. `adb shell` runs the output through a pty on older
	// transports, which rewrites 0x0a into 0x0d 0x0a and corrupts every PNG
	// that happens to contain one -- which is all of them.
	raw, err := t.adb.ExecOut(ctx, t.info.Serial, "screencap", "-p")
	if err != nil {
		return nil, fmt.Errorf("screencap: %w", err)
	}
	// A foldable has two panels, and screencap without -d prints a warning
	// in place of the PNG (on Hinge_Pixel_Fold, before the PNG on stdout).
	// The warning itself says its fallback display is not guaranteed, so the
	// retry names the lit panel. Single-panel devices never take this path
	// and pay no extra round trip.
	if bytes.Contains(raw[:min(len(raw), 200)], []byte("Multiple displays")) {
		out, err := t.adb.Shell(ctx, t.info.Serial, "dumpsys", "display")
		if err != nil {
			return nil, fmt.Errorf("dumpsys display: %w", err)
		}
		id, ok := litPhysicalDisplay(out)
		if !ok {
			return nil, fmt.Errorf("screencap: %s has several displays and none reads state ON", t.info.Serial)
		}
		if raw, err = t.adb.ExecOut(ctx, t.info.Serial, "screencap", "-d", id, "-p"); err != nil {
			return nil, fmt.Errorf("screencap -d %s: %w", id, err)
		}
	}
	if !bytes.HasPrefix(raw, pngMagic) {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg == "" {
			msg = fmt.Sprintf("%d bytes, no PNG signature", len(raw))
		}
		return nil, fmt.Errorf("screencap did not return a PNG: %s", msg)
	}
	return raw, nil
}

// displayDeviceRe reads one physical panel from `dumpsys display`: its
// uniqueId carries the SurfaceFlinger id screencap's -d takes, and its
// state says whether it is the one lit.
var displayDeviceRe = regexp.MustCompile(`DisplayDeviceInfo\{[^\n]*?uniqueId="local:(\d+)"[^\n]*?, state (\w+),`)

// litPhysicalDisplay names the one internal panel that is on. Folded, the
// Pixel Fold's outer panel reads ON and the inner OFF, and unfolded the
// reverse. Anything but exactly one lit panel is no answer, since guessing
// would return a picture of the wrong screen.
func litPhysicalDisplay(dumpsys string) (string, bool) {
	var lit []string
	for _, m := range displayDeviceRe.FindAllStringSubmatch(dumpsys, -1) {
		if m[2] == "ON" {
			lit = append(lit, m[1])
		}
	}
	if len(lit) != 1 {
		return "", false
	}
	return lit[0], true
}
