package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/emugrpc"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/wsx"
)

// MicMessage is a text frame GET /api/mic sends after the upgrade:
// {"state":"live"} once the emulator's microphone call is open, or
// {"error":..., "code":...} just before loupe closes the socket. code is one
// of notAvd, noGrpc, micBusy, emulatorGone or bad.
type MicMessage struct {
	State string `json:"state,omitempty"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

const (
	micStateLive = "live"

	// micNotAvd: the target is not an Android emulator.
	micNotAvd = "notAvd"
	// micNoGrpc: the emulator has no gRPC service, because it was booted
	// before loupe passed -grpc or by something other than loupe.
	// Restarting it from loupe fixes it.
	micNoGrpc = "noGrpc"
	// micBusy: another feed holds the emulator's one microphone, through
	// loupe or straight to its gRPC.
	micBusy = "micBusy"
	// micEmulatorGone: the emulator's gRPC connection or call ended under
	// the feed.
	micEmulatorGone = "emulatorGone"
	// micBad is anything else.
	micBad = "bad"
)

const (
	// micRate, mono S16LE: the only format the socket takes.
	micRate = 16000
	// micPacketBytes is 20 ms of it, the size of every packet loupe sends
	// the emulator. A packet longer than the emulator's ~300 ms buffer is
	// refused as INVALID_ARGUMENT.
	micPacketBytes = micRate * 2 / 50
	micPacketUs    = 20_000
	// micQueuePackets bounds the audio waiting on a slow gRPC write: 320 ms,
	// about what the emulator buffers. Past it the oldest is dropped, so the
	// guest hears late audio as a gap instead of a growing delay.
	micQueuePackets = 16
	// micLiveWait is how long the call has to be refused before loupe says
	// live. The emulator answers nothing to a call it accepts until the
	// call ends; it refused a second call 2-4 ms after it opened on
	// emulator 37.3.6.
	micLiveWait = 200 * time.Millisecond
	// micKeepalive pings the client as /ws does, so a peer that vanished
	// without closing the socket is noticed and stops holding the
	// emulator's microphone.
	micKeepalive = 20 * time.Second
	// micEndWait bounds ending the call once the socket has closed: sending
	// what is still queued, then half-closing and waiting for the status
	// while the emulator plays out what it buffered, about 300 ms. Past it
	// the call is cancelled.
	micEndWait = time.Second
	// micSlotWait is how long a new socket waits for the target's slot when
	// the socket holding it has already closed and its call is ending, so a
	// client that reconnects at once gets the microphone, not micBusy.
	micSlotWait = micEndWait + 500*time.Millisecond
)

var micFormat = emugrpc.AudioFormat{SamplingRate: micRate, Channels: emugrpc.Mono, Format: emugrpc.S16, Mode: emugrpc.ModeRealTime}

// micSlots keeps one /api/mic socket per target. The emulator refuses a
// second call anyway; refusing it here as well keeps a second socket from
// opening a call that would end the moment it opened.
type micSlots struct {
	mu   sync.Mutex
	held map[string]*micSlot
}

// micSlot is one target's hold, from the upgrade until its call has ended
// on the emulator.
type micSlot struct {
	// ending is set once the socket has closed: what is left is the call
	// playing out, which a new socket waits for.
	ending bool
	freed  chan struct{}
}

// take holds id's slot. A slot held by a live socket is refused at once; one
// whose socket has closed is waited for, up to wait.
func (m *micSlots) take(id string, wait time.Duration) (*micSlot, bool) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		m.mu.Lock()
		cur := m.held[id]
		if cur == nil {
			if m.held == nil {
				m.held = map[string]*micSlot{}
			}
			sl := &micSlot{freed: make(chan struct{})}
			m.held[id] = sl
			m.mu.Unlock()
			return sl, true
		}
		ending, freed := cur.ending, cur.freed
		m.mu.Unlock()
		if !ending {
			return nil, false
		}
		select {
		case <-freed:
		case <-deadline.C:
			return nil, false
		}
	}
}

// end marks sl's socket closed.
func (m *micSlots) end(sl *micSlot) {
	m.mu.Lock()
	sl.ending = true
	m.mu.Unlock()
}

func (m *micSlots) give(id string, sl *micSlot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held[id] == sl {
		delete(m.held, id)
	}
	close(sl.freed)
}

// handleMic feeds an AVD's microphone from the socket. The client sends
// binary frames of 16 kHz mono S16LE PCM, any length; loupe cuts them into
// 20 ms packets and forwards them to the emulator's gRPC injectAudio in
// real-time mode. See docs/agent-api.md, /api/mic.
//
// Auth, a missing or unknown target and a request that is not a WebSocket
// upgrade answer over HTTP before the upgrade, as /api/camera's do.
// Everything after is a text frame.
func (s *Server) handleMic(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	conn, err := wsx.Upgrade(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error() + " (docs/agent-api.md, /api/mic)"})
		return
	}
	defer conn.Close()

	rec := map[string]any{"kind": "agent", "target": t.ID(), "action": "mic", "method": r.Method, "ua": r.UserAgent()}
	defer func() {
		rec["tookMs"] = time.Since(began).Milliseconds()
		s.trace.Record(rec)
	}()
	refuse := func(code string, err error) {
		rec["outcome"], rec["code"], rec["err"] = "refused", code, err.Error()
		writeMic(conn, MicMessage{Error: err.Error(), Code: code})
	}

	mf, ok := t.(target.MicFeeder)
	if !ok {
		refuse(micNotAvd, fmt.Errorf("target %q is not an AVD this loupe runs: only those take a microphone feed (docs/agent-api.md, /api/mic)", t.ID()))
		return
	}
	slot, ok := s.mics.take(t.ID(), micSlotWait)
	if !ok {
		refuse(micBusy, fmt.Errorf("another /api/mic socket is feeding %s's microphone", t.ID()))
		return
	}
	// Deferred first, so it runs last: the slot frees only once the call
	// below has ended on the emulator too.
	defer s.mics.give(t.ID(), slot)
	s.awake.Touch(agentAwake)

	call, err := mf.OpenMic(r.Context())
	if err != nil {
		refuse(micCode(err), err)
		return
	}
	defer call.Close()
	select {
	case <-call.Done():
		refuse(micCode(call.Err()), call.Err())
		return
	case <-time.After(micLiveWait):
	}
	if err := writeMic(conn, MicMessage{State: micStateLive}); err != nil {
		rec["outcome"], rec["err"] = "closed", err.Error()
		return
	}
	live := time.Now()
	rec["live"] = true

	st := s.pumpMic(conn, call, func() { s.mics.end(slot) })
	rec["liveMs"] = time.Since(live).Milliseconds()
	rec["packets"], rec["dropped"], rec["bytes"] = st.sent, st.dropped, st.bytes
	switch {
	case st.err != nil:
		rec["outcome"], rec["code"], rec["err"] = "error", micCode(st.err), st.err.Error()
	case st.endErr != nil:
		rec["outcome"], rec["err"] = "ended", st.endErr.Error()
	default:
		rec["outcome"] = "ended"
	}
}

type micStats struct {
	sent, dropped, bytes int
	// err is why the call failed under a live socket, which the client was
	// sent; nil when the client closed the socket first.
	err error
	// endErr is why ending the call after the client closed did not end
	// with OK: a send of the queued audio failed, or the status did not
	// come in time.
	endErr error
}

// pumpMic forwards the socket's audio to call until either ends. The socket
// is read here and the call written from another goroutine, through a
// drop-oldest queue, so a slow gRPC write never stalls the read.
//
// When the call fails first, the client gets the error frame and the socket
// closes. When the client closes first, socketEnded runs, the audio already
// queued is sent and the call is half-closed, so the emulator plays out
// what it buffered; past micEndWait the call is cancelled. Either way the
// call is closed when pumpMic returns.
func (s *Server) pumpMic(conn *wsx.Conn, call target.MicCall, socketEnded func()) micStats {
	var st micStats
	// socketDone closes when the read loop ends. From then on the call
	// ending is the teardown, not news for the client.
	socketDone := make(chan struct{})
	// failed closes when the call failed under a live socket.
	failed := make(chan struct{})
	var failOnce sync.Once
	fail := func(err error) {
		select {
		case <-socketDone:
			return
		default:
		}
		failOnce.Do(func() {
			st.err = err
			close(failed)
			writeMic(conn, MicMessage{Error: err.Error(), Code: micCode(err)})
			conn.Close()
		})
	}

	q := newMicQueue(micQueuePackets)
	sendDone := make(chan struct{})
	var sent int
	var sendErr error
	go func() {
		defer close(sendDone)
		sent, sendErr = sendMic(call, q, failed, fail)
	}()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ping := time.NewTicker(micKeepalive)
		defer ping.Stop()
		for {
			select {
			case <-call.Done():
				fail(callEnded(call.Err()))
				return
			case <-ping.C:
				if err := conn.WritePing(); err != nil {
					conn.Close()
					return
				}
			case <-socketDone:
				return
			}
		}
	}()

	var chunks micChunker
	for {
		op, payload, err := conn.Read()
		if err != nil {
			break
		}
		if op != wsx.OpBinary {
			continue // text frames carry nothing the server reads
		}
		st.bytes += len(payload)
		for _, p := range chunks.add(payload) {
			if q.push(p) {
				st.dropped++
			}
		}
	}
	close(socketDone)
	socketEnded()
	q.close()
	select {
	case <-sendDone:
	case <-time.After(micEndWait):
	}
	call.Close()
	<-sendDone
	<-watchDone
	st.sent = sent
	if st.err == nil && sendErr != nil {
		st.endErr = sendErr
	}
	return st
}

// sendMic writes queued packets to call, the first one carrying the format,
// the only one whose format the emulator reads. It is the first packet sent,
// not the first queued, since the queue may drop that one.
//
// Once the queue is closed and empty it half-closes the call and waits for
// its status, unless failed is closed: a call that already failed has
// nothing to end. A Send that fails goes to fail, which tells a client that
// is still there.
func sendMic(call target.MicCall, q *micQueue, failed <-chan struct{}, fail func(error)) (sent int, err error) {
	for p := range q.ch {
		if sent == 0 {
			f := micFormat
			p.Format = &f
		}
		if err := call.Send(p); err != nil {
			err = callEnded(err)
			fail(err)
			return sent, err
		}
		sent++
	}
	select {
	case <-failed:
		return sent, nil
	default:
	}
	return sent, call.CloseAndRecv(micEndWait)
}

// callEnded is why a call that ended under a live socket ended. A call the
// emulator closed with OK still ended the feed.
func callEnded(err error) error {
	var st *emugrpc.Status
	if errors.As(err, &st) && st.Code == emugrpc.OK {
		return fmt.Errorf("the emulator ended the microphone call: %w", err)
	}
	return err
}

// micCode is the code an error frame carries for err.
func micCode(err error) string {
	if errors.Is(err, emugrpc.ErrNoGRPC) {
		return micNoGrpc
	}
	if errors.Is(err, emugrpc.ErrConnection) {
		return micEmulatorGone
	}
	var st *emugrpc.Status
	if errors.As(err, &st) {
		switch st.Code {
		case emugrpc.FailedPrecondition:
			return micBusy
		case emugrpc.OK, emugrpc.Canceled, emugrpc.Unavailable:
			return micEmulatorGone
		}
	}
	return micBad
}

func writeMic(conn *wsx.Conn, m MicMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return conn.WriteText(b)
}

// micChunker cuts the socket's byte stream into 20 ms packets, stamped in
// capture order: packet n is n*20 ms, so a packet the queue drops leaves a
// gap in the timestamps rather than shifting the rest. Frames need not hold
// whole packets or even whole samples; what is left over waits for the next
// frame, and a partial packet left when the socket closes is not sent.
type micChunker struct {
	buf []byte
	seq uint64
}

func (c *micChunker) add(b []byte) []emugrpc.AudioPacket {
	c.buf = append(c.buf, b...)
	var out []emugrpc.AudioPacket
	off := 0
	for ; len(c.buf)-off >= micPacketBytes; off += micPacketBytes {
		out = append(out, emugrpc.AudioPacket{
			Timestamp: c.seq * micPacketUs,
			Audio:     append([]byte(nil), c.buf[off:off+micPacketBytes]...),
		})
		c.seq++
	}
	c.buf = c.buf[:copy(c.buf, c.buf[off:])]
	return out
}

// micQueue is a bounded queue that drops its oldest packet to take a new one
// when full. One goroutine pushes, and closes it when it has no more.
type micQueue struct {
	ch chan emugrpc.AudioPacket
}

// close tells the reader nothing more is coming; it still gets what is
// queued.
func (q *micQueue) close() { close(q.ch) }

func newMicQueue(n int) *micQueue { return &micQueue{ch: make(chan emugrpc.AudioPacket, n)} }

// push adds p and reports whether a packet was dropped for it. It never
// blocks: with one pusher, the send after a full queue gave up a packet
// always has room.
func (q *micQueue) push(p emugrpc.AudioPacket) (dropped bool) {
	select {
	case q.ch <- p:
		return false
	default:
	}
	select {
	case <-q.ch:
		dropped = true
	default:
	}
	q.ch <- p
	return dropped
}
