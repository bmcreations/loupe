package main

import (
	"image"
	"image/color"
	"image/draw"
	"sort"
)

// Options tune a comparison. The zero value is not usable; start from
// defaultOptions.
type Options struct {
	// Block is the side of the square blocks the screen is cut into. A block
	// is the unit that differs or doesn't, so one stray antialiased pixel
	// can't fail a comparison on its own.
	Block int
	// PixelDelta is how far apart, on the largest of the three channels, two
	// pixels must be to count as different. JPEG noise and font hinting stay
	// under it.
	PixelDelta int
	// BlockShare is the share of a block's unmasked pixels that must differ
	// for the block to fail.
	BlockShare float64
	// Slack is how far, in reference pixels, a shot pixel may look for a
	// matching reference pixel. A reference scaled up to the screen blurs
	// every edge, and text rasterises a pixel apart from Figma's, so without
	// it every glyph outline counts as a difference.
	Slack int
}

var defaultOptions = Options{Block: 16, PixelDelta: 40, BlockShare: 0.08, Slack: 1}

// Rect is [x1, y1, x2, y2] in screenshot pixels, the space /api/ui uses.
type Rect [4]int

func (r Rect) empty() bool { return r[2] <= r[0] || r[3] <= r[1] }

func (r Rect) contains(o Rect) bool {
	return o[0] >= r[0] && o[1] >= r[1] && o[2] <= r[2] && o[3] <= r[3]
}

func (r Rect) intersect(o Rect) Rect {
	x := Rect{max(r[0], o[0]), max(r[1], o[1]), min(r[2], o[2]), min(r[3], o[3])}
	if x.empty() {
		return Rect{}
	}
	return x
}

func (r Rect) area() int {
	if r.empty() {
		return 0
	}
	return (r[2] - r[0]) * (r[3] - r[1])
}

// Region is a run of neighbouring failed blocks.
type Region struct {
	Bounds Rect `json:"bounds"`
	// Delta is the share of the region's unmasked pixels that differ.
	Delta float64  `json:"delta"`
	Node  *NodeRef `json:"node,omitempty"`
}

// Result is a comparison's outcome.
type Result struct {
	// Score is the share of the unmasked screen in failed blocks: 0 matches.
	Score   float64  `json:"score"`
	Regions []Region `json:"regions"`
	// Placed is where the scaled reference sits on the screenshot.
	Placed Rect `json:"placed"`
}

// placeReference scales ref to the shot's width and puts it top, below the
// given offset. A Figma frame is in points and the shot in pixels, and the
// aspect ratios rarely match, so the width decides the scale and the heights
// are left to disagree: rows the reference doesn't cover count as masked.
func placeReference(ref image.Image, shotW, top int) Rect {
	rb := ref.Bounds()
	if rb.Dx() == 0 {
		return Rect{}
	}
	h := rb.Dy() * shotW / rb.Dx()
	return Rect{0, top, shotW, top + h}
}

// scaleTo resamples src into a w×h RGBA with a box filter when shrinking and
// nearest neighbour when growing. Good enough for a block comparison; nothing
// here is shown to anyone at full size.
func scaleTo(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	for y := 0; y < h; y++ {
		y0 := sb.Min.Y + y*sh/h
		y1 := max(sb.Min.Y+(y+1)*sh/h, y0+1)
		for x := 0; x < w; x++ {
			x0 := sb.Min.X + x*sw/w
			x1 := max(sb.Min.X+(x+1)*sw/w, x0+1)
			var r, g, b, n uint32
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, _ := src.At(sx, sy).RGBA()
					r, g, b, n = r+cr, g+cg, b+cb, n+1
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(b / n >> 8), 0xff})
		}
	}
	return dst
}

func toRGBA(src image.Image) *image.RGBA {
	if m, ok := src.(*image.RGBA); ok && m.Bounds().Min == (image.Point{}) {
		return m
	}
	b := src.Bounds()
	m := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(m, m.Bounds(), src, b.Min, draw.Src)
	return m
}

