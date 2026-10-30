package target

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// The edit menu's Paste can be on screen and absent from the tree. With the
// keyboard up its items have no parent, so simcap's walk from the
// application never meets them: in Flipcash on an iPhone 17 simulator,
// iOS 27, the tree had the focused composer and every key but no Paste,
// while a hit test at the menu answered Paste at once. A drop that finds no
// Paste in the tree hit-tests around the field for it instead.

const (
	// pasteProbeReach is how far above and below the drop point the probe
	// looks, in pixels. The menu sits just above the caret, or below it
	// when there is no room: Flipcash's Paste was 90 to 225px above the
	// drop point.
	pasteProbeReach = 480
	// pasteProbeStep is the probe's spacing, in pixels: under the menu's
	// 132px height and Paste's 211px width at scale 3.
	pasteProbeStep = 30
)

// canHit is whether simcap's -caps lists hit. A -caps that gave no answer
// is not cached, so it is asked again on the next drop.
func (s *Simulator) canHit(ctx context.Context) bool {
	s.pbImageMu.Lock()
	defer s.pbImageMu.Unlock()
	if s.hitCap == nil {
		has, err := simcapHas(ctx, s.simcap, "hit")
		if err != nil {
			return false
		}
		s.hitCap = &has
	}
	return *s.hitCap
}

// hitAt is the element at x, y in device pixels, or nil when the point hits
// nothing. scale is pixels per point, from the tree the point came from.
func (s *Simulator) hitAt(ctx context.Context, x, y int, scale float64) (*Node, error) {
	out, err := s.input().do(ctx, 0, "hit %g %g", float64(x)/scale, float64(y)/scale)
	if err != nil {
		return nil, fmt.Errorf("hit test: %w", err)
	}
	var a axNode
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		return nil, fmt.Errorf("parse hit test: %w", err)
	}
	if a.Role == "" {
		return nil, nil
	}
	return axToNode(&a, scale), nil
}

// findPasteByHit hit-tests pasteProbes for the edit menu's Paste. It is nil
// when none answers, or when ui carries no scale to turn pixels into points.
func (s *Simulator) findPasteByHit(ctx context.Context, ui *UI, x, y int) *Node {
	if ui == nil || ui.scale <= 0 {
		return nil
	}
	for _, p := range pasteProbes(ui, x, y) {
		n, err := s.hitAt(ctx, p[0], p[1], ui.scale)
		if err != nil {
			return nil
		}
		if n != nil && strings.EqualFold(n.Text, pasteMatch.Text) {
			return n
		}
	}
	return nil
}

// pasteProbes are the points findPasteByHit tries, nearest the drop first:
// rows every pasteProbeStep within pasteProbeReach of y, in columns at the
// drop point and across the focused field, since the menu is placed on the
// caret and the caret can be anywhere along the field.
func pasteProbes(ui *UI, x, y int) [][2]int {
	xs := []int{x}
	if f := FocusedAll(ui); len(f) > 0 {
		b := f[len(f)-1].Bounds
		w := b[2] - b[0]
		for _, frac := range []float64{0.15, 0.5, 0.85} {
			xs = append(xs, b[0]+int(math.Round(float64(w)*frac)))
		}
	}
	var cols []int
	for _, c := range xs {
		c = min(max(c, 0), ui.Width-1)
		if !slices.ContainsFunc(cols, func(o int) bool { return o-c < pasteProbeStep && c-o < pasteProbeStep }) {
			cols = append(cols, c)
		}
	}
	var pts [][2]int
	for d := pasteProbeStep; d <= pasteProbeReach; d += pasteProbeStep {
		for _, row := range []int{y - d, y + d} {
			if row < 0 || row >= ui.Height {
				continue
			}
			for _, c := range cols {
				pts = append(pts, [2]int{c, row})
			}
		}
	}
	return pts
}
