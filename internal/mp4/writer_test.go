package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
)

// Parameter sets from a one-frame libx264 baseline encode of testsrc, with
// their start codes stripped:
//
//	ffmpeg -f lavfi -i testsrc=size=320x240:rate=60 -frames:v 1 -pix_fmt yuv420p \
//	  -c:v libx264 -profile:v baseline -f h264 -
//
// spsTall is the same at 240x320. ppsAlt is pps with its last byte before the
// stop bit changed; nothing parses it, it only has to differ.
var (
	sps320x240 = []byte{0x67, 0x42, 0xc0, 0x15, 0xd9, 0x01, 0x41, 0xfb, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x07, 0x80, 0xf1, 0x62, 0xe4, 0x80}
	pps        = []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
	spsTall    = []byte{0x67, 0x42, 0xc0, 0x15, 0xd9, 0x03, 0xc2, 0x9b, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x07, 0x80, 0xf1, 0x62, 0xe4, 0x80}
	ppsAlt     = []byte{0x68, 0xcb, 0x83, 0xca, 0x20}
)

// spsHigh and ppsHigh are from a libx264 High profile encode of testsrc2,
// start codes stripped. spsHigh holds an emulation prevention byte.
//
//	ffmpeg -f lavfi -i testsrc2=s=320x240:r=30 -t 1 -c:v libx264 \
//	  -profile:v high -bf 0 -f h264 out.h264
var (
	spsHigh = []byte{0x67, 0x64, 0x00, 0x0d, 0xac, 0xb2, 0x02, 0x83, 0xf6, 0x02, 0x20, 0x00, 0x00, 0x03, 0x00, 0x20, 0x00, 0x00, 0x07, 0x81, 0xe2, 0x85, 0x49}
	ppsHigh = []byte{0x68, 0xeb, 0xc3, 0xcb, 0x22, 0xc0}
)

func annexB(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(b, 0, 0, 0, 1)
		b = append(b, n...)
	}
	return b
}

// idr and slice are fake VCL NALs: the writer copies them and never parses
// past the header byte.
func idr(n int) []byte   { return append([]byte{0x65}, bytes.Repeat([]byte{0xAA}, n)...) }
func slice(n int) []byte { return append([]byte{0x41}, bytes.Repeat([]byte{0xBB}, n)...) }

func ms(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func write(t *testing.T, fn func(w *Writer)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.mp4")
	w, err := Create(path)
	must(t, err)
	fn(w)
	must(t, w.Close())
	return path
}

// find returns the first box of typ in a depth-first walk.
func find(boxes []Box, typ string) *Box {
	for i := range boxes {
		if boxes[i].Type == typ {
			return &boxes[i]
		}
		if b := find(boxes[i].Children, typ); b != nil {
			return b
		}
	}
	return nil
}

func mustFind(t *testing.T, path, typ string) Box {
	t.Helper()
	boxes, err := ReadBoxes(path)
	must(t, err)
	b := find(boxes, typ)
	if b == nil {
		t.Fatalf("no %s box", typ)
	}
	return *b
}

func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

func TestSampleDurationsFollowVariablePTS(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(100)), 0, true))
		must(t, w.WriteSample(annexB(slice(50)), ms(16.7), false))
		must(t, w.WriteSample(annexB(slice(50)), ms(33.4), false))
		must(t, w.WriteSample(annexB(slice(50)), ms(533.4), false)) // a stall
	})
	got, err := ReadSamples(path)
	must(t, err)
	// 16.7ms is 1503 ticks; the stall is 45000; the last repeats the one before.
	want := []uint32{1503, 1503, 45000, 45000}
	if !equalU32(got.Durations, want) {
		t.Fatalf("durations %v, want %v", got.Durations, want)
	}
	if !equalU32(got.Keys, []uint32{1}) {
		t.Fatalf("keys %v, want [1]", got.Keys)
	}
}

func TestPTSGoingBackwardsIsClampedToOneTick(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
		must(t, w.WriteSample(annexB(slice(10)), ms(20), false))
		must(t, w.WriteSample(annexB(slice(10)), ms(10), false))
	})
	got, err := ReadSamples(path)
	must(t, err)
	if want := []uint32{1800, 1, 1}; !equalU32(got.Durations, want) {
		t.Fatalf("durations %v, want %v", got.Durations, want)
	}
}

func TestSingleSampleLastsOneSixtieth(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
	})
	got, err := ReadSamples(path)
	must(t, err)
	if want := []uint32{1500}; !equalU32(got.Durations, want) {
		t.Fatalf("durations %v, want %v", got.Durations, want)
	}
}

