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

func passSession() (*Session, *passStream) {
	ps := &passStream{alt: make(chan target.Frame, 8)}
	ps.meta = target.Meta{Width: 600, Height: 1300}
	s := &Session{ID: "t", key: "t", stream: ps, viewers: map[uint64]*viewer{}, done: make(chan struct{}),
		init: InitPayload{Codec: "avc1.640028", CodedWidth: 600, CodedHeight: 1300}}
	s.startPassthrough(ps)
	return s, ps
}

var h265 = ViewerOptions{Decodes: []string{"h265"}}

// feedPass sends frames down the passthrough track and waits for watch, a viewer
// already attached, to receive them, so the pump has cached them.
func feedPass(t *testing.T, ps *passStream, watch <-chan OutMsg, frames ...target.Frame) {
	t.Helper()
	for _, f := range frames {
		ps.alt <- f
		next(t, watch)
	}
}

func key(ms int) target.Frame {
	return target.Frame{Key: true, PTS: time.Duration(ms) * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x26, 0x01, byte(ms)}}
}

func delta(ms int) target.Frame {
	return target.Frame{PTS: time.Duration(ms) * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x02, 0x01, byte(ms)}}
}

// expect reads the next video messages on ch and checks their PTS, in ms.
func expect(t *testing.T, ch <-chan OutMsg, pts ...int) {
	t.Helper()
	for _, want := range pts {
		m := next(t, ch)
		if m.PTSMicros != uint64(want)*1000 {
			t.Fatalf("got pts %dus key=%v config=%v, want %dms", m.PTSMicros, m.Key, m.Config, want)
		}
		if time.Since(m.QueuedAt) > time.Second {
			t.Fatalf("pts %dms queued %v ago", want, time.Since(m.QueuedAt))
		}
	}
}

func drained(t *testing.T, ch <-chan OutMsg) {
	t.Helper()
	for {
		select {
		case m := <-ch:
			if !m.Control {
				t.Fatalf("extra frame pts %dus", m.PTSMicros)
			}
		default:
			return
		}
	}
}

// A viewer that joins a still phone gets the last keyframe and the frames
// since, then the live stream, and decodes all of it.
func TestPassthroughJoinerGetsLastGOP(t *testing.T) {
	s, ps := passSession()
	defer close(ps.alt)
	_, watch := s.AddViewerWith(h265)
	feedPass(t, ps, watch, target.Frame{Config: true, Data: spsAU}, key(5), delta(6), delta(7))

	_, late := s.AddViewerWith(h265)
	expect(t, late, 0, 5, 6, 7)
	drained(t, late)

	feedPass(t, ps, watch, delta(8))
	expect(t, late, 8)
	drained(t, late)
	if n := s.WaitCount(); n != 0 {
		t.Fatalf("joiner waited on %d frames", n)
	}
}

// A new keyframe starts the cache over.
func TestPassthroughGOPRestartsAtKey(t *testing.T) {
	s, ps := passSession()
	defer close(ps.alt)
	_, watch := s.AddViewerWith(h265)
	feedPass(t, ps, watch, target.Frame{Config: true, Data: spsAU}, key(5), delta(6), key(7), delta(8))

	_, late := s.AddViewerWith(h265)
	expect(t, late, 0, 7, 8)
	drained(t, late)
}

// A frame cached but not yet broadcast when a viewer joins reaches it once.
func TestPassthroughReplayNotRepeated(t *testing.T) {
	s := &Session{ID: "t", key: "t", stream: &rotStream{}, viewers: map[uint64]*viewer{}, done: make(chan struct{}),
		pass: &passTrack{fourcc: "h265", codec: "hvc1"}}
	k := OutMsg{Data: make([]byte, FrameHeaderLen+1), Key: true, PTSMicros: 5000, passthrough: true}
	s.mu.Lock()
	s.pass.cacheLocked(&k)
	s.mu.Unlock()

	_, v := s.AddViewerWith(h265)
	s.broadcast(k)
	expect(t, v, 5)
	drained(t, v)
}

// A GOP too long to keep is dropped whole: replaying it without its keyframe
// would decode to garbage.
func TestPassthroughGOPOverflow(t *testing.T) {
	p := &passTrack{}
	k := OutMsg{Data: make([]byte, 10), Key: true}
	p.cacheLocked(&k)
	for i := 0; i < gopMaxFrames-1; i++ {
		d := OutMsg{Data: make([]byte, 10)}
		if p.cacheLocked(&d) {
			t.Fatalf("overflow at %d frames", i+2)
		}
	}
	d := OutMsg{Data: make([]byte, 10)}
	if !p.cacheLocked(&d) || p.gop != nil {
		t.Fatalf("GOP of %d frames kept", gopMaxFrames+1)
	}
	if p.cacheLocked(&d) || p.gop != nil {
		t.Fatalf("delta after overflow started a GOP")
	}
	big := OutMsg{Data: make([]byte, gopMaxBytes)}
	p.cacheLocked(&k)
	if !p.cacheLocked(&big) {
		t.Fatalf("%d bytes kept", gopMaxBytes+10)
	}
}
