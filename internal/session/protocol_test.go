package session

import (
	"bytes"
	"math"
	"testing"
)

func TestEncodeFrameRoundTrip(t *testing.T) {
	payload := []byte{0, 0, 0, 1, 0x65, 0xAA, 0xBB}
	out := EncodeFrame(nil, 123456, true, false, 99887766, payload)

	if out[0] != MsgFrame {
		t.Errorf("type = 0x%02x, want 0x%02x", out[0], MsgFrame)
	}
	if out[1]&flagKey == 0 {
		t.Error("key flag not set")
	}
	if out[1]&flagConfig != 0 {
		t.Error("config flag set unexpectedly")
	}
	if got := out[FrameHeaderLen:]; !bytes.Equal(got, payload) {
		t.Errorf("payload = %v, want %v", got, payload)
	}
	if len(out) != FrameHeaderLen+len(payload) {
		t.Errorf("length = %d, want %d", len(out), FrameHeaderLen+len(payload))
	}
}

func TestEncodeFrameReusesBuffer(t *testing.T) {
	// The fan-out loop reuses one scratch buffer across frames; growing it must
	// not corrupt the result, and shrinking must reslice rather than pad.
	big := EncodeFrame(nil, 1, false, false, 0, make([]byte, 4096))
	small := EncodeFrame(big, 2, true, false, 0, []byte{0xAB, 0xCD})

	if len(small) != FrameHeaderLen+2 {
		t.Fatalf("reused buffer length = %d, want %d", len(small), FrameHeaderLen+2)
	}
	if small[FrameHeaderLen] != 0xAB || small[FrameHeaderLen+1] != 0xCD {
		t.Error("payload not written correctly into the reused buffer")
	}
}

func TestDecodeTouch(t *testing.T) {
	// action=2 (move), pointer=7, x=100, y=200, pressure=0xFFFF (full)
	body := []byte{2, 0, 0, 0, 7, 0, 0, 0, 100, 0, 0, 0, 200, 0xFF, 0xFF}
	e, err := DecodeTouch(body)
	if err != nil {
		t.Fatalf("DecodeTouch: %v", err)
	}
	if e.Action != 2 || e.PointerID != 7 || e.X != 100 || e.Y != 200 {
		t.Errorf("decoded %+v, want action=2 pointer=7 x=100 y=200", e)
	}
	if math.Abs(float64(e.Pressure)-1) > 1e-4 {
		t.Errorf("pressure = %v, want 1", e.Pressure)
	}

	if _, err := DecodeTouch(body[:5]); err != ErrMalformed {
		t.Errorf("short buffer error = %v, want ErrMalformed", err)
	}
}

func TestDecodeKey(t *testing.T) {
	// action=0 (down), keycode=4 (back)
	e, err := DecodeKey([]byte{0, 0, 0, 0, 4})
	if err != nil {
		t.Fatalf("DecodeKey: %v", err)
	}
	if e.Action != 0 || e.Keycode != 4 {
		t.Errorf("decoded %+v, want action=0 keycode=4", e)
	}
	if _, err := DecodeKey([]byte{0}); err != ErrMalformed {
		t.Errorf("short buffer error = %v, want ErrMalformed", err)
	}
}

func TestDecodeScroll(t *testing.T) {
	body := make([]byte, 16)
	// x=50, y=60
	body[3] = 50
	body[7] = 60
	// hscroll = 0.0, vscroll = 1.0 (float32 bits of 1.0 are 0x3F800000)
	copy(body[12:], []byte{0x3F, 0x80, 0x00, 0x00})

	e, err := DecodeScroll(body)
	if err != nil {
		t.Fatalf("DecodeScroll: %v", err)
	}
	if e.X != 50 || e.Y != 60 {
		t.Errorf("position = (%d,%d), want (50,60)", e.X, e.Y)
	}
	if e.VScroll != 1 {
		t.Errorf("vscroll = %v, want 1", e.VScroll)
	}
}

func TestPingPong(t *testing.T) {
	const clientMicros = 1234567890
	pong := EncodePong(clientMicros, 999)
	if pong[0] != MsgPong {
		t.Errorf("type = 0x%02x, want 0x%02x", pong[0], MsgPong)
	}
	got, err := DecodePing(pong[1:9])
	if err != nil {
		t.Fatalf("DecodePing: %v", err)
	}
	if got != clientMicros {
		t.Errorf("echoed timestamp = %d, want %d", got, clientMicros)
	}
}

func TestNegotiateProtocol(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want int
		ok   bool
	}{
		{"", 1, true},
		{"1", 1, true},
		{"1-1", 1, true},
		{"1-5", ProtocolVersion, true},
		{"2", 0, false},
		{"2-4", 0, false},
		{"0", 0, false},
		{"3-1", 0, false},
		{"x", 0, false},
		{"1-", 0, false},
	} {
		got, err := NegotiateProtocol(tc.spec)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("NegotiateProtocol(%q) = %d, %v; want %d, ok %v", tc.spec, got, err, tc.want, tc.ok)
		}
	}
}
