package session

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// goldenBytes reads a fixture from testdata/protocol/<name>. A .hex fixture
// is one hex-encoded byte string per non-comment, non-blank line,
// concatenated in order -- comment lines starting with '#' document the
// message field by field so the fixture can be checked against the prose in
// docs/protocol.md without decoding it by hand. A .json fixture is read
// verbatim.
func goldenBytes(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "protocol", name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	if !strings.HasSuffix(name, ".hex") {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return b
	}

	var out []byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("%s: bad hex line %q: %v", name, line, err)
		}
		out = append(out, b...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

// TestFrameGoldenWS pins the 18-byte WebSocket frame envelope
// (session.FrameHeaderLen) against testdata/protocol/frame-ws.hex. A byte
// changing here -- a reordered field, a width change, a flipped flag bit --
// must fail this test, which is the property docs/protocol.md's fixtures
// exist to guarantee. See TestMutationsBreakTheGoldenFixtures for the proof.
func TestFrameGoldenWS(t *testing.T) {
	want := goldenBytes(t, "frame-ws.hex")
	data := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xaa, 0xbb, 0xcc}
	got := EncodeFrame(nil, 352074200000, true, false, 1727177400123456, data)
	if !bytes.Equal(got, want) {
		t.Errorf("frame-ws.hex mismatch:\n got  %x\n want %x", got, want)
	}
}

// TestFrameGoldenWSConfig pins the WS envelope for a config frame (SPS/PPS),
// carrying flagConfig set and flagKey clear, and pts 0 -- see trap 31 and
// docs/protocol.md: a config frame carries no picture, so no presentation
// time applies.
func TestFrameGoldenWSConfig(t *testing.T) {
	want := goldenBytes(t, "frame-ws-config.hex")
	data := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0xc0, 0x29}
	got := EncodeFrame(nil, 0, false, true, 1727177400000000, data)
	if !bytes.Equal(got, want) {
		t.Errorf("frame-ws-config.hex mismatch:\n got  %x\n want %x", got, want)
	}
}

func TestPongGolden(t *testing.T) {
	want := goldenBytes(t, "pong.hex")
	got := EncodePong(1234567890, 1234567999)
	if !bytes.Equal(got, want) {
		t.Errorf("pong.hex mismatch:\n got  %x\n want %x", got, want)
	}
}

func TestPingGolden(t *testing.T) {
	raw := goldenBytes(t, "ping.hex")
	if raw[0] != MsgPing {
		t.Fatalf("ping.hex type byte = 0x%02x, want MsgPing (0x%02x)", raw[0], MsgPing)
	}
	got, err := DecodePing(raw[1:])
	if err != nil {
		t.Fatalf("DecodePing: %v", err)
	}
	if got != 1234567890 {
		t.Errorf("clientMicros = %d, want 1234567890", got)
	}
}

func TestTouchGolden(t *testing.T) {
	raw := goldenBytes(t, "touch.hex")
	if raw[0] != MsgTouch {
		t.Fatalf("touch.hex type byte = 0x%02x, want MsgTouch (0x%02x)", raw[0], MsgTouch)
	}
	got, err := DecodeTouch(raw[1:])
	if err != nil {
		t.Fatalf("DecodeTouch: %v", err)
	}
	want := TouchEvent{Action: 2, PointerID: 100000, X: 700000, Y: 1500000, Pressure: float32(0x1234) / 65535}
	if got != want {
		t.Errorf("touch.hex decoded = %+v, want %+v", got, want)
	}
}

func TestKeyGolden(t *testing.T) {
	raw := goldenBytes(t, "key.hex")
	if raw[0] != MsgKey {
		t.Fatalf("key.hex type byte = 0x%02x, want MsgKey (0x%02x)", raw[0], MsgKey)
	}
	got, err := DecodeKey(raw[1:])
	if err != nil {
		t.Fatalf("DecodeKey: %v", err)
	}
	want := KeyEvent{Action: 1, Keycode: 0x7e020304}
	if got != want {
		t.Errorf("key.hex decoded = %+v, want %+v", got, want)
	}
}

func TestScrollGolden(t *testing.T) {
	raw := goldenBytes(t, "scroll.hex")
	if raw[0] != MsgScroll {
		t.Fatalf("scroll.hex type byte = 0x%02x, want MsgScroll (0x%02x)", raw[0], MsgScroll)
	}
	got, err := DecodeScroll(raw[1:])
	if err != nil {
		t.Fatalf("DecodeScroll: %v", err)
	}
	want := ScrollEvent{X: -300000, Y: 400000, HScroll: -2.5, VScroll: 3.5}
	if got != want {
		t.Errorf("scroll.hex decoded = %+v, want %+v", got, want)
	}
}

// TestEventGoldenTap pins a tap event -- the plainest shape EncodeEvent
// produces, sourced from a viewer.
func TestEventGoldenTap(t *testing.T) {
	want := goldenBytes(t, "event.hex")
	got := EncodeEvent(Event{
		Seq: 1, T: 1727177400000, Source: "viewer", Viewer: 2,
		Kind: "tap", From: &Point{X: 0.512, Y: 0.734},
	})
	if !bytes.Equal(got, want) {
		t.Errorf("event.hex mismatch:\n got  %s\n want %s", got, want)
	}
}

