package h264

import (
	"errors"
	"fmt"
)

// errSPSTruncated is returned when the bitstream ends inside a field. It is
// distinct from a malformed NAL because a truncated parameter set is a
// transport problem, not a codec one.
var errSPSTruncated = errors.New("h264: SPS ended mid-bitstream")

// SPSDimensions returns the cropped luma dimensions a sequence parameter set
// describes: the picture size a decoder will actually present.
//
// This is the only in-band statement of the stream's size. scrcpy sends its
// codec meta once, at stream start, and never again -- so after a rotation the
// header says one thing and the pictures are another shape entirely. Reading
// the SPS is what closes that gap, because a new parameter set is emitted on
// every rotation (measured on emulator-5554 with cmd/rotprobe).
//
// Cropping is load-bearing rather than a detail. Macroblocks are 16x16, so a
// 1080-pixel axis is coded as 1088 and cropped by 8. Reporting the coded size
// would put every touch coordinate off by a factor of 1088/1080 -- small
// enough to look like imprecision rather than a bug, and largest exactly where
// people aim, at the far edge of the screen.
func SPSDimensions(sps []byte) (width, height int, err error) {
	if len(sps) < 4 {
		return 0, 0, fmt.Errorf("h264: SPS too short (%d bytes)", len(sps))
	}
	if Type(sps) != NALSPS {
		return 0, 0, fmt.Errorf("h264: expected SPS, got NAL type %d", Type(sps))
	}

	profileIDC := sps[1]
	// sps[2] is the constraint flags and sps[3] the level; neither affects the
	// geometry. The bitstream proper starts after them.
	r := &bitReader{buf: unescape(sps[4:])}

	if _, err := r.ue(); err != nil { // seq_parameter_set_id
		return 0, 0, err
	}

	// 4:2:0 unless a high profile says otherwise. The chroma format decides
	// the units the crop offsets are counted in, so it has to be tracked even
	// though nothing else here uses it.
	chromaFormatIDC := uint32(1)
	separateColourPlane := false

	switch profileIDC {
	case 100, 110, 122, 144, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		if chromaFormatIDC, err = r.ue(); err != nil {
			return 0, 0, err
		}
		if chromaFormatIDC == 3 {
			b, err := r.u1()
			if err != nil {
				return 0, 0, err
			}
			separateColourPlane = b == 1
		}
		if _, err := r.ue(); err != nil { // bit_depth_luma_minus8
			return 0, 0, err
		}
		if _, err := r.ue(); err != nil { // bit_depth_chroma_minus8
			return 0, 0, err
		}
		if _, err := r.u1(); err != nil { // qpprime_y_zero_transform_bypass_flag
			return 0, 0, err
		}
		present, err := r.u1()
		if err != nil {
			return 0, 0, err
		}
		if present == 1 {
			lists := 8
			if chromaFormatIDC == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if err := r.skipScalingList(i); err != nil {
					return 0, 0, err
				}
			}
		}
	}

	if _, err := r.ue(); err != nil { // log2_max_frame_num_minus4
		return 0, 0, err
	}
	pocType, err := r.ue()
	if err != nil {
		return 0, 0, err
	}
	switch pocType {
	case 0:
		if _, err := r.ue(); err != nil { // log2_max_pic_order_cnt_lsb_minus4
			return 0, 0, err
		}
	case 1:
		if _, err := r.u1(); err != nil { // delta_pic_order_always_zero_flag
			return 0, 0, err
		}
		if _, err := r.se(); err != nil { // offset_for_non_ref_pic
			return 0, 0, err
		}
		if _, err := r.se(); err != nil { // offset_for_top_to_bottom_field
			return 0, 0, err
		}
		n, err := r.ue() // num_ref_frames_in_pic_order_cnt_cycle
		if err != nil {
			return 0, 0, err
		}
		for i := uint32(0); i < n; i++ {
			if _, err := r.se(); err != nil {
				return 0, 0, err
			}
		}
	}

	if _, err := r.ue(); err != nil { // max_num_ref_frames
		return 0, 0, err
	}
	if _, err := r.u1(); err != nil { // gaps_in_frame_num_value_allowed_flag
		return 0, 0, err
	}
	widthMBs, err := r.ue() // pic_width_in_mbs_minus1
	if err != nil {
		return 0, 0, err
	}
	heightMapUnits, err := r.ue() // pic_height_in_map_units_minus1
	if err != nil {
		return 0, 0, err
	}
	frameMBsOnly, err := r.u1()
	if err != nil {
		return 0, 0, err
	}
	if frameMBsOnly == 0 {
		if _, err := r.u1(); err != nil { // mb_adaptive_frame_field_flag
			return 0, 0, err
		}
	}
	if _, err := r.u1(); err != nil { // direct_8x8_inference_flag
		return 0, 0, err
	}

	width = int(widthMBs+1) * 16
	// A field-coded stream stores half a frame per map unit, so the frame is
	// twice as tall as the map units say.
	height = (2 - int(frameMBsOnly)) * int(heightMapUnits+1) * 16

	cropping, err := r.u1()
	if err != nil {
		return 0, 0, err
	}
	if cropping == 1 {
		left, err := r.ue()
		if err != nil {
			return 0, 0, err
		}
		right, err := r.ue()
		if err != nil {
			return 0, 0, err
		}
		top, err := r.ue()
		if err != nil {
			return 0, 0, err
		}
		bottom, err := r.ue()
		if err != nil {
			return 0, 0, err
		}

		// Crop offsets are in chroma samples, so the luma units they represent
		// depend on the subsampling. Monochrome and 4:4:4-with-separate-planes
		// count in luma samples directly.
		cropUnitX, cropUnitY := 1, 2-int(frameMBsOnly)
		if chromaFormatIDC != 0 && !separateColourPlane {
			subWidthC, subHeightC := 2, 2
			switch chromaFormatIDC {
			case 2: // 4:2:2 is subsampled horizontally only
				subHeightC = 1
			case 3: // 4:4:4 is not subsampled
				subWidthC, subHeightC = 1, 1
			}
			cropUnitX = subWidthC
			cropUnitY = subHeightC * (2 - int(frameMBsOnly))
		}

		width -= cropUnitX * int(left+right)
		height -= cropUnitY * int(top+bottom)
	}

	if width <= 0 || height <= 0 {
		return 0, 0, fmt.Errorf("h264: SPS describes a %dx%d picture", width, height)
	}
	return width, height, nil
}

