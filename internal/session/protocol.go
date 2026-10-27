// Package session owns the live streaming sessions and the binary protocol
// spoken between this server and a browser client.
//
// The protocol is deliberately semantic rather than a scrcpy passthrough: the
// client sends "finger 2 moved to x,y", not scrcpy control bytes. That keeps
// the browser ignorant of which backend is driving the session, which is what
// lets the same client drive a phone, an emulator or a test pattern, and what
// will let a native iOS client reuse this server unchanged.
package session

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ProtocolVersion is the newest session protocol this server speaks, and
// MinProtocolVersion the oldest it still serves. A client names the range it
// speaks on connect (?proto= on /ws and /wt, QUICOpen.Proto on raw QUIC) and
// the server refuses one it shares no version with, with the ended reason
// EndedUnsupportedProtocol, before it acquires the target. Bump
// ProtocolVersion for any change an older client would misread; raise
// MinProtocolVersion only once no client in use still needs the old one, since
// an installed app stays on a phone for months.
const (
	ProtocolVersion    = 1
	MinProtocolVersion = 1
)

// EndedUnsupportedProtocol is the ended reason for a client whose protocol
// range the server cannot serve. It travels the way every ended reason does:
// MsgEnded on /ws, close code 3 on /wt, QUICCloseEnded on raw QUIC.
const EndedUnsupportedProtocol = "unsupportedProtocol"

// NegotiateProtocol picks the version to speak with a client that sent spec,
// either one version ("1") or an inclusive range ("1-3"). An empty spec is a
// client from before negotiation existed, which speaks version 1. The result
// is the newest version both sides speak.
func NegotiateProtocol(spec string) (int, error) {
	lo, hi := 1, 1
	if spec != "" {
		a, b, isRange := strings.Cut(spec, "-")
		var err error
		if lo, err = strconv.Atoi(a); err != nil || lo < 1 {
			return 0, fmt.Errorf("bad protocol range %q", spec)
		}
		hi = lo
		if isRange {
			if hi, err = strconv.Atoi(b); err != nil || hi < lo {
				return 0, fmt.Errorf("bad protocol range %q", spec)
			}
		}
	}
	v := min(hi, ProtocolVersion)
	if v < max(lo, MinProtocolVersion) {
		return 0, fmt.Errorf("client speaks protocol %d-%d, server speaks %d-%d", lo, hi, MinProtocolVersion, ProtocolVersion)
	}
	return v, nil
}

// Server-to-client message types.
const (
	MsgInit  byte = 0x01
	MsgFrame byte = 0x02
	MsgStats byte = 0x03
	MsgPong  byte = 0x04
	MsgError byte = 0x05
	// MsgEvent carries one completed gesture, from a viewer's touches or from
	// /api/input, as JSON. See internal/session/events.go.
	MsgEvent byte = 0x06
	// MsgClipboard carries the device's clipboard, as UTF-8 with no length
	// prefix, each time something is copied on the device. See
	// Session.watchClipboard.
	MsgClipboard byte = 0x07
	// MsgControl carries who holds the target against agents and what the
	// most recent agent is doing, as JSON (ControlState). Sent on every
	// change and once to a joining viewer. See arbiter.go.
	MsgControl byte = 0x08
	// MsgRotate asks the client to open a second WebTransport connection
	// and move to it, because this one is nearing a flow-control limit the
	// browser will never raise (trap 52). JSON: {bytes, streams}, the
	// counts that prompted it. See internal/httpapi/peerwindow.go.
	MsgRotate byte = 0x09
	// MsgEnded tells a viewer the session is ending on purpose and why, so it
	// does not reconnect: a reconnect opens the target, which boots a device
	// that was just shut down. JSON {"reason":"shutDown"|"deleted"}.
	MsgEnded byte = 0x0A
	// MsgCursor carries where an agent is pointing, from /api/input's move
	// action, as Event JSON with kind "move" and no seq. It is its own type
	// rather than a MsgEvent because a move is not history: it skips the
	// replay ring, the event observer and the event log, and its seq-less
	// body would read as a gap there. See Session.PublishCursor.
	MsgCursor byte = 0x0B
	// MsgOverlay carries every overlay an extension has put on the target,
	// as JSON {"overlays":[...]}: the whole set each time, so a client
	// replaces what it drew rather than patching it, and an empty list
	// clears. Kept on the session and sent to each joiner after init. The
	// images themselves are fetched over HTTP, never sent on this stream,
	// so a large PNG cannot spend a WebTransport window (trap 51). See
	// Session.SetOverlays and internal/httpapi/overlay.go.
	MsgOverlay byte = 0x0C
)

