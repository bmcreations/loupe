package mp4

import (
	"encoding/binary"
	"fmt"
	"os"
)

// Box is one parsed box. Payload is what follows the header; mdat's is left
// empty so walking a recording does not copy its video.
type Box struct {
	Type     string
	Offset   int64
	Size     int64
	Payload  []byte
	Children []Box
}

var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true, "dinf": true,
}

// ReadBoxes parses the box tree of a file this package wrote. It exists for
// tests and for checking a recording, not as a general MP4 reader.
func ReadBoxes(path string) ([]Box, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseBoxes(data, 0, int64(len(data)))
}

// parseBoxes reads the boxes in data[start:end]; offsets are file offsets.
func parseBoxes(data []byte, start, end int64) ([]Box, error) {
	var out []Box
	for pos := start; pos < end; {
		if end-pos < 8 {
			return nil, fmt.Errorf("mp4: truncated box header at %d", pos)
		}
		size := int64(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		hdr := int64(8)
		if size == 1 {
			if end-pos < 16 {
				return nil, fmt.Errorf("mp4: truncated largesize at %d", pos)
			}
			size = int64(binary.BigEndian.Uint64(data[pos+8:]))
			hdr = 16
		}
		if size < hdr || pos+size > end {
			return nil, fmt.Errorf("mp4: %s at %d has size %d", typ, pos, size)
		}
		b := Box{Type: typ, Offset: pos, Size: size}
		body := pos + hdr
		boxEnd := pos + size
		if typ != "mdat" {
			b.Payload = data[body:boxEnd]
		}
		// stsd's entries follow its version, flags and count; avc1's
		// children follow its fixed 78-byte visual sample entry.
		var childStart int64 = -1
		switch {
		case containers[typ]:
			childStart = body
		case typ == "stsd":
			childStart = body + 8
		case typ == "avc1":
			childStart = body + 78
		}
		if childStart >= 0 {
			if childStart > boxEnd {
				return nil, fmt.Errorf("mp4: %s at %d is too short", typ, pos)
			}
			var err error
			if b.Children, err = parseBoxes(data, childStart, boxEnd); err != nil {
				return nil, err
			}
		}
		out = append(out, b)
		pos = boxEnd
	}
	return out, nil
}

// Samples is what a recording's sample tables say.
type Samples struct {
	Durations []uint32 // one per sample, in 90kHz ticks
	Keys      []uint32 // 1-based key sample numbers; nil when every sample is a key
	Width     int
	Height    int
}

// ReadSamples expands a file's stts, stss and avc1 size.
func ReadSamples(path string) (Samples, error) {
	boxes, err := ReadBoxes(path)
	if err != nil {
		return Samples{}, err
	}
	if descend(boxes, "moov") != nil && descend(boxes, "moov", "trak") == nil {
		// The writer leaves out the track when nothing was configured.
		return Samples{}, nil
	}
	stbl := descend(boxes, "moov", "trak", "mdia", "minf", "stbl")
	if stbl == nil {
		return Samples{}, fmt.Errorf("mp4: %s has no sample table", path)
	}
	var s Samples
	for _, c := range stbl.Children {
		p := c.Payload
		switch c.Type {
		case "stts":
			n, err := count(p, 8, "stts")
			if err != nil {
				return Samples{}, err
			}
			for i := 0; i < n; i++ {
				e := p[8+8*i:]
				cnt, delta := binary.BigEndian.Uint32(e), binary.BigEndian.Uint32(e[4:])
				for j := uint32(0); j < cnt; j++ {
					s.Durations = append(s.Durations, delta)
				}
			}
		case "stss":
			n, err := count(p, 4, "stss")
			if err != nil {
				return Samples{}, err
			}
			s.Keys = []uint32{}
			for i := 0; i < n; i++ {
				s.Keys = append(s.Keys, binary.BigEndian.Uint32(p[8+4*i:]))
			}
		case "stsd":
			for _, e := range c.Children {
				if e.Type == "avc1" && len(e.Payload) >= 28 {
					s.Width = int(binary.BigEndian.Uint16(e.Payload[24:]))
					s.Height = int(binary.BigEndian.Uint16(e.Payload[26:]))
					break
				}
			}
		}
	}
	return s, nil
}

// count reads a table's entry count at p[4:8] and checks entrySize bytes per
// entry follow the 8-byte header.
func count(p []byte, entrySize int, typ string) (int, error) {
	if len(p) < 8 {
		return 0, fmt.Errorf("mp4: short %s", typ)
	}
	n := int(binary.BigEndian.Uint32(p[4:]))
	if 8+n*entrySize > len(p) {
		return 0, fmt.Errorf("mp4: %s claims %d entries in %d bytes", typ, n, len(p))
	}
	return n, nil
}

func descend(boxes []Box, path ...string) *Box {
	for _, typ := range path {
		var next *Box
		for i := range boxes {
			if boxes[i].Type == typ {
				next = &boxes[i]
				break
			}
		}
		if next == nil {
			return nil
		}
		if typ == path[len(path)-1] {
			return next
		}
		boxes = next.Children
	}
	return nil
}
