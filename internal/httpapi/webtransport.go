package httpapi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/trace"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// WebTransport carries the stream over QUIC instead of TCP.
//
// The design question is what a "message" maps onto. Datagrams are the obvious
// reach, but a QUIC datagram must fit one packet and an access unit is tens of
// kilobytes, so datagrams would mean hand-rolling fragmentation, reassembly and
// partial-loss handling for every frame.
//
// One unidirectional stream per access unit gives all the properties we
// actually want, and QUIC already implements them:
//
//   - Streams are delivered independently, so a slow or lost frame cannot block
//     the frames behind it. That head-of-line blocking is the single defect
//     that made TCP wrong for this.
//   - Delivery within a stream is reliable and ordered, so a frame either
//     arrives whole or not at all. No partial-frame corruption to detect.
//   - A stream can be reset. A frame that goes stale mid-flight is cancelled
//     and stops consuming the link, which TCP simply cannot express: once bytes
//     are written they will be delivered however old they become.
//
// That last point is the whole reason for this transport. Everywhere else in
// this server, "drop the stale frame" is a decision made before sending and
// irrevocable afterwards. Here it stays available right up until the frame
// lands.
//
// Wire format per video stream:
//
//	u32 frameID · u64 ptsMicros · u8 flags · Annex B payload to end of stream
//
// Control runs over a single bidirectional stream, length-prefixed, carrying
// exactly the same semantic messages as the WebSocket transport.
const (
	wtFrameHeaderLen = 4 + 8 + 1
	wtFlagKey        = 1 << 0
	wtFlagConfig     = 1 << 1
)

// wtEncodeFrameHeader builds the 13-byte header SendFrame writes ahead of an
// access unit's payload on its own uni stream: frameID (u32), ptsMicros (u64),
// flags (u8). Pulled out of SendFrame so it has a call site a test can drive
// without a live QUIC stream -- see docs/protocol.md and
// testdata/protocol/frame-wt.hex.
func wtEncodeFrameHeader(id uint32, ptsMicros uint64, key, config bool) []byte {
	var flags byte
	if key {
		flags |= wtFlagKey
	}
	if config {
		flags |= wtFlagConfig
	}
	header := make([]byte, wtFrameHeaderLen)
	binary.BigEndian.PutUint32(header[0:], id)
	binary.BigEndian.PutUint64(header[4:], ptsMicros)
	header[12] = flags
	return header
}

// wtConn adapts a WebTransport session to the Conn interface.
//
// The frameID counter starts at 1 (the zero value plus the first increment)
// and advances on every attempted frame, including one that goes on to be
// abandoned -- the increment happens before the write that might fail. Uni
// streams can arrive at the client out of order, since each is delivered
// independently, so a gap in the frameID sequence the client observes is what
// tells it a frame was reset rather than merely reordered.
type wtConn struct {
	sess    *webtransport.Session
	control *webtransport.Stream
	// qconn is the QUIC connection underneath this session, recovered from the
	// request context. It is what makes PathRTT possible.
	qconn *quic.Conn

	mu      sync.Mutex
	frameID uint32

	readMu  sync.Mutex
	readBuf []byte

	// Send-path counters.
	//
	// SendFrame returns nil when it abandons a frame, so an abandoned frame is
	// invisible to every counter above this layer: the per-second record showed
	// framesOut 10, shed 8 and serverDrop 3 against a 60fps source, and the
	// missing two thirds were being written off here without a trace. These say
	// where they went, and separate the two ways this transport can refuse a
	// frame -- no stream to open, or a write that outran its deadline.
	stat wtStats

	// What this connection has spent of the peer's flow-control window, for
	// peerwindow.go: every byte handed to a stream's Write, including the
	// part of a frame written before it was reset, and every stream opened.
	// lastSent is when a frame was last delivered, in UnixNano, starting at
	// the connection's creation.
	spentBytes   atomic.Uint64
	spentStreams atomic.Uint64
	lastSent     atomic.Int64
	// starvedSince is when OpenUniStream first refused for want of credit,
	// in UnixNano, or zero once a stream opens without waiting. lastRecv is when the
	// client last sent on the control stream. A refusal is only seen when a
	// frame tries to open a stream, so this relies on the source sending
	// frames at rest, which synthetic, simcap's 250ms idle floor and scrcpy
	// all do.
	starvedSince atomic.Int64
	lastRecv     atomic.Int64
}

