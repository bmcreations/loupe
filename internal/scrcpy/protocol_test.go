package scrcpy

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"
)

func TestEncodeTouchLayout(t *testing.T) {
	buf := make([]byte, TouchMessageLen)
	n, err := EncodeTouch(buf, ActionDown, 3, 120, 340, 1080, 2400, 1.0, 0)
	if err != nil {
		t.Fatalf("EncodeTouch: %v", err)
	}
	if n != TouchMessageLen {
		t.Fatalf("wrote %d bytes, want %d", n, TouchMessageLen)
	}
	if buf[0] != typeInjectTouchEvent {
		t.Errorf("type = %d, want %d", buf[0], typeInjectTouchEvent)
	}
	if buf[1] != ActionDown {
		t.Errorf("action = %d, want %d", buf[1], ActionDown)
	}
	if got := binary.BigEndian.Uint64(buf[2:]); got != 3 {
		t.Errorf("pointerId = %d, want 3", got)
	}
	if got := int32(binary.BigEndian.Uint32(buf[10:])); got != 120 {
		t.Errorf("x = %d, want 120", got)
	}
	if got := int32(binary.BigEndian.Uint32(buf[14:])); got != 340 {
		t.Errorf("y = %d, want 340", got)
	}
	if got := binary.BigEndian.Uint16(buf[18:]); got != 1080 {
		t.Errorf("screenWidth = %d, want 1080", got)
	}
	if got := binary.BigEndian.Uint16(buf[20:]); got != 2400 {
		t.Errorf("screenHeight = %d, want 2400", got)
	}
	if got := binary.BigEndian.Uint16(buf[22:]); got != 0xFFFF {
		t.Errorf("pressure = 0x%04x, want 0xFFFF", got)
	}

	if _, err := EncodeTouch(make([]byte, 8), ActionDown, 0, 0, 0, 0, 0, 1, 0); err != ErrShortBuffer {
		t.Errorf("short buffer error = %v, want ErrShortBuffer", err)
	}
}

func TestEncodeKeycodeLayout(t *testing.T) {
	buf := make([]byte, KeycodeMessageLen)
	n, err := EncodeKeycode(buf, KeyActionDown, 4, 0, 0)
	if err != nil {
		t.Fatalf("EncodeKeycode: %v", err)
	}
	if n != KeycodeMessageLen {
		t.Fatalf("wrote %d bytes, want %d", n, KeycodeMessageLen)
	}
	if buf[0] != typeInjectKeycode || buf[1] != KeyActionDown {
		t.Errorf("header = %v, want type=%d action=%d", buf[:2], typeInjectKeycode, KeyActionDown)
	}
	if got := int32(binary.BigEndian.Uint32(buf[2:])); got != 4 {
		t.Errorf("keycode = %d, want 4", got)
	}
}

func TestFixedPointPressureClamps(t *testing.T) {
	// Values outside [0,1] must clamp, not wrap: a wrapped pressure reads to
	// Android as a released finger and silently eats the gesture.
	if got := encodeFixedU16(2.5); got != 0xFFFF {
		t.Errorf("encodeFixedU16(2.5) = 0x%04x, want 0xFFFF", got)
	}
	if got := encodeFixedU16(-1); got != 0 {
		t.Errorf("encodeFixedU16(-1) = 0x%04x, want 0", got)
	}
}

func TestReadStreamHeader(t *testing.T) {
	var buf bytes.Buffer
	name := make([]byte, deviceNameLen)
	copy(name, "Pixel 8 Pro")
	buf.Write(name)

	meta := make([]byte, 12)
	binary.BigEndian.PutUint32(meta[0:], 0x68323634) // "h264"
	binary.BigEndian.PutUint32(meta[4:], 1080)
	binary.BigEndian.PutUint32(meta[8:], 2400)
	buf.Write(meta)

	h, err := ReadStreamHeader(&buf)
	if err != nil {
		t.Fatalf("ReadStreamHeader: %v", err)
	}
	if h.DeviceName != "Pixel 8 Pro" {
		t.Errorf("device name = %q, want %q", h.DeviceName, "Pixel 8 Pro")
	}
	if h.Codec != "h264" {
		t.Errorf("codec = %q, want %q", h.Codec, "h264")
	}
	if h.Width != 1080 || h.Height != 2400 {
		t.Errorf("dimensions = %dx%d, want 1080x2400", h.Width, h.Height)
	}
}

