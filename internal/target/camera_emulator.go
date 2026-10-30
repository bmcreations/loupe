package target

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
	"path/filepath"
	"strconv"
)

// The emulator's imagefile camera does not show the image the way it reads.
// With the back camera's sensor at 90 degrees, which is the emulator's
// default and what every AVD here has, it center-crops the image to the
// sensor's 4:3, then hands the app only the left 3:4 of that crop, upright.
// Measured on Pixel_7a_API_36 (emulator 37.2.12, sensor 1280x960) with a
// labeled grid: a 1920x1080 image showed x 240-1050 and every row, a
// 1080x1920 one x 0-610 and the middle 810 rows. A centered code ends up on
// the frame's right edge or off it.
//
// So loupe lays the image out for that: a 4:3 canvas with the image fitted
// whole into its left 3:4, on white, the way the iOS shim fits it. The app
// then sees the whole image, centered. Other sensor orientations are passed
// through untouched, unmeasured.
const (
	camCanvasW = 1280
	camCanvasH = 960
	camSliceW  = camCanvasH * 3 / 4
)

// backCameraOrientation is hw.camera.back.orientation from the AVD's
// config.ini, or the emulator's default of 90 when it is unset or unread.
func backCameraOrientation(avdDir string) int {
	if avdDir == "" {
		return 90
	}
	kv, err := readINI(filepath.Join(avdDir, "config.ini"))
	if err != nil {
		return 90
	}
	if n, err := strconv.Atoi(kv["hw.camera.back.orientation"]); err == nil {
		return n
	}
	return 90
}

// emulatorCameraImage is the PNG staged for an emulator whose back sensor is
// at orientation degrees, from the PNG the caller sent.
func emulatorCameraImage(pngBytes []byte, orientation int) ([]byte, error) {
	if orientation != 90 {
		return pngBytes, nil
	}
	src, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, err
	}
	dst := image.NewRGBA(image.Rect(0, 0, camCanvasW, camCanvasH))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	sb := src.Bounds()
	w, h := camSliceW, sb.Dy()*camSliceW/sb.Dx()
	if h > camCanvasH {
		w, h = sb.Dx()*camCanvasH/sb.Dy(), camCanvasH
	}
	w, h = max(w, 1), max(h, 1)
	at := image.Pt((camSliceW-w)/2, (camCanvasH-h)/2)
	scaleInto(dst, image.Rectangle{Min: at, Max: at.Add(image.Pt(w, h))}, src)
	var b bytes.Buffer
	if err := png.Encode(&b, dst); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// scaleInto draws src over r in dst, each destination pixel the average of
// the source pixels under it, so a large photo shrinks without aliasing and a
// small code grows with hard edges.
func scaleInto(dst *image.RGBA, r image.Rectangle, src image.Image) {
	sb := src.Bounds()
	s := image.NewRGBA(image.Rect(0, 0, sb.Dx(), sb.Dy()))
	draw.Draw(s, s.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(s, s.Bounds(), src, sb.Min, draw.Over)
	sw, sh, dw, dh := sb.Dx(), sb.Dy(), r.Dx(), r.Dy()
	for y := 0; y < dh; y++ {
		y0 := y * sh / dh
		y1 := max((y+1)*sh/dh, y0+1)
		for x := 0; x < dw; x++ {
			x0 := x * sw / dw
			x1 := max((x+1)*sw/dw, x0+1)
			var sum [3]int
			for sy := y0; sy < y1; sy++ {
				row := s.Pix[sy*s.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4:]
					sum[0] += int(p[0])
					sum[1] += int(p[1])
					sum[2] += int(p[2])
				}
			}
			n := (y1 - y0) * (x1 - x0)
			d := dst.Pix[(r.Min.Y+y)*dst.Stride+(r.Min.X+x)*4:]
			d[0], d[1], d[2], d[3] = uint8(sum[0]/n), uint8(sum[1]/n), uint8(sum[2]/n), 255
		}
	}
}