// wtStats is atomic because the read happens on the stats ticker while
// SendFrame is mid-flight on the same select loop. Nothing here is on the
// critical path; they are increments on counters already in cache.
type wtStats struct {
	sent       atomic.Uint64 // frames written whole and closed
	openErr    atomic.Uint64 // OpenUniStream refused: no stream credit
	headerErr  atomic.Uint64 // abandoned writing the 13-byte header
	payloadErr atomic.Uint64 // abandoned writing the access unit
	closeErr   atomic.Uint64 // written, but the close failed

	openNanos atomic.Uint64 // cumulative time inside OpenUniStream
	openMax   atomic.Uint64 // longest single OpenUniStream, reset each report
	writeMax  atomic.Uint64 // longest single frame write, reset each report

	// The send path blocks about one round trip at a time, which is what a
	// credit window refilling looks like rather than a link running out of
	// capacity. These size that window from the outside: the server's outgoing
	// budget is whatever the peer advertised in SETTINGS_WT_INITIAL_MAX_DATA,
	// topped up by WT_MAX_DATA capsules, and none of that is reachable through
	// webtransport-go's API. Bytes delivered between two stalls is the same
	// number measured from this side.
	bytes      atomic.Uint64 // header+payload bytes written
	writes     atomic.Uint64 // completed frame writes
	writeNanos atomic.Uint64 // cumulative time in frame writes

	// Keyframes and parameter sets are measured apart from everything else.
	// They are the frames the decoder cannot proceed without, they are much the
	// largest, and trap 35 was invisible partly because their write durations
	// were averaged in with inter-frames an order of magnitude smaller.
	keyWrites     atomic.Uint64 // completed keyframe/parameter-set writes
	keyWriteNanos atomic.Uint64 // cumulative time in those writes
	keyWriteMax   atomic.Uint64 // worst such write this report
	keyAbandon    atomic.Uint64 // keyframes reset at their deadline

	// The deadline runs from when a frame was queued, not from when its write
	// begins, so a keyframe could in principle spend its whole budget waiting
	// behind the frames in front of it. Measured on a 400 kbps shaped link, it
	// does not: this reads 0.0ms in most seconds and peaks at 336.8, while the
	// write itself reaches 1186.5. The budget goes on the write. Keep the
	// counter anyway -- it is what makes that an observation rather than an
	// assumption, and the two are indistinguishable without it.
	keyQueueMax atomic.Uint64 // worst queue wait before a keyframe write

	stalls        atomic.Uint64 // frame writes that took longer than stallFloor
	stallBytes    atomic.Uint64 // bytes written between the last two stalls
	stallBytesMin atomic.Uint64 // smallest such run this report, 0 if none
	sinceStall    atomic.Uint64 // bytes written since the last stall

	lastErr atomic.Value // string: most recent refusal, whatever kind
}

// stallFloor is what counts as a blocked write. The measured path baseline is
// around 150ms, so anything at this level is already an order of magnitude
// above a write that merely buffers, and well below one round trip.
const stallFloor = 50 * time.Millisecond

// noteKeyWrite records one completed keyframe or parameter-set write, beside
// the general counters rather than instead of them.
func (s *wtStats) noteKeyWrite(queued, d time.Duration) {
	s.keyWrites.Add(1)
	s.keyWriteNanos.Add(uint64(d))
	s.note(&s.keyWriteMax, d)
	s.note(&s.keyQueueMax, queued)
}

// noteWrite records one completed frame write and, when that write blocked,
// closes off the run of bytes that preceded it. The run length is the estimate
// of the peer's data window.
func (s *wtStats) noteWrite(n int, d time.Duration) {
	s.writes.Add(1)
	s.bytes.Add(uint64(n))
	s.writeNanos.Add(uint64(d))
	s.note(&s.writeMax, d)

	run := s.sinceStall.Add(uint64(n))
	if d < stallFloor {
		return
	}
	s.stalls.Add(1)
	s.sinceStall.Store(0)
	s.stallBytes.Store(run)
	for {
		cur := s.stallBytesMin.Load()
		if cur != 0 && cur <= run {
			return
		}
		if s.stallBytesMin.CompareAndSwap(cur, run) {
			return
		}
	}
}