func TestReadStreamHeaderRejectsGarbage(t *testing.T) {
	// A version mismatch or a wrong send_*_meta setting shows up as nonsense
	// dimensions. Catching it here beats a corrupt-bitstream error later.
	buf := bytes.NewReader(make([]byte, deviceNameLen+12))
	if _, err := ReadStreamHeader(buf); err == nil {
		t.Error("expected an error for zero dimensions")
	}
}

func TestReadPacketHeaderFlags(t *testing.T) {
	var buf bytes.Buffer
	raw := uint64(1 << 62) // key frame flag
	raw |= 500000          // 500ms PTS
	_ = binary.Write(&buf, binary.BigEndian, raw)
	_ = binary.Write(&buf, binary.BigEndian, uint32(4096))

	h, err := ReadPacketHeader(&buf)
	if err != nil {
		t.Fatalf("ReadPacketHeader: %v", err)
	}
	if !h.Key {
		t.Error("key flag not decoded")
	}
	if h.Config {
		t.Error("config flag set unexpectedly")
	}
	if h.PTS != 500*time.Millisecond {
		t.Errorf("PTS = %v, want 500ms", h.PTS)
	}
	if h.Size != 4096 {
		t.Errorf("size = %d, want 4096", h.Size)
	}

	if _, err := ReadPacketHeader(bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Error("expected an error on a truncated header")
	} else if err != io.ErrUnexpectedEOF {
		t.Logf("truncated header error: %v", err)
	}
}

// TestEncodeResetVideo pins the opcode. It was read out of scrcpy-server
// 3.3.4's own dex (`dexdump -d`, ControlMessage.TYPE_RESET_VIDEO = 17) and
// confirmed against a live emulator: six idle seconds produced exactly one
// keyframe, and one of these produced another 130ms later.
//
// Getting it wrong is silent. scrcpy ignores an opcode it does not know, so
// the viewer would simply never recover from a shed and the screen would stay
// frozen with no error anywhere.
func TestEncodeResetVideo(t *testing.T) {
	var buf [8]byte
	n, err := EncodeResetVideo(buf[:])
	if err != nil {
		t.Fatalf("EncodeResetVideo: %v", err)
	}
	if n != 1 {
		t.Errorf("wrote %d bytes, want 1", n)
	}
	if buf[0] != 17 {
		t.Errorf("opcode %d, want 17 (TYPE_RESET_VIDEO)", buf[0])
	}

	if _, err := EncodeResetVideo(nil); err == nil {
		t.Error("EncodeResetVideo(nil) succeeded; a short buffer must be reported")
	}
}

func TestAppendInjectText(t *testing.T) {
	got, err := AppendInjectText(nil, "hé")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 0, 0, 0, 3, 'h', 0xc3, 0xa9}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
	if _, err := AppendInjectText(nil, strings.Repeat("a", InjectTextMax+1)); err == nil {
		t.Fatal("a string over the limit was encoded")
	}
}

// The layout parseUhidCreate reads: u16 id, u16 vendor, u16 product, a name
// behind one length byte, a descriptor behind two.
func TestAppendUhidCreate(t *testing.T) {
	got, err := AppendUhidCreate(nil, 2, 0x1234, 0x5678, "kb", []byte{0xaa, 0xbb, 0xcc})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{12, 0, 2, 0x12, 0x34, 0x56, 0x78, 2, 'k', 'b', 0, 3, 0xaa, 0xbb, 0xcc}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
	if d := AppendUhidDestroy(nil, 2); !bytes.Equal(d, []byte{14, 0, 2}) {
		t.Fatalf("destroy: got % x", d)
	}
}
