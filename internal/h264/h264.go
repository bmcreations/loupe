// Package h264 provides the minimum bitstream inspection the server needs:
// locating parameter sets in an Annex B stream and deriving the codec string
// a browser's VideoDecoder expects.
package h264

import "fmt"

// NAL unit types we care about.
const (
	NALSlice uint8 = 1 // non-IDR picture
	NALIDR   uint8 = 5 // IDR picture, independently decodable
	NALSEI   uint8 = 6
	NALSPS   uint8 = 7
	NALPPS   uint8 = 8
	NALAUD   uint8 = 9
)

// Type returns the NAL unit type of a payload whose first byte is the NAL
// header. It returns 0 for an empty payload, which is not a valid type.
func Type(nal []byte) uint8 {
	if len(nal) == 0 {
		return 0
	}
	return nal[0] & 0x1f
}

// IsVCL reports whether a NAL carries picture data, meaning it terminates an
// access unit. Parameter sets and SEI do not.
func IsVCL(nal []byte) bool {
	t := Type(nal)
	return t == NALSlice || t == NALIDR
}

// SplitAnnexB walks an Annex B buffer and calls fn for each NAL unit payload,
// with start codes stripped. The slices alias buf and are only valid until the
// caller's next use of it.
func SplitAnnexB(buf []byte, fn func(nal []byte)) {
	starts := findStartCodes(buf)
	for i, s := range starts {
		end := len(buf)
		if i+1 < len(starts) {
			end = starts[i+1].offset
		}
		nal := buf[s.offset+s.length : end]
		// Trailing zero bytes belong to the next start code prefix, not the NAL.
		for len(nal) > 0 && nal[len(nal)-1] == 0 {
			nal = nal[:len(nal)-1]
		}
		if len(nal) > 0 {
			fn(nal)
		}
	}
}

type startCode struct {
	offset int
	length int
}

func findStartCodes(buf []byte) []startCode {
	var out []startCode
	for i := 0; i+2 < len(buf); {
		if buf[i] == 0 && buf[i+1] == 0 {
			if buf[i+2] == 1 {
				out = append(out, startCode{offset: i, length: 3})
				i += 3
				continue
			}
			if i+3 < len(buf) && buf[i+2] == 0 && buf[i+3] == 1 {
				out = append(out, startCode{offset: i, length: 4})
				i += 4
				continue
			}
		}
		i++
	}
	return out
}

// FindSPS returns the first sequence parameter set in an Annex B buffer, or
// nil if there is none.
func FindSPS(buf []byte) []byte {
	var sps []byte
	SplitAnnexB(buf, func(nal []byte) {
		if sps == nil && Type(nal) == NALSPS {
			sps = nal
		}
	})
	return sps
}

// CodecString derives the RFC 6381 codec identifier from a sequence parameter
// set, for example "avc1.42E01E".
//
// This matters more than it looks: a browser VideoDecoder configured with a
// guessed codec string either refuses to configure or silently falls back to a
// software decoder, and on an iPhone a software H.264 decode at 60fps is the
// difference between a usable session and a hot phone showing a slideshow.
func CodecString(sps []byte) (string, error) {
	// sps[0] is the NAL header; profile_idc, constraint flags and level_idc
	// follow immediately.
	if len(sps) < 4 {
		return "", fmt.Errorf("h264: SPS too short (%d bytes)", len(sps))
	}
	if Type(sps) != NALSPS {
		return "", fmt.Errorf("h264: expected SPS, got NAL type %d", Type(sps))
	}
	return fmt.Sprintf("avc1.%02X%02X%02X", sps[1], sps[2], sps[3]), nil
}

// StartsNewPicture reports whether a VCL NAL begins a new access unit.
//
// A picture may be split into several slices, each its own NAL with its own
// start code. Treating every slice as a frame inflates any frame-rate
// measurement by the slice count, so the boundary has to be read from the
// bitstream rather than assumed.
//
// The slice header opens with first_mb_in_slice, an unsigned Exp-Golomb value.
// The encoding of zero is a single 1 bit, so a picture's first slice is
// exactly the case where the top bit of the first RBSP byte is set.
func StartsNewPicture(nal []byte) bool {
	if len(nal) < 2 || !IsVCL(nal) {
		return false
	}
	return nal[1]&0x80 != 0
}
