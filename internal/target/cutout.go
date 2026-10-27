package target

import "strings"

// defaultDensity is the framework's baseline dpi, the divisor that turns dp
// into pixels.
const defaultDensity = 160

// parseCutoutSpec turns Android's cutout spec into a path plus the transform
// that places it, or nil when the spec carries no path.
//
// The spec is an SVG path followed by suffixes: "@dp" states the coordinates
// are density-independent, and "@left" or "@right" anchor the path to an edge
// rather than to the centre of the top. Everything else -- "@bottom",
// "@center_vertical", a second path after "@cutout" -- is dropped, because the
// frame draws one cutout at the top and a stream whose device has two has no
// way to show the other anyway.
//
// The path itself is returned untouched. Rewriting it would mean parsing and
// re-emitting every SVG command for the sake of an offset the renderer applies
// for free.
func parseCutoutSpec(spec string, displayWidth, density int) *FrameCutout {
	if density <= 0 {
		density = defaultDensity
	}

	// Anchors and units are read from the whole spec, since they may follow
	// any of its paths, but only the text before the first suffix is a path.
	lower := strings.ToLower(spec)
	scale := 1.0
	if strings.Contains(lower, "@dp") {
		scale = float64(density) / defaultDensity
	}

	dx := displayWidth / 2
	switch {
	case strings.Contains(lower, "@left"):
		dx = 0
	case strings.Contains(lower, "@right"):
		dx = displayWidth
	}

	path := spec
	if i := strings.IndexByte(path, '@'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return &FrameCutout{Path: path, Scale: scale, DX: dx}
}
