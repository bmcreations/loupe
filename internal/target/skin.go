package target

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// skinNode is one brace block from an emulator skin layout.
//
// The format is nested blocks of `name { ... }` with `key value` fields, and
// nothing here needs more than that: no quoting, no arrays, no escapes. A
// hand-rolled reader is smaller than a grammar and fails on the one shape that
// matters, an unbalanced brace.
type skinNode struct {
	fields map[string]string
	kids   map[string]*skinNode
}

func newSkinNode() *skinNode {
	return &skinNode{fields: map[string]string{}, kids: map[string]*skinNode{}}
}

// at walks a path of block names, returning nil rather than an error, because
// every caller's next move on a miss is the same: treat the skin as unusable.
func (n *skinNode) at(path ...string) *skinNode {
	for _, name := range path {
		if n == nil {
			return nil
		}
		n = n.kids[name]
	}
	return n
}

// intField reads a field as an int, reporting whether it was there and numeric.
func (n *skinNode) intField(key string) (int, bool) {
	if n == nil {
		return 0, false
	}
	v, err := strconv.Atoi(n.fields[key])
	if err != nil {
		return 0, false
	}
	return v, true
}

func (n *skinNode) field(key string) string {
	if n == nil {
		return ""
	}
	return n.fields[key]
}

// parseSkinLayout reads a skin layout into a tree.
//
// Tokenised rather than read a line at a time. The SDK's own skins put every
// brace on its own line, but nothing in the format requires that, and a
// line-oriented reader silently mis-parses `display { width 1080` as a field
// named display -- wrong values rather than an error, on a skin this machine
// does not happen to have installed.
func parseSkinLayout(src string) (*skinNode, error) {
	toks := skinTokens(src)
	root := newSkinNode()
	stack := []*skinNode{root}

	for i := 0; i < len(toks); i++ {
		switch tok := toks[i]; tok {
		case "}":
			if len(stack) == 1 {
				return nil, fmt.Errorf("skin layout: unmatched }")
			}
			stack = stack[:len(stack)-1]

		case "{":
			return nil, fmt.Errorf("skin layout: unnamed block")

		default:
			if i+1 >= len(toks) {
				return nil, fmt.Errorf("skin layout: %q has no value", tok)
			}
			i++
			if toks[i] == "{" {
				kid := newSkinNode()
				stack[len(stack)-1].kids[tok] = kid
				stack = append(stack, kid)
				continue
			}
			if toks[i] == "}" {
				return nil, fmt.Errorf("skin layout: %q has no value", tok)
			}
			stack[len(stack)-1].fields[tok] = toks[i]
		}
	}

	if len(stack) != 1 {
		return nil, fmt.Errorf("skin layout ends inside a block")
	}
	return root, nil
}

// skinTokens splits a layout into braces and words, dropping comments. The
// comment strip is per line because "#" runs to the end of one.
func skinTokens(src string) []string {
	var toks []string
	for _, line := range strings.Split(src, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, field := range strings.Fields(line) {
			for field != "" {
				i := strings.IndexAny(field, "{}")
				switch {
				case i < 0:
					toks = append(toks, field)
					field = ""
				case i > 0:
					toks = append(toks, field[:i], string(field[i]))
					field = field[i+1:]
				default:
					toks = append(toks, string(field[0]))
					field = field[1:]
				}
			}
		}
	}
	return toks
}

