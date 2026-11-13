package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/emugrpc"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
	"github.com/bmcreations/loupe/internal/wsx"
)

// fakeMicTarget is an AVD whose OpenMic hands out fakeMicCalls.
type fakeMicTarget struct {
	stubTarget
	id   string
	open func() (target.MicCall, error)
	// playout is how long each call's CloseAndRecv takes to end it OK.
	playout time.Duration

	mu    sync.Mutex
	calls []*fakeMicCall
	// overlapped is set when a call opened before an earlier one closed,
	// which the emulator refuses.
	overlapped bool
}

func (f *fakeMicTarget) ID() string { return f.id }

func (f *fakeMicTarget) OpenMic(ctx context.Context) (target.MicCall, error) {
	if f.open != nil {
		return f.open()
	}
	c := newFakeMicCall()
	c.playout = f.playout
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, prev := range f.calls {
		select {
		case <-prev.closed:
		default:
			f.overlapped = true
		}
	}
	f.calls = append(f.calls, c)
	return c, nil
}

func (f *fakeMicTarget) call(t *testing.T, i int) *fakeMicCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.calls) {
		t.Fatalf("only %d calls opened, want call %d", len(f.calls), i)
	}
	return f.calls[i]
}

// fakeMicCall records what it is sent. A non-nil block makes the first Send
// signal blocked and wait for Close. CloseAndRecv counts half-closes and
// ends the call OK after playout, as the emulator does once it has played
// out its buffer.
type fakeMicCall struct {
	mu         sync.Mutex
	got        []emugrpc.AudioPacket
	halfCloses int
	block      bool
	blocked    chan struct{}
	playout    time.Duration

	done      chan struct{}
	err       error
	endOnce   sync.Once
	closed    chan struct{}
	closeOnce sync.Once
}

func newFakeMicCall() *fakeMicCall {
	return &fakeMicCall{blocked: make(chan struct{}), done: make(chan struct{}), closed: make(chan struct{})}
}

func (c *fakeMicCall) Send(p emugrpc.AudioPacket) error {
	select {
	case <-c.closed:
		return errors.New("fake: closed")
	case <-c.done:
		return c.err
	default:
	}
	c.mu.Lock()
	c.got = append(c.got, p)
	block := c.block && len(c.got) == 1
	c.mu.Unlock()
	if block {
		close(c.blocked)
		<-c.closed
		return errors.New("fake: closed")
	}
	return nil
}

func (c *fakeMicCall) end(err error) {
	c.endOnce.Do(func() { c.err = err; close(c.done) })
}

func (c *fakeMicCall) CloseAndRecv(timeout time.Duration) error {
	c.mu.Lock()
	c.halfCloses++
	c.mu.Unlock()
	select {
	case <-c.done:
		return c.err
	case <-c.closed:
		return errors.New("fake: closed")
	case <-time.After(timeout):
		return errors.New("fake: no status")
	case <-time.After(c.playout):
	}
	c.end(&emugrpc.Status{Code: emugrpc.OK})
	return nil
}

func (c *fakeMicCall) halfClosed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.halfCloses
}

func (c *fakeMicCall) Done() <-chan struct{} { return c.done }
func (c *fakeMicCall) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

func (c *fakeMicCall) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	c.end(fmt.Errorf("%w: closed", emugrpc.ErrConnection))
	return nil
}

func (c *fakeMicCall) packets() []emugrpc.AudioPacket {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]emugrpc.AudioPacket(nil), c.got...)
}

func (c *fakeMicCall) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-c.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the call was never closed")
	}
}

// micServer serves the targets with a trace it returns the path of.
func micServer(t *testing.T, targets ...target.Target) (*httptest.Server, string) {
	t.Helper()
	m := NewManager(target.Options{})
	for _, tg := range targets {
		m.Register(tg)
	}
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tw.Close() })
	srv := httptest.NewServer((&Server{mgr: m, token: "secret", trace: tw}).Handler())
	t.Cleanup(srv.Close)
	return srv, path
}

