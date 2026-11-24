package target

import (
	"context"
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
	// ContentRotation is how many quarter turns clockwise the client turns
	// the stream picture, and this whole frame with it, to show the device the
	// way it is being held. Omitted when it is 0.
	//
	// Rotation above is for a device whose picture follows a turn: an Android
	// display's encoder output changes shape and the geometry here is already
	// laid out for it, so ContentRotation stays 0 there while the display
	// follows the turn. An app that locks its orientation keeps the display
	// where it wants it; the device is then held another way than its picture
	// and ContentRotation is the difference (ContentRotationFor). A simulator's picture
	// keeps its natural size when it turns; the content is drawn turned inside
	// it, and the geometry and art stay upright. Turning the picture and frame
	// together is the only step a client needs on either kind.
	ContentRotation int `json:"contentRotation,omitempty"`
	// DisplayRotation is how far the display is turned from its natural
	// orientation, as Android's ROTATION_ quarter-turn count, on a derived
	// frame only. Omitted when it is 0, and on a skin, whose geometry is
	// already turned (Rotation).
	//
	// A derived frame is laid out in the natural orientation whichever way
	// the display faces, and its bezel is symmetric, so the box at 90 and at
	// 270 is the same; only the cutout tells them apart. The cutout path is
	// in the natural orientation's pixels, and Android shows that picture
	// turned counter-clockwise by this many quarters: at ROTATION_90 the
	// cutout is on the left edge, at ROTATION_270 on the right.
	DisplayRotation int `json:"displayRotation,omitempty"`
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

// DeclaresAsset reports whether the frame named this file.
//
// This is an equality against two names the skin itself declared rather than a
// path test. A caller cannot reach outside the skin directory because it
// cannot name anything the layout did not, so there is no traversal to
// sanitise and no separate encoding of "..", "/" or a symlink to get wrong.
//
// The asset endpoint checks the whole skin rather than this, because a skin
// with its own landscape art names different files per orientation.
func (f *DeviceFrame) DeclaresAsset(name string) bool {
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
