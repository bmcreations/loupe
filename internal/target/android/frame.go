package android

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// derivedBezelFraction is how much bezel to draw on the derived path, as a
// fraction of the screen's shorter side. A device does not report its bezel
// any more than it reports its corner radius, so this is chosen to read as a
// phone rather than measured from anything.
const derivedBezelFraction = 0.035

// derivedCornerFraction is the same choice for the screen's corner radius.
const derivedCornerFraction = 0.055

// DeviceFrame describes the device's bezel, preferring the emulator skin the
// SDK already has over anything derived.
//
// The skin is read from the user's own SDK rather than vendored. The art is
// pixel-accurate and it is the only way to make a Pixel look like a Pixel, but
// the Android SDK terms restrict redistribution, so it stays where it was
// installed and THIRD_PARTY_NOTICES.md stays as it is.
func (t *Device) DeviceFrame(ctx context.Context) (*target.DeviceFrame, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}
	// A rotation the device will not report costs the turn, not the bezel: the
	// natural orientation is what every frame answered before rotation was
	// handled at all, and the client hides a frame that does not suit the
	// stream anyway.
	display, err := t.adb.DisplayRotation(ctx, t.info.Serial)
	readDisplay := err == nil
	if err != nil {
		display = 0
	}
	var f *target.DeviceFrame
	if t.info.Emulator {
		f, err = t.skinFrame(ctx, display)
		// A missing skin is the ordinary case for an AVD built from a
		// hardware profile with no art, not a reason to answer nothing.
	}
	if f == nil {
		if f, err = t.derivedFrame(ctx); err != nil {
			return nil, err
		}
		f.DisplayRotation = display
	}
	// The geometry above follows the display, which is the stream's shape.
	// A device held another way than its display (an app that locks its
	// orientation) is shown turned, as a simulator is.
	// A display it cannot read says nothing about the difference.
	if readDisplay {
		f.ContentRotation = target.ContentRotationFor(t.held(ctx, display), display)
	}
	return f, nil
}

// FrameAsset serves one of the skin images. The name must be one the parsed
// layout declared, which is the whole check: a caller cannot ask for a path
// the layout does not mention, so there is nothing to escape.
func (t *Device) FrameAsset(ctx context.Context, name string) ([]byte, string, error) {
	dir, layout, err := t.skinLayout(ctx)
	if err != nil {
		return nil, "", err
	}
	if !layout.declaresAsset(name) {
		return nil, "", fmt.Errorf("frame asset %q is not declared by this skin", name)
	}

	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, "", fmt.Errorf("read frame asset: %w", err)
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".webp":
		return raw, "image/webp", nil
	case ".png":
		return raw, "image/png", nil
	}
	return nil, "", fmt.Errorf("frame asset %q is not an image", name)
}

func (t *Device) skinFrame(ctx context.Context, rotation int) (*target.DeviceFrame, error) {
	_, layout, err := t.skinLayout(ctx)
	if err != nil {
		return nil, err
	}
	frame := layout.frameFor(rotation)
	if frame == nil {
		return nil, fmt.Errorf("skin layout declares no usable display")
	}
	return frame, nil
}

// skinLayout resolves a running emulator back to the skin directory the SDK
// installed for it, and parses the layout inside.
//
// The chain is emulator console -> AVD config -> skin path. config.ini records
// skin.path absolutely, which is why nothing here has to find ANDROID_HOME --
// it is routinely unset on a machine where the SDK still works fine.
func (t *Device) skinLayout(ctx context.Context) (string, *skinNode, error) {
	avd, err := nameEmulator(ctx, t.adb, t.info.Serial)
	if err != nil {
		return "", nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}

	cfg := filepath.Join(home, ".android", "avd", avd+".avd", "config.ini")
	dir, err := skinPathFromConfig(cfg)
	if err != nil {
		return "", nil, err
	}

	raw, err := os.ReadFile(filepath.Join(dir, "layout"))
	if err != nil {
		return "", nil, fmt.Errorf("read skin layout: %w", err)
	}
	node, err := parseSkinLayout(string(raw))
	if err != nil {
		return "", nil, err
	}
	return dir, node, nil
}

// skinPathFromConfig reads skin.path out of an AVD's config.ini.
func skinPathFromConfig(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read avd config: %w", err)
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	for scan.Scan() {
		key, value, ok := strings.Cut(scan.Text(), "=")
		if !ok || strings.TrimSpace(key) != "skin.path" {
			continue
		}
		value = strings.TrimSpace(value)
		// "_no_skin" is what the emulator writes for an AVD with none, and a
		// relative path is relative to an SDK root this does not resolve.
		if value == "" || value == "_no_skin" || !filepath.IsAbs(value) {
			return "", fmt.Errorf("avd declares no absolute skin path")
		}
		return value, nil
	}
	if err := scan.Err(); err != nil {
		return "", fmt.Errorf("read avd config: %w", err)
	}
	return "", fmt.Errorf("avd config has no skin.path")
}

// derivedFrame builds a generic bezel around what the device says about
// itself. The proportions are chosen; the screen size and the cutout are not.
func (t *Device) derivedFrame(ctx context.Context) (*target.DeviceFrame, error) {
	w, h, err := t.adb.DisplaySize(ctx, t.info.Serial)
	if err != nil {
		return nil, err
	}
	density, err := t.adb.DisplayDensity(ctx, t.info.Serial)
	if err != nil {
		// A missing density only costs the dp-scaled cutouts, so carry on with
		// the framework default rather than refusing to draw a frame at all.
		density = defaultDensity
	}

	short := w
	if h < short {
		short = h
	}
	bezel := int(float64(short) * derivedBezelFraction)

	frame := &target.DeviceFrame{
		Source:       "derived",
		Width:        w + 2*bezel,
		Height:       h + 2*bezel,
		Display:      target.FrameRect{X: bezel, Y: bezel, Width: w, Height: h},
		CornerRadius: int(float64(short) * derivedCornerFraction),
	}
	if spec, err := t.adb.CutoutSpec(ctx, t.info.Serial); err == nil {
		frame.Cutout = parseCutoutSpec(spec, w, density)
	}
	return frame, nil
}
