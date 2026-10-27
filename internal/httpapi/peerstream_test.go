package httpapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/peer"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/quic-go/quic-go"
)

// recTarget is an owner-side target that streams a steady fake picture and
// records the input that reaches it, which the synthetic target refuses.
type recTarget struct {
	mu      sync.Mutex
	touches []target.Touch
	keys    []target.Key
	scrolls []target.Scroll
	texts   []string
	streams []*recStream
}

func (r *recTarget) ID() string        { return "rec" }
func (r *recTarget) Kind() target.Kind { return target.KindDevice }
func (r *recTarget) Label() string     { return "Recording device" }
func (r *recTarget) Open(context.Context, target.Options) (target.Stream, error) {
	s := &recStream{t: r, frames: make(chan target.Frame, 4), stop: make(chan struct{})}
	r.mu.Lock()
	r.streams = append(r.streams, s)
	r.mu.Unlock()
	go s.pump()
	return s, nil
}

// endStreams makes the owner's capture end on its own, as a pulled cable does.
func (r *recTarget) endStreams() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.streams {
		s.once.Do(func() { close(s.stop) })
	}
}

type recStream struct {
	t      *recTarget
	frames chan target.Frame
	stop   chan struct{}
	once   sync.Once
}

func (s *recStream) pump() {
	defer close(s.frames)
	send := func(f target.Frame) bool {
		select {
		case s.frames <- f:
			return true
		case <-s.stop:
			return false
		}
	}
	if !send(target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67, 1, 2, 3, 0, 0, 0, 1, 0x68, 4}, Captured: time.Now()}) {
		return
	}
	for i := 0; ; i++ {
		// A payload that tells the frames apart, and big enough that slots
		// finishing out of order is possible.
		data := append([]byte{0, 0, 0, 1, 0x41}, make([]byte, 2000+(i%3)*30000)...)
		data[5] = byte(i)
		if !send(target.Frame{PTS: time.Duration(i) * 16 * time.Millisecond, Key: i%30 == 0, Data: data, Captured: time.Now()}) {
			return
		}
		select {
		case <-time.After(8 * time.Millisecond):
		case <-s.stop:
			return
		}
	}
}

func (s *recStream) Meta() target.Meta {
	return target.Meta{Codec: "h264", Width: 360, Height: 780, Name: "Rec 1"}
}
func (s *recStream) Frames() <-chan target.Frame { return s.frames }
func (s *recStream) Touch(t target.Touch) error {
	s.t.mu.Lock()
	s.t.touches = append(s.t.touches, t)
	s.t.mu.Unlock()
	return nil
}
func (s *recStream) Scroll(sc target.Scroll) error {
	s.t.mu.Lock()
	s.t.scrolls = append(s.t.scrolls, sc)
	s.t.mu.Unlock()
	return nil
}
func (s *recStream) Key(k target.Key) error {
	s.t.mu.Lock()
	s.t.keys = append(s.t.keys, k)
	s.t.mu.Unlock()
	return nil
}
func (s *recStream) Text(text string) error {
	s.t.mu.Lock()
	s.t.texts = append(s.t.texts, text)
	s.t.mu.Unlock()
	return nil
}
func (s *recStream) RequestKeyframe() error { return nil }
func (s *recStream) Err() error             { return nil }
func (s *recStream) Close() error           { s.once.Do(func() { close(s.stop) }); return nil }

// quicOwner is a loupe server reachable over HTTPS and raw QUIC on one port
// number, as a real one is.
type quicOwner struct {
	*Server
	url string
	rec *recTarget
	ts  *httptest.Server

	mu    sync.Mutex
	conns []*quic.Conn
}

// dropConns closes every raw-QUIC connection from outside, as a crash would.
func (o *quicOwner) dropConns() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, c := range o.conns {
		_ = c.CloseWithError(0, "")
	}
}

func listenSamePort(t *testing.T) (net.Listener, net.PacketConn) {
	t.Helper()
	for range 20 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		pc, err := net.ListenPacket("udp", l.Addr().String())
		if err == nil {
			return l, pc
		}
		_ = l.Close()
	}
	t.Fatal("no port free on both tcp and udp")
	return nil, nil
}

func startQUICOwner(t *testing.T, serve func(*Server, *quic.Conn)) *quicOwner {
	t.Helper()
	cert := testCert(t)
	l, pc := listenSamePort(t)
	rec := &recTarget{}
	m := NewManager(target.Options{})
	m.Register(rec)
	o := &quicOwner{Server: &Server{mgr: m, claims: newClaimBook(nil), token: "tok"}, rec: rec}

	ts := httptest.NewUnstartedServer(o.Handler())
	ts.Listener.Close()
	ts.Listener = l
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	o.url = ts.URL
	o.ts = ts

	ln, err := quic.ListenEarly(pc, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{session.QUICALPN}},
		&quic.Config{EnableStreamResetPartialDelivery: true, InitialPacketSize: 1200})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		_ = pc.Close()
		o.dropConns()
		m.Shutdown()
	})
	go func() {
		for {
			qc, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			o.mu.Lock()
			o.conns = append(o.conns, qc)
			o.mu.Unlock()
			go serve(o.Server, qc)
		}
	}()
	return o
}