// kidNames lists a block's child blocks in a stable order.
//
// skinNode holds its children in a map, and a skin declares more than one
// layout, so without this the layout picked for a given orientation would
// depend on Go's map iteration and differ between runs.
func (n *skinNode) kidNames() []string {
	if n == nil {
		return nil
	}
	names := make([]string, 0, len(n.kids))
	for name := range n.kids {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// displaySize is the screen the skin's device part declares, in the
// orientation the art was drawn for.
func (n *skinNode) displaySize() (int, int, bool) {
	display := n.at("parts", "device", "display")
	w, okW := display.intField("width")
	h, okH := display.intField("height")
	if !okW || !okH || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// frame is the bezel for a device in its natural orientation, and is what the
// frame endpoint answered before rotation was handled at all.
func (n *skinNode) frame() *DeviceFrame {
	return n.frameFor(0)
}

// frameFor picks the bezel to draw for a device at the given rotation, which
// is a quarter-turn count matching Android's Surface.ROTATION_* constants.
//
// A skin may declare a layout per orientation, and the block names are labels
// rather than an answer: pixel_tablet's only layout is called "landscape" and
// describes the tablet's natural, landscape-shaped orientation. So a layout is
// classified by the shape of the display rect it places, never by its name.
//
// Every Pixel 3 and later skin the SDK installs declares exactly one layout,
// so on the hardware this is used with there is nothing to pick and the frame
// is produced by turning that one. Turning skin art is exact rather than an
// approximation: the art is a photograph of a rigid object, and the same
// object a quarter turn round is the same photograph a quarter turn round.
func (n *skinNode) frameFor(rotation int) *DeviceFrame {
	dw, dh, ok := n.displaySize()
	if !ok {
		return nil
	}

	// What the viewer should be looking at: the natural shape, turned over if
	// the device is on its side.
	wantW, wantH := dw, dh
	if rotation%2 != 0 {
		wantW, wantH = dh, dw
	}

	var natural *DeviceFrame
	for _, name := range n.at("layouts").kidNames() {
		f := n.frameFrom(name)
		if f == nil {
			continue
		}
		if landscape(f.Display.Width, f.Display.Height) == landscape(wantW, wantH) {
			// Upside down is the one case a matching layout cannot draw by
			// itself: every layout is authored the right way up.
			if rotation == 2 {
				return f.turned(2)
			}
			return f.turned(0)
		}
		if natural == nil && landscape(f.Display.Width, f.Display.Height) == landscape(dw, dh) {
			natural = f
		}
	}
	if natural == nil {
		return nil
	}
	// Turning the art clockwise is the opposite of turning the device, which
	// is what a rotation reports.
	return natural.turned((4 - rotation%4) % 4)
}

func landscape(w, h int) bool { return w > h }

// frameFrom builds the bezel one named layout describes, as the skin drew it.
//
// The display's offset and the art both live in partN blocks that name an
// entry in "parts", and the numbering is not fixed across skins. The device is
// found as the part that declares a display rather than by the name "device",
// and whatever other part the layout names supplies the art, so a skin that
// calls its pieces something else still parses.
func (n *skinNode) frameFrom(name string) *DeviceFrame {
	layout := n.at("layouts", name)
	fw, okFW := layout.intField("width")
	fh, okFH := layout.intField("height")
	if !okFW || !okFH || fw <= 0 || fh <= 0 {
		return nil
	}

	var display, art *skinNode
	x, y, quarters := 0, 0, 0
	for _, part := range layout.kidNames() {
		if !strings.HasPrefix(part, "part") {
			continue
		}
		kid := layout.kids[part]
		ref := n.at("parts", kid.field("name"))
		if ref == nil {
			continue
		}
		if d := ref.at("display"); d != nil {
			px, okX := kid.intField("x")
			py, okY := kid.intField("y")
			if !okX || !okY {
				continue
			}
			display, x, y = d, px, py
			quarters, _ = kid.intField("rotation")
			continue
		}
		if art == nil {
			art = ref
		}
	}
	if display == nil {
		return nil
	}

	dw, okW := display.intField("width")
	dh, okH := display.intField("height")
	if !okW || !okH || dw <= 0 || dh <= 0 {
		return nil
	}
	// A layout may place the display on its side, which is how a skin with
	// separate landscape art states that the screen is turned inside it.
	if quarters%2 != 0 {
		dw, dh = dh, dw
	}

	corner, _ := display.intField("corner_radius")
	return &DeviceFrame{
		Source:       "skin",
		Width:        fw,
		Height:       fh,
		Display:      FrameRect{X: x, Y: y, Width: dw, Height: dh},
		CornerRadius: corner,
		Background:   art.at("background").field("image"),
		Mask:         art.at("foreground").field("mask"),
	}
}

// declaresAsset reports whether any layout in this skin names the file, which
// is what the asset endpoint checks against.
//
// It is the whole skin rather than the one frame being drawn, because a skin
// with separate landscape art names different files per orientation and the
// browser keeps the other orientation's art cached across a rotation.
func (n *skinNode) declaresAsset(name string) bool {
	if n == nil || name == "" {
		return false
	}
	for _, part := range n.at("parts").kidNames() {
		p := n.at("parts", part)
		if name == p.at("background").field("image") || name == p.at("foreground").field("mask") {
			return true
		}
	}
	return false
}

// turned returns the frame rotated a quarter turn clockwise, quarters times,
// carrying the count so the client can turn the art the same way.
//
// Only the geometry moves here. The art keeps its filename and its own pixel
// grid, because rotating a webp server-side would mean re-encoding it and the
// browser already turns an image for free.
func (f *DeviceFrame) turned(quarters int) *DeviceFrame {
	if f == nil {
		return nil
	}
	quarters = ((quarters % 4) + 4) % 4
	out := *f
	for i := 0; i < quarters; i++ {
		d := out.Display
		out.Display = FrameRect{
			X:      out.Height - (d.Y + d.Height),
			Y:      d.X,
			Width:  d.Height,
			Height: d.Width,
		}
		out.Width, out.Height = out.Height, out.Width
	}
	out.Rotation = quarters
	return &out
}
