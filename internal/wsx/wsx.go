// Package wsx is a minimal RFC 6455 WebSocket server implementation.
//
// It exists so the binary has no external dependencies, which is the whole
// reason Go was chosen for this server: a single static file to drop on a
// machine inside someone else's network, with nothing to vendor or audit.
// It implements exactly the subset this application needs, namely a server
// endpoint speaking binary and text frames with ping/pong keepalive.
//
// It deliberately does NOT implement: extensions (permessage-deflate),
// client-side dialing, or subprotocol negotiation.
package wsx

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// guid is the fixed value RFC 6455 specifies for the handshake accept hash.
const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes.
const (
	OpContinuation byte = 0x0
	OpText         byte = 0x1
	OpBinary       byte = 0x2
	OpClose        byte = 0x8
	OpPing         byte = 0x9
	OpPong         byte = 0xA
)

// sndBufBytes caps the kernel send buffer per connection. See the rationale in
// Upgrade: this is a latency control, not a throughput one. Too small and
// throughput suffers on a fast link; 64 KiB is comfortably above the
// bandwidth-delay product of a LAN or a typical WAN path while still bounding
// buffered latency to a fraction of a second on a slow one.
const sndBufBytes = 64 * 1024

// maxMessageSize bounds a single inbound message. Control messages from the
// client are tens of bytes; anything approaching this cap is a bug or an
// attack, and unbounded growth here would be a trivial memory exhaustion.
const maxMessageSize = 1 << 20

// ErrClosed is returned once the connection has been closed.
var ErrClosed = errors.New("wsx: connection closed")

// Conn is a WebSocket connection. Write methods are safe for concurrent use.
// Read must be called from a single goroutine.
type Conn struct {
	raw net.Conn
	br  *bufio.Reader

	writeMu sync.Mutex

	closeOnce sync.Once
	closedCh  chan struct{}
}

// Upgrade completes the WebSocket handshake and takes ownership of the
// connection. The caller must Close the returned Conn.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("wsx: not a websocket upgrade request")
	}
	if !headerContainsToken(r.Header.Get("Connection"), "upgrade") {
		return nil, errors.New("wsx: missing Connection: Upgrade")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, fmt.Errorf("wsx: unsupported websocket version %q", r.Header.Get("Sec-WebSocket-Version"))
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("wsx: missing Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("wsx: ResponseWriter does not support hijacking")
	}
	raw, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("wsx: hijack: %w", err)
	}

	accept := acceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		_ = raw.Close()
		return nil, err
	}

	if tcp, ok := raw.(*net.TCPConn); ok {
		// Video frames are latency sensitive and already well above MSS;
		// Nagle only adds delay here.
		_ = tcp.SetNoDelay(true)

		// Cap the kernel send buffer. This is the difference between a stream
		// that degrades and one that drowns.
		//
		// The OS sizes SO_SNDBUF for throughput, so by default it will happily
		// accept megabytes of video that the link has not sent yet. Write()
		// returns immediately, the application believes the frame is delivered,
		// and the data sits in the kernel for as long as the link needs. On a
		// slow path that is seconds of latency accumulated somewhere no
		// application-level queue check can observe, and it cannot be undone:
		// once bytes are in the socket they are committed.
		//
		// A small buffer makes Write() block as soon as the link is saturated,
		// which is exactly when the staleness check upstream should start
		// shedding. Worst-case buffered latency becomes sndBufBytes/link_rate:
		// at 64 KiB and 1 Mbps that is half a second instead of seven.
		_ = tcp.SetWriteBuffer(sndBufBytes)
	}

	return &Conn{
		raw:      raw,
		br:       brw.Reader,
		closedCh: make(chan struct{}),
	}, nil
}

func acceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+guid)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func headerContainsToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// Closed returns a channel that is closed when the connection shuts down.
func (c *Conn) Closed() <-chan struct{} { return c.closedCh }

// WriteBinary sends one binary message.
func (c *Conn) WriteBinary(payload []byte) error { return c.write(OpBinary, payload) }

// WriteText sends one text message.
func (c *Conn) WriteText(payload []byte) error { return c.write(OpText, payload) }

// WritePing sends a ping frame.
func (c *Conn) WritePing() error { return c.write(OpPing, nil) }

func (c *Conn) write(opcode byte, payload []byte) error {
	select {
	case <-c.closedCh:
		return ErrClosed
	default:
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	var header [10]byte
	header[0] = 0x80 | opcode // FIN set: this application never fragments output
	n := 2
	switch l := len(payload); {
	case l < 126:
		header[1] = byte(l)
	case l <= 0xFFFF:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:], uint16(l))
		n = 4
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:], uint64(l))
		n = 10
	}

	// A stalled client must not wedge the stream goroutine forever. Dropping
	// the connection is the right outcome: the client will reconnect and
	// resynchronize on the next keyframe.
	if err := c.raw.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if _, err := c.raw.Write(header[:n]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.raw.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// Read returns the next data message, transparently handling ping, pong and
// close control frames and reassembling fragmented messages.
func (c *Conn) Read() (opcode byte, payload []byte, err error) {
	for {
		fin, op, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch op {
		case OpPing:
			if err := c.write(OpPong, data); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.Close()
			return 0, nil, io.EOF
		}

		if fin {
			return op, data, nil
		}

		// Fragmented message: accumulate continuation frames.
		msg := data
		msgOp := op
		for {
			fin, op, data, err := c.readFrame()
			if err != nil {
				return 0, nil, err
			}
			if op == OpPing {
				if err := c.write(OpPong, data); err != nil {
					return 0, nil, err
				}
				continue
			}
			if op == OpPong {
				continue
			}
			if op == OpClose {
				_ = c.Close()
				return 0, nil, io.EOF
			}
			if op != OpContinuation {
				return 0, nil, errors.New("wsx: expected continuation frame")
			}
			msg = append(msg, data...)
			if len(msg) > maxMessageSize {
				return 0, nil, errors.New("wsx: message exceeds size limit")
			}
			if fin {
				return msgOp, msg, nil
			}
		}
	}
}

func (c *Conn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}

	fin = head[0]&0x80 != 0
	opcode = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}

	if length > maxMessageSize {
		return false, 0, nil, fmt.Errorf("wsx: frame of %d bytes exceeds limit", length)
	}

	// RFC 6455 requires client frames to be masked. Accepting unmasked frames
	// would make this endpoint usable as a cache-poisoning gadget.
	if !masked {
		return false, 0, nil, errors.New("wsx: client frame was not masked")
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return false, 0, nil, err
	}

	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return fin, opcode, payload, nil
}

// Close shuts down the connection. It is safe to call more than once.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closedCh)
		// Best-effort close frame; the peer may already be gone.
		c.writeMu.Lock()
		_ = c.raw.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = c.raw.Write([]byte{0x88, 0x00})
		c.writeMu.Unlock()
		_ = c.raw.Close()
	})
	return nil
}
