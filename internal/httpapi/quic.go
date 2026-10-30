package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/quic-go/quic-go"
)

// quicSlotBacklog is how many slots one connection may hold unused. The client
// keeps 32 open; more than this is a client opening slots faster than frames
// use them, and each one past the limit is refused rather than queued.
const quicSlotBacklog = 64

// quicOpenTimeout bounds the wait for the control stream and its open message,
// as /wt bounds the wait for its control stream.
const quicOpenTimeout = 10 * time.Second

// ServeQUIC runs one connection that negotiated session.QUICALPN. It returns
// when the viewer ends.
func (s *Server) ServeQUIC(qc *quic.Conn) {
	// The listener is quic.ListenEarly, which hands the connection over before
	// the handshake completes. Nothing is read until it has, so nothing that
	// decides access rides on early data.
	select {
	case <-qc.HandshakeComplete():
	case <-qc.Context().Done():
		return
	}
	s.trace.Record(map[string]any{"kind": "quic-attempt", "remote": qc.RemoteAddr().String()})

	c := &qConn{qc: qc, slots: make(chan *quic.Stream, quicSlotBacklog)}
	ctlCh := make(chan *quic.Stream, 1)
	go c.acceptStreams(ctlCh)

	ctx, cancel := context.WithTimeout(qc.Context(), quicOpenTimeout)
	defer cancel()
	var control *quic.Stream
	select {
	case control = <-ctlCh:
	case <-ctx.Done():
		s.trace.Record(map[string]any{"kind": "quic-error", "stage": "control-stream", "err": ctx.Err().Error()})
		_ = qc.CloseWithError(quic.ApplicationErrorCode(session.QUICCloseBadOpen), "no control stream")
		return
	}
	c.control = control

	_ = control.SetReadDeadline(time.Now().Add(quicOpenTimeout))
	b, buf, err := wtReadControlFrame(control, nil)
	c.readBuf = buf
	_ = control.SetReadDeadline(time.Time{})
	var open session.QUICOpen
	if err == nil {
		err = json.Unmarshal(b, &open)
	}
	if err == nil && open.Target == "" {
		err = errors.New("open names no target")
	}
	if err != nil {
		s.trace.Record(map[string]any{"kind": "quic-error", "stage": "open", "err": err.Error()})
		_ = qc.CloseWithError(quic.ApplicationErrorCode(session.QUICCloseBadOpen), truncateRunes(err.Error(), 256))
		return
	}
	// The token is checked here rather than by the authed middleware, which
	// this path never passes through. An empty server token is -no-auth. A
	// paired device sends its lsn_ session, as it would as a Bearer.
	if s.token != "" && !s.tokenMatches(open.Token) && s.sessionFromToken(open.Token) == nil {
		s.trace.Record(map[string]any{"kind": "quic-error", "stage": "auth", "target": open.Target})
		_ = qc.CloseWithError(quic.ApplicationErrorCode(session.QUICCloseUnauthorized), "unauthorized")
		return
	}
	if _, err := session.NegotiateProtocol(open.Proto); err != nil {
		s.trace.Record(map[string]any{"kind": "quic-error", "stage": "protocol", "target": open.Target, "err": err.Error()})
		_ = qc.CloseWithError(quic.ApplicationErrorCode(session.QUICCloseEnded), session.EndedUnsupportedProtocol)
		return
	}

	attempt := map[string]any{"kind": "quic-open-attempt", "target": open.Target}
	if s.startTarget(open.Start, open.Target) {
		attempt["start"] = true
	}
	s.trace.Record(attempt)

	sess, err := s.mgr.Acquire(qc.Context(), open.Target)
	if err != nil {
		s.trace.Record(map[string]any{"kind": "quic-error", "stage": "acquire", "err": err.Error()})
		if errors.Is(err, ErrJustShutDown) {
			_ = qc.CloseWithError(quic.ApplicationErrorCode(session.QUICCloseEnded), endedReason(err))
			return
		}
		_ = qc.CloseWithError(quic.ApplicationErrorCode(session.QUICCloseAcquireFailed), truncateRunes(err.Error(), 256))
		return
	}

	s.trace.Record(map[string]any{"kind": "quic-open", "target": open.Target})
	s.recordPath("quic", open.Target, qc.RemoteAddr().String())
	s.serveViewer(open.Target, sess, c)
}

// qConn is the raw-QUIC transport. Frames go one per client-opened slot, with
// /wt's 13-byte header, and control messages are framed as on /wt.
type qConn struct {
	qc      *quic.Conn
	control *quic.Stream
	slots   chan *quic.Stream

	mu      sync.Mutex // control writes
	frameID uint32     // SendFrame's alone; the viewer loop is its one caller

	readMu  sync.Mutex
	readBuf []byte

	sent      atomic.Uint64 // frames written whole and closed
	abandoned atomic.Uint64 // frames reset at their deadline
	starved   atomic.Uint64 // frames that found no slot before their deadline
	refused   atomic.Uint64 // slots refused past quicSlotBacklog
	bytes     atomic.Uint64 // header+payload bytes written
	slotWait  atomic.Uint64 // longest wait for a slot, reset each report
	lastErr   atomic.Value  // string
}