// nextFrameHeader assigns the next frameID and builds the 13-byte header for
// f. SendFrame calls this directly, ahead of opening the stream it writes the
// header to, so a test can drive the exact assignment-and-encode path a live
// frame takes -- config flag included -- without a QUIC connection behind it.
// See testdata/protocol/frame-wt-config.hex.
func (c *wtConn) nextFrameHeader(f session.OutMsg) (id uint32, header []byte) {
	c.mu.Lock()
	c.frameID++
	id = c.frameID
	c.mu.Unlock()
	return id, wtEncodeFrameHeader(id, f.PTSMicros, f.Key, f.Config)
}

func (c *wtConn) Name() string { return "webtransport" }

// qconnOf is QUICConnFrom without the presence flag, for the call site that
// only cares whether there is a connection to ask.
func qconnOf(ctx context.Context) *quic.Conn {
	conn, _ := QUICConnFrom(ctx)
	return conn
}

// PathRTT reports the round trip QUIC has measured on this connection, or 0
// when there is no connection to ask or no sample yet.
//
// This exists because the client's own measurement arrives over the link it is
// describing. The report is an /api/log POST, so it queues behind the video the
// pacer is throttling, and it stops arriving precisely when the path is in
// trouble. Eight stale runs of three to seven seconds appear in 96 rows of the
// 07:36 trace, each one bracketing a congestion event, and in the worst of them
// the pacer re-read a three-second-old 111ms sample while shed climbed by 45
// frames and held itself wide open.
//
// QUIC already knows. SmoothedRTT is maintained per ACK by the transport,
// beneath anything that could queue it. Keep the client's number too: it is the
// only figure that includes the phone's own decode and render backlog, and the
// gap between the two is what tells a slow link from a slow client.
func (c *wtConn) PathRTT() time.Duration {
	if c.qconn == nil {
		return 0
	}
	return c.qconn.ConnectionStats().SmoothedRTT
}

// SendFrame opens a fresh unidirectional stream for one access unit and writes
// it there. If the frame outlives its deadline before the write completes, the
// stream is reset so the remaining bytes are never sent and the receiver
// discards what it has.
func (c *wtConn) SendFrame(f session.OutMsg, deadline time.Time) error {
	// OpenUniStream does not block: it refuses when the session has no stream
	// credit left. Timing it anyway is the cheap way to tell "credit was there"
	// from "credit arrived late", which a bare error count cannot.
	openStart := time.Now()
	stream, err := c.sess.OpenUniStream()
	waited := isStreamLimit(err)
	if waited {
		c.starvedSince.CompareAndSwap(0, openStart.UnixNano())
		// The peer grants stream credit as it reads, so running out means
		// the phone is behind, not that the session is over. This used to
		// return an error the viewer loop treated as fatal. Wait for credit
		// until the frame stops mattering.
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		stream, err = c.sess.OpenUniStreamSync(ctx)
		cancel()
	}
	c.stat.note(&c.stat.openMax, time.Since(openStart))
	c.stat.openNanos.Add(uint64(time.Since(openStart)))
	if err != nil {
		err = wtErr(err)
		c.stat.openErr.Add(1)
		c.stat.lastErr.Store("open: " + err.Error())
		if isStreamLimit(err) || errors.Is(err, context.DeadlineExceeded) {
			if f.Key || f.Config {
				c.stat.keyAbandon.Add(1)
			}
			return errFrameAbandoned
		}
		return fmt.Errorf("open frame stream: %w", err)
	}

	// Only an open that found credit waiting ends a stall. On the phone on
	// 12 October grants trickled in for 4-5s before they stopped, each
	// opening one late stream at 6-11fps, and clearing the stall on those
	// put off every switch until the trickle ended.
	if !waited {
		c.starvedSince.Store(0)
	}
	c.spentStreams.Add(1)
	c.spentBytes.Add(wtStreamPreamble)
	_, header := c.nextFrameHeader(f)

	// The deadline is what makes this transport different. QUIC will happily
	// keep retransmitting a frame that stopped mattering; this stops it.
	_ = stream.SetWriteDeadline(deadline)

	writeStart := time.Now()
	n, err := stream.Write(header)
	c.spentBytes.Add(uint64(n))
	if err != nil {
		c.stat.headerErr.Add(1)
		c.stat.lastErr.Store("header: " + wtErr(err).Error())
		c.stat.note(&c.stat.writeMax, time.Since(writeStart))
		if f.Key || f.Config {
			c.stat.keyAbandon.Add(1)
		}
		c.abandon(stream, f)
		return errFrameAbandoned
	}
	// Data carries the WebSocket envelope; this transport has its own header,
	// so send only the access unit.
	payload := f.Data
	if len(payload) > session.FrameHeaderLen {
		payload = payload[session.FrameHeaderLen:]
	}
	n, err = stream.Write(payload)
	c.spentBytes.Add(uint64(n))
	if err != nil {
		c.stat.payloadErr.Add(1)
		c.stat.lastErr.Store("payload: " + wtErr(err).Error())
		c.stat.note(&c.stat.writeMax, time.Since(writeStart))
		if f.Key || f.Config {
			c.stat.keyAbandon.Add(1)
		}
		c.abandon(stream, f)
		return errFrameAbandoned
	}
	took := time.Since(writeStart)
	c.stat.noteWrite(len(header)+len(payload), took)
	if f.Key || f.Config {
		c.stat.noteKeyWrite(writeStart.Sub(f.QueuedAt), took)
	}
	if err := stream.Close(); err != nil {
		// quic-go refuses to close a stream the peer has already stopped
		// with STOP_SENDING. That is one frame the phone gave up on, not a
		// dead session, so it is abandoned like a frame past its deadline.
		c.stat.closeErr.Add(1)
		c.stat.lastErr.Store("close: " + wtErr(err).Error())
		return errFrameAbandoned
	}
	c.stat.sent.Add(1)
	c.lastSent.Store(time.Now().UnixNano())
	return nil
}