func TestAllKeySamplesOmitStss(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
		must(t, w.WriteSample(annexB(idr(10)), ms(16), true))
	})
	got, err := ReadSamples(path)
	must(t, err)
	if got.Keys != nil {
		t.Fatalf("keys %v, want nil", got.Keys)
	}
}

func TestParameterSetsAndAUDAreStrippedFromSamples(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB([]byte{0x09, 0xF0}, sps320x240, pps, idr(10)), 0, true))
	})
	stsz := mustFind(t, path, "stsz")
	// version/flags, sample size 0, count 1, then the sizes.
	if n := be32(stsz.Payload[8:]); n != 1 {
		t.Fatalf("%d samples, want 1", n)
	}
	if sz := be32(stsz.Payload[12:]); sz != 4+11 {
		t.Fatalf("sample size %d, want %d", sz, 4+11)
	}
	data, err := os.ReadFile(path)
	must(t, err)
	want := append([]byte{0, 0, 0, 0x0B}, idr(10)...)
	co64 := mustFind(t, path, "co64")
	off := binary.BigEndian.Uint64(co64.Payload[8:])
	if got := data[off : off+15]; !bytes.Equal(got, want) {
		t.Fatalf("mdat holds % x, want % x", got, want)
	}
}

func TestMdatSizeIs64BitAndPatched(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(100)), 0, true))
		must(t, w.WriteSample(annexB(slice(50)), ms(16), false))
	})
	data, err := os.ReadFile(path)
	must(t, err)
	boxes, err := ReadBoxes(path)
	must(t, err)
	if len(boxes) != 3 || boxes[0].Type != "ftyp" || boxes[1].Type != "mdat" || boxes[2].Type != "moov" {
		t.Fatalf("top-level boxes %v", boxes)
	}
	mdat := boxes[1]
	if be32(data[mdat.Offset:]) != 1 {
		t.Fatalf("mdat size field %d, want 1", be32(data[mdat.Offset:]))
	}
	large := binary.BigEndian.Uint64(data[mdat.Offset+8:])
	stsz := mustFind(t, path, "stsz")
	var sum uint64
	for i := 0; i < int(be32(stsz.Payload[8:])); i++ {
		sum += uint64(be32(stsz.Payload[12+4*i:]))
	}
	if large != 16+sum || int64(large) != mdat.Size {
		t.Fatalf("largesize %d, want %d (box size %d)", large, 16+sum, mdat.Size)
	}
	if boxes[2].Offset != mdat.Offset+int64(large) {
		t.Fatalf("moov at %d, want %d", boxes[2].Offset, mdat.Offset+int64(large))
	}
}

func stscEntries(t *testing.T, path string) [][3]uint32 {
	t.Helper()
	b := mustFind(t, path, "stsc")
	n := int(be32(b.Payload[4:]))
	var out [][3]uint32
	for i := 0; i < n; i++ {
		p := b.Payload[8+12*i:]
		out = append(out, [3]uint32{be32(p), be32(p[4:]), be32(p[8:])})
	}
	return out
}

func TestSameSizeConfigAddsASampleDescription(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
		must(t, w.WriteSample(annexB(slice(10)), ms(16), false))
		must(t, w.SetConfig(sps320x240, ppsAlt))
		must(t, w.WriteSample(annexB(idr(10)), ms(32), true))
		must(t, w.WriteSample(annexB(slice(10)), ms(48), false))
	})
	if n := be32(mustFind(t, path, "stsd").Payload[4:]); n != 2 {
		t.Fatalf("stsd entries %d, want 2", n)
	}
	want := [][3]uint32{{1, 1, 1}, {3, 1, 2}}
	got := stscEntries(t, path)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("stsc %v, want %v", got, want)
	}
}

func TestConfigBeforeAnySampleReplacesTheDescription(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.SetConfig(sps320x240, ppsAlt))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
	})
	if n := be32(mustFind(t, path, "stsd").Payload[4:]); n != 1 {
		t.Fatalf("stsd entries %d, want 1", n)
	}
	if got := stscEntries(t, path); len(got) != 1 || got[0] != [3]uint32{1, 1, 1} {
		t.Fatalf("stsc %v", got)
	}
}

func TestIdenticalConfigAddsNothing(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(slice(10)), ms(16), false))
	})
	if n := be32(mustFind(t, path, "stsd").Payload[4:]); n != 1 {
		t.Fatalf("stsd entries %d, want 1", n)
	}
	if got := stscEntries(t, path); len(got) != 1 {
		t.Fatalf("stsc %v, want one entry", got)
	}
}

func TestDifferentSizeConfigIsErrResized(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
		if err := w.SetConfig(spsTall, pps); !errors.Is(err, ErrResized) {
			t.Fatalf("SetConfig = %v, want ErrResized", err)
		}
	})
	got, err := ReadSamples(path)
	must(t, err)
	if len(got.Durations) != 1 || got.Width != 320 || got.Height != 240 {
		t.Fatalf("samples %+v", got)
	}
}

