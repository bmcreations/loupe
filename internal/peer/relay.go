package peer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/quic-go/quic-go"
)

// The hub reaches an owner over the raw-QUIC transport (session.QUICALPN),
// not /wt: /wt rotates its connection at 12 MiB for Safari
// (internal/httpapi/peerwindow.go), and a Go client has no rotation to
// follow. The client is the one cmd/wtprobe drives, as a session-protocol
// viewer of the owner.

// frameHeaderLen is the 13-byte header on each slot: id u32, pts u64, flags.
const frameHeaderLen = 4 + 8 + 1

const (
	// relaySlots is how many slots the client keeps open, as the native
	// client and wtprobe do.
	relaySlots = 32
	// maxFrameBytes bounds one frame read off a slot.
	maxFrameBytes = 64 << 20
	// frameGap is how long the relay waits for a frame id that has not
	// arrived while later ones have. Slots finish independently, so a large
	// frame can land after a small one behind it; an id still missing after
	// this was reset by the owner (abandoned) and is skipped.
	frameGap = 40 * time.Millisecond
	// maxPending skips a gap at once when this many later frames wait on it.
	maxPending = 32
	// dialTimeout bounds the QUIC handshake. The wait for Init is bounded by
	// the caller's context, since opening a target can boot a device.
	dialTimeout = 10 * time.Second
	initWait    = 3 * time.Minute
	// probeTimeout bounds the add-time protocol probe.
	probeTimeout = 5 * time.Second
	// probeTarget is a target id no owner has. A protocol the owner cannot
	// serve is refused before the target is looked up, so the probe learns
	// the verdict without opening anything.
	probeTarget = "_loupe-protocol-probe"
	// opSendTimeout bounds one control write.
	opSendTimeout = 2 * time.Second
	// clipSlots is how many of the owner's clipboard changes wait for the
	// hub's session. Past it the oldest goes: only the newest copy matters.
	clipSlots = 4
	// relayedSlots is how many of the owner's cursors and events wait for
	// the hub's session. Past it the newest is dropped, as a full viewer
	// queue drops one, rather than stall the read of the owner's Init.
	relayedSlots = 64
)

// UnsupportedProtocolError is the owner ending a connection with
// session.EndedUnsupportedProtocol: it speaks no version this server does.
type UnsupportedProtocolError struct {
	Server string
	Reason string
}

func (e *UnsupportedProtocolError) Error() string {
	return fmt.Sprintf("server %s cannot serve this server's protocol: %s", e.Server, e.Reason)
}