// acceptStreams sorts the client's streams by their first byte: the first
// control stream goes to ctlCh, and slots queue for SendFrame. It ends with the
// connection.
func (c *qConn) acceptStreams(ctlCh chan<- *quic.Stream) {
	ctx := c.qc.Context()
	for {
		st, err := c.qc.AcceptStream(ctx)
		if err != nil {
			return
		}
		go func() {
			// A stream the client opened and has not yet written to may not
			// exist here yet, but one that arrived has its first byte on the
			// way. The deadline keeps a silent stream from holding a goroutine.
			_ = st.SetReadDeadline(time.Now().Add(quicOpenTimeout))
			var kind [1]byte
			_, err := io.ReadFull(st, kind[:])
			_ = st.SetReadDeadline(time.Time{})
			if err != nil {
				st.CancelRead(0)
				st.CancelWrite(0)
				return
			}
			switch kind[0] {
			case session.QUICStreamSlot:
				select {
				case c.slots <- st:
				default:
					c.refused.Add(1)
					st.CancelRead(0)
					st.CancelWrite(0)
				}
			case session.QUICStreamControl:
				// Only the first one counts; a second is refused.
				select {
				case ctlCh <- st:
					return
				default:
				}
				fallthrough
			default:
				st.CancelRead(0)
				st.CancelWrite(0)
			}
		}()
	}
}

func (c *qConn) Name() string { return "quic" }

func (c *qConn) PathRTT() time.Duration { return c.qc.ConnectionStats().SmoothedRTT }

// SendFrame writes f on the next slot. With no slot ready it waits until the
// deadline, as wtConn waits for stream credit: a client that has not returned
// slots is a client that has not read its frames, which is the backpressure
// the pacer acts on.
func (c *qConn) SendFrame(f session.OutMsg, deadline time.Time) error {
	var st *quic.Stream
	select {
	case st = <-c.slots:
	default:
		waitStart := time.Now()
		t := time.NewTimer(time.Until(deadline))
		select {
		case st = <-c.slots:
			t.Stop()
		case <-t.C:
			c.starved.Add(1)
			c.lastErr.Store("no slot before the deadline")
			return errFrameAbandoned
		case <-c.qc.Context().Done():
			t.Stop()
			return fmt.Errorf("frame slot: %w", context.Cause(c.qc.Context()))
		}
		noteMax(&c.slotWait, time.Since(waitStart))
	}

	c.frameID++
	header := wtEncodeFrameHeader(c.frameID, f.PTSMicros, f.Key, f.Config)
	payload := f.Data
	if len(payload) > session.FrameHeaderLen {
		payload = payload[session.FrameHeaderLen:]
	}

	_ = st.SetWriteDeadline(deadline)
	if _, err := st.Write(header); err != nil {
		return c.abandon(st, "header", err)
	}
	if _, err := st.Write(payload); err != nil {
		return c.abandon(st, "payload", err)
	}
	if err := st.Close(); err != nil {
		// The client stopped reading this slot: one frame it gave up on.
		c.lastErr.Store("close: " + err.Error())
		return errFrameAbandoned
	}
	c.bytes.Add(uint64(len(header) + len(payload)))
	c.sent.Add(1)
	return nil
}

func (c *qConn) abandon(st *quic.Stream, stage string, err error) error {
	st.CancelWrite(0)
	c.abandoned.Add(1)
	c.lastErr.Store(stage + ": " + err.Error())
	if c.qc.Context().Err() != nil {
		return fmt.Errorf("frame %s: %w", stage, err)
	}
	return errFrameAbandoned
}

func (c *qConn) SendControl(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.control.Write(wtFrameControl(b))
	return err
}

func (c *qConn) Recv() ([]byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	b, buf, err := wtReadControlFrame(c.control, c.readBuf)
	c.readBuf = buf
	return b, err
}

// Keepalive is QUIC's own: the client sets an idle timeout and the transport
// keeps the path alive.
func (c *qConn) Keepalive() error { return nil }

func (c *qConn) Close() error { return c.qc.CloseWithError(0, "") }

func (c *qConn) SetEndDeadline(t time.Time) { _ = c.control.SetWriteDeadline(t) }

func (c *qConn) CloseWithReason(code uint32, msg string) {
	_ = c.qc.CloseWithError(quic.ApplicationErrorCode(code), truncateRunes(msg, 256))
}

func (c *qConn) TransportStats() map[string]any {
	m := map[string]any{
		"sent":      c.sent.Load(),
		"abandoned": c.abandoned.Load(),
		"starved":   c.starved.Load(),
		"bytes":     c.bytes.Load(),
		"slotsHeld": len(c.slots),
	}
	if v := c.refused.Load(); v > 0 {
		m["slotsRefused"] = v
	}
	if v := c.slotWait.Swap(0); v > 0 {
		m["slotWaitMaxMs"] = float64(v) / 1e6
	}
	if v, ok := c.lastErr.Load().(string); ok {
		m["lastErr"] = v
	}
	return m
}

func noteMax(dst *atomic.Uint64, d time.Duration) {
	n := uint64(d)
	for {
		old := dst.Load()
		if n <= old || dst.CompareAndSwap(old, n) {
			return
		}
	}
}

// QUICALPN is session.QUICALPN, for the startup banner.
const QUICALPN = session.QUICALPN