func TestSampleBeforeConfigIsErrNoConfig(t *testing.T) {
	w, err := Create(filepath.Join(t.TempDir(), "out.mp4"))
	must(t, err)
	defer w.Close()
	if err := w.WriteSample(annexB(idr(10)), 0, true); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("WriteSample = %v, want ErrNoConfig", err)
	}
}

func TestEmptyFileIsValid(t *testing.T) {
	path := write(t, func(w *Writer) {})
	got, err := ReadSamples(path)
	must(t, err)
	if len(got.Durations) != 0 {
		t.Fatalf("durations %v", got.Durations)
	}
	mustFind(t, path, "moov")
}

func TestTrackSizeComesFromTheSPS(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
	})
	tkhd := mustFind(t, path, "tkhd")
	// version/flags, 2 times, track, reserved, duration, 8, 8 (layer..volume,
	// reserved), 36 matrix, then width and height.
	p := tkhd.Payload[len(tkhd.Payload)-8:]
	if be32(p) != 320<<16 || be32(p[4:]) != 240<<16 {
		t.Fatalf("tkhd size %x x %x", be32(p), be32(p[4:]))
	}
	got, err := ReadSamples(path)
	must(t, err)
	if got.Width != 320 || got.Height != 240 {
		t.Fatalf("avc1 size %dx%d", got.Width, got.Height)
	}
}

// avcCPayload returns the first avcC box's payload.
func avcCPayload(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.ReadFile(path)
	must(t, err)
	i := bytes.Index(f, []byte("avcC"))
	if i < 4 {
		t.Fatal("no avcC box")
	}
	size := int(be32(f[i-4:]))
	return f[i+4 : i-4+size]
}

// A High profile avcC carries chroma format and bit depths after the PPS
// (ISO/IEC 14496-15 5.3.3.1.2). Without them a strict parser reads past the
// box.
func TestHighProfileAvcCHasTheExtension(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(spsHigh, ppsHigh))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
	})
	p := avcCPayload(t, path)
	want := 6 + 2 + len(spsHigh) + 1 + 2 + len(ppsHigh) + 4
	if len(p) != want {
		t.Fatalf("avcC payload %d bytes, want %d", len(p), want)
	}
	// 4:2:0, 8-bit luma and chroma, no SPS extensions.
	if ext := p[len(p)-4:]; !bytes.Equal(ext, []byte{0xFD, 0xF8, 0xF8, 0x00}) {
		t.Fatalf("extension % x, want fd f8 f8 00", ext)
	}
}

func TestBaselineAvcCHasNoExtension(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
	})
	if p, want := avcCPayload(t, path), 6+2+len(sps320x240)+1+2+len(pps); len(p) != want {
		t.Fatalf("avcC payload %d bytes, want %d", len(p), want)
	}
}

// encodeAUs encodes ten frames of testsrc with libx264 at profile and splits
// them into access units, with the last SPS and PPS seen.
func encodeAUs(t *testing.T, ffmpeg, profile string) (sps, ppsNAL []byte, aus [][]byte) {
	t.Helper()
	raw, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=60",
		"-frames:v", "10", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-profile:v", profile,
		"-g", "5", "-bf", "0", "-f", "h264", "-").Output()
	must(t, err)

	// Group NALs into access units: a new picture starts at a slice whose
	// first_mb_in_slice is 0, and any parameter sets before it belong to it.
	var cur [][]byte
	var curHasVCL bool
	flush := func() {
		if len(cur) > 0 {
			aus = append(aus, annexB(cur...))
		}
		cur, curHasVCL = nil, false
	}
	h264.SplitAnnexB(raw, func(nal []byte) {
		nal = append([]byte(nil), nal...)
		switch h264.Type(nal) {
		case h264.NALSPS:
			sps = nal
		case h264.NALPPS:
			ppsNAL = nal
		}
		if h264.IsVCL(nal) && h264.StartsNewPicture(nal) && curHasVCL {
			flush()
		} else if !h264.IsVCL(nal) && curHasVCL {
			flush()
		}
		cur = append(cur, nal)
		if h264.IsVCL(nal) {
			curHasVCL = true
		}
	})
	flush()
	if len(aus) != 10 || sps == nil || ppsNAL == nil {
		t.Fatalf("split %d access units, sps %v, pps %v", len(aus), sps != nil, ppsNAL != nil)
	}
	return sps, ppsNAL, aus
}

