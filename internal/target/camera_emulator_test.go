package target

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// halves is w x h, red on the left half and blue on the right.
func halves(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{255, 0, 0, 255}
			if x >= w/2 {
				c = color.RGBA{0, 0, 255, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestEmulatorCameraImageFitsIntoTheLeftSlice(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
		// fit is the rectangle the image should land on.
		fit image.Rectangle
	}{
		{"landscape", 1920, 1080, image.Rect(0, 277, 720, 682)},
		{"portrait", 1080, 1920, image.Rect(90, 0, 630, 960)},
		{"small square", 300, 300, image.Rect(0, 120, 720, 840)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := emulatorCameraImage(encodePNG(t, halves(tc.w, tc.h)), 90)
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			if b := img.Bounds(); b.Dx() != camCanvasW || b.Dy() != camCanvasH {
				t.Fatalf("canvas %v, want %dx%d", b, camCanvasW, camCanvasH)
			}
			mid := (tc.fit.Min.Y + tc.fit.Max.Y) / 2
			check := func(x, y int, want color.RGBA) {
				t.Helper()
				r, g, b, _ := img.At(x, y).RGBA()
				if got := (color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 255}); got != want {
					t.Errorf("(%d,%d) = %v, want %v", x, y, got, want)
				}
			}
			red, blue, white := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}, color.RGBA{255, 255, 255, 255}
			check(tc.fit.Min.X+2, mid, red)
			check(tc.fit.Max.X-3, mid, blue)
			check(camSliceW+10, mid, white) // right of the slice the app never sees
			if tc.fit.Min.Y > 0 {
				check(camSliceW/2, tc.fit.Min.Y-2, white)
			}
			if tc.fit.Min.X > 0 {
				check(tc.fit.Min.X-2, mid, white)
			}
		})
	}
}

func TestEmulatorCameraImagePassesOtherOrientationsThrough(t *testing.T) {
	in := encodePNG(t, halves(64, 48))
	out, err := emulatorCameraImage(in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Fatal("orientation 0 changed the image")
	}
}

func TestBackCameraOrientation(t *testing.T) {
	dir := t.TempDir()
	if got := backCameraOrientation(dir); got != 90 {
		t.Errorf("no config.ini: %d, want the default 90", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.ini"), []byte("hw.camera.back = emulated\nhw.camera.back.orientation = 270\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := backCameraOrientation(dir); got != 270 {
		t.Errorf("got %d, want 270", got)
	}
}
