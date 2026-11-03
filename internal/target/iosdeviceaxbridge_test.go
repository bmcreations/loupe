package target

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// frame length-prefixes payload the way simcap-bridge does.
func frame(payload []byte) []byte {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	return append(hdr[:], payload...)
}

func TestReadAXFrames(t *testing.T) {
	a, b := []byte("initial-dump"), []byte("additional-data")
	r := bytes.NewReader(append(frame(a), frame(b)...))
	got, err := readAXFrames(r)
	if err != nil {
		t.Fatalf("readAXFrames: %v", err)
	}
	if len(got) != 2 || !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) {
		t.Fatalf("frames = %q, want [%q %q]", got, a, b)
	}
}

func TestReadAXFramesEmpty(t *testing.T) {
	got, err := readAXFrames(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("readAXFrames(empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("frames = %q, want none", got)
	}
}

func TestReadAXFramesTruncatedHeader(t *testing.T) {
	// A length prefix cut short is a desynced pipe, not a clean EOF.
	_, err := readAXFrames(bytes.NewReader([]byte{0x10, 0x00}))
	if err == nil || err == io.EOF {
		t.Fatalf("err = %v, want a non-EOF error", err)
	}
}

func TestReadAXFramesTruncatedBody(t *testing.T) {
	// Header promises 16 bytes, body has 3.
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 16)
	_, err := readAXFrames(bytes.NewReader(append(hdr[:], []byte("abc")...)))
	if err == nil {
		t.Fatalf("err = nil, want a short-read error")
	}
}