// Client-to-server message types.
const (
	MsgTouch  byte = 0x10
	MsgKey    byte = 0x11
	MsgScroll byte = 0x12
	MsgPing   byte = 0x13
	// MsgClientStats carries the metrics only the client can measure, as JSON.
	MsgClientStats byte = 0x14
	// MsgHello is sent by the client immediately after opening its control
	// stream, and carries nothing.
	//
	// QUIC creates streams lazily: opening one is a local operation, and the
	// peer learns about it only when the first bytes arrive. A server waiting
	// to accept the control stream therefore waits forever behind a client
	// that has opened it and has nothing to say yet. This is the first byte
	// that makes the stream real.
	MsgHello byte = 0x15
	// MsgText carries text a viewer typed, as UTF-8 with no length prefix:
	// the message boundary is the end of the string. See Session.ViewerText.
	MsgText byte = 0x16
	// MsgTakeOver is the banner's Take over, with no body: the viewer holds
	// the target against agents and nothing reaches the device. Every other
	// message that counts the person also acts on the device. See
	// Session.ViewerTakeOver.
	MsgTakeOver byte = 0x17
)

// The raw-QUIC transport, ALPN QUICALPN on the same UDP port as /wt, for a
// native client: Network.framework has QUIC but no WebTransport. There is no
// URL, so what /wt reads from its query arrives in the open message instead.
//
// Every stream is client-opened and bidirectional, and its first byte says
// what it is. A server-opened stream per frame, which /wt uses, stalls on
// Network.framework: it never grants more stream credit than its initial
// limit, measured at 100 of 100 uni and 100 of 100 bidi streams. quic-go does
// grant credit for streams the client opens, so the client opens slots ahead
// of need and the server writes one frame on each (tools/nwquic).
const (
	QUICALPN = "loupe/1"

	// QUICStreamControl starts the control stream. After it come control
	// messages framed as on /wt (u32 length, then the message), the first of
	// them a QUICOpen as JSON with no opcode.
	QUICStreamControl byte = 0x18
	// QUICStreamSlot is a slot: this one byte, then FIN. The server writes a
	// 13-byte frame header and one access unit on it and closes it, or resets
	// it to abandon the frame. The client opens another as each one ends.
	QUICStreamSlot byte = 0x19
)

// QUICOpen is the control stream's first message.
type QUICOpen struct {
	Target string `json:"target"`
	Token  string `json:"token,omitempty"`
	// Start is /wt's start=1: an explicit Start, which lifts a shut-down
	// target's reopen guard.
	Start bool `json:"start,omitempty"`
	// Proto is the protocol range the client speaks, as ?proto= on /ws and
	// /wt. See NegotiateProtocol.
	Proto string `json:"proto,omitempty"`
}

// QUIC application close codes. 1 and 3 mean what they mean on /wt.
const (
	QUICCloseAcquireFailed uint64 = 1
	QUICCloseEnded         uint64 = 3
	QUICCloseUnauthorized  uint64 = 4
	QUICCloseBadOpen       uint64 = 5
)

// MaxTextLen bounds one MsgText payload in bytes. A phone keyboard sends a
// character or a word at a time; a paste larger than this is split by the
// client.
const MaxTextLen = 4096

// Frame header flags.
const (
	flagKey    byte = 1 << 0
	flagConfig byte = 1 << 1
)

// FrameHeaderLen is the fixed prefix on every MsgFrame payload.
const FrameHeaderLen = 1 + 1 + 8 + 8

// ErrMalformed indicates a client message that could not be decoded.
var ErrMalformed = errors.New("session: malformed client message")

