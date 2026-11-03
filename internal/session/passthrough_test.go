package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// passStream is a rotStream with a passthrough track beside it.
type passStream struct {
	rotStream
	alt chan target.Frame
}

func (p *passStream) Passthrough() <-chan target.Frame { return p.alt }
func (p *passStream) PassthroughCodec() string         { return "h265" }

// spsAU is a Main-profile HEVC VPS/SPS/PPS, the shape simcap's config
// packet has.
var spsAU = []byte{
	0, 0, 0, 1, 0x40, 0x01, 0x0c,
	0, 0, 0, 1, 0x42, 0x01, 0x01, 0x01, 0x60, 0x00, 0x00, 0x00, 0xb0, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x99, 0xa0,
	0, 0, 0, 1, 0x44, 0x01, 0xc0,
}

func initOf(t *testing.T, msg OutMsg) InitPayload {
	t.Helper()
	if !msg.Control || msg.Data[0] != MsgInit {
		t.Fatalf("not init")
	}
	var p InitPayload
	if err := json.Unmarshal(msg.Data[1:], &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// next returns the next video message on ch, skipping control messages.
func next(t *testing.T, ch <-chan OutMsg) OutMsg {
	t.Helper()
	for {
		select {
		case m := <-ch:
			if !m.Control {
				return m
			}
		case <-time.After(time.Second):
			t.Fatal("no video message")
		}
	}
}

// A viewer that decodes h265 gets the passthrough track and an hvc1 codec; one
// that does not, a browser, keeps H.264 and never sees an HEVC byte.
func TestPassthroughGoesOnlyToViewersThatDecodeIt(t *testing.T) {
	ps := &passStream{alt: make(chan target.Frame, 4)}
	ps.meta = target.Meta{Width: 600, Height: 1300}
	s := &Session{ID: "t", key: "t", stream: ps, viewers: map[uint64]*viewer{}, done: make(chan struct{}),
		init: InitPayload{Codec: "avc1.640028", CodedWidth: 600, CodedHeight: 1300}}
	s.startPassthrough(ps)

	_, web := s.AddViewerWith(ViewerOptions{})
	_, mac := s.AddViewerWith(ViewerOptions{Decodes: []string{"h265"}})
	if c := initOf(t, <-web).Codec; c != "avc1.640028" {
		t.Fatalf("browser codec %q", c)
	}
	if c := initOf(t, <-mac).Codec; c != "hvc1" {
		t.Fatalf("passthrough viewer's first codec %q", c)
	}

	ps.alt <- target.Frame{Config: true, Data: spsAU}
	ps.alt <- target.Frame{Key: true, PTS: 5 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x26, 0x01, 0xaf}}
	// The resolved codec string reaches the passthrough viewer as a new init.
	deadline := time.After(time.Second)
	for got := ""; got != "hvc1.1.6.L153.B0"; {
		select {
		case m := <-mac:
			if m.Control && m.Data[0] == MsgInit {
				got = initOf(t, m).Codec
			}
		case <-deadline:
			t.Fatalf("codec string never resolved (last %q)", got)
		}
	}
	if m := next(t, mac); !m.Config || !m.passthrough {
		t.Fatalf("passthrough config: %+v", m)
	}
	if m := next(t, mac); !m.Key || m.PTSMicros != 5000 {
		t.Fatalf("passthrough keyframe: key=%v pts=%d", m.Key, m.PTSMicros)
	}
	s.broadcast(OutMsg{Data: []byte{1}, Key: true})
	if m := next(t, web); !m.Key || m.passthrough {
		t.Fatalf("browser got %+v", m)
	}
	select {
	case m := <-web:
		if !m.Control {
			t.Fatalf("browser got a passthrough frame")
		}
	default:
	}
	select {
	case m := <-mac:
		if !m.Control {
			t.Fatalf("passthrough viewer got an H.264 frame")
		}
	default:
	}
	close(ps.alt)
}

// A stream without the track leaves a viewer that asks for h265 on H.264.
func TestNoPassthroughTrackKeepsH264(t *testing.T) {
	rs := &rotStream{meta: target.Meta{Width: 600, Height: 1300}}
	s := &Session{ID: "t", key: "t", stream: rs, viewers: map[uint64]*viewer{}, done: make(chan struct{}),
		init: InitPayload{Codec: "avc1.640028"}}
	s.startPassthrough(rs)
	_, mac := s.AddViewerWith(ViewerOptions{Decodes: []string{"h265"}})
	if c := initOf(t, <-mac).Codec; c != "avc1.640028" {
		t.Fatalf("codec %q", c)
	}
}

func TestParseDecode(t *testing.T) {
	if got := ParseDecode(" h265, ,av01"); len(got) != 2 || got[0] != "h265" || got[1] != "av01" {
		t.Fatalf("%q", got)
	}
	if ParseDecode("") != nil {
		t.Fatal("empty")
	}
}