// unescape removes emulation prevention bytes: the encoder inserts a 0x03
// after any 0x0000 that would otherwise look like a start code, and those
// bytes are not part of the bitstream. Leaving one in shifts every field after
// it, which produces a confidently wrong size rather than an error.
func unescape(rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp))
	zeros := 0
	for _, b := range rbsp {
		if zeros >= 2 && b == 0x03 {
			zeros = 0
			continue
		}
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, b)
	}
	return out
}

// bitReader reads the Exp-Golomb and fixed-width fields an SPS is made of.
type bitReader struct {
	buf []byte
	pos int // in bits
}

func (r *bitReader) u1() (uint32, error) {
	if r.pos >= len(r.buf)*8 {
		return 0, errSPSTruncated
	}
	b := (r.buf[r.pos/8] >> (7 - uint(r.pos%8))) & 1
	r.pos++
	return uint32(b), nil
}

func (r *bitReader) un(n int) (uint32, error) {
	var v uint32
	for i := 0; i < n; i++ {
		b, err := r.u1()
		if err != nil {
			return 0, err
		}
		v = v<<1 | b
	}
	return v, nil
}

// ue reads an unsigned Exp-Golomb value: n leading zeros, a 1, then n more
// bits, giving 2^n - 1 + those bits.
func (r *bitReader) ue() (uint32, error) {
	zeros := 0
	for {
		b, err := r.u1()
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		zeros++
		// A run this long is not a value, it is a desynchronized reader. Stop
		// rather than shifting into an overflow.
		if zeros > 31 {
			return 0, fmt.Errorf("h264: SPS field has %d leading zeros", zeros)
		}
	}
	if zeros == 0 {
		return 0, nil
	}
	rest, err := r.un(zeros)
	if err != nil {
		return 0, err
	}
	return (1 << uint(zeros)) - 1 + rest, nil
}

// se reads a signed Exp-Golomb value, which maps the unsigned codes onto
// 0, 1, -1, 2, -2 and so on.
func (r *bitReader) se() (int32, error) {
	v, err := r.ue()
	if err != nil {
		return 0, err
	}
	if v%2 == 0 {
		return -int32(v / 2), nil
	}
	return int32(v/2) + 1, nil
}

func (r *bitReader) skipScalingList(i int) error {
	present, err := r.u1()
	if err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	size := 16
	if i >= 6 {
		size = 64
	}
	last := int32(8)
	next := int32(8)
	for j := 0; j < size; j++ {
		if next != 0 {
			delta, err := r.se()
			if err != nil {
				return err
			}
			next = (last + delta + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
	return nil
}

// SPSChroma returns the chroma format and bit depths an SPS declares, the
// fields an avcC box repeats for the High profiles. A profile that does not
// carry them is 4:2:0 at 8 bits: chroma 1 and both depths 0 (minus 8).
func SPSChroma(sps []byte) (chromaFormatIDC, bitDepthLumaMinus8, bitDepthChromaMinus8 uint32, err error) {
	if len(sps) < 4 {
		return 0, 0, 0, fmt.Errorf("h264: SPS too short (%d bytes)", len(sps))
	}
	if Type(sps) != NALSPS {
		return 0, 0, 0, fmt.Errorf("h264: expected SPS, got NAL type %d", Type(sps))
	}
	switch sps[1] {
	case 100, 110, 122, 144, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
	default:
		return 1, 0, 0, nil
	}
	r := &bitReader{buf: unescape(sps[4:])}
	if _, err := r.ue(); err != nil { // seq_parameter_set_id
		return 0, 0, 0, err
	}
	if chromaFormatIDC, err = r.ue(); err != nil {
		return 0, 0, 0, err
	}
	if chromaFormatIDC == 3 {
		if _, err := r.u1(); err != nil { // separate_colour_plane_flag
			return 0, 0, 0, err
		}
	}
	if bitDepthLumaMinus8, err = r.ue(); err != nil {
		return 0, 0, 0, err
	}
	if bitDepthChromaMinus8, err = r.ue(); err != nil {
		return 0, 0, 0, err
	}
	return chromaFormatIDC, bitDepthLumaMinus8, bitDepthChromaMinus8, nil
}