// Compare lines ref up against shot and reports where they differ. masks are
// in shot pixels; pixels under one, or outside the placed reference, are
// neither counted nor drawn as different. diff is the shot, dimmed, with the
// differing pixels in red and the masked ones hatched grey.
func Compare(ref, shot image.Image, top int, masks []Rect, o Options) (Result, *image.RGBA) {
	s := toRGBA(shot)
	W, H := s.Bounds().Dx(), s.Bounds().Dy()
	placed := placeReference(ref, W, top)
	r := scaleTo(ref, placed[2]-placed[0], placed[3]-placed[1])
	screen := Rect{0, 0, W, H}
	cover := placed.intersect(screen)
	thr := uint8(o.PixelDelta)

	masked := func(x, y int) bool {
		if x < cover[0] || x >= cover[2] || y < cover[1] || y >= cover[3] {
			return true
		}
		for _, m := range masks {
			if x >= m[0] && x < m[2] && y >= m[1] && y < m[3] {
				return true
			}
		}
		return false
	}

	// rad is Slack in shot pixels: one reference pixel spans W/refW of them.
	rad := 0
	if rw := ref.Bounds().Dx(); o.Slack > 0 && rw > 0 {
		rad = (o.Slack*W + rw - 1) / rw
	}
	rW, rH := r.Bounds().Dx(), r.Bounds().Dy()
	far := func(sp color.RGBA, px, py int) bool {
		for dy := -rad; dy <= rad; dy++ {
			for dx := -rad; dx <= rad; dx++ {
				qx, qy := px+dx, py+dy
				if qx < 0 || qy < 0 || qx >= rW || qy >= rH {
					continue
				}
				rp := r.RGBAAt(qx, qy)
				if absd(sp.R, rp.R) <= thr && absd(sp.G, rp.G) <= thr && absd(sp.B, rp.B) <= thr {
					return false
				}
			}
		}
		return true
	}

	bs := o.Block
	cols, rows := (W+bs-1)/bs, (H+bs-1)/bs
	bad := make([]int, cols*rows)  // differing pixels per block
	live := make([]int, cols*rows) // unmasked pixels per block
	// tight is each block's box around its differing pixels, so a region
	// hugs what changed instead of the block grid, and lands inside the node
	// it's about.
	tight := make([]Rect, cols*rows)
	diff := image.NewRGBA(image.Rect(0, 0, W, H))
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			sp := s.RGBAAt(x, y)
			if masked(x, y) {
				g := uint8(110)
				if (x+y)/6%2 == 0 {
					g = 140
				}
				diff.SetRGBA(x, y, color.RGBA{g, g, g, 0xff})
				continue
			}
			bi := (y/bs)*cols + x/bs
			live[bi]++
			if far(sp, x-placed[0], y-placed[1]) {
				if bad[bi] == 0 {
					tight[bi] = Rect{x, y, x + 1, y + 1}
				} else {
					t := &tight[bi]
					t[0], t[1], t[2], t[3] = min(t[0], x), min(t[1], y), max(t[2], x+1), max(t[3], y+1)
				}
				bad[bi]++
				diff.SetRGBA(x, y, color.RGBA{0xff, 0x2d, 0x2d, 0xff})
				continue
			}
			// Dim the rest so the red reads.
			diff.SetRGBA(x, y, color.RGBA{sp.R/3 + 150, sp.G/3 + 150, sp.B/3 + 150, 0xff})
		}
	}

	failed := make([]bool, cols*rows)
	liveTotal, failedLive := 0, 0
	for i := range bad {
		liveTotal += live[i]
		if live[i] > 0 && float64(bad[i]) >= o.BlockShare*float64(live[i]) {
			failed[i] = true
			failedLive += live[i]
		}
	}
	res := Result{Placed: placed, Regions: []Region{}}
	if liveTotal > 0 {
		res.Score = float64(failedLive) / float64(liveTotal)
	}

	// Merge neighbouring failed blocks (4-connected) into regions.
	seen := make([]bool, len(failed))
	for start := range failed {
		if !failed[start] || seen[start] {
			continue
		}
		stack := []int{start}
		seen[start] = true
		box := Rect{W, H, 0, 0}
		nb, nl := 0, 0
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			cx, cy := i%cols, i/cols
			t := tight[i]
			box = Rect{min(box[0], t[0]), min(box[1], t[1]), max(box[2], t[2]), max(box[3], t[3])}
			nb += bad[i]
			nl += live[i]
			for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				nx, ny := cx+d[0], cy+d[1]
				if nx < 0 || ny < 0 || nx >= cols || ny >= rows {
					continue
				}
				j := ny*cols + nx
				if failed[j] && !seen[j] {
					seen[j] = true
					stack = append(stack, j)
				}
			}
		}
		res.Regions = append(res.Regions, Region{Bounds: box, Delta: float64(nb) / float64(nl)})
	}
	// Biggest first: the region worth looking at leads.
	sort.SliceStable(res.Regions, func(i, j int) bool {
		return res.Regions[i].Bounds.area() > res.Regions[j].Bounds.area()
	})
	return res, diff
}

func absd(a, b uint8) uint8 {
	if a > b {
		return a - b
	}
	return b - a
}
