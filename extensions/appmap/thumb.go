package main

import (
	"bytes"
	"image"
	"image/png"
)

// thumbnail scales a screenshot down to width w, averaging each block of
// pixels so text stays readable as grey rather than breaking into noise.
func thumbnail(b []byte, w int) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	r := src.Bounds()
	if r.Dx() <= w {
		return b, nil
	}
	h := r.Dy() * w / r.Dx()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := r.Min.Y+y*r.Dy()/h, r.Min.Y+(y+1)*r.Dy()/h
		for x := 0; x < w; x++ {
			x0, x1 := r.Min.X+x*r.Dx()/w, r.Min.X+(x+1)*r.Dx()/w
			var sr, sg, sb, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, _ := src.At(xx, yy).RGBA()
					sr, sg, sb, n = sr+uint64(cr), sg+uint64(cg), sb+uint64(cb), n+1
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = uint8(sr/n>>8), uint8(sg/n>>8), uint8(sb/n>>8), 255
		}
	}
	var out bytes.Buffer
	err = png.Encode(&out, dst)
	return out.Bytes(), err
}
