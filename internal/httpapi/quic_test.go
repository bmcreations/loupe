package httpapi

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/quic-go/quic-go"
)

// Network.framework's NWConnectionGroup opens a base stream it never writes
// to, and a reset of that stream fails the whole group: Handset on iOS dropped
// with EINVAL 10s into every session. A stream that stays silent past the
// deadline must be left open.
func TestQUICSilentStreamIsNotReset(t *testing.T) {
	defer func(d time.Duration) { quicStreamKindTimeout = d }(quicStreamKindTimeout)
	quicStreamKindTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ln, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{testCert(t)},
		NextProtos:   []string{session.QUICALPN},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	client, err := quic.DialAddr(ctx, ln.Addr().String(), &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{session.QUICALPN},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseWithError(0, "")
	server, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := &qConn{qc: server, slots: make(chan *quic.Stream, quicSlotBacklog)}
	ctlCh := make(chan *quic.Stream, 1)
	go c.acceptStreams(ctlCh)

	// Stream 0 is the silent base stream; opening stream 4 opens it on the
	// server too.
	silent, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	control, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Write([]byte{session.QUICStreamControl}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctlCh:
	case <-ctx.Done():
		t.Fatal("control stream never arrived")
	}

	time.Sleep(5 * quicStreamKindTimeout)
	_ = silent.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = silent.Read(make([]byte, 1))
	var se *quic.StreamError
	if errors.As(err, &se) {
		t.Fatalf("the silent stream was reset: %v", err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read on the silent stream = %v, want a read timeout", err)
	}
	if client.Context().Err() != nil {
		t.Fatalf("connection closed: %v", context.Cause(client.Context()))
	}
}
