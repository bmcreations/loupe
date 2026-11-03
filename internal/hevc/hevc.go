// Package hevc reads what loupe needs from an HEVC (H.265) parameter set: the
// RFC 6381 codec string a viewer's decoder is configured with. A physical
// iPhone over Wi-Fi sends HEVC, which reaches a viewer that asked for it
// without being decoded or re-encoded (session.ViewerOptions).
package hevc

import (
	"fmt"
	"strings"

	"github.com/bmcreations/loupe/internal/h264"
)

// NAL unit types, ITU-T H.265 table 7-1.
const (
	NalVPS = 32
	NalSPS = 33
	NalPPS = 34
)

// Type is a NAL unit's type, from the first of its two header bytes.
func Type(nal []byte) uint8 {
	if len(nal) == 0 {
		return 0xff
	}
	return (nal[0] >> 1) & 0x3f
}

// FindSPS returns the first SPS NAL unit in an Annex B buffer, header
// included, or nil.
func FindSPS(buf []byte) []byte {
	var sps []byte
	h264.SplitAnnexB(buf, func(nal []byte) {
		if sps == nil && Type(nal) == NalSPS {
			sps = nal
		}
	})
	return sps
}

// unescape drops the emulation prevention byte from each 00 00 03.
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// CodecString derives the RFC 6381 string ("hvc1.1.6.L153.B0") from an SPS's
// general profile_tier_level, as ISO/IEC 14496-15 annex E spells it: profile
// space and idc, the compatibility flags bit-reversed in hex, tier and level,
// then the constraint bytes with trailing zero bytes left off.
func CodecString(sps []byte) (string, error) {
	if Type(sps) != NalSPS {
		return "", fmt.Errorf("hevc: not an SPS (type %d)", Type(sps))
	}
	// After the 2-byte NAL header: one byte of vps id, max sub layers and
	// temporal nesting, then the general profile_tier_level's 12 bytes.
	b := unescape(sps[2:])
	if len(b) < 13 {
		return "", fmt.Errorf("hevc: SPS too short (%d bytes)", len(b))
	}
	ptl := b[1:13]
	space := []string{"", "A", "B", "C"}[ptl[0]>>6]
	tier := "L"
	if ptl[0]&0x20 != 0 {
		tier = "H"
	}
	idc := ptl[0] & 0x1f
	compat := uint32(ptl[1])<<24 | uint32(ptl[2])<<16 | uint32(ptl[3])<<8 | uint32(ptl[4])
	var rev uint32
	for i := 0; i < 32; i++ {
		if compat&(1<<i) != 0 {
			rev |= 1 << (31 - i)
		}
	}
	cons := ptl[5:11]
	n := len(cons)
	for n > 0 && cons[n-1] == 0 {
		n--
	}
	parts := []string{"hvc1", fmt.Sprintf("%s%d", space, idc), fmt.Sprintf("%X", rev), fmt.Sprintf("%s%d", tier, ptl[11])}
	for _, c := range cons[:n] {
		parts = append(parts, fmt.Sprintf("%X", c))
	}
	return strings.Join(parts, "."), nil
}
