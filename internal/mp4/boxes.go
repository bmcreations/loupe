package mp4

import (
	"encoding/binary"

	"github.com/bmcreations/loupe/internal/h264"
)

// unity is the identity transform matrix mvhd and tkhd carry.
var unity = []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(vs ...uint32) []byte {
	var b []byte
	for _, v := range vs {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
func zeros(n int) []byte  { return make([]byte, n) }

func cat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func box(typ string, payload ...[]byte) []byte {
	body := cat(payload...)
	return cat(u32(uint32(8+len(body))), []byte(typ), body)
}

func fullBox(typ string, version uint8, flags uint32, payload ...[]byte) []byte {
	return box(typ, u32(uint32(version)<<24|flags&0xFFFFFF), cat(payload...))
}

func ftypBox() []byte {
	return box("ftyp", []byte("isom"), u32(0x200), []byte("isom"), []byte("iso2"), []byte("avc1"), []byte("mp41"))
}

func mvhdBox(duration uint32) []byte {
	return fullBox("mvhd", 0, 0,
		u32(0, 0, timescale, duration, 0x00010000), u16(0x0100), zeros(10),
		u32(unity...), zeros(24), u32(2))
}

func tkhdBox(duration uint32, width, height int) []byte {
	return fullBox("tkhd", 0, 3,
		u32(0, 0, 1), zeros(4), u32(duration), zeros(8),
		u16(0), u16(0), u16(0), zeros(2), // layer, group, volume, reserved
		u32(unity...), u32(uint32(width)<<16, uint32(height)<<16))
}

func mdhdBox(duration uint32) []byte {
	return fullBox("mdhd", 0, 0, u32(0, 0, timescale, duration), u16(0x55C4), u16(0))
}

func hdlrBox() []byte {
	return fullBox("hdlr", 0, 0, u32(0), []byte("vide"), zeros(12), []byte("VideoHandler\x00"))
}

func vmhdBox() []byte { return fullBox("vmhd", 0, 1, zeros(8)) }

func dinfBox() []byte {
	return box("dinf", fullBox("dref", 0, 0, u32(1), fullBox("url ", 0, 1)))
}

func avcCBox(sps, pps []byte) []byte {
	return box("avcC", []byte{1, sps[1], sps[2], sps[3], 0xFF, 0xE1},
		u16(uint16(len(sps))), sps, []byte{1}, u16(uint16(len(pps))), pps,
		avcCExtension(sps))
}

// avcCExtension is the tail ISO/IEC 14496-15 adds for the High profiles:
// chroma format and bit depths, each behind reserved one bits, then no SPS
// extensions. Other profiles end at the PPS. An SPS that does not parse
// writes 4:2:0 at 8 bits, which is what every encoder here produces.
func avcCExtension(sps []byte) []byte {
	switch sps[1] {
	case 100, 110, 122, 144:
	default:
		return nil
	}
	chroma, lumaDepth, chromaDepth, err := h264.SPSChroma(sps)
	if err != nil {
		chroma, lumaDepth, chromaDepth = 1, 0, 0
	}
	return []byte{
		0xFC | byte(chroma&0x03),
		0xF8 | byte(lumaDepth&0x07),
		0xF8 | byte(chromaDepth&0x07),
		0,
	}
}

func avc1Box(width, height int, sps, pps []byte) []byte {
	return box("avc1", zeros(6), u16(1), zeros(16),
		u16(uint16(width)), u16(uint16(height)),
		u32(0x00480000, 0x00480000, 0), u16(1), zeros(32),
		u16(0x0018), u16(0xFFFF), avcCBox(sps, pps))
}

func stsdBox(width, height int, descs []description) []byte {
	entries := make([][]byte, len(descs))
	for i, d := range descs {
		entries[i] = avc1Box(width, height, d.sps, d.pps)
	}
	return fullBox("stsd", 0, 0, u32(uint32(len(descs))), cat(entries...))
}

type run struct{ count, delta uint32 }

func sttsBox(runs []run) []byte {
	b := u32(uint32(len(runs)))
	for _, r := range runs {
		b = append(b, u32(r.count, r.delta)...)
	}
	return fullBox("stts", 0, 0, b)
}

func stscBox(starts []descStart) []byte {
	b := u32(uint32(len(starts)))
	for _, s := range starts {
		b = append(b, u32(s.firstSample, 1, s.index)...)
	}
	return fullBox("stsc", 0, 0, b)
}

func stszBox(samples []sample) []byte {
	b := u32(0, uint32(len(samples)))
	for _, s := range samples {
		b = append(b, u32(s.size)...)
	}
	return fullBox("stsz", 0, 0, b)
}

// stssBox returns nil when every sample is a key, which is how the format
// says so.
func stssBox(samples []sample) []byte {
	var keys []byte
	n := 0
	for i, s := range samples {
		if s.key {
			keys = append(keys, u32(uint32(i+1))...)
			n++
		}
	}
	if n == len(samples) {
		return nil
	}
	return fullBox("stss", 0, 0, u32(uint32(n)), keys)
}

func co64Box(samples []sample) []byte {
	b := u32(uint32(len(samples)))
	for _, s := range samples {
		b = append(b, u64(s.offset)...)
	}
	return fullBox("co64", 0, 0, b)
}