func writeAUs(t *testing.T, sps, ppsNAL []byte, aus [][]byte) string {
	t.Helper()
	return write(t, func(w *Writer) {
		must(t, w.SetConfig(sps, ppsNAL))
		for i, au := range aus {
			key := false
			h264.SplitAnnexB(au, func(nal []byte) {
				if h264.Type(nal) == h264.NALIDR {
					key = true
				}
			})
			must(t, w.WriteSample(au, ms(float64(i)*16.7), key))
		}
	})
}

func lookFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not on PATH")
	}
	return ffmpeg, ffprobe
}

// decodeErrors decodes the whole file and returns what ffmpeg printed at
// level error, which is nothing for a clean file.
func decodeErrors(t *testing.T, ffmpeg, path string) string {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestFFprobeDecodesTheFile is the one test that asks a real decoder. It
// needs real slices, so it encodes ten frames with ffmpeg and remuxes them.
func TestFFprobeDecodesTheFile(t *testing.T) {
	ffmpeg, ffprobe := lookFFmpeg(t)
	sps, ppsNAL, aus := encodeAUs(t, ffmpeg, "baseline")
	path := writeAUs(t, sps, ppsNAL, aus)
	out, err := exec.Command(ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
		"-show_entries", "stream=nb_read_frames,width,height", "-of", "csv=p=0", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if got != "320,240,10" {
		t.Fatalf("ffprobe says %q, want 320,240,10", got)
	}
}

func TestFFmpegDecodesAHighProfileFile(t *testing.T) {
	ffmpeg, ffprobe := lookFFmpeg(t)
	sps, ppsNAL, aus := encodeAUs(t, ffmpeg, "high")
	if sps[1] != 100 {
		t.Fatalf("profile_idc %d, want 100", sps[1])
	}
	path := writeAUs(t, sps, ppsNAL, aus)
	if msg := decodeErrors(t, ffmpeg, path); msg != "" {
		t.Fatalf("decode errors:\n%s", msg)
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=profile", "-of", "csv=p=0", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "High" {
		t.Fatalf("ffprobe profile %q, want High", got)
	}
	if p := avcCPayload(t, path); !bytes.Equal(p[len(p)-4:], []byte{0xFD, 0xF8, 0xF8, 0x00}) {
		t.Fatalf("avcC ends % x", p[len(p)-4:])
	}
}

// A config after the final sample would start a description at a chunk the
// file does not have. It is dropped at Close.
func TestConfigAfterTheLastSampleIsDropped(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
		must(t, w.WriteSample(annexB(idr(10)), 0, true))
		must(t, w.WriteSample(annexB(slice(10)), ms(16), false))
		must(t, w.SetConfig(sps320x240, ppsAlt))
	})
	if got := stscEntries(t, path); len(got) != 1 || got[0] != [3]uint32{1, 1, 1} {
		t.Fatalf("stsc %v, want one entry at sample 1", got)
	}
	if n := be32(mustFind(t, path, "stsd").Payload[4:]); n != 1 {
		t.Fatalf("stsd entries %d, want 1", n)
	}
}

func TestFFmpegReadsAFileEndingInAConfig(t *testing.T) {
	ffmpeg, _ := lookFFmpeg(t)
	sps, ppsNAL, aus := encodeAUs(t, ffmpeg, "baseline")
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps, ppsNAL))
		for i, au := range aus {
			must(t, w.WriteSample(au, ms(float64(i)*16.7), i%5 == 0))
		}
		alt := append(append([]byte(nil), ppsNAL...), 0x80)
		must(t, w.SetConfig(sps, alt))
	})
	if msg := decodeErrors(t, ffmpeg, path); msg != "" {
		t.Fatalf("decode errors:\n%s", msg)
	}
}

// A recording stopped before its first keyframe has a config and no samples.
func TestConfigWithNoSamplesIsValid(t *testing.T) {
	path := write(t, func(w *Writer) {
		must(t, w.SetConfig(sps320x240, pps))
	})
	if got := stscEntries(t, path); len(got) != 0 {
		t.Fatalf("stsc %v, want none", got)
	}
	got, err := ReadSamples(path)
	must(t, err)
	if len(got.Durations) != 0 {
		t.Fatalf("durations %v", got.Durations)
	}
	_, ffprobe := lookFFmpeg(t)
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_name,width,height",
		"-of", "csv=p=0", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "h264,320,240" {
		t.Fatalf("ffprobe says %q, want h264,320,240", s)
	}
}

func TestFFprobeReadsAnEmptyFile(t *testing.T) {
	_, ffprobe := lookFFmpeg(t)
	path := write(t, func(w *Writer) {})
	out, err := exec.Command(ffprobe, "-v", "error", path).CombinedOutput()
	if err != nil || len(bytes.TrimSpace(out)) > 0 {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
}
