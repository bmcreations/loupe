package h264

import (
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

// The two parameter sets emulator-5554 actually emitted either side of a
// rotation, captured with cmd/rotprobe. The portrait figure is cross-checked
// against scrcpy's own codec meta, which reported 480x1080 for the same
// stream, so this is pinned to a number the device stated independently.
func TestSPSDimensionsReadTheRealRotationPair(t *testing.T) {
	for _, tc := range []struct {
		name string
		sps  string
		w, h int
	}{
		{"portrait", "6742c0298d681e0227e59080808083c2211a80", 480, 1080},
		{"landscape", "6742c0298d680440f7979080808083c2211a80", 1080, 480},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, h, err := SPSDimensions(mustHex(t, tc.sps))
			if err != nil {
				t.Fatalf("SPSDimensions: %v", err)
			}
			if w != tc.w || h != tc.h {
				t.Fatalf("got %dx%d, want %dx%d", w, h, tc.w, tc.h)
			}
		})
	}
}

// Cropping is not decoration. 1080 is not a multiple of 16, so the encoder
// codes 1088 rows and crops 8 off the bottom; a parser that reports the
// macroblock size is wrong by exactly the amount that makes every touch
// coordinate drift, and is wrong in the direction that still looks plausible.
func TestSPSDimensionsAppliesCropping(t *testing.T) {
	w, h, err := SPSDimensions(mustHex(t, "6742c0298d680440f7979080808083c2211a80"))
	if err != nil {
		t.Fatalf("SPSDimensions: %v", err)
	}
	if w%16 == 0 && h%16 == 0 {
		t.Fatalf("got %dx%d, which is macroblock-aligned on both axes: "+
			"the 1080 axis must have been cropped from 1088", w, h)
	}
}

func TestSPSDimensionsRejectsNonSPS(t *testing.T) {
	// A PPS (NAL type 8), not an SPS.
	if _, _, err := SPSDimensions([]byte{0x68, 0xce, 0x3c, 0x80}); err == nil {
		t.Fatal("want an error for a non-SPS NAL, got none")
	}
	if _, _, err := SPSDimensions(nil); err == nil {
		t.Fatal("want an error for an empty NAL, got none")
	}
	// Truncated mid-bitstream: the reader must report running out rather than
	// returning a plausible-looking size built from zeros.
	if _, _, err := SPSDimensions(mustHex(t, "6742c029")); err == nil {
		t.Fatal("want an error for a truncated SPS, got none")
	}
}

// 0x000003 is an emulation prevention byte and is not part of the bitstream.
// Leaving one in shifts every field after it, which yields a confidently wrong
// size rather than an error.
//
// This is asserted against unescape directly, and deliberately not end to end.
// The escape needs about 22 byte-aligned zero bits, and the fields
// SPSDimensions reads are dense Exp-Golomb codes that never produce a run that
// long: a search over the header fields, every width to 4816 and the plausible
// heights found no baseline parameter set with an escape before the cropping
// flag. So removing the unescape call leaves an end-to-end test green no
// matter what SPS it is handed, and a test that cannot fail is worse than no
// test. unescape stays because the bitstream syntax requires it and a high
// profile's scaling lists are a far longer run of signed codes.
func TestUnescapeRemovesEmulationPreventionBytes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in, want []byte
	}{
		{"strips after two zeros", []byte{0x00, 0x00, 0x03, 0x01}, []byte{0x00, 0x00, 0x01}},
		{"keeps a lone 0x03", []byte{0x12, 0x03, 0x34}, []byte{0x12, 0x03, 0x34}},
		{"keeps 0x03 after one zero", []byte{0x00, 0x03, 0x34}, []byte{0x00, 0x03, 0x34}},
		{"resets the run after the escape", []byte{0x00, 0x00, 0x03, 0x00, 0x03}, []byte{0x00, 0x00, 0x00, 0x03}},
		{"strips twice", []byte{0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x02}, []byte{0x00, 0x00, 0x00, 0x00, 0x02}},
		{"leaves clean bytes alone", []byte{0x67, 0x42, 0xc0, 0x29}, []byte{0x67, 0x42, 0xc0, 0x29}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unescape(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %x, want %x", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %x, want %x", got, tc.want)
				}
			}
		})
	}
}

// SPSs from libx264, start codes stripped, each with an emulation prevention
// byte inside:
//
//	ffmpeg -f lavfi -i testsrc2=s=320x240:r=30 -frames:v 1 -c:v libx264 \
//	  -profile:v high -f h264 -
//	... -pix_fmt yuv422p10le -c:v libx264 -profile:v high422 -f h264 -
func TestSPSChromaReadsHighProfiles(t *testing.T) {
	cases := []struct {
		name                string
		sps                 []byte
		chroma, luma, cdpth uint32
	}{
		{"high", []byte{0x67, 0x64, 0x00, 0x0d, 0xac, 0xb2, 0x02, 0x83, 0xf6, 0x02, 0x20, 0x00, 0x00, 0x03, 0x00, 0x20, 0x00, 0x00, 0x07, 0x81, 0xe2, 0x85, 0x49}, 1, 0, 0},
		{"high422 10-bit", []byte{0x67, 0x7a, 0x00, 0x0d, 0xb6, 0xcd, 0x94, 0x14, 0x1f, 0xb0, 0x11, 0x00, 0x00, 0x03, 0x00, 0x01, 0x00, 0x00, 0x03, 0x00, 0x3c, 0x0f, 0x14, 0x29, 0x96}, 2, 2, 2},
		{"baseline", []byte{0x67, 0x42, 0xc0, 0x15, 0xd9, 0x01, 0x41, 0xfb, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x07, 0x80, 0xf1, 0x62, 0xe4, 0x80}, 1, 0, 0},
	}
	for _, c := range cases {
		chroma, luma, cdpth, err := SPSChroma(c.sps)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if chroma != c.chroma || luma != c.luma || cdpth != c.cdpth {
			t.Fatalf("%s: chroma %d depths %d/%d, want %d %d/%d", c.name, chroma, luma, cdpth, c.chroma, c.luma, c.cdpth)
		}
	}
	if _, _, _, err := SPSChroma([]byte{0x68, 0x64, 0x00, 0x0d}); err == nil {
		t.Fatal("a PPS parsed as an SPS")
	}
}