// quicDial connects to the owner's raw-QUIC listener, the same host and port
// as its HTTPS address, with the TLS config the HTTP client uses.
func quicDial(ctx context.Context, srv Server) (*quic.Conn, error) {
	u, err := url.Parse(srv.URL)
	if err != nil {
		return nil, err
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	cfg := TLSConfig(host, srv.Pin, nil)
	cfg.NextProtos = []string{session.QUICALPN}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	qc, err := quic.DialAddr(dctx, net.JoinHostPort(host, port), cfg, &quic.Config{
		InitialPacketSize: 1200,
		KeepAlivePeriod:   5 * time.Second,
	})
	if err != nil {
		if errors.Is(err, ErrCertChanged) || strings.Contains(err.Error(), ErrCertChanged.Error()) {
			return nil, fmt.Errorf("%w: %v", ErrCertChanged, err)
		}
		return nil, fmt.Errorf("%w: quic: %v", ErrServerDown, err)
	}
	return qc, nil
}

func controlMsg(b ...byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	return append(out, b...)
}

func readControl(r io.Reader) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n > 1<<20 {
		return nil, fmt.Errorf("control message of %d bytes exceeds limit", n)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

// openControl opens the control stream and sends the open message and Hello.
func openControl(ctx context.Context, qc *quic.Conn, srv Server, ownerID string) (*quic.Stream, error) {
	control, err := qc.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open control stream: %w", err)
	}
	open, _ := json.Marshal(session.QUICOpen{Target: ownerID, Token: srv.Token, Proto: "1"})
	buf := append([]byte{session.QUICStreamControl}, controlMsg(open...)...)
	buf = append(buf, controlMsg(session.MsgHello)...)
	if _, err := control.Write(buf); err != nil {
		return nil, fmt.Errorf("send open: %w", err)
	}
	return control, nil
}

// closeError turns a connection's end into the error the caller reports.
func closeError(server string, cause error) error {
	var ae *quic.ApplicationError
	if errors.As(cause, &ae) && ae.Remote {
		if uint64(ae.ErrorCode) == session.QUICCloseEnded && ae.ErrorMessage == session.EndedUnsupportedProtocol {
			return &UnsupportedProtocolError{Server: server, Reason: ae.ErrorMessage}
		}
		return fmt.Errorf("server %s ended the stream (code %d): %s", server, ae.ErrorCode, ae.ErrorMessage)
	}
	return fmt.Errorf("%w: %v", ErrServerDown, cause)
}

// ProbeProtocol opens a connection to the owner and reports whether it will
// serve this server's protocol version, without opening any target. It
// returns *UnsupportedProtocolError when the owner refuses the version; any
// other error means the probe could not tell (UDP blocked, say), and says
// why. A nil return means the owner got as far as looking the target up.
func ProbeProtocol(ctx context.Context, srv Server) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	qc, err := quicDial(ctx, srv)
	if err != nil {
		return err
	}
	defer qc.CloseWithError(0, "probe done")
	if _, err := openControl(ctx, qc, srv, probeTarget); err != nil {
		return err
	}
	select {
	case <-qc.Context().Done():
		cause := context.Cause(qc.Context())
		var ae *quic.ApplicationError
		if errors.As(cause, &ae) && ae.Remote && uint64(ae.ErrorCode) == session.QUICCloseAcquireFailed {
			return nil // past the protocol check: the target lookup failed
		}
		err := closeError(srv.Name, cause)
		var up *UnsupportedProtocolError
		if errors.As(err, &up) {
			return err
		}
		if errors.As(cause, &ae) && ae.Remote && uint64(ae.ErrorCode) == session.QUICCloseUnauthorized {
			return fmt.Errorf("server %s refused the token over QUIC", srv.Name)
		}
		return err
	case <-ctx.Done():
		return fmt.Errorf("no answer over QUIC within %v (is UDP blocked?)", probeTimeout)
	}
}

type rawFrame struct {
	id uint32
	f  target.Frame
}

// relayStream is a viewer of the owner's target, as a target.Stream.
type relayStream struct {
	server string
	qc     *quic.Conn
	ctl    *quic.Stream

	frames  chan target.Frame
	got     chan rawFrame
	ready   chan struct{} // closed when the first Init has been read
	clip    chan string   // the owner's MsgClipboard bodies
	relayed chan []byte   // the owner's MsgCursor and MsgEvent messages

	wmu sync.Mutex // control writes

	mu      sync.Mutex
	meta    target.Meta
	viewer  uint64
	control bool
	err     error
	closed  bool
}

// DialStream opens the owner's target as a viewer of the owner. It returns
// once the owner's Init has arrived, which is when Meta is known. opts are
// the hub's and do not reach the owner, which opens its target with its own.
func DialStream(ctx context.Context, srv Server, ownerID string, _ target.Options) (target.Stream, error) {
	qc, err := quicDial(ctx, srv)
	if err != nil {
		return nil, err
	}
	ctl, err := openControl(ctx, qc, srv, ownerID)
	if err != nil {
		_ = qc.CloseWithError(0, "")
		return nil, err
	}
	s := &relayStream{
		server: srv.Name, qc: qc, ctl: ctl,
		frames:  make(chan target.Frame, 16),
		got:     make(chan rawFrame, relaySlots),
		ready:   make(chan struct{}),
		clip:    make(chan string, clipSlots),
		relayed: make(chan []byte, relayedSlots),
	}
	go s.readControl()
	for range relaySlots {
		go s.slotLoop()
	}
	go s.order()

	wait := time.NewTimer(initWait)
	defer wait.Stop()
	select {
	case <-s.ready:
		return s, nil
	case <-qc.Context().Done():
		err := closeError(srv.Name, context.Cause(qc.Context()))
		_ = s.Close()
		return nil, err
	case <-ctx.Done():
		_ = s.Close()
		return nil, ctx.Err()
	case <-wait.C:
		_ = s.Close()
		return nil, fmt.Errorf("server %s sent no init within %v", srv.Name, initWait)
	}
}