// PeerWindow implements peerWindow.
func (c *wtConn) PeerWindow() peerSpend {
	p := peerSpend{
		Bytes:     c.spentBytes.Load(),
		Streams:   c.spentStreams.Load(),
		SinceSent: time.Since(time.Unix(0, c.lastSent.Load())),
		SinceRecv: time.Since(time.Unix(0, c.lastRecv.Load())),
	}
	if at := c.starvedSince.Load(); at != 0 {
		p.Starved = time.Since(time.Unix(0, at))
	}
	return p
}

// CloseExhausted implements peerWindow.
func (c *wtConn) CloseExhausted() error {
	return c.sess.CloseWithError(wtPeerWindowExhausted, "peer flow control exhausted")
}

// note keeps a running maximum. The maxima are reset by TransportStats, so each
// one describes the second it is reported in rather than the whole session; a
// cumulative maximum would be pinned by the first bad second forever.
func (s *wtStats) note(dst *atomic.Uint64, d time.Duration) {
	n := uint64(d)
	for {
		old := dst.Load()
		if n <= old || dst.CompareAndSwap(old, n) {
			return
		}
	}
}

// TransportStats reports the send path for one stats tick. viewer.go picks it
// up through an optional interface, so the Conn interface stays transport-blind.
func (c *wtConn) TransportStats() map[string]any {
	ms := func(n uint64) float64 {
		return float64(time.Duration(n).Microseconds()) / 1000
	}
	writes := c.stat.writes.Load()
	m := map[string]any{
		"sent":       c.stat.sent.Load(),
		"openErr":    c.stat.openErr.Load(),
		"headerErr":  c.stat.headerErr.Load(),
		"payloadErr": c.stat.payloadErr.Load(),
		"closeErr":   c.stat.closeErr.Load(),
		"openTotMs":  ms(c.stat.openNanos.Load()),
		"openMaxMs":  ms(c.stat.openMax.Swap(0)),
		"writeMaxMs": ms(c.stat.writeMax.Swap(0)),
		"bytes":      c.stat.bytes.Load(),
		"writes":     writes,
		"stalls":     c.stat.stalls.Load(),
	}
	// Mean is the number that says whether every write pays the round trip or
	// only the worst one each second does. writeMaxMs alone cannot tell those
	// apart, and they point at different layers.
	if writes > 0 {
		m["writeAvgMs"] = ms(c.stat.writeNanos.Load() / writes)
	}
	// Bytes delivered between two blocked writes: the peer's data window,
	// measured rather than read out of a library that does not expose it.
	// A keyframe abandoned at its deadline is the one loss nothing recovers
	// from without another keyframe, so it is reported on its own rather than
	// folded into payloadErr.
	if keys := c.stat.keyWrites.Load(); keys > 0 {
		m["keyWrites"] = keys
		m["keyWriteAvgMs"] = ms(c.stat.keyWriteNanos.Load() / keys)
	}
	if v := c.stat.keyWriteMax.Swap(0); v > 0 {
		m["keyWriteMaxMs"] = ms(v)
	}
	if v := c.stat.keyQueueMax.Swap(0); v > 0 {
		m["keyQueueMaxMs"] = ms(v)
	}
	if v := c.stat.keyAbandon.Load(); v > 0 {
		m["keyAbandon"] = v
	}
	if v := c.stat.stallBytes.Load(); v > 0 {
		m["stallBytes"] = v
	}
	if v := c.stat.stallBytesMin.Swap(0); v > 0 {
		m["stallBytesMin"] = v
	}
	if v, ok := c.stat.lastErr.Load().(string); ok && v != "" {
		m["lastErr"] = v
	}
	return m
}