// EncodeFrame writes a video frame message into dst, growing it as needed, and
// returns the result.
//
// sentAtMicros is the server's wall clock at send time. The client subtracts it
// from its own clock on arrival to show transport latency. The two clocks are
// not synchronized, so the absolute number is meaningless, but its variance is
// exactly what tells you whether the network is the problem.
func EncodeFrame(dst []byte, pts uint64, key, config bool, sentAtMicros uint64, data []byte) []byte {
	need := FrameHeaderLen + len(data)
	if cap(dst) < need {
		dst = make([]byte, need)
	}
	dst = dst[:need]

	dst[0] = MsgFrame
	var flags byte
	if key {
		flags |= flagKey
	}
	if config {
		flags |= flagConfig
	}
	dst[1] = flags
	binary.BigEndian.PutUint64(dst[2:], pts)
	binary.BigEndian.PutUint64(dst[10:], sentAtMicros)
	copy(dst[FrameHeaderLen:], data)
	return dst
}

// TouchEvent is a decoded pointer event from the client, in stream coordinates.
type TouchEvent struct {
	Action    uint8
	PointerID uint64
	X, Y      int32
	Pressure  float32
}

// DecodeTouch parses a MsgTouch payload, excluding the leading type byte.
func DecodeTouch(b []byte) (TouchEvent, error) {
	// action(1) + pointerId(4) + x(4) + y(4) + pressure(2)
	const want = 15
	if len(b) < want {
		return TouchEvent{}, ErrMalformed
	}
	return TouchEvent{
		Action:    b[0],
		PointerID: uint64(binary.BigEndian.Uint32(b[1:])),
		X:         int32(binary.BigEndian.Uint32(b[5:])),
		Y:         int32(binary.BigEndian.Uint32(b[9:])),
		Pressure:  float32(binary.BigEndian.Uint16(b[13:])) / math.MaxUint16,
	}, nil
}

// KeyEvent is a decoded key event from the client.
type KeyEvent struct {
	Action  uint8
	Keycode int32
}

// DecodeKey parses a MsgKey payload, excluding the leading type byte.
func DecodeKey(b []byte) (KeyEvent, error) {
	const want = 5
	if len(b) < want {
		return KeyEvent{}, ErrMalformed
	}
	return KeyEvent{
		Action:  b[0],
		Keycode: int32(binary.BigEndian.Uint32(b[1:])),
	}, nil
}

// DecodeText parses a MsgText payload, excluding the leading type byte.
func DecodeText(b []byte) (string, error) {
	if len(b) == 0 || len(b) > MaxTextLen || !utf8.Valid(b) {
		return "", ErrMalformed
	}
	return string(b), nil
}

// ScrollEvent is a decoded scroll gesture from the client.
type ScrollEvent struct {
	X, Y             int32
	HScroll, VScroll float32
}

// DecodeScroll parses a MsgScroll payload, excluding the leading type byte.
func DecodeScroll(b []byte) (ScrollEvent, error) {
	const want = 16
	if len(b) < want {
		return ScrollEvent{}, ErrMalformed
	}
	return ScrollEvent{
		X:       int32(binary.BigEndian.Uint32(b[0:])),
		Y:       int32(binary.BigEndian.Uint32(b[4:])),
		HScroll: math.Float32frombits(binary.BigEndian.Uint32(b[8:])),
		VScroll: math.Float32frombits(binary.BigEndian.Uint32(b[12:])),
	}, nil
}

// EncodePong builds a latency probe reply echoing the client's timestamp.
func EncodePong(clientMicros, serverMicros uint64) []byte {
	buf := make([]byte, 1+8+8)
	buf[0] = MsgPong
	binary.BigEndian.PutUint64(buf[1:], clientMicros)
	binary.BigEndian.PutUint64(buf[9:], serverMicros)
	return buf
}

// DecodePing parses a MsgPing payload, excluding the leading type byte.
func DecodePing(b []byte) (uint64, error) {
	if len(b) < 8 {
		return 0, ErrMalformed
	}
	return binary.BigEndian.Uint64(b), nil
}