// readControl reads the owner's control messages. Init sets Meta. The
// device's clipboard goes to ClipboardChanges, and cursors and events to
// RelayedControl, since a phone on the hub watches the same device. The rest
// (stats, pongs, control state) is the owner's account of its own link and
// viewers, and is dropped.
func (s *relayStream) readControl() {
	defer close(s.clip)
	defer close(s.relayed)
	var readyOnce sync.Once
	for {
		b, err := readControl(s.ctl)
		if err != nil {
			return // the connection's end is reported by order
		}
		if len(b) == 0 {
			continue
		}
		switch b[0] {
		case session.MsgClipboard:
			pushNewest(s.clip, string(b[1:]))
			continue
		case session.MsgCursor, session.MsgEvent:
			select {
			case s.relayed <- b:
			default:
			}
			continue
		case session.MsgInit:
		default:
			continue
		}
		var init session.InitPayload
		if json.Unmarshal(b[1:], &init) != nil {
			continue
		}
		s.mu.Lock()
		s.meta = target.Meta{Codec: fourcc(init.Codec), Width: init.CodedWidth, Height: init.CodedHeight, Name: init.Name}
		if init.Viewer != 0 {
			s.viewer = init.Viewer
		}
		s.control = init.Control
		s.mu.Unlock()
		readyOnce.Do(func() { close(s.ready) })
	}
}

