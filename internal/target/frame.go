package target

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DeviceFrame describes the bezel to draw around a stream, in the frame's own
// pixel space. The client scales the whole thing to fit and positions the
// canvas at Display, so nothing here depends on the viewport.
//
// It is deliberately not part of Meta. Meta describes what the encoder
// produced and exists only while something is streaming; a frame is a property
// of the physical device and is the same whether or not anyone is watching.
type DeviceFrame struct {
	// Source is "skin" for real device art off the SDK, "derived" for a
	// generic bezel built from what the device reports about itself.
	Source string `json:"source"`
	// Width and Height are the whole frame including the bezel.
	Width  int `json:"width"`
	Height int `json:"height"`
	// Display is where the screen sits inside it.
	Display FrameRect `json:"display"`
	// CornerRadius rounds the screen's corners, in the same pixel space.
	//
	// A device does not report this. Android's RoundedCorner API reads 0 on
	// both an emulator and an Acme One, so on the derived path this is a chosen
	// constant and only the skin path carries the real value.
	CornerRadius int `json:"cornerRadius"`
	// Background and Mask are asset names to fetch from the same target, empty
	// on the derived path. Only names that appear here can be fetched, which is
	// what keeps the asset endpoint from reaching anywhere else on disk.
	Background string `json:"background,omitempty"`
	Mask       string `json:"mask,omitempty"`
	// Cutout is the notch or punch-hole the device declares, nil when it
	// declares none.
	Cutout *FrameCutout `json:"cutout,omitempty"`
	// Rotation is how many quarter turns clockwise the client must turn the
	// art to match this frame, 0 when it is drawn the way the skin drew it.
	//
	// The geometry above is already final. This says only what to do with the
	// images, which stay on the grid they were authored on: a skin's art is
	// drawn for one orientation (trap 40), and a device turned on its side
	// needs that same art turned rather than stretched to a new box. Turning
	// it in the browser costs a transform; turning it here would mean
	// re-encoding a webp.
	Rotation int `json:"rotation,omitempty"`
}

// FrameCutout is the device's own cutout path plus the transform that places
// it, rather than a path this code rewrote.
//
// Android states the cutout as SVG in either device pixels or dp, anchored
// left, right or centred. Applying that to the path means parsing and
// rewriting every SVG command; handing the untouched path to an SVG <g> with
// "translate(DX,0) scale(Scale)" gets the identical result from the renderer
// that already implements it. SVG applies the rightmost transform first, so
// Scale converts dp to pixels and DX then anchors, in that order.
type FrameCutout struct {
	Path  string  `json:"path"`
	Scale float64 `json:"scale"`
	DX    int     `json:"dx"`
}

// declaresAsset reports whether the frame named this file.
//
// This is an equality against two names the skin itself declared rather than a
// path test. A caller cannot reach outside the skin directory because it
// cannot name anything the layout did not, so there is no traversal to
// sanitise and no separate encoding of "..", "/" or a symlink to get wrong.
//
// The asset endpoint checks the whole skin rather than this, because a skin
// with its own landscape art names different files per orientation.
func (f *DeviceFrame) declaresAsset(name string) bool {
	if f == nil || name == "" {
		return false
	}
	return name == f.Background || name == f.Mask
}

// FrameRect is a rectangle in frame pixel space.
type FrameRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Framer is implemented by targets that can describe their own bezel.
// Synthetic cannot, so this is not part of Target, the same way Inspector and
// Screenshotter are not.
type Framer interface {
	DeviceFrame(ctx context.Context) (*DeviceFrame, error)
	// FrameAsset returns one image the frame named, and its media type.
	FrameAsset(ctx context.Context, name string) ([]byte, string, error)
}

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
func (t *ADBTarget) DeviceFrame(ctx context.Context) (*DeviceFrame, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}
	if t.info.Emulator {
		if f, err := t.skinFrame(ctx); err == nil {
			return f, nil
		}
		// Fall through. A missing skin is the ordinary case for an AVD built
		// from a hardware profile with no art, not a reason to answer nothing.
	}
	return t.derivedFrame(ctx)
}

// FrameAsset serves one of the skin images. The name must be one the parsed
// layout declared, which is the whole check: a caller cannot ask for a path
// the layout does not mention, so there is nothing to escape.
func (t *ADBTarget) FrameAsset(ctx context.Context, name string) ([]byte, string, error) {
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

func (t *ADBTarget) skinFrame(ctx context.Context) (*DeviceFrame, error) {
	_, layout, err := t.skinLayout(ctx)
	if err != nil {
		return nil, err
	}
	// A rotation the device will not report costs the turn, not the bezel: the
	// natural orientation is what every frame answered before rotation was
	// handled at all, and the client hides a frame that does not suit the
	// stream anyway.
	rotation, err := t.adb.DisplayRotation(ctx, t.info.Serial)
	if err != nil {
		rotation = 0
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
func (t *ADBTarget) skinLayout(ctx context.Context) (string, *skinNode, error) {
	avd, err := t.adb.AVDName(ctx, t.info.Serial)
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
func (t *ADBTarget) derivedFrame(ctx context.Context) (*DeviceFrame, error) {
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

	frame := &DeviceFrame{
		Source:       "derived",
		Width:        w + 2*bezel,
		Height:       h + 2*bezel,
		Display:      FrameRect{X: bezel, Y: bezel, Width: w, Height: h},
		CornerRadius: int(float64(short) * derivedCornerFraction),
	}
	if spec, err := t.adb.CutoutSpec(ctx, t.info.Serial); err == nil {
		frame.Cutout = parseCutoutSpec(spec, w, density)
	}
	return frame, nil
}