// wsClient is the client end of a WebSocket, enough for these tests.
type wsClient struct {
	c  net.Conn
	br *bufio.Reader
}

// dialMic upgrades GET /api/mic?query. It returns the HTTP status, and the
// socket when that is 101.
func dialMic(t *testing.T, srv *httptest.Server, query string, auth bool) (int, *wsClient) {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET /api/mic?" + query + " HTTP/1.1\r\nHost: x\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	if auth {
		req += "Authorization: Bearer secret\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	return resp.StatusCode, &wsClient{c: c, br: br}
}

func (w *wsClient) send(t *testing.T, op byte, payload []byte) {
	t.Helper()
	head := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n <= 0xffff:
		head = append(head, 0x80|126)
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head = append(head, 0x80|127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	mask := [4]byte{1, 2, 3, 4}
	head = append(head, mask[:]...)
	body := make([]byte, len(payload))
	for i := range payload {
		body[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.c.Write(append(head, body...)); err != nil {
		t.Fatal(err)
	}
}

// next reads the next frame the server sent.
func (w *wsClient) next() (op byte, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(w.br, head[:]); err != nil {
		return 0, nil, err
	}
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(w.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(w.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	payload = make([]byte, n)
	_, err = io.ReadFull(w.br, payload)
	return head[0] & 0x0f, payload, err
}

func (w *wsClient) message(t *testing.T) MicMessage {
	t.Helper()
	op, payload, err := w.next()
	if err != nil {
		t.Fatalf("reading a message: %v", err)
	}
	if op != wsx.OpText {
		t.Fatalf("frame op %d (% x), want a text frame", op, payload)
	}
	var m MicMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("%q: %v", payload, err)
	}
	return m
}

// wantClosed reads to the server's close frame or the end of the socket.
func (w *wsClient) wantClosed(t *testing.T) {
	t.Helper()
	for {
		op, payload, err := w.next()
		if err != nil || op == wsx.OpClose {
			return
		}
		t.Fatalf("frame op %d %q after the last message, want close", op, payload)
	}
}

func (w *wsClient) wantError(t *testing.T, code string) MicMessage {
	t.Helper()
	m := w.message(t)
	if m.Code != code || m.Error == "" || m.State != "" {
		t.Fatalf("got %+v, want an error with code %q", m, code)
	}
	w.wantClosed(t)
	return m
}

func (w *wsClient) wantLive(t *testing.T) {
	t.Helper()
	if m := w.message(t); m != (MicMessage{State: "live"}) {
		t.Fatalf("got %+v, want live", m)
	}
}

// micRecord waits for the handler's trace record for the n-th socket.
func micRecord(t *testing.T, path string, n int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		var recs []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil && rec["action"] == "mic" {
				recs = append(recs, rec)
			}
		}
		if len(recs) > n {
			return recs[n]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no trace record for socket %d in %s", n, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pcm(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestMicBeforeTheUpgradeIsHTTP(t *testing.T) {
	srv, _ := micServer(t, &fakeMicTarget{id: "avd:x"})
	for _, c := range []struct {
		query string
		auth  bool
		want  int
	}{
		{"target=avd:x", false, http.StatusUnauthorized},
		{"", true, http.StatusBadRequest},
		{"target=avd:nope", true, http.StatusNotFound},
	} {
		if got, _ := dialMic(t, srv, c.query, c.auth); got != c.want {
			t.Errorf("?%s auth=%v: %d, want %d", c.query, c.auth, got, c.want)
		}
	}
	// A plain GET is not an upgrade.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/mic?target=avd:x", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("plain GET: %d, want 400", resp.StatusCode)
	}
}

func TestMicNotAnAVD(t *testing.T) {
	srv, tr := micServer(t, &stubTarget{})
	_, ws := dialMic(t, srv, "target=stub", true)
	ws.wantError(t, "notAvd")
	if rec := micRecord(t, tr, 0); rec["outcome"] != "refused" || rec["code"] != "notAvd" || rec["kind"] != "agent" {
		t.Errorf("trace %v", rec)
	}
}

func TestMicForwardsPCMIn20msPackets(t *testing.T) {
	avd := &fakeMicTarget{id: "avd:x"}
	srv, tr := micServer(t, avd)
	_, ws := dialMic(t, srv, "target=avd:x", true)
	ws.wantLive(t)

	// 3 whole packets and 101 bytes over, in frames that split packets and
	// samples, with a text frame between that loupe ignores.
	in := pcm(3*640 + 101)
	ws.send(t, wsx.OpBinary, in[:1000])
	ws.send(t, wsx.OpBinary, in[1000:1001])
	ws.send(t, wsx.OpText, []byte(`{"hello":1}`))
	ws.send(t, wsx.OpBinary, in[1001:])
	ws.send(t, wsx.OpClose, nil)
	call := avd.call(t, 0)
	call.waitClosed(t)

	got := call.packets()
	if len(got) != 3 {
		t.Fatalf("%d packets, want 3", len(got))
	}
	for i, p := range got {
		if !bytes.Equal(p.Audio, in[i*640:(i+1)*640]) {
			t.Errorf("packet %d audio differs", i)
		}
		if p.Timestamp != uint64(i)*20000 {
			t.Errorf("packet %d timestamp %d, want %d", i, p.Timestamp, i*20000)
		}
		if (p.Format != nil) != (i == 0) {
			t.Errorf("packet %d format %v", i, p.Format)
		}
	}
	want := emugrpc.AudioFormat{SamplingRate: 16000, Channels: emugrpc.Mono, Format: emugrpc.S16, Mode: emugrpc.ModeRealTime}
	if *got[0].Format != want {
		t.Errorf("format %+v, want %+v", *got[0].Format, want)
	}
	// The client closing ends the call the gRPC way, so the emulator plays
	// out what it buffered instead of the call being cancelled under it.
	if n := call.halfClosed(); n != 1 {
		t.Errorf("%d half-closes, want 1", n)
	}
	rec := micRecord(t, tr, 0)
	if rec["outcome"] != "ended" || rec["err"] != nil || rec["live"] != true || rec["packets"] != 3.0 || rec["bytes"] != float64(len(in)) {
		t.Errorf("trace %v", rec)
	}

	// The slot is free for the next socket.
	_, ws2 := dialMic(t, srv, "target=avd:x", true)
	ws2.wantLive(t)
}

func TestMicOneSocketPerTarget(t *testing.T) {
	avd := &fakeMicTarget{id: "avd:x"}
	srv, _ := micServer(t, avd, &fakeMicTarget{id: "avd:y"})
	_, first := dialMic(t, srv, "target=avd:x", true)
	first.wantLive(t)

	_, second := dialMic(t, srv, "target=avd:x", true)
	second.wantError(t, "micBusy")
	// Another target is its own slot.
	_, other := dialMic(t, srv, "target=avd:y", true)
	other.wantLive(t)

	// The first socket still feeds its call.
	first.send(t, wsx.OpBinary, pcm(640))
	first.send(t, wsx.OpClose, nil)
	call := avd.call(t, 0)
	call.waitClosed(t)
	if n := len(call.packets()); n != 1 {
		t.Errorf("first call got %d packets after the second socket, want 1", n)
	}
}

func TestMicRefusals(t *testing.T) {
	failed := func(err error) func() (target.MicCall, error) {
		return func() (target.MicCall, error) {
			c := newFakeMicCall()
			c.end(err)
			return c, nil
		}
	}
	for _, c := range []struct {
		name string
		open func() (target.MicCall, error)
		code string
	}{
		{"no grpc in the running file", func() (target.MicCall, error) {
			return nil, fmt.Errorf("%w: emulator-5558 was started without -grpc", emugrpc.ErrNoGRPC)
		}, "noGrpc"},
		{"emulator holds another feed", failed(&emugrpc.Status{Code: emugrpc.FailedPrecondition, Message: "There can be only one microphone active"}), "micBusy"},
		{"nothing listening", func() (target.MicCall, error) {
			return nil, fmt.Errorf("%w: connection refused", emugrpc.ErrConnection)
		}, "emulatorGone"},
		{"bad token", failed(&emugrpc.Status{Code: emugrpc.Unauthenticated}), "bad"},
		{"not running", func() (target.MicCall, error) {
			return nil, errors.New("pixel is not running; open it in the viewer to boot it")
		}, "bad"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, tr := micServer(t, &fakeMicTarget{id: "avd:x", open: c.open})
			_, ws := dialMic(t, srv, "target=avd:x", true)
			ws.wantError(t, c.code)
			if rec := micRecord(t, tr, 0); rec["outcome"] != "refused" || rec["code"] != c.code || rec["live"] != nil {
				t.Errorf("trace %v", rec)
			}
			// A refused socket leaves the slot free.
			_, again := dialMic(t, srv, "target=avd:x", true)
			if again.message(t).Code == "micBusy" && c.code != "micBusy" {
				t.Error("the refused socket kept the slot")
			}
		})
	}
}

func TestMicCallEndsUnderALiveSocket(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		code string
	}{
		{"connection died", fmt.Errorf("%w: EOF", emugrpc.ErrConnection), "emulatorGone"},
		{"emulator ended the call", &emugrpc.Status{Code: emugrpc.OK}, "emulatorGone"},
		{"refused a packet", &emugrpc.Status{Code: emugrpc.InvalidArgument, Message: "too big"}, "bad"},
	} {
		t.Run(c.name, func(t *testing.T) {
			avd := &fakeMicTarget{id: "avd:x"}
			srv, tr := micServer(t, avd)
			_, ws := dialMic(t, srv, "target=avd:x", true)
			ws.wantLive(t)
			call := avd.call(t, 0)
			call.end(c.err)
			ws.wantError(t, c.code)
			call.waitClosed(t)
			if n := call.halfClosed(); n != 0 {
				t.Errorf("%d half-closes of a call that had already ended", n)
			}
			if rec := micRecord(t, tr, 0); rec["outcome"] != "error" || rec["code"] != c.code || rec["live"] != true {
				t.Errorf("trace %v", rec)
			}
			_, again := dialMic(t, srv, "target=avd:x", true)
			again.wantLive(t)
		})
	}
}

// A gRPC write that does not return must not stop the socket being read:
// the queue keeps the newest 320 ms and drops the oldest, and closing the
// socket still ends the call.
func TestMicSlowSendDropsOldest(t *testing.T) {
	call := newFakeMicCall()
	call.block = true
	avd := &fakeMicTarget{id: "avd:x", open: func() (target.MicCall, error) { return call, nil }}
	srv, tr := micServer(t, avd)
	_, ws := dialMic(t, srv, "target=avd:x", true)
	ws.wantLive(t)

	ws.send(t, wsx.OpBinary, pcm(640))
	select {
	case <-call.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("the first packet never reached Send")
	}
	// 39 more while Send is stuck: 16 fit, 23 push older ones out.
	for i := 0; i < 39; i++ {
		ws.send(t, wsx.OpBinary, pcm(640))
	}
	ws.send(t, wsx.OpClose, nil)
	call.waitClosed(t)
	rec := micRecord(t, tr, 0)
	if rec["dropped"] != 23.0 || rec["packets"] != 0.0 || rec["outcome"] != "ended" {
		t.Errorf("trace %v, want 23 dropped, 0 sent", rec)
	}
	if got := call.packets(); len(got) != 1 || got[0].Format == nil {
		t.Errorf("Send got %d packets, want the first with its format", len(got))
	}
}

// A client that closes and reconnects at once gets the microphone once the
// first call has played out, not micBusy, and the two calls never overlap.
func TestMicReconnectWaitsForTheEndingCall(t *testing.T) {
	avd := &fakeMicTarget{id: "avd:x", playout: 300 * time.Millisecond}
	srv, _ := micServer(t, avd)
	_, first := dialMic(t, srv, "target=avd:x", true)
	first.wantLive(t)
	first.send(t, wsx.OpBinary, pcm(640))
	first.send(t, wsx.OpClose, nil)
	first.wantClosed(t)

	_, second := dialMic(t, srv, "target=avd:x", true)
	second.wantLive(t)
	avd.mu.Lock()
	overlapped := avd.overlapped
	avd.mu.Unlock()
	if overlapped {
		t.Error("the second call opened before the first closed")
	}
	if n := avd.call(t, 0).halfClosed(); n != 1 {
		t.Errorf("first call: %d half-closes, want 1", n)
	}
}

func TestMicSlots(t *testing.T) {
	var m micSlots
	a, ok := m.take("x", time.Second)
	if !ok {
		t.Fatal("first take refused")
	}
	// Live: refused without waiting.
	began := time.Now()
	if _, ok := m.take("x", time.Second); ok || time.Since(began) > 100*time.Millisecond {
		t.Fatalf("take of a live slot: ok %v after %v", ok, time.Since(began))
	}
	// Ending: waited for, up to the wait.
	m.end(a)
	if _, ok := m.take("x", 50*time.Millisecond); ok {
		t.Fatal("take of an ending slot that was never given succeeded")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		m.give("x", a)
	}()
	b, ok := m.take("x", time.Second)
	if !ok {
		t.Fatal("take after the ending slot was given refused")
	}
	m.give("x", b)
	if _, ok := m.take("x", 0); !ok {
		t.Fatal("take after give refused")
	}
}

func TestMicChunker(t *testing.T) {
	var c micChunker
	in := pcm(5*640 + 3)
	var got []emugrpc.AudioPacket
	for _, n := range []int{639, 1, 1, 1280, 2, 1280} {
		got = append(got, c.add(in[:n])...)
		in = in[n:]
	}
	if len(in) != 0 {
		t.Fatalf("test feeds %d bytes too few", len(in))
	}
	if len(got) != 5 {
		t.Fatalf("%d packets, want 5", len(got))
	}
	all := pcm(5*640 + 3)
	for i, p := range got {
		if p.Timestamp != uint64(i)*20000 || !bytes.Equal(p.Audio, all[i*640:(i+1)*640]) {
			t.Errorf("packet %d: ts %d, audio match %v", i, p.Timestamp, bytes.Equal(p.Audio, all[i*640:(i+1)*640]))
		}
	}
	if len(c.buf) != 3 {
		t.Errorf("%d bytes held, want 3", len(c.buf))
	}
	// The 3 held bytes start the sixth packet.
	more := pcm(637)
	p := c.add(more)
	if len(p) != 1 || p[0].Timestamp != 100000 || !bytes.Equal(p[0].Audio, append(all[5*640:], more...)) {
		t.Errorf("sixth packet %+v", p)
	}
}

func TestMicQueueDropsOldest(t *testing.T) {
	q := newMicQueue(3)
	var drops int
	for i := 0; i < 5; i++ {
		if q.push(emugrpc.AudioPacket{Timestamp: uint64(i)}) {
			drops++
		}
	}
	if drops != 2 {
		t.Errorf("%d drops, want 2", drops)
	}
	for _, want := range []uint64{2, 3, 4} {
		if p := <-q.ch; p.Timestamp != want {
			t.Errorf("got %d, want %d", p.Timestamp, want)
		}
	}
}
