// Package mp4 writes one H.264 video track to an MP4 file, streaming samples
// to disk and holding only the sample table in memory.
//
// It is for recording a session's access units as the encoder made them, so
// it re-encodes nothing and assumes no B-frames: sample durations come from
// PTS deltas, which equal decode-order deltas only when PTS and DTS agree.
// scrcpy and simcap both emit baseline-style streams with none.
package mp4

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
)

const timescale = 90000

var (
	// ErrResized is a config whose picture size differs from the track's. A
	// track has one size, so the caller ends the file here.
	ErrResized = errors.New("mp4: picture size changed")
	// ErrNoConfig is a sample written before any SetConfig.
	ErrNoConfig = errors.New("mp4: sample before parameter sets")
)

type sample struct {
	size   uint32
	offset uint64
	pts    int64 // ticks
	key    bool
}

type description struct {
	sps, pps []byte
}

// descStart says that samples from firstSample on (1-based) use description
// index (1-based).
type descStart struct{ firstSample, index uint32 }

type Writer struct {
	f             *os.File
	bw            *bufio.Writer
	mdatStart     int64 // offset of mdat's header
	pos           uint64
	width, height int
	descs         []description
	descStarts    []descStart
	samples       []sample
	buf           []byte
}

// Create opens path and writes ftyp and mdat's header.
func Create(path string) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	ftyp := ftypBox()
	// Size 1 says a 64-bit size follows the type; Close patches it.
	hdr := cat(u32(1), []byte("mdat"), u64(0))
	w := &Writer{f: f, bw: bufio.NewWriterSize(f, 1<<20), mdatStart: int64(len(ftyp))}
	w.pos = uint64(len(ftyp) + len(hdr))
	if _, err := w.bw.Write(cat(ftyp, hdr)); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

// SetConfig records the stream's parameter sets. Identical bytes do nothing;
// different bytes at the same size start a new sample description; a
// different size is ErrResized and leaves the writer as it was.
func (w *Writer) SetConfig(sps, pps []byte) error {
	width, height, err := h264.SPSDimensions(sps)
	if err != nil {
		return err
	}
	if len(pps) == 0 {
		return errors.New("mp4: empty PPS")
	}
	if len(w.descs) > 0 {
		if width != w.width || height != w.height {
			return ErrResized
		}
		last := w.descs[len(w.descs)-1]
		if string(last.sps) == string(sps) && string(last.pps) == string(pps) {
			return nil
		}
	} else {
		w.width, w.height = width, height
	}
	d := description{sps: append([]byte(nil), sps...), pps: append([]byte(nil), pps...)}
	first := uint32(len(w.samples) + 1)
	if n := len(w.descStarts); n > 0 && w.descStarts[n-1].firstSample == first {
		// No sample used the previous description, so replace it rather
		// than list two starts at the same sample.
		w.descs[len(w.descs)-1] = d
		return nil
	}
	w.descs = append(w.descs, d)
	w.descStarts = append(w.descStarts, descStart{first, uint32(len(w.descs))})
	return nil
}

// WriteSample rewrites one Annex B access unit to length-prefixed NALs,
// dropping SPS (7), PPS (8) and AUD (9), and appends it to mdat.
func (w *Writer) WriteSample(au []byte, pts time.Duration, key bool) error {
	if len(w.descs) == 0 {
		return ErrNoConfig
	}
	w.buf = w.buf[:0]
	h264.SplitAnnexB(au, func(nal []byte) {
		switch h264.Type(nal) {
		case h264.NALSPS, h264.NALPPS, h264.NALAUD:
			return
		}
		w.buf = append(w.buf, u32(uint32(len(nal)))...)
		w.buf = append(w.buf, nal...)
	})
	if len(w.buf) == 0 {
		return nil
	}
	if uint64(len(w.buf)) > math.MaxUint32 {
		return fmt.Errorf("mp4: sample of %d bytes", len(w.buf))
	}
	if _, err := w.bw.Write(w.buf); err != nil {
		return err
	}
	w.samples = append(w.samples, sample{
		size:   uint32(len(w.buf)),
		offset: w.pos,
		pts:    int64(pts) * timescale / int64(time.Second),
		key:    key,
	})
	w.pos += uint64(len(w.buf))
	return nil
}

// durations turns PTS into per-sample durations: the gap to the next sample,
// at least one tick, with the last sample repeating the one before it.
func (w *Writer) durations() []uint32 {
	n := len(w.samples)
	d := make([]uint32, n)
	for i := 0; i+1 < n; i++ {
		v := w.samples[i+1].pts - w.samples[i].pts
		if v < 1 {
			v = 1
		}
		if v > math.MaxUint32 {
			v = math.MaxUint32
		}
		d[i] = uint32(v)
	}
	switch {
	case n == 1:
		d[0] = timescale / 60
	case n > 1:
		d[n-1] = d[n-2]
	}
	return d
}

// Close writes moov, patches mdat's size and closes the file. A file never
// closed has no moov and plays nowhere; the store deletes such files.
func (w *Writer) Close() error {
	err := w.finish()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (w *Writer) finish() error {
	durs := w.durations()
	var total uint64
	var runs []run
	for _, d := range durs {
		total += uint64(d)
		if n := len(runs); n > 0 && runs[n-1].delta == d {
			runs[n-1].count++
		} else {
			runs = append(runs, run{1, d})
		}
	}
	if total > math.MaxUint32 {
		total = math.MaxUint32
	}
	dur := uint32(total)

	if len(w.descs) == 0 {
		// Nothing was configured, so there is no track to describe: a
		// track with no sample description does not parse.
		return w.writeMoov(box("moov", mvhdBox(dur)))
	}
	descs, starts := w.usedDescriptions()
	stbl := box("stbl",
		stsdBox(w.width, w.height, descs),
		sttsBox(runs),
		stscBox(starts),
		stszBox(w.samples),
		stssBox(w.samples),
		co64Box(w.samples))
	minf := box("minf", vmhdBox(), dinfBox(), stbl)
	mdia := box("mdia", mdhdBox(dur), hdlrBox(), minf)
	trak := box("trak", tkhdBox(dur, w.width, w.height), mdia)
	return w.writeMoov(box("moov", mvhdBox(dur), trak))
}

// usedDescriptions drops the description starts past the last sample, which
// a config after the final sample leaves, and the descriptions only they
// named: an stsc entry for a chunk the file lacks fails ffmpeg's open with
// "contradictionary STSC and STCO". With no samples it keeps the first
// description, since a track needs one to parse, and no starts.
func (w *Writer) usedDescriptions() ([]description, []descStart) {
	n := uint32(len(w.samples))
	starts := w.descStarts
	for len(starts) > 0 && starts[len(starts)-1].firstSample > n {
		starts = starts[:len(starts)-1]
	}
	keep := 1
	if len(starts) > 0 {
		keep = int(starts[len(starts)-1].index)
	}
	return w.descs[:keep], starts
}

func (w *Writer) writeMoov(moov []byte) error {
	if _, err := w.bw.Write(moov); err != nil {
		return err
	}
	if err := w.bw.Flush(); err != nil {
		return err
	}
	// mdat's largesize sits after its size and type fields.
	_, err := w.f.WriteAt(u64(w.pos-uint64(w.mdatStart)), w.mdatStart+8)
	return err
}