// TestEventGoldenRedactedText pins a text event whose Text was withheld
// because the focused node in the view hierarchy reported a password field.
// Redacted is true and TextLen carries the rune count of what was actually
// typed; Text carries the fixed marker, never the characters. See
// internal/httpapi/agent.go's publishAgentEvent.
func TestEventGoldenRedactedText(t *testing.T) {
	want := goldenBytes(t, "event-redacted.hex")
	got := EncodeEvent(Event{
		Seq: 2, T: 1727177400500, Source: "agent",
		Kind: "text", Text: "•••", Redacted: true, TextLen: 8,
	})
	if !bytes.Equal(got, want) {
		t.Errorf("event-redacted.hex mismatch:\n got  %s\n want %s", got, want)
	}

	var decoded Event
	if err := json.Unmarshal(got[1:], &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.Redacted {
		t.Error("Redacted = false, want true -- this fixture must actually carry redacted:true")
	}
	if decoded.TextLen != 8 {
		t.Errorf("TextLen = %d, want 8", decoded.TextLen)
	}
	if decoded.Text != "•••" {
		t.Errorf("Text = %q, want the fixed redaction marker, not the typed characters", decoded.Text)
	}
}

// TestEventGoldenButton pins kind:"button", published either by a viewer's
// own key-up whose keycode names a device button (Session.ViewerKey,
// target.ButtonForKeycode) or by an agent's POST /api/input {"action":
// "button"}.
func TestEventGoldenButton(t *testing.T) {
	want := goldenBytes(t, "event-button.hex")
	got := EncodeEvent(Event{
		Seq: 3, T: 1727177401000, Source: "viewer", Viewer: 2,
		Kind: "button", Button: "home",
	})
	if !bytes.Equal(got, want) {
		t.Errorf("event-button.hex mismatch:\n got  %s\n want %s", got, want)
	}
}

// TestInitPayloadGolden pins the MsgInit JSON body against
// testdata/protocol/init.json.
func TestInitPayloadGolden(t *testing.T) {
	want := goldenBytes(t, "init.json")
	got, err := json.Marshal(InitPayload{
		Codec: "avc1.42E01E", CodedWidth: 1080, CodedHeight: 2424,
		Name: "emulator-5554", Kind: "emulator", Control: true,
		SessionID: "emulator-5554", MaxBufferMs: 400, Viewer: 1, Protocol: 1,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, want) {
		t.Errorf("init.json mismatch:\n got  %s want %s", got, want)
	}
}

// TestEventPageGolden pins GET /api/events' body: session, oldest, latest and
// events, built from one eventRing carrying the tap, redacted-text and
// button fixtures above.
func TestEventPageGolden(t *testing.T) {
	want := goldenBytes(t, "eventpage.json")
	page := EventPage{
		Session: "a1b2c3d4e5f6a7b8",
		Oldest:  1,
		Latest:  3,
		Events: []Event{
			{Seq: 1, T: 1727177400000, Source: "viewer", Viewer: 2, Kind: "tap", From: &Point{X: 0.512, Y: 0.734}},
			{Seq: 2, T: 1727177400500, Source: "agent", Kind: "text", Text: "•••", Redacted: true, TextLen: 8},
			{Seq: 3, T: 1727177401000, Source: "viewer", Viewer: 2, Kind: "button", Button: "home"},
		},
	}
	got, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, want) {
		t.Errorf("eventpage.json mismatch:\n got  %s want %s", got, want)
	}
}

// TestEventGoldenFull pins one event carrying to, ms, pointers, scroll,
// rotation:0 and err all at once -- see testdata/protocol/event-full.hex for
// why this combination does not correspond to any one real gesture.
func TestEventGoldenFull(t *testing.T) {
	want := goldenBytes(t, "event-full.hex")
	rotation := 0
	got := EncodeEvent(Event{
		Seq: 4, T: 1727177401500, Source: "agent", Kind: "swipe",
		To: &Point{X: 0.9, Y: 0.8}, Ms: 250, Pointers: 2, Scale: 1.5,
		Scroll: &Point{X: -1.5, Y: 0.25}, Rotation: &rotation,
		Err: "context deadline exceeded",
	})
	if !bytes.Equal(got, want) {
		t.Errorf("event-full.hex mismatch:\n got  %s\n want %s", got, want)
	}
}

// TestEventJSONTagsAreExplicit lists every json tag Event.MarshalJSON (via
// the struct tags EncodeEvent's json.Marshal reads) can produce, in
// declaration order. A field added to Event with `omitempty` and no
// corresponding line here fails this test rather than silently going
// unpinned by every fixture above -- an omitempty field marshals identically
// to a field that was never added when its zero value is never set, so no
// existing fixture would ever notice it.
func TestEventJSONTagsAreExplicit(t *testing.T) {
	// Full tag strings, including omitempty, so a field silently gaining (or
	// losing) omitempty fails here too -- comparing only the name half would
	// miss exactly that, and omitempty is what makes a zero-value field
	// invisible to every fixture above.
	want := []string{
		"seq", "t", "source", "viewer,omitempty", "agent,omitempty", "kind", "from,omitempty",
		"to,omitempty", "ms,omitempty", "pointers,omitempty",
		"scale,omitempty", "keycode,omitempty", "button,omitempty", "text,omitempty",
		"rotation,omitempty", "scroll,omitempty", "redacted,omitempty",
		"textLen,omitempty", "err,omitempty",
	}
	typ := reflect.TypeOf(Event{})
	if typ.NumField() != len(want) {
		t.Fatalf("Event has %d fields, this test names %d -- update want", typ.NumField(), len(want))
	}
	for i, tag := range want {
		got := typ.Field(i).Tag.Get("json")
		if got != tag {
			t.Errorf("field %d: json tag %q, want %q", i, got, tag)
		}
	}
}
