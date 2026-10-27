package composetree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Agent ops. See tools/composeagent/src/dev/loupe/compose/Host.java.
const (
	OpHello = 'H' // reply: {"pid":N,"composeVersion":"1.12.1"}
	OpLoad  = 'L' // payload: device path of inspector.jar
	OpTree  = 'C' // reply: u32 count, then u32 len + GetComposables reply per window
)

// maxReply bounds a reply's declared length, so a desynced stream fails
// instead of allocating whatever four stray bytes say.
const maxReply = 64 << 20

// Call sends one request to the agent and returns its reply body. A request
// is op u8, len u32 BE, payload; a reply is status u8 (0 ok), len u32 BE,
// body, and a non-zero status carries the agent's error text as its body.
func Call(rw io.ReadWriter, op byte, payload []byte) ([]byte, error) {
	req := make([]byte, 5, 5+len(payload))
	req[0] = op
	binary.BigEndian.PutUint32(req[1:], uint32(len(payload)))
	if _, err := rw.Write(append(req, payload...)); err != nil {
		return nil, err
	}
	var hdr [5]byte
	if _, err := io.ReadFull(rw, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxReply {
		return nil, fmt.Errorf("compose agent: reply of %d bytes", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(rw, body); err != nil {
		return nil, err
	}
	if hdr[0] != 0 {
		return nil, fmt.Errorf("compose agent: %s", body)
	}
	return body, nil
}

// Windows splits an OpTree reply into one GetComposables reply per window.
// A window with no Compose content has an empty one.
func Windows(b []byte) ([][]byte, error) {
	errShort := errors.New("compose agent: truncated tree reply")
	if len(b) < 4 {
		return nil, errShort
	}
	n := binary.BigEndian.Uint32(b)
	b = b[4:]
	var out [][]byte
	for i := uint32(0); i < n; i++ {
		if len(b) < 4 {
			return nil, errShort
		}
		l := binary.BigEndian.Uint32(b)
		b = b[4:]
		if uint32(len(b)) < l {
			return nil, errShort
		}
		out = append(out, b[:l])
		b = b[l:]
	}
	return out, nil
}