// pushNewest queues v, dropping the oldest queued value when ch is full.
// Only readControl sends, so the loop ends once a slot is free.
func pushNewest(ch chan string, v string) {
	for {
		select {
		case ch <- v:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}

// fourcc inverts the owner's Init codec, an RFC 6381 string, to the name
// Meta.Codec carries.
func fourcc(rfc6381 string) string {
	switch {
	case strings.HasPrefix(rfc6381, "avc1"), strings.HasPrefix(rfc6381, "avc3"):
		return "h264"
	case strings.HasPrefix(rfc6381, "hvc1"), strings.HasPrefix(rfc6381, "hev1"):
		return "h265"
	case strings.HasPrefix(rfc6381, "av01"):
		return "av1"
	}
	return rfc6381
}

// slotLoop opens a slot, reads its one frame and opens the next, so
// relaySlots are always waiting on the owner.
func (s *relayStream) slotLoop() {
	ctx := s.qc.Context()
	for ctx.Err() == nil {
		st, err := s.qc.OpenStreamSync(ctx)
		if err != nil {
			return
		}
		if _, err := st.Write([]byte{session.QUICStreamSlot}); err != nil {
			return
		}
		_ = st.Close()
		b, err := io.ReadAll(io.LimitReader(st, maxFrameBytes))
		got := time.Now()
		// A reset slot is a frame the owner abandoned.
		if err != nil || len(b) < frameHeaderLen {
			continue
		}
		rf := rawFrame{
			id: binary.BigEndian.Uint32(b[0:4]),
			f: target.Frame{
				PTS:      time.Duration(binary.BigEndian.Uint64(b[4:12])) * time.Microsecond,
				Key:      b[12]&1 != 0,
				Config:   b[12]&2 != 0,
				Data:     b[frameHeaderLen:],
				Captured: got,
			},
		}
		select {
		case s.got <- rf:
		case <-ctx.Done():
			return
		}
	}
}

// order puts frames back in the owner's order, which independent slots do not
// keep, and closes Frames when the connection ends.
func (s *relayStream) order() {
	ctx := s.qc.Context()
	defer func() {
		s.mu.Lock()
		if !s.closed {
			s.err = closeError(s.server, context.Cause(ctx))
		}
		s.mu.Unlock()
		close(s.frames)
	}()
	next := uint32(1)
	pending := map[uint32]target.Frame{}
	gap := time.NewTimer(time.Hour)
	gap.Stop()
	armed := false
	emit := func() bool {
		for {
			f, ok := pending[next]
			if !ok {
				return true
			}
			delete(pending, next)
			next++
			select {
			case s.frames <- f:
			case <-ctx.Done():
				return false
			}
		}
	}
	skip := func() bool {
		first := true
		var lo uint32
		for id := range pending {
			if first || id < lo {
				lo, first = id, false
			}
		}
		if first {
			return true
		}
		next = lo
		return emit()
	}
	for {
		select {
		case rf := <-s.got:
			if rf.id < next {
				continue // behind a gap already skipped
			}
			pending[rf.id] = rf.f
			if !emit() {
				return
			}
			if len(pending) >= maxPending && !skip() {
				return
			}
			switch {
			case len(pending) == 0 && armed:
				gap.Stop()
				armed = false
			case len(pending) > 0 && !armed:
				gap.Reset(frameGap)
				armed = true
			}
		case <-gap.C:
			armed = false
			if !skip() {
				return
			}
			if len(pending) > 0 {
				gap.Reset(frameGap)
				armed = true
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *relayStream) Meta() target.Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta
}

func (s *relayStream) Frames() <-chan target.Frame { return s.frames }

// ClipboardChanges implements target.ClipboardWatcher with the owner's
// MsgClipboard, the owner device's clipboard each time it changes.
func (s *relayStream) ClipboardChanges() <-chan string { return s.clip }

// RelayedControl implements target.ControlRelayer.
func (s *relayStream) RelayedControl() <-chan []byte { return s.relayed }

// OwnerViewer is the viewer id the owner gave this relay in its Init.
func (s *relayStream) OwnerViewer() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewer
}

func (s *relayStream) send(b []byte) error {
	s.mu.Lock()
	ok := s.control
	s.mu.Unlock()
	if !ok {
		return target.ErrNoControl
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.ctl.SetWriteDeadline(time.Now().Add(opSendTimeout))
	_, err := s.ctl.Write(controlMsg(b...))
	return err
}

func (s *relayStream) Touch(t target.Touch) error {
	b := []byte{session.MsgTouch, byte(t.Action)}
	b = binary.BigEndian.AppendUint32(b, uint32(t.PointerID))
	b = binary.BigEndian.AppendUint32(b, uint32(t.X))
	b = binary.BigEndian.AppendUint32(b, uint32(t.Y))
	p := math.Min(1, math.Max(0, float64(t.Pressure)))
	b = binary.BigEndian.AppendUint16(b, uint16(math.Round(p*math.MaxUint16)))
	return s.send(b)
}

func (s *relayStream) Key(k target.Key) error {
	b := []byte{session.MsgKey, byte(k.Action)}
	b = binary.BigEndian.AppendUint32(b, uint32(k.Keycode))
	return s.send(b)
}

func (s *relayStream) Scroll(sc target.Scroll) error {
	b := []byte{session.MsgScroll}
	b = binary.BigEndian.AppendUint32(b, uint32(sc.X))
	b = binary.BigEndian.AppendUint32(b, uint32(sc.Y))
	b = binary.BigEndian.AppendUint32(b, math.Float32bits(sc.HScroll))
	b = binary.BigEndian.AppendUint32(b, math.Float32bits(sc.VScroll))
	return s.send(b)
}

// Text types s on the owner's target. A string past session.MaxTextLen is
// sent in pieces that split between runes.
func (s *relayStream) Text(text string) error {
	for text != "" {
		n := min(len(text), session.MaxTextLen)
		for n < len(text) && n > 0 && !isRuneStart(text[n]) {
			n--
		}
		if n == 0 {
			return session.ErrMalformed
		}
		if err := s.send(append([]byte{session.MsgText}, text[:n]...)); err != nil {
			return err
		}
		text = text[n:]
	}
	return nil
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// RequestKeyframe does nothing: the protocol has no message for it, and the
// owner sends keyframes on its own interval.
func (s *relayStream) RequestKeyframe() error { return nil }

func (s *relayStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *relayStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.qc.CloseWithError(0, "")
}

var _ target.PeerStream = (*relayStream)(nil)
var _ target.TextTyper = (*relayStream)(nil)
var _ target.ClipboardWatcher = (*relayStream)(nil)
var _ target.ControlRelayer = (*relayStream)(nil)
