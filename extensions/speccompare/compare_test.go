package main

import (
	"image"
	"image/color"
	"image/draw"
	"testing"
)

func solid(w, h int, c color.RGBA) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(m, m.Bounds(), &image.Uniform{c}, image.Point{}, draw.Src)
	return m
}

func fill(m *image.RGBA, r Rect, c color.RGBA) {
	draw.Draw(m, image.Rect(r[0], r[1], r[2], r[3]), &image.Uniform{c}, image.Point{}, draw.Src)
}

var (
	white = color.RGBA{0xff, 0xff, 0xff, 0xff}
	blue  = color.RGBA{0x0a, 0x84, 0xff, 0xff}
)

func TestIdenticalImagesScoreZero(t *testing.T) {
	shot := solid(320, 640, white)
	fill(shot, Rect{40, 100, 280, 160}, blue)
	res, _ := Compare(shot, shot, 0, nil, defaultOptions)
	if res.Score != 0 || len(res.Regions) != 0 {
		t.Fatalf("score %v, %d regions; want 0 and none", res.Score, len(res.Regions))
	}
}

// A Figma frame is in points: a reference a third the size of the shot must
// scale up and still match.
func TestReferenceInPointsScalesToTheShot(t *testing.T) {
	ref := solid(120, 240, white)
	fill(ref, Rect{20, 40, 100, 60}, blue)
	shot := solid(360, 720, white)
	fill(shot, Rect{60, 120, 300, 180}, blue)
	res, _ := Compare(ref, shot, 0, nil, defaultOptions)
	if res.Score != 0 {
		t.Fatalf("score %v, regions %v; want a match", res.Score, res.Regions)
	}
	if res.Placed != (Rect{0, 0, 360, 720}) {
		t.Fatalf("placed %v", res.Placed)
	}
}

func TestAMovedButtonIsOneRegionCoveringBothPlaces(t *testing.T) {
	ref := solid(320, 640, white)
	fill(ref, Rect{40, 400, 280, 448}, blue)
	shot := solid(320, 640, white)
	fill(shot, Rect{40, 416, 280, 464}, blue) // 16 px lower
	res, diff := Compare(ref, shot, 0, nil, defaultOptions)
	if len(res.Regions) != 2 {
		// The top and bottom strips differ; the overlap in the middle doesn't.
		t.Fatalf("%d regions %v; want the two strips the move uncovered", len(res.Regions), res.Regions)
	}
	for _, reg := range res.Regions {
		if reg.Bounds[1] < 384 || reg.Bounds[3] > 480 {
			t.Fatalf("region %v strays from the button", reg.Bounds)
		}
	}
	if res.Score <= 0 || res.Score > 0.05 {
		t.Fatalf("score %v; want small but not zero", res.Score)
	}
	if c := diff.RGBAAt(100, 405); c.R != 0xff || c.G > 0x40 {
		t.Fatalf("diff at a moved pixel is %v, want red", c)
	}
}

func TestMasksAreNotCompared(t *testing.T) {
	ref := solid(320, 640, white)
	shot := solid(320, 640, white)
	fill(shot, Rect{0, 0, 320, 48}, blue) // the live status bar
	if res, _ := Compare(ref, shot, 0, nil, defaultOptions); res.Score == 0 {
		t.Fatal("unmasked status bar compared as equal")
	}
	res, _ := Compare(ref, shot, 0, []Rect{{0, 0, 320, 48}}, defaultOptions)
	if res.Score != 0 {
		t.Fatalf("masked status bar still scores %v", res.Score)
	}
}

// Rows the reference doesn't reach are not differences: aspect ratios
// rarely match, and the extra height is someone else's problem.
func TestRowsBelowAShortReferenceAreMasked(t *testing.T) {
	ref := solid(320, 400, white)
	shot := solid(320, 640, white)
	fill(shot, Rect{0, 500, 320, 640}, blue)
	res, _ := Compare(ref, shot, 0, nil, defaultOptions)
	if res.Score != 0 {
		t.Fatalf("score %v from rows the reference doesn't cover", res.Score)
	}
}

func TestTopShiftsTheReferenceDown(t *testing.T) {
	ref := solid(320, 600, white)
	fill(ref, Rect{40, 0, 280, 40}, blue)
	shot := solid(320, 640, white)
	fill(shot, Rect{40, 40, 280, 80}, blue)
	if res, _ := Compare(ref, shot, 40, nil, defaultOptions); res.Score != 0 {
		t.Fatalf("score %v with top=40", res.Score)
	}
}

func TestSmallNoiseDoesNotFail(t *testing.T) {
	ref := solid(320, 640, white)
	shot := solid(320, 640, color.RGBA{0xf0, 0xf0, 0xf0, 0xff}) // under PixelDelta
	shot.SetRGBA(10, 10, blue)                                  // one stray pixel
	if res, _ := Compare(ref, shot, 0, nil, defaultOptions); res.Score != 0 {
		t.Fatalf("noise scored %v", res.Score)
	}
}

// Text rasterises a pixel apart from Figma's: thin strokes one reference
// pixel off must not fail, while a stroke well away from its spec does.
func TestEdgesOneReferencePixelOffMatch(t *testing.T) {
	ref := solid(120, 240, white)
	shot := solid(360, 720, white)
	for i := 0; i < 8; i++ {
		y := 20 + i*25
		fill(ref, Rect{10, y, 110, y + 1}, blue)
		fill(shot, Rect{30, (y + 1) * 3, 330, (y + 2) * 3}, blue) // one ref pixel lower
	}
	if res, _ := Compare(ref, shot, 0, nil, defaultOptions); res.Score != 0 {
		t.Fatalf("score %v, regions %v; want a pixel's offset to match", res.Score, res.Regions)
	}
	off := defaultOptions
	off.Slack = 0
	if res, _ := Compare(ref, shot, 0, nil, off); res.Score == 0 {
		t.Fatal("with no slack the offset strokes should differ, or this test checks nothing")
	}
	fill(shot, Rect{30, 650, 330, 656}, blue) // a stroke the spec doesn't have
	if res, _ := Compare(ref, shot, 0, nil, defaultOptions); len(res.Regions) != 1 {
		t.Fatalf("regions %v; want the one extra stroke", res.Regions)
	}
}