// abandon resets a frame stream that could not be delivered in time.
//
// A failed frame is not a failed connection: the next frame gets its own
// stream and is unaffected, which is exactly the independence this transport
// exists for. Returning an error here would tear down a session over one late
// picture.
func (c *wtConn) abandon(stream *webtransport.SendStream, f session.OutMsg) {
	// Code 0: the frame expired. Nothing is wrong with the session.
	stream.CancelWrite(0)
	_ = f
}

// wtControlLimit caps a single control-stream message. Control messages --
// init, stats, pong, event, and the client's touch/key/scroll/ping/hello --
// are tens of bytes; anything near this cap on the way in is a bug or an
// attack, and an unbounded allocation from an attacker-chosen length prefix
// would be trivial to exploit.
const wtControlLimit = 1 << 20 // 1 MiB

// wtFrameControl prepends the u32 big-endian length prefix SendControl writes
// ahead of every control-stream message: a hello frames as `00000001 15`, and
// that pairing is what testdata/protocol/wt-control-hello.hex pins. Pulled
// out of SendControl so the framing has a call site a test can drive without
// a live stream.
func wtFrameControl(b []byte) []byte {
	framed := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(framed[:4], uint32(len(b)))
	copy(framed[4:], b)
	return framed
}

// wtReadControlFrame reads one length-prefixed message off r: the framing
// wtFrameControl writes, a u32 big-endian length followed by that many bytes,
// capped at wtControlLimit. buf is reused when it already has enough
// capacity; the (possibly new) buffer is returned alongside the message so
// the caller can keep reusing it across calls the way Recv does. Pulled out
// of Recv so the same decode a live control stream runs can be driven in a
// test against a bytes.Reader instead.
func wtReadControlFrame(r io.Reader, buf []byte) ([]byte, []byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, buf, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n > wtControlLimit {
		return nil, buf, fmt.Errorf("control message of %d bytes exceeds limit", n)
	}
	if cap(buf) < int(n) {
		buf = make([]byte, n)
	}
	out := buf[:n]
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, buf, err
	}
	return out, buf, nil
}

// SendControl writes a length-prefixed control message on the control
// stream: init, stats, pong and event all go out this way, the same framing
// the client's touch/key/scroll/ping/hello arrive in on Recv below.
func (c *wtConn) SendControl(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.control.Write(wtFrameControl(b))
	c.spentBytes.Add(uint64(n))
	return wtErr(err)
}

// Recv reads the next length-prefixed message from the control stream.
func (c *wtConn) Recv() ([]byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	b, buf, err := wtReadControlFrame(c.control, c.readBuf)
	c.readBuf = buf
	if err == nil {
		c.lastRecv.Store(time.Now().UnixNano())
	}
	return b, wtErr(err)
}

// Keepalive is a no-op: QUIC maintains its own connection liveness, so there
// is nothing here for an idle intermediary to reap.
func (c *wtConn) Keepalive() error { return nil }

func (c *wtConn) Close() error {
	return c.sess.CloseWithError(0, "")
}