func serveReal(s *Server, qc *quic.Conn) { s.ServeQUIC(qc) }

// quicHub is a hub with the real add-time probe, polling the owner.
func quicHub(t *testing.T) *hub {
	t.Helper()
	h := newHub(t)
	h.peers.SetProbe(peer.ProbeProtocol)
	return h
}

func openPeer(t *testing.T, h *hub, id string) target.PeerStream {
	t.Helper()
	tgt, ok := h.mgr.Target(id)
	if !ok {
		t.Fatalf("%s does not resolve", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := tgt.Open(ctx, target.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st.(target.PeerStream)
}

func TestPeerStreamRelaysFramesMetaAndInput(t *testing.T) {
	o := startQUICOwner(t, serveReal)
	h := quicHub(t)
	if w := h.add(t, "mini", o.url, "tok"); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	var added ServerInfo
	_ = json.Unmarshal(h.do(http.MethodGet, "/api/servers", "").Body.Bytes(), &[]ServerInfo{})
	_ = added
	eventually(t, "mini/rec listed", func() bool { return find(h.targets(t, ""), "mini/rec") != nil })

	st := openPeer(t, h, "mini/rec")
	meta := st.Meta()
	if meta.Codec != "h264" || meta.Width != 360 || meta.Height != 780 || meta.Name != "Rec 1" {
		t.Fatalf("Meta = %+v", meta)
	}
	if st.OwnerViewer() == 0 {
		t.Fatal("no owner viewer id")
	}

	// Frames arrive in the owner's order, stamped when they reached the hub.
	var last time.Duration = -1
	var sawKey, sawConfig bool
	deadline := time.After(10 * time.Second)
	for n := 0; n < 60; {
		select {
		case f, ok := <-st.Frames():
			if !ok {
				t.Fatalf("frames closed early: %v", st.Err())
			}
			if time.Since(f.Captured) > 5*time.Second || f.Captured.IsZero() {
				t.Fatalf("Captured = %v", f.Captured)
			}
			if f.Config {
				sawConfig = true
				continue
			}
			if f.PTS <= last {
				t.Fatalf("pts went %v -> %v", last, f.PTS)
			}
			last = f.PTS
			sawKey = sawKey || f.Key
			if len(f.Data) < 5 || f.Data[3] != 1 {
				t.Fatalf("payload is not Annex B: % x", f.Data[:min(8, len(f.Data))])
			}
			n++
		case <-deadline:
			t.Fatalf("only %d frames", n)
		}
	}
	if !sawKey || !sawConfig {
		t.Fatalf("key=%v config=%v", sawKey, sawConfig)
	}

	// Input reaches the owner's stream.
	if err := st.Touch(target.Touch{Action: target.TouchDown, PointerID: 3, X: 120, Y: 340, Pressure: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Key(target.Key{Action: target.KeyUp, Keycode: target.KeycodeBack}); err != nil {
		t.Fatal(err)
	}
	if err := st.Scroll(target.Scroll{X: 5, Y: 6, HScroll: 0, VScroll: -2.5}); err != nil {
		t.Fatal(err)
	}
	if err := st.(target.TextTyper).Text("héllo"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "input at the owner", func() bool {
		o.rec.mu.Lock()
		defer o.rec.mu.Unlock()
		return len(o.rec.touches) == 1 && len(o.rec.keys) == 1 && len(o.rec.scrolls) == 1 && len(o.rec.texts) == 1
	})
	o.rec.mu.Lock()
	defer o.rec.mu.Unlock()
	if g := o.rec.touches[0]; g.Action != target.TouchDown || g.PointerID != 3 || g.X != 120 || g.Y != 340 || g.Pressure < 0.99 {
		t.Errorf("touch = %+v", g)
	}
	if g := o.rec.keys[0]; g.Action != target.KeyUp || g.Keycode != target.KeycodeBack {
		t.Errorf("key = %+v", g)
	}
	if g := o.rec.scrolls[0]; g.X != 5 || g.Y != 6 || g.VScroll != -2.5 {
		t.Errorf("scroll = %+v", g)
	}
	if o.rec.texts[0] != "héllo" {
		t.Errorf("text = %q", o.rec.texts[0])
	}
}

func TestHubSessionOverAPeerRelaysAndSkipsAdaptiveBitrate(t *testing.T) {
	o := startQUICOwner(t, serveReal)
	h := quicHub(t)
	h.mgr.SetAdaptiveBitRate(true)
	if w := h.add(t, "mini", o.url, "tok"); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	eventually(t, "mini/rec listed", func() bool { return find(h.targets(t, ""), "mini/rec") != nil })

	sess, err := h.mgr.Acquire(context.Background(), "mini/rec")
	if err != nil {
		t.Fatal(err)
	}
	defer h.mgr.Release("mini/rec")
	_, out := sess.AddViewer()
	var init session.InitPayload
	frames := 0
	deadline := time.After(10 * time.Second)
	for frames < 20 {
		select {
		case m := <-out:
			switch {
			case m.Control && len(m.Data) > 0 && m.Data[0] == session.MsgInit:
				if err := json.Unmarshal(m.Data[1:], &init); err != nil {
					t.Fatal(err)
				}
			case !m.Control:
				frames++
			}
		case <-deadline:
			t.Fatalf("hub viewer got %d frames", frames)
		}
	}
	if init.CodedWidth != 360 || init.CodedHeight != 780 || init.Name != "Rec 1" || init.Kind != string(target.KindDevice) {
		t.Errorf("hub init = %+v", init)
	}
	if err := sess.Touch(session.TouchEvent{Action: uint8(target.TouchUp), X: 10, Y: 20, Pressure: 0.5}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "hub input at the owner", func() bool {
		o.rec.mu.Lock()
		defer o.rec.mu.Unlock()
		return len(o.rec.touches) == 1 && o.rec.touches[0].X == 10 && o.rec.touches[0].Y == 20
	})
}

func TestPeerStreamEndsWithAnErrorWhenTheOwnerGoes(t *testing.T) {
	for name, kill := range map[string]func(*quicOwner){
		"connection dropped": func(o *quicOwner) { o.dropConns() },
		"capture ended":      func(o *quicOwner) { o.rec.endStreams() },
	} {
		t.Run(name, func(t *testing.T) {
			o := startQUICOwner(t, serveReal)
			h := quicHub(t)
			if w := h.add(t, "mini", o.url, "tok"); w.Code != http.StatusCreated {
				t.Fatalf("add = %d %s", w.Code, w.Body)
			}
			eventually(t, "mini/rec listed", func() bool { return find(h.targets(t, ""), "mini/rec") != nil })
			st := openPeer(t, h, "mini/rec")
			<-st.Frames()
			kill(o)
			deadline := time.After(10 * time.Second)
			for open := true; open; {
				select {
				case _, open = <-st.Frames():
				case <-deadline:
					t.Fatal("frames never closed")
				}
			}
			if st.Err() == nil {
				t.Fatal("Err is nil after the owner went")
			}
		})
	}
}

func TestPeerStreamCloseIsClean(t *testing.T) {
	o := startQUICOwner(t, serveReal)
	h := quicHub(t)
	h.add(t, "mini", o.url, "tok")
	eventually(t, "mini/rec listed", func() bool { return find(h.targets(t, ""), "mini/rec") != nil })
	st := openPeer(t, h, "mini/rec")
	<-st.Frames()
	_ = st.Close()
	for range st.Frames() {
	}
	if err := st.Err(); err != nil {
		t.Fatalf("Err after our own Close = %v", err)
	}
}

func TestAddRefusesAnOwnerThatCannotServeTheProtocol(t *testing.T) {
	o := startQUICOwner(t, func(_ *Server, qc *quic.Conn) {
		// An owner from the future: it reads the open, then ends the
		// connection the way ServeQUIC does for a range it cannot serve.
		time.Sleep(50 * time.Millisecond)
		_ = qc.CloseWithError(quic.ApplicationErrorCode(session.QUICCloseEnded), session.EndedUnsupportedProtocol)
	})
	h := quicHub(t)
	w := h.add(t, "future", o.url, "tok")
	var e ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != http.StatusBadGateway || e.Code != ErrCodeUnsupportedProtocol || e.Error == "" {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if got := h.peers.Names(); len(got) != 0 {
		t.Fatalf("a refused server was stored: %v", got)
	}
}

func TestAddOfARealOwnerPassesTheProbe(t *testing.T) {
	o := startQUICOwner(t, serveReal)
	h := quicHub(t)
	w := h.add(t, "mini", o.url, "tok")
	var info ServerInfo
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	if w.Code != http.StatusCreated || info.Warning != "" {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
}

func TestAddWithNoQUICAnswerStillSucceedsWithAWarning(t *testing.T) {
	// HTTPS only, nothing on the UDP port: a firewall that drops UDP.
	h := quicHub(t)
	ts := startTLS(t, "", testCert(t), ownerServer().Handler())
	w := h.add(t, "mini", ts.URL, "tok")
	var info ServerInfo
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	if w.Code != http.StatusCreated || info.Warning == "" {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if len(h.peers.Names()) != 1 {
		t.Fatal("server not stored")
	}
}
