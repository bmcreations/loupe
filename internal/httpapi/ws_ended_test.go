package httpapi

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// A /ws open right after a shut down upgrades and answers MsgEnded, so a
// reconnect hears the same reason the original close gave.
func TestWSOpenAfterShutDownAnswersMsgEnded(t *testing.T) {
	for _, reason := range []string{"shutDown", "deleted"} {
		t.Run(reason, func(t *testing.T) { wsOpenAfterEnd(t, reason) })
	}
}

func wsOpenAfterEnd(t *testing.T, reason string) {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "avd:ws"})
	if _, err := m.Acquire(context.Background(), "avd:ws"); err != nil {
		t.Fatal(err)
	}
	m.EndSession("avd:ws", reason)
	srv := httptest.NewServer((&Server{mgr: m, token: "secret"}).Handler())
	defer srv.Close()

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(c, "GET /ws?target=avd:ws HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer secret\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q, want 101", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	hdr := make([]byte, 2) // server frames are unmasked; the payload is short
	if _, err := io.ReadFull(br, hdr); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, hdr[1]&0x7f)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(session.EndedMessage(reason)) {
		t.Errorf("frame = %q, want MsgEnded %s", payload, reason)
	}
}

// wsFirstByte opens /ws with the given query and returns the first byte of the
// first server frame: session.MsgEnded for a refusal, anything else for a stream.
func wsFirstByte(t *testing.T, srv *Server, query string) byte {
	t.Helper()
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	c, err := net.Dial("tcp", hs.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(c, "GET /ws?"+query+" HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer secret\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q, want 101", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		t.Fatal(err)
	}
	if hdr[1]&0x7f == 126 {
		if _, err := io.ReadFull(br, make([]byte, 2)); err != nil {
			t.Fatal(err)
		}
	}
	first := make([]byte, 1)
	if _, err := io.ReadFull(br, first); err != nil {
		t.Fatal(err)
	}
	return first[0]
}

func TestWSStartLiftsAShutDownGuardOnly(t *testing.T) {
	setup := func(reason string) *Server {
		m := NewManager(target.Options{})
		m.Register(&describingTarget{id: "avd:ws"})
		if _, err := m.Acquire(context.Background(), "avd:ws"); err != nil {
			t.Fatal(err)
		}
		// The shutdown has returned: start=1 may lift its grace.
		m.FinishEnded("avd:ws", m.EndSession("avd:ws", reason))
		return &Server{mgr: m, token: "secret"}
	}
	if b := wsFirstByte(t, setup("shutDown"), "target=avd:ws&start=1"); b != session.MsgInit {
		t.Errorf("start=1 after a shut down: first byte %#x, want MsgInit", b)
	}
	if b := wsFirstByte(t, setup("deleted"), "target=avd:ws&start=1"); b != session.MsgEnded {
		t.Errorf("start=1 after a delete: first byte %#x, want MsgEnded", b)
	}
	srv := setup("shutDown")
	if !srv.lifeBegin("avd:ws") {
		t.Fatal("lifeBegin")
	}
	if b := wsFirstByte(t, srv, "target=avd:ws&start=1"); b != session.MsgEnded {
		t.Errorf("start=1 with a lifecycle request in flight: first byte %#x, want MsgEnded", b)
	}
}

// A /ws open naming a protocol range the server cannot serve is refused with
// MsgEnded unsupportedProtocol, before the target is acquired.
func TestWSRefusesAnUnsupportedProtocolBeforeAcquiring(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "avd:ws"})
	srv := &Server{mgr: m, token: "secret"}
	if b := wsFirstByte(t, srv, "target=avd:ws&proto=2-3"); b != session.MsgEnded {
		t.Errorf("proto=2-3: first byte %#x, want MsgEnded", b)
	}
	m.mu.Lock()
	n := len(m.sessions)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("refused open left %d sessions, want 0", n)
	}
	for _, q := range []string{"target=avd:ws&proto=1", "target=avd:ws&proto=1-9", "target=avd:ws"} {
		if b := wsFirstByte(t, srv, q); b != session.MsgInit {
			t.Errorf("%s: first byte %#x, want MsgInit", q, b)
		}
	}
}