// SetEndDeadline bounds writes on the control stream, so flushing the last
// control messages of an ended session cannot hang on a client that stopped
// reading.
func (c *wtConn) SetEndDeadline(t time.Time) {
	_ = c.control.SetWriteDeadline(t)
}

// CloseWithReason closes the session with a code and message the client can
// read. Close resets the control stream, which discards bytes not yet
// delivered, so a MsgEnded written just before it is usually lost; the close
// capsule is written before the stream's FIN and is not.
func (c *wtConn) CloseWithReason(code uint32, msg string) {
	_ = c.sess.CloseWithError(webtransport.SessionErrorCode(code), truncateRunes(msg, 256))
}

// wtEnded is the session close code for a session ended on purpose; the
// message is the reason ("shutDown" or "deleted"). The client must not
// reconnect, since that would boot the device just shut down. 1 is
// wtAcquireFailed and 2 belongs to another branch's reconnect backstop.
const wtEnded uint32 = 3

// wtAcquireFailed is the session close code for a target that could not be
// opened after the session was accepted, so the trace on either side can tell
// it from an ordinary hang-up, which closes with 0.
const wtAcquireFailed webtransport.SessionErrorCode = 1

// wtPeerWindowExhausted is the session close code for a connection that spent
// the peer's flow-control window and stopped delivering (peerwindow.go). The
// client reconnects at once rather than backing off.
const wtPeerWindowExhausted webtransport.SessionErrorCode = 2

// handleWebTransport upgrades a request to a WebTransport session and runs a
// viewer over it.
func (s *Server) handleWebTransport(w http.ResponseWriter, r *http.Request) {
	if s.wtServer == nil {
		http.Error(w, "webtransport not enabled", http.StatusNotFound)
		return
	}

	targetID := r.URL.Query().Get("target")
	if targetID == "" {
		http.Error(w, "missing ?target=", http.StatusBadRequest)
		return
	}
	_, protoErr := session.NegotiateProtocol(r.URL.Query().Get("proto"))
	if protoErr != nil {
		s.trace.Record(map[string]any{"kind": "wt-error", "stage": "protocol", "target": targetID, "err": protoErr.Error()})
	} else {
		attempt := map[string]any{"kind": "wt-attempt", "target": targetID, "ua": r.UserAgent()}
		if s.explicitStart(r, targetID) {
			attempt["start"] = true
		}
		s.trace.Record(attempt)
	}

	// Upgrade before acquiring, not after. Acquiring a shut-down AVD boots it,
	// which took 12s cold and up to 8.5s warm on a loaded Mac, and the client
	// gives the handshake 6s before it abandons WebTransport for WebSocket.
	// Answering CONNECT only once the target was up sent every such open to
	// WebSocket: acmechat_blockstore on 7 October at 22:37 and
	// Hinge_Pixel_Fold on 9 October at 11:58 both stalled "at ready" with
	// wt-attempt logged. The client now waits for init, not the handshake.
	wtSess, err := s.wtServer.Upgrade(w, r)
	if err != nil {
		s.trace.Record(map[string]any{"kind": "wt-error", "stage": "upgrade", "err": err.Error()})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Refused before acquiring, so a client the server cannot serve never
	// boots the target, and as an end so the client shows the reason rather
	// than retrying.
	if protoErr != nil {
		_ = wtSess.CloseWithError(webtransport.SessionErrorCode(wtEnded), session.EndedUnsupportedProtocol)
		return
	}

	// The client opens the control stream immediately after connecting. Bound
	// the wait so a client that connects and then does nothing cannot hold
	// this handler open.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Which stream directions does this client actually get? Safari has been
	// seen to leave createBidirectionalStream() pending forever on a session
	// whose `ready` resolved in under 200ms, and from here that is
	// indistinguishable from a client that cannot open any stream at all. This
	// opens one of each from the server side while the client reports what it
	// can open from its own, so a single page load separates the two.
	go diagnoseStreams(ctx, wtSess, s.trace)

	control, err := wtSess.AcceptStream(ctx)
	if err != nil {
		s.trace.Record(map[string]any{"kind": "wt-error", "stage": "control-stream", "err": err.Error()})
		_ = wtSess.CloseWithError(0, "no control stream")
		return
	}

	// A refusal now arrives as a closed session rather than a failed CONNECT.
	// The client reads the reason from /api/targets either way (explainFailure),
	// and it no longer retries the same refusal over WebSocket.
	sess, err := s.mgr.Acquire(r.Context(), targetID)
	if err != nil {
		s.trace.Record(map[string]any{"kind": "wt-error", "stage": "acquire", "err": err.Error()})
		if errors.Is(err, ErrJustShutDown) {
			// Say what the original close said, so a reconnect does not
			// read a shut down as a failed open.
			_ = wtSess.CloseWithError(webtransport.SessionErrorCode(wtEnded), endedReason(err))
			return
		}
		_ = wtSess.CloseWithError(wtAcquireFailed, truncateRunes(err.Error(), 256))
		return
	}

	s.trace.Record(map[string]any{"kind": "wt-open", "target": targetID})
	s.recordPath("webtransport", targetID, r.RemoteAddr)
	conn := &wtConn{
		sess:    wtSess,
		control: control,
		qconn:   qconnOf(r.Context()),
	}
	conn.lastSent.Store(time.Now().UnixNano())
	conn.lastRecv.Store(time.Now().UnixNano())
	s.serveViewer(targetID, sess, conn)
}

// diagnoseStreams opens one stream of each direction and records how long each
// took, so a client that cannot open streams can be told apart from one that
// cannot receive them.
//
// Both payloads are inert. Four bytes on a unidirectional stream is shorter
// than wtFrameHeaderLen, which the client's frame reader discards; four zero
// bytes on a bidirectional stream is a zero-length control message.
func diagnoseStreams(ctx context.Context, sess *webtransport.Session, tr *trace.Writer) {
	rec := func(dir string, start time.Time, err error) {
		m := map[string]any{
			"kind": "wt-diag", "dir": dir,
			"ms": time.Since(start).Milliseconds(),
		}
		if err != nil {
			m["err"] = err.Error()
		}
		tr.Record(m)
	}

	start := time.Now()
	uni, err := sess.OpenUniStreamSync(ctx)
	if err == nil {
		_, err = uni.Write([]byte{0, 0, 0, 0})
		_ = uni.Close()
	}
	rec("server-uni", start, err)

	start = time.Now()
	bidi, err := sess.OpenStreamSync(ctx)
	if err == nil {
		_, err = bidi.Write([]byte{0, 0, 0, 0})
	}
	rec("server-bidi", start, err)
}

// isStreamLimit reports whether err is a refusal to open a stream past the
// peer's credit. There are two limits and two types with the same message:
// webtransport-go's per-session WT_MAX_STREAMS, and quic-go's per-connection
// MAX_STREAMS, which webtransport-go passes through unwrapped. Matching only
// the first let "too many open streams" end a Acmechat simulator session on
// 26 September at 22:27:49. Both are returned as pointers.
func isStreamLimit(err error) bool {
	var wt *webtransport.StreamLimitReachedError
	var q *quic.StreamLimitReachedError
	return errors.As(err, &wt) || errors.As(err, &q)
}

// sessionClosedError names a WebTransport session close that carried no
// message. webtransport-go's SessionError.Error() is its Message alone, and
// both wtConn.Close and Safari close with an empty one, so every error a
// closed session hands back read as "" -- ten viewer-stop records in the
// 28 September 07:05 trace said only `open frame stream: `, with nothing to
// tell the phone hanging up from this server closing first. The side and the
// code are what the message leaves out.
type sessionClosedError struct {
	*webtransport.SessionError
}

func (e sessionClosedError) Error() string {
	by := "locally"
	if e.Remote {
		by = "by the peer"
	}
	return fmt.Sprintf("session closed %s, code %d", by, e.ErrorCode)
}

func (e sessionClosedError) Unwrap() error { return e.SessionError }

// wtErr gives a session error with no message its side and code. Every error
// wtConn returns goes through it, because once a session closes, opens,
// writes and reads all return that same SessionError.
func wtErr(err error) error {
	var named sessionClosedError
	if errors.As(err, &named) {
		return err
	}
	var se *webtransport.SessionError
	if errors.As(err, &se) && se.Message == "" {
		if _, bare := err.(*webtransport.SessionError); bare {
			return sessionClosedError{se}
		}
		// Wrapped: keep the wrapper's type in errType and say what closed.
		return fmt.Errorf("%w (%s)", err, sessionClosedError{se}.Error())
	}
	return err
}
