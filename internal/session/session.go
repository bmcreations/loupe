package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bmcreations/loupe/internal/h264"
	"github.com/bmcreations/loupe/internal/target"
)

// InitPayload is the JSON body of the first message every viewer receives.
type InitPayload struct {
	// Codec is the RFC 6381 string for VideoDecoder.configure, derived from the
	// stream's own SPS rather than guessed.
	Codec string `json:"codec"`
	// CodedWidth and CodedHeight are the dimensions of the encoded video.
	CodedWidth  int `json:"codedWidth"`
	CodedHeight int `json:"codedHeight"`
	// Name labels the session in the UI.
	Name string `json:"name"`
	// Kind is "device", "emulator" or "synthetic".
	Kind string `json:"kind"`
	// Control reports whether this session accepts input. The client hides its
	// input affordances when false rather than silently swallowing taps.
	Control bool `json:"control"`
	// SessionID identifies the session for reconnects.
	SessionID string `json:"sessionId"`
	// MaxBufferMs is the ceiling on the client's jitter buffer. It is a server
	// setting because the operator is the one who knows the link, and only the
	// client can act on it, so it rides init rather than living in the client.
	MaxBufferMs int `json:"maxBufferMs"`
	// Viewer is the id this viewer's own gestures carry as Event.viewer.
	// Every viewer is sent every event, its own included, and without this a
	// client cannot tell its own tap echoed back from another viewer's.
	Viewer uint64 `json:"viewer,omitempty"`
	// Protocol is the session protocol version the server speaks on this
	// connection. While the server speaks one version it is always
	// ProtocolVersion; once it speaks a range, the negotiated version has to
	// reach buildInit per viewer.
	Protocol int `json:"protocol"`
}

// MaxStaleness bounds how long a frame may sit between being queued for a
// viewer and actually reaching the socket. Past this, the frame is stale: the
// picture it carries is already history, and sending it only pushes every
// later frame further behind.
//
// This is the load-shedding threshold for the whole server. A blocking socket
// write is the one place a bandwidth shortfall shows up, and it is invisible
// to a queue-depth check: the kernel send buffer absorbs megabytes while the
// Go channel sits empty. Measuring how long a frame waited catches it.
const MaxStaleness = 150 * time.Millisecond

// stalenessCap bounds what StalenessFor will return, so a wild baseline cannot
// disable shedding altogether.
const stalenessCap = 600 * time.Millisecond

// StalenessFor returns the staleness threshold for a path whose baseline round
// trip is base.
//
// A fixed threshold is the same mistake trap 5 records, one layer down. It was
// fixed for the pacer's own thresholds and never for this one, and the two
// disagree: the pacer can decide a 230ms path is perfectly healthy while every
// frame on it trips a 150ms staleness check. The 07:36 trace shows the result,
// twelve rows reporting a write that hit its deadline at 250ms against an
// average write of 27ms -- frames given up on by the clock rather than by the
// link.
//
// Half the baseline is the one-way estimate: a frame queued on a path with a
// 230ms round trip has spent roughly 115ms in flight before anything is wrong.
// Only delay beyond that is delay this server caused.
func StalenessFor(base time.Duration) time.Duration {
	if base < 0 {
		base = 0
	}
	if d := MaxStaleness + base/2; d < stalenessCap {
		return d
	}
	return stalenessCap
}

// OutMsg is one framed message queued for a viewer.
type OutMsg struct {
	Data []byte
	// Key and Config mark messages that must never be shed. Dropping them
	// leaves the decoder with nothing to resynchronize against.
	Key, Config bool
	// Control marks a message that is not video at all: init, stats. It shares
	// this queue so ordering against the stream is preserved, but a transport
	// with separate framing for video must not send it as a frame.
	Control bool
	// QueuedAt is when fan-out handed this to the viewer.
	QueuedAt time.Time
	// PTSMicros is the frame's presentation timestamp. Data carries the
	// WebSocket envelope, so a transport with its own framing reads the
	// timestamp here and takes the payload from Data[FrameHeaderLen:] rather
	// than parsing an envelope it does not use.
	PTSMicros uint64
}

// viewer is one connected client.
type viewer struct {
	id  uint64
	out chan OutMsg
	// needsKey is set when a viewer has missed frames and must wait for the
	// next keyframe before it can decode again. Sending it inter-frames in the
	// meantime produces visible corruption, which is worse than a brief freeze.
	needsKey bool

	// joined is when this viewer attached, and sent is how many payload bytes
	// have actually reached it. The bitrate controller reads both: a viewer
	// that joined part way through an interval carried part of an interval's
	// bytes, and counting it would read a fresh connection as a slow link.
	joined time.Time
	sent   atomic.Uint64

	// scrollMu guards scrollAccH, scrollAccV, scrollLastX, scrollLastY and
	// scrollTimer. ViewerScroll runs on this viewer's own read loop, but the
	// trailing flush that ends a burst runs on a time.AfterFunc goroutine, so
	// unlike GestureAssembler this genuinely has two writers and needs a lock.
	scrollMu               sync.Mutex
	scrollAccH, scrollAccV float32
	// scrollPending is whether anything has accumulated into scrollAccH/V
	// since the last flush -- distinct from the sum being nonzero. A burst of
	// +2 then -2 sums to zero but is a real gesture the trailing flush must
	// still report; checking accH == 0 && accV == 0 instead would read that
	// burst as untouched and silently drop it (see flushViewerScroll).
	scrollPending bool
	// scrollLastX and scrollLastY are the position of the most recent scroll
	// in this burst, kept so a trailing flush -- which has no ScrollEvent of
	// its own to read a position from -- can still report where the gesture
	// happened.
	scrollLastX, scrollLastY int32
	// scrollTimer is armed on the leading edge of a burst and fires the
	// trailing flush scrollPublishInterval later. Nil means no burst is open,
	// which is what ViewerScroll reads to decide whether an incoming delta is
	// a new leading edge or an addition to one already running.
	scrollTimer *time.Timer
}

// Session is one live capture fanned out to any number of viewers.
type Session struct {
	ID     string
	target target.Target

	// key identifies this particular Session instance, distinct from ID (the
	// target id, which is shared across a restart): two Opens against the
	// same target get different keys. Set once at construction; see
	// newSessionKey and EventPage for why it exists.
	key string

	// streamMu guards everything about which encoder is live: stream, opts,
	// ended, closing and relaunching. It is deliberately not mu, because a
	// relaunch opens a new encoder and that takes hundreds of milliseconds --
	// long enough that holding the fan-out lock across it would stall every
	// viewer for the duration of the thing meant to help them.
	streamMu sync.Mutex
	stream   target.Stream
	opts     target.Options
	// ended records that pump has decided the current stream is the last one.
	// A relaunch that committed after that would hand a fresh encoder to a
	// loop which has already exited, so both decisions are taken under this
	// one lock.
	ended       bool
	closing     bool
	relaunching bool

	// nativeEdge is the longest edge a stream opened at its native size
	// (MaxSize 0) first encoded at, the top rung of the resize ladder.
	// Guarded by streamMu.
	nativeEdge int

	// openBitRate is the rate the session opened with. opts.BitRate follows
	// every change, so this is what the resize ladder holds the native size at.
	openBitRate int

	// queueLen is the fan-out channel depth for a joining viewer, sized from
	// the capture rate this session was opened with.
	queueLen int

	// onBitrate receives every interval the bitrate controller evaluates,
	// including the ones it declines to act on. It is a callback rather than a
	// trace writer because this package sits below httpapi and does not import
	// trace; the manager installs one that records.
	//
	// It is set once, before adaptLoop starts, and read from that goroutine
	// only.
	onBitrate func(BitrateReport)

	// onResize receives every change of the encoder's picture size, which in
	// practice means a rotation. It is a callback for the same reason
	// onBitrate is: this package sits below httpapi and does not import trace.
	//
	// Without it a rotation is invisible on the timeline. The frame rate dips
	// while the encoder restarts its GOP and the client's canvas changes
	// shape, which from the trace alone is indistinguishable from the idle
	// panel of trap 20.
	onResize func(oldW, oldH, newW, newH int)

	// onEvent receives every event published, from a viewer's gesture or from
	// /api/input. Same shape as onResize and onBitrate, and for the same
	// reason: the manager installs one that records it on the timeline.
	onEvent func(Event)

	// onClipboard receives the size of each device clipboard sent to
	// viewers, and how many it reached. Never the text: the trace is not a
	// place for what someone copied. Guarded by mu, unlike the observers
	// above, because watchClipboard starts inside Open, before the manager
	// can install it.
	onClipboard func(runes, viewers int)

	// pointers is every finger a viewer has down, so one that leaves
	// mid-gesture can be lifted (pointers.go). onLifted records each lift.
	pointers downPointers
	onLifted func(viewer uint64, n int)

	// ctlMu guards the input arbiter and everything around it: see
	// control.go. It is taken before mu, never inside it.
	ctlMu     sync.Mutex
	arb       *Arbiter
	ctlTimer  *time.Timer
	ctlSent   ControlState
	ctlClosed bool
	onControl func(ControlChange)

	mu      sync.RWMutex
	viewers map[uint64]*viewer
	// events keeps the last 256 published events for replay, and gestures
	// holds one assembler per viewer, both guarded by mu like everything else
	// about the viewer set. A GestureAssembler itself needs no lock -- see its
	// own doc comment -- but the map that finds one by viewer id does.
	events   *eventRing
	gestures map[uint64]*GestureAssembler
	nextID   uint64
	lastCfg  *OutMsg // most recent parameter sets, replayed to joiners
	overlays []byte  // latest MsgOverlay body, sent to joiners; nil for none
	init     InitPayload

	// taps, lastCfgRaw, holds and bitrateLog belong to tap.go; taps,
	// lastCfgRaw and bitrateLog are guarded by mu.
	taps       []*Tap
	lastCfgRaw target.Frame
	holds      atomic.Int64
	bitrateLog []BitrateChange

	// codecResolved guards the one-time derivation of the codec string, which
	// can only happen once the first parameter set arrives.
	codecResolved bool

	// clientStats is the newest metrics sample reported by a viewer, and
	// clientStatsAt is when it was recorded. The timestamp exists for the
	// same reason clientRTTAt does: this is session state handed to whichever
	// viewer is current, and Health (trap 26) needs to tell a fresh report
	// from one that has stopped arriving, for every field in the sample, not
	// only rtt.
	clientStats   map[string]any
	clientStatsAt time.Time
	// clientRTT is the phone-measured round trip, in milliseconds, and
	// clientRTTAt is when it was recorded. The timestamp is load-bearing: this
	// is session state, so a viewer that joins is handed whatever the previous
	// viewer last reported, and the 07:36:31 reconnect was handed 1400ms from a
	// connection that had already failed.
	clientRTT   float64
	clientRTTAt time.Time

	// framesIn counts access units the encoder produced, before any fan-out
	// decision. It is the denominator the bitrate controller needs: framesOut
	// is per-viewer and already has shedding taken out of it.
	framesIn atomic.Uint64
	// keysIn counts keyframes among them. Without it a keyframe flood is
	// invisible: the counters it moves are framesIn and framesDrop, which a
	// slow link moves the same way. Diagnosing one cost a session.
	keysIn atomic.Uint64
	// configIn counts parameter-set frames among framesIn. A config frame is
	// never decoded into a picture (web/app.js's decFps never counts one), so
	// the health verdict's delivery ratio has to subtract these out of
	// what it treats as "produced" -- otherwise an idle panel that only
	// resends its parameter sets (every wt-open, and after a rotation) reads
	// as an undelivered frame instead of no frame at all.
	configIn atomic.Uint64
	// mediaIn counts access units among framesIn that are NOT a config frame
	// -- the health verdict's own "produced", incremented directly by the
	// pump rather than derived as framesIn minus configIn at read time. The
	// derived form read two separate atomics (FrameCount then ConfigCount)
	// that do not update together, so a config frame landing between the two
	// reads could be counted in "produced" a moment before it was counted in
	// "config", understating mediaIn's own diff for that tick by one. mediaIn
	// is the frame kind the pump already knows at the point it increments
	// framesIn, so it costs one more atomic add here to remove that read-time
	// race entirely rather than merely bound it.
	mediaIn    atomic.Uint64
	framesOut  atomic.Uint64
	framesDrop atomic.Uint64
	framesShed atomic.Uint64
	// framesWait counts frames suppressed while a viewer awaits a keyframe.
	framesWait atomic.Uint64
	// shedPacer, shedStale, shedAwaitKey and shedAbandon break framesShed
	// down by cause.
	shedPacer    atomic.Uint64
	shedStale    atomic.Uint64
	shedAwaitKey atomic.Uint64
	shedAbandon  atomic.Uint64

	// eventsDropped counts published events a viewer's control channel was
	// too full to accept, the same fate PublishEvent's fan-out select gives a
	// stale frame.
	eventsDropped atomic.Uint64

	keyMu      sync.Mutex
	lastKeyReq time.Time
	// keyPending is set while a delayed edge request is scheduled, so a run of
	// latches coalesces into one ask instead of one per frame.
	keyPending bool

	closeOnce sync.Once
	// endReason is why End closed the session, "" for any other close. A
	// transport that cannot rely on MsgEnded arriving (WebTransport resets
	// the control stream on close) reads it to carry the reason itself.
	endReason atomic.Pointer[string]
	done      chan struct{}
}

// Open starts a session against t.
func Open(ctx context.Context, id string, t target.Target, opts target.Options) (*Session, error) {
	stream, err := t.Open(ctx, opts)
	if err != nil {
		return nil, err
	}

	meta := stream.Meta()
	s := &Session{
		ID:          id,
		key:         newSessionKey(),
		target:      t,
		stream:      stream,
		opts:        opts,
		openBitRate: opts.BitRate,
		viewers:     make(map[uint64]*viewer),
		events:      newEventRing(eventRingSize),
		gestures:    make(map[uint64]*GestureAssembler),
		done:        make(chan struct{}),
		queueLen:    viewerQueueLen(opts.MaxFPS),
		init: InitPayload{
			// Provisional. The real codec string is derived from the first SPS;
			// this fallback only matters if a stream somehow never sends one,
			// in which case the client is going to fail anyway and a plausible
			// guess at least produces a clear decoder error.
			Codec:       "avc1.42E01E",
			CodedWidth:  meta.Width,
			CodedHeight: meta.Height,
			Name:        meta.Name,
			Kind:        string(t.Kind()),
			Control:     t.Kind().AcceptsInput(),
			SessionID:   id,
			Protocol:    ProtocolVersion,
		},
	}

	go s.pump()
	s.watchClipboard(stream)
	return s, nil
}

// Bounds on a viewer's fan-out channel. The floor keeps a low capture rate
// from producing a queue so shallow that any hiccup drops; the ceiling stops
// one slow viewer pinning unbounded memory, since every queued OutMsg holds a
// frame's bytes.
const (
	minViewerQueue = 16
	maxViewerQueue = 240
)

// viewerQueueLen sizes a viewer's fan-out channel in time rather than frames.
//
// It was a flat 8, which is 133ms at 60fps -- and 800ms on trap 20's idle 10Hz
// panel. The same constant meaning different things at different rates is why
// it looked adequate on a desk. The 09:53 5G trace showed write stalls of
// 280-297ms against that 133ms: one stall overflowed the channel, broadcast
// dropped a frame, the viewer latched, and 340 of that session's 390 shed
// frames were the keyframe wait that followed. Pacer and staleness together
// accounted for 50.
//
// One second at the configured rate. The old comment's warning still holds --
// a deep queue relocates latency rather than removing it -- but the writer's
// staleness check is what bounds what actually goes out, and this only decides
// how much of a stall the fan-out rides out before forcing a resync. A resync
// costs far more than the queueing does.
func viewerQueueLen(maxFPS int) int {
	if maxFPS <= 0 {
		maxFPS = target.DefaultMaxFPS
	}
	switch {
	case maxFPS < minViewerQueue:
		return minViewerQueue
	case maxFPS > maxViewerQueue:
		return maxViewerQueue
	}
	return maxFPS
}

// currentStream returns the encoder that is live right now. Every caller
// outside pump goes through this, because a relaunch replaces the stream
// underneath a running session and a cached copy would keep driving an encoder
// that has already been shut down.
func (s *Session) currentStream() target.Stream {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return s.stream
}

// Stream is the stream the session reads from now. A hub reaches a peer
// relay's owner-side viewer id through it.
func (s *Session) Stream() target.Stream { return s.currentStream() }

// Meta exposes the underlying stream metadata.
func (s *Session) Meta() target.Meta { return s.currentStream().Meta() }

// ShedReason names why the writer gave up on a frame.
//
// The three reasons were one counter until the 07:36 trace needed them apart.
// Pacer rejection is the controller working as designed, staleness is the link
// failing to keep up, and a keyframe wait is the decoder unable to use anything
// until the encoder answers -- three different faults producing one identical
// number, which is trap 19's unmeasured-branch shape a fourth time.
type ShedReason int

const (
	ShedPacer ShedReason = iota
	ShedStale
	ShedAwaitKey
	// ShedAbandon is a frame the transport started sending and then reset
	// mid-flight because it outlived its deadline. It is a fourth cause, not a
	// variant of staleness: staleness is decided before the write and the
	// frame never reaches the link, while this one consumed link time and
	// arrived nowhere. Only WebTransport can produce it -- TCP cannot recall a
	// frame once written.
	ShedAbandon
)

// NoteShed records a frame the writer shed, and why. It is separate from the
// fan-out drop counter so those two causes stay distinguishable as well: a
// fan-out drop means a slow reader, a shed means a slow link.
func (s *Session) NoteShed(reason ShedReason) {
	s.framesShed.Add(1)
	switch reason {
	case ShedPacer:
		s.shedPacer.Add(1)
	case ShedStale:
		s.shedStale.Add(1)
	case ShedAwaitKey:
		s.shedAwaitKey.Add(1)
	case ShedAbandon:
		s.shedAbandon.Add(1)
	}
}

// NoteSent records payload bytes that reached a viewer. This is the only
// direct measurement of what the link will carry: every other counter says
// what was offered to it.
//
// Only frames the transport delivered whole belong here. A WebTransport frame
// that outruns its deadline is reset mid-flight and the receiver discards what
// it has, and counting those made goodput read 1.56 Mbps across a link shaped
// to 400 kbps -- four times the truth, in the one condition the figure is
// consulted. See ShedAbandon.
func (s *Session) NoteSent(id uint64, n int) {
	if n <= 0 {
		return
	}
	s.mu.RLock()
	v := s.viewers[id]
	s.mu.RUnlock()
	if v != nil {
		v.sent.Add(uint64(n))
	}
}

// ShedCounts is framesShed split by cause. The four fields sum to ShedCount.
type ShedCounts struct {
	Pacer    uint64
	Stale    uint64
	AwaitKey uint64
	Abandon  uint64
}

// ShedBreakdown reports shed frames by reason.
func (s *Session) ShedBreakdown() ShedCounts {
	return ShedCounts{
		Pacer:    s.shedPacer.Load(),
		Stale:    s.shedStale.Load(),
		AwaitKey: s.shedAwaitKey.Load(),
		Abandon:  s.shedAbandon.Load(),
	}
}

// RecordClientStats stores the most recent metrics reported by a viewer.
// Only the newest sample is kept: this feeds a once-per-second trace record,
// not a history.
func (s *Session) RecordClientStats(raw map[string]any) {
	s.mu.Lock()
	s.clientStats = raw
	s.clientStatsAt = time.Now()
	// The client's own round-trip measurement is the only latency signal that
	// survives the path. A local socket check cannot see a reverse proxy, a
	// relay, or any other buffer between here and the phone, and will report a
	// healthy connection while seconds of video sit queued downstream.
	if v, ok := raw["rttMs"].(float64); ok && v > 0 {
		s.clientRTT = v
		s.clientRTTAt = time.Now()
	}
	s.mu.Unlock()
}

// ClientRTT returns the phone's most recent round-trip measurement in
// milliseconds, or 0 if it has not reported one yet.
func (s *Session) ClientRTT() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clientRTT
}

// ClientRTTAt returns the same measurement together with when it was recorded,
// so a caller can tell a fresh reading from one that has stopped arriving.
//
// It has to be able to. The report travels over /api/log, which queues behind
// the very video the pacer is throttling, so the signal goes stale exactly when
// it is needed. In the 07:36 trace the server emitted a byte-identical client
// block for three consecutive seconds -- rtt 111.3ms, 2fps -- while shed
// climbed 270 to 315 and the pacer, re-reading that same stale number, held
// wide open at 1.00.
func (s *Session) ClientRTTAt() (float64, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clientRTT, s.clientRTTAt
}

// ClientStats returns the last metrics a viewer reported, or nil.
func (s *Session) ClientStats() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clientStats
}

// ClientStatsAt returns the same sample together with when it was recorded,
// so a caller can tell a fresh report from one that has stopped arriving
// (trap 26) for fields, like decoded fps, that clientRTTAt does not cover.
func (s *Session) ClientStatsAt() (map[string]any, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clientStats, s.clientStatsAt
}

// DropCount reports frames dropped at fan-out because a viewer's queue was
// full, which is distinct from frames shed at the socket.
func (s *Session) DropCount() uint64 { return s.framesDrop.Load() }

// ShedCount reports how many frames have been shed for this session.
func (s *Session) ShedCount() uint64 { return s.framesShed.Load() }

// WaitCount reports how many frames were suppressed because a viewer was
// waiting for a keyframe. A climbing value means frames are still arriving
// from the device and the fan-out is holding them back; a flat one means the
// source itself has gone quiet.
func (s *Session) WaitCount() uint64 { return s.framesWait.Load() }

// FrameCount reports how many access units the encoder produced, before any
// fan-out decision. It is the denominator keysIn and framesDrop are read
// against.
func (s *Session) FrameCount() uint64 { return s.framesIn.Load() }

// KeyCount reports how many of the access units the encoder produced were
// keyframes. Against framesIn it says whether the stream is being asked for
// them faster than the GOP would emit them.
func (s *Session) KeyCount() uint64 { return s.keysIn.Load() }

// ConfigCount reports how many of the access units the encoder produced were
// parameter sets, a subset of FrameCount. Kept for Stats/tracing; the health
// verdict reads MediaCount directly instead of deriving it from this.
func (s *Session) ConfigCount() uint64 { return s.configIn.Load() }

// MediaCount reports how many of the access units the encoder produced were
// NOT a parameter set -- the health verdict's own "produced", since
// web/app.js's decFps never counts a config frame. Incremented directly by
// the pump rather than derived as FrameCount minus ConfigCount, because those
// two are separate atomics that do not update together (see mediaIn's field
// comment).
func (s *Session) MediaCount() uint64 { return s.mediaIn.Load() }

// Stats reports counters useful for a health endpoint.
func (s *Session) Stats() map[string]any {
	s.mu.RLock()
	n := len(s.viewers)
	s.mu.RUnlock()
	return map[string]any{
		"id":            s.ID,
		"target":        s.target.Label(),
		"kind":          string(s.target.Kind()),
		"viewers":       n,
		"framesOut":     s.framesOut.Load(),
		"framesDropped": s.framesDrop.Load(),
		"framesShed":    s.framesShed.Load(),
	}
}

// AddViewer registers a client and returns its id plus the channel it should
// drain. The init message and current parameter sets are queued immediately so
// a joiner can configure its decoder without waiting for the stream to repeat
// them.
func (s *Session) AddViewer() (uint64, <-chan OutMsg) {
	s.mu.Lock()

	s.nextID++
	// A Session built directly rather than through Open has no queueLen. The
	// depth cannot be left at zero: the init message below is sent while mu is
	// held, so an unbuffered channel deadlocks the session on its first viewer.
	depth := s.queueLen
	if depth <= 0 {
		depth = viewerQueueLen(0)
	}
	v := &viewer{
		id:       s.nextID,
		out:      make(chan OutMsg, depth),
		needsKey: true,
		joined:   time.Now(),
	}
	s.viewers[v.id] = v

	// A Session built directly rather than through Open (every test in this
	// package but the ones that call Open itself) has no gestures map either,
	// for the same reason it has no queueLen: only the production constructor
	// sets it up. Lazily creating one here, exactly like the depth fallback
	// above, means a viewer always has somewhere for its assembler to live.
	if s.gestures == nil {
		s.gestures = make(map[uint64]*GestureAssembler)
	}
	s.gestures[v.id] = NewGestureAssembler()

	v.out <- s.buildInit(v.id)
	if s.lastCfg != nil {
		// Queued now, not when the stream first sent it: the write deadline
		// runs from QueuedAt, and a replay minutes old goes out expired and is
		// reset on WebTransport. simcap sends parameter sets once, so a joiner
		// that loses them never decodes.
		cfg := *s.lastCfg
		cfg.QueuedAt = time.Now()
		v.out <- cfg
	}
	if s.overlays != nil {
		v.out <- OutMsg{Data: append([]byte{MsgOverlay}, s.overlays...), Control: true, QueuedAt: time.Now()}
	}
	s.mu.Unlock()

	// Who holds the target against agents, after init so the client has
	// somewhere to show it.
	s.sendControlTo(v.id)

	// Parameter sets are not enough: a joiner lands mid-GOP and needsKey is set
	// from the start, so it discards every inter-frame until a keyframe arrives.
	// scrcpy emits none on a timer, so on a quiet screen that wait is unbounded
	// and the viewer shows nothing at all. Ask, rate limited like every other
	// caller.
	s.askKeyframe()

	return v.id, v.out
}

// RemoveViewer deregisters a client and closes its channel. A scroll burst
// this viewer had open is flushed first: without this, a viewer that
// disconnects mid-burst leaves its tail sitting in an accumulator nobody
// will ever read, undercounting the gesture it made.
func (s *Session) RemoveViewer(id uint64) {
	s.mu.Lock()
	v, ok := s.viewers[id]
	if ok {
		delete(s.viewers, id)
		delete(s.gestures, id)
	}
	w, h := s.init.CodedWidth, s.init.CodedHeight
	s.mu.Unlock()
	if !ok {
		return
	}

	s.flushViewerScroll(id, v, w, h)
	s.liftPointers(id)
	close(v.out)
	s.noteViewerLeft(id)
}

// ViewerCount reports how many clients are attached.
func (s *Session) ViewerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.viewers)
}

// SetClientBuffer sets the jitter-buffer ceiling reported to viewers. Call it
// before the first viewer joins. Init is cached once built, so like SetOptions
// this does not reach a session that is already running: the value is settled
// when the session is, and the settings API reports it as applying next
// session for that reason.
func (s *Session) SetClientBuffer(ms int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init.MaxBufferMs = ms
}

// buildInit encodes init for one viewer. It is built per viewer rather than
// cached, because it carries that viewer's id; a marshal of eight fields on a
// join or a resize costs nothing worth caching.
func (s *Session) buildInit(viewer uint64) OutMsg {
	init := s.init
	init.Viewer = viewer
	body, err := json.Marshal(init)
	if err != nil {
		// InitPayload has no unmarshalable fields, so this cannot fail in
		// practice; returning a valid error message beats panicking in a
		// long-lived server.
		body = []byte(`{"error":"init encode failed"}`)
	}
	msg := make([]byte, 1+len(body))
	msg[0] = MsgInit
	copy(msg[1:], body)
	// Control, not a frame: it carries no picture. Unsheddable either way,
	// since a viewer that misses init never configures its decoder at all.
	return OutMsg{Data: msg, Control: true, QueuedAt: time.Now()}
}

// RequestKeyframe asks the device's encoder for a fresh keyframe.
func (s *Session) RequestKeyframe() error {
	return s.currentStream().RequestKeyframe()
}

// Touch forwards a pointer event to the device.
func (s *Session) Touch(e TouchEvent) error {
	return s.currentStream().Touch(target.Touch{
		Action:    target.TouchAction(e.Action),
		PointerID: e.PointerID,
		X:         e.X,
		Y:         e.Y,
		Pressure:  e.Pressure,
	})
}

// Key forwards a key event to the device.
func (s *Session) Key(e KeyEvent) error {
	return s.currentStream().Key(target.Key{
		Action:  target.KeyAction(e.Action),
		Keycode: e.Keycode,
	})
}

// Text types s into whatever has focus on the device, for a stream that
// can take a string whole (target.TextTyper).
func (s *Session) Text(text string) error {
	tt, ok := s.currentStream().(target.TextTyper)
	if !ok {
		return target.ErrNoControl
	}
	return tt.Text(text)
}

// Scroll forwards a scroll gesture to the device.
func (s *Session) Scroll(e ScrollEvent) error {
	return s.currentStream().Scroll(target.Scroll{
		X:       e.X,
		Y:       e.Y,
		HScroll: e.HScroll,
		VScroll: e.VScroll,
	})
}

// scrollPublishInterval bounds how often a viewer's wheel produces an event.
// A wheel emits many small deltas in a burst; one event per tick would flood
// both the ring and every other viewer's control channel with what is really
// one continuous gesture. Deltas between publishes are accumulated rather
// than discarded, so the coalesced event still reports the true total.
const scrollPublishInterval = 250 * time.Millisecond

// ViewerTouch forwards a viewer's pointer event to the device and records the
// gesture it completes. Forwarding comes first and is not conditional on the
// assembler: the log is a record of input, never a gate on it.
func (s *Session) ViewerTouch(viewerID uint64, e TouchEvent) error {
	err := s.Touch(e)
	s.pointers.note(viewerID, e)
	s.noteTouch(viewerID, e)

	s.mu.RLock()
	g := s.gestures[viewerID]
	w, h := s.init.CodedWidth, s.init.CodedHeight
	s.mu.RUnlock()

	if g != nil {
		if ev := g.Touch(e, w, h, time.Now()); ev != nil {
			ev.Viewer = viewerID
			if err != nil {
				ev.Err = err.Error()
			}
			s.PublishEvent(*ev)
		}
	}
	return err
}

// ViewerKey forwards a viewer's key event to the device and, on key-up only,
// records it. A key-down alone says nothing complete -- a held key repeats --
// and the up is when the press is known to be over.
//
// The keycode is classified through target.ButtonForKeycode: a keycode that
// names a device button (home, back, recents, lock) publishes kind:"button"
// with the name instead of the raw code, matching what /api/input's "button"
// action publishes. A keycode with no name -- an ordinary key, not a device
// button -- stays kind:"key" with the raw keycode, unredacted (see the known
// gap in the server plan's Task 5: a PIN typed as key actions is not caught
// by the password redaction that covers "text").
//
// target.ButtonForKeycode's table is Android's keycodes, and ViewerKey uses
// it for every viewer regardless of which target the session is streaming.
// That is fine today -- every target is an Android device or emulator -- but
// it will need to become per-target once an iOS target exists, since iOS has
// no Android keycode space to classify against.
func (s *Session) ViewerKey(viewerID uint64, e KeyEvent) error {
	err := s.Key(e)
	s.noteHuman(viewerID)

	if e.Action == uint8(target.KeyUp) {
		ev := Event{Source: "viewer", Viewer: viewerID}
		if name, ok := target.ButtonForKeycode(int(e.Keycode)); ok {
			ev.Kind = "button"
			ev.Button = name
		} else {
			ev.Kind = "key"
			ev.Keycode = int(e.Keycode)
		}
		if err != nil {
			ev.Err = err.Error()
		}
		s.PublishEvent(ev)
	}
	return err
}

// TextMarker stands in for text withheld from an event. It carries no
// information about length; TextLen does.
const TextMarker = "•••"

// ViewerText types a viewer's text into the device and records it redacted:
// the event says text was typed and how many characters, never what.
//
// /api/input's text action reads the focused field first and publishes the
// text when it is not a password (httpapi's redactionForFocusedField). That
// read is a uiautomator dump, 2.1s on an emulator, and a phone keyboard
// sends a message per keystroke, so doing it here would put two seconds
// behind every character. Withholding the text always is the answer that
// read falls back to whenever it cannot tell.
func (s *Session) ViewerText(viewerID uint64, text string) error {
	err := s.Text(text)
	s.noteHuman(viewerID)
	ev := Event{
		Source:   "viewer",
		Viewer:   viewerID,
		Kind:     "text",
		Text:     TextMarker,
		Redacted: true,
		TextLen:  utf8.RuneCountInString(text),
	}
	if err != nil {
		ev.Err = err.Error()
	}
	s.PublishEvent(ev)
	return err
}

// ViewerScroll forwards a viewer's scroll gesture to the device and records
// it as a coalesced event; see scrollPublishInterval.
//
// The leading delta of a burst publishes immediately, the same as before.
// What changed is what happens to the rest: earlier, a delta that arrived
// after the window had elapsed was what triggered the flush, so a burst that
// simply stopped -- the common case, a wheel spun once and released -- left
// its tail sitting in the accumulator until the next unrelated burst finally
// flushed it, reporting a stale position and a delayed time. A timer armed on
// the leading edge now flushes the tail on its own, scrollPublishInterval
// after the burst started, whether or not anything else arrives.
func (s *Session) ViewerScroll(viewerID uint64, e ScrollEvent) error {
	err := s.Scroll(e)
	s.noteHuman(viewerID)

	s.mu.RLock()
	v := s.viewers[viewerID]
	w, h := s.init.CodedWidth, s.init.CodedHeight
	s.mu.RUnlock()
	if v == nil {
		// The viewer disconnected between the dispatch loop reading this
		// message and getting here. The device already has the input; there
		// is nowhere left to accumulate or publish an event.
		return err
	}

	// v.scrollTimer, unlike GestureAssembler, genuinely has two writers: this
	// call, on the viewer's own read loop, and the timer's own goroutine when
	// it fires. scrollMu is what makes that safe.
	v.scrollMu.Lock()
	leading := v.scrollTimer == nil
	v.scrollLastX, v.scrollLastY = e.X, e.Y
	if leading {
		v.scrollTimer = time.AfterFunc(scrollPublishInterval, func() {
			s.flushViewerScroll(viewerID, v, w, h)
		})
	} else {
		v.scrollAccH += e.HScroll
		v.scrollAccV += e.VScroll
		v.scrollPending = true
	}
	v.scrollMu.Unlock()

	if !leading {
		return err
	}

	ev := Event{
		Source: "viewer",
		Viewer: viewerID,
		Kind:   "scroll",
		From:   viewerPoint(e.X, e.Y, w, h),
		Scroll: &Point{X: float64(e.HScroll), Y: float64(e.VScroll)},
	}
	if err != nil {
		ev.Err = err.Error()
	}
	s.PublishEvent(ev)
	return err
}

// flushViewerScroll publishes whatever a viewer has accumulated since the
// leading edge of its current scroll burst and closes the burst, so the next
// delta starts a new one. It is called by the trailing timer ViewerScroll
// arms, by RemoveViewer so a viewer that disconnects mid-burst does not leave
// its tail uncounted, and by the pump's own teardown for the same reason (see
// its call site).
//
// v is passed in rather than looked up, because by the time RemoveViewer or
// the pump teardown calls this the viewer may already be gone from
// s.viewers. w and h are the size captured at the start of the burst, in
// ViewerScroll, not re-read here -- a resize mid-burst must not change what
// position the trailing event reports. The published event carries no Err:
// only the forwarding half of a scroll (Session.Scroll) can fail, and that
// failure was already attached to the leading event.
func (s *Session) flushViewerScroll(viewerID uint64, v *viewer, w, h int) {
	v.scrollMu.Lock()
	pending := v.scrollPending
	accH, accV := v.scrollAccH, v.scrollAccV
	x, y := v.scrollLastX, v.scrollLastY
	v.scrollAccH, v.scrollAccV = 0, 0
	v.scrollPending = false
	v.scrollTimer = nil
	v.scrollMu.Unlock()

	if !pending {
		// The leading edge already published its own delta and nothing has
		// arrived since. Checking accH == 0 && accV == 0 here instead of a
		// separate pending flag would drop a real burst that happens to sum
		// to zero, such as +2 then -2 -- a genuine scroll out and back.
		return
	}

	s.PublishEvent(Event{
		Source: "viewer",
		Viewer: viewerID,
		Kind:   "scroll",
		From:   viewerPoint(x, y, w, h),
		Scroll: &Point{X: float64(accH), Y: float64(accV)},
	})
}

// viewerPoint is NormPoint for a viewer's stream coordinates, where nil
// covers the "nothing to report a position in" case GestureAssembler.norm
// and .finish handle for a gesture that completes before the stream has
// learned a real size. A point past the edge is reported unclamped.
func viewerPoint(x, y int32, w, h int) *Point {
	pt, _ := NormPoint(int(x), int(y), w, h)
	return pt
}

// PublishEvent numbers an event, keeps it for replay, sends it to every
// viewer, and reports it to the event observer, if one is installed. The send
// to each viewer is non-blocking like every other control message; a viewer
// that misses one sees the gap in seq and fetches /api/events. A full
// channel counts toward eventsDropped rather than blocking, because
// PublishEvent runs under s.mu and a blocking send here would stall every
// other viewer and publisher on the session behind one slow reader.
//
// Numbering, fan-out and the observer call all happen under one s.mu critical
// section rather than being split across it. Releasing the lock between any
// two of these steps lets two concurrent publishers interleave: whichever
// wins the numbering race is not guaranteed to win the next race, so a viewer
// -- or the observer -- could see seq 6 before seq 5. Reproduced directly:
// with the observer call moved after s.mu.Unlock(), 8 concurrent publishers
// produced an out-of-order pair (16 delivered before 11) in 3 of 20 runs.
// Seq order, delivery order and observer order all have to agree, and holding
// the lock across all three steps is what makes them agree.
//
// Because of that, the installed observer must not block and must not call
// back into this Session -- AddViewer, RemoveViewer, PublishEvent or anything
// else that takes s.mu would deadlock against the lock this call already
// holds. SetEventObserver's own doc comment carries the same constraint.
func (s *Session) PublishEvent(e Event) Event {
	if e.T == 0 {
		e.T = time.Now().UnixMilli()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		// A Session built directly rather than through Open has no ring yet;
		// see the same lazy-init on gestures in AddViewer.
		s.events = newEventRing(eventRingSize)
	}
	e = s.events.add(e)

	msg := OutMsg{Data: EncodeEvent(e), Control: true, QueuedAt: time.Now()}
	for _, v := range s.viewers {
		select {
		case v.out <- msg:
		default:
			s.eventsDropped.Add(1)
		}
	}

	if s.onEvent != nil {
		s.onEvent(e)
	}
	return e
}

// PublishCursor sends e, an agent's move, to every viewer as a MsgCursor.
// Unlike PublishEvent it keeps nothing: no seq, no ring entry, no observer
// call, since a pointer position is stale by the next one and a catch-up
// that replayed it would draw a cursor where the agent no longer is.
func (s *Session) PublishCursor(e Event) {
	if e.T == 0 {
		e.T = time.Now().UnixMilli()
	}
	body, _ := json.Marshal(e)
	msg := OutMsg{Data: append([]byte{MsgCursor}, body...), Control: true, QueuedAt: time.Now()}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.viewers {
		select {
		case v.out <- msg:
		default:
			s.eventsDropped.Add(1)
		}
	}
}

// SetOverlays replaces the overlays every viewer draws with body, a
// MsgOverlay's JSON, and keeps it for viewers that join later. Unlike a
// cursor an overlay is state: a person who opens the target after the
// extension posted still sees it.
func (s *Session) SetOverlays(body []byte) {
	msg := OutMsg{Data: append([]byte{MsgOverlay}, body...), Control: true, QueuedAt: time.Now()}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.overlays = body
	for _, v := range s.viewers {
		select {
		case v.out <- msg:
		default:
			s.eventsDropped.Add(1)
		}
	}
}

// EventsDroppedCount reports how many published events were not delivered to
// some viewer because its control channel was full -- the same fate as a
// dropped frame, one layer up. Publishing is bursty, session-lifetime-rare
// event traffic rather than a per-second stream, so unlike the frame counters
// this has no per-second sampled trace row; the manager reads it once, into
// its stream-end record.
func (s *Session) EventsDroppedCount() uint64 { return s.eventsDropped.Load() }

// EventsAfter returns the kept events with seq greater than after.
func (s *Session) EventsAfter(after uint64) []Event {
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	if events == nil {
		return nil
	}
	return events.after(after)
}

// EventsSince returns this Session's identity together with the events after
// 'after' -- see EventPage's doc comment for how a caller uses Session,
// Oldest and Latest to detect a restart or a wrapped ring, neither of which
// EventsAfter's plain slice can express. Oldest, Latest and Events all come
// from one call to the ring's own page, under the ring's lock, so they
// cannot disagree with one another the way two separate reads could.
func (s *Session) EventsSince(after uint64) EventPage {
	s.mu.RLock()
	key := s.key
	events := s.events
	s.mu.RUnlock()

	if events == nil {
		return EventPage{Session: key, Events: []Event{}}
	}
	oldest, latest, page := events.page(after)
	return EventPage{Session: key, Oldest: oldest, Latest: latest, Events: page}
}

// watchClipboard sends every viewer the device's clipboard each time the
// stream reports it changed. A relaunch calls it again for the new stream;
// the old one's channel closes with it.
func (s *Session) watchClipboard(st target.Stream) {
	cw, ok := st.(target.ClipboardWatcher)
	if !ok {
		return
	}
	changes := cw.ClipboardChanges()
	if changes == nil {
		return
	}
	go func() {
		for text := range changes {
			s.sendClipboard(text)
		}
	}()
}

// sendClipboard queues the clipboard to every viewer. A viewer whose queue
// is full misses it, as it would an event.
func (s *Session) sendClipboard(text string) {
	msg := OutMsg{Data: append([]byte{MsgClipboard}, text...), Control: true, QueuedAt: time.Now()}
	reached := 0
	s.mu.RLock()
	for _, v := range s.viewers {
		select {
		case v.out <- msg:
			reached++
		default:
			s.eventsDropped.Add(1)
		}
	}
	observe := s.onClipboard
	s.mu.RUnlock()
	if observe != nil {
		observe(utf8.RuneCountInString(text), reached)
	}
}

// EndedMessage is the MsgEnded wire message for a reason.
func EndedMessage(reason string) []byte {
	body, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{reason})
	return append([]byte{MsgEnded}, body...)
}

// End tells every viewer why the session is ending, then closes it. The
// message is queued ahead of the close; the viewer loop drains queued control
// messages when it sees the session done, so it is written before the
// connection closes.
func (s *Session) End(reason string) error {
	msg := OutMsg{Data: EndedMessage(reason), Control: true, QueuedAt: time.Now()}
	s.endReason.Store(&reason)
	s.mu.RLock()
	for _, v := range s.viewers {
		select {
		case v.out <- msg:
			continue
		default:
		}
		// Full queue: give up the oldest message to make room, once. Losing
		// a frame costs nothing against a viewer never learning why.
		select {
		case <-v.out:
		default:
		}
		select {
		case v.out <- msg:
		default:
			s.eventsDropped.Add(1)
		}
	}
	s.mu.RUnlock()
	return s.Close()
}

// EndReason is the reason End was called with, or "" if the session has not
// been ended on purpose.
func (s *Session) EndReason() string {
	if p := s.endReason.Load(); p != nil {
		return *p
	}
	return ""
}

// SetLiftObserver installs the hook that records fingers a departing viewer
// left down and this session lifted.
func (s *Session) SetLiftObserver(fn func(viewer uint64, n int)) {
	s.mu.Lock()
	s.onLifted = fn
	s.mu.Unlock()
}

func (s *Session) traceLifted(viewer uint64, n int) {
	s.mu.RLock()
	fn := s.onLifted
	s.mu.RUnlock()
	if fn != nil {
		fn(viewer, n)
	}
}

// SetClipboardObserver installs the hook that records each device clipboard
// sent to viewers, by size.
func (s *Session) SetClipboardObserver(fn func(runes, viewers int)) {
	s.mu.Lock()
	s.onClipboard = fn
	s.mu.Unlock()
}

// SetEventObserver installs the hook that records every published event.
// Like SetResizeObserver and SetBitrateObserver it is set once, before any
// viewer is attached.
//
// PublishEvent calls fn under s.mu -- see its own doc comment for why -- so
// fn must return quickly and must never call back into this Session. The
// manager's own observer honours this by handing the event to a buffered
// channel and doing the slow part (trace.Record) on a separate goroutine.
func (s *Session) SetEventObserver(fn func(Event)) { s.onEvent = fn }

// Done returns a channel closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err reports why the session ended.
func (s *Session) Err() error { return s.currentStream().Err() }

// Close tears down the session and disconnects every viewer.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.streamMu.Lock()
		s.closing = true
		stream := s.stream
		s.streamMu.Unlock()

		// Before the stream goes: an up needs the stream to travel on.
		for _, id := range s.pointers.viewers() {
			s.liftPointers(id)
		}

		// Taking the stream under the lock is what makes this safe against a
		// relaunch in flight: the swap commits under the same lock and checks
		// closing, so either it lost the race and aborts, or it won and this
		// closes the stream it installed. Neither ordering leaves a live
		// encoder behind.
		_ = stream.Close()
		<-s.done
		s.stopControl()
	})
	return nil
}

// minRelaunchPTSGap is the smallest jump the timeline may make across a
// relaunch. The two encoders overlap by design, so the new one's first frames
// are captured while the old one is still draining and the measured gap can be
// zero or negative; a PTS that fails to advance is worse than one that
// advances by a millisecond too much.
const minRelaunchPTSGap = time.Millisecond

// pump is the fan-out loop. It owns every viewer channel's writes.
//
// It reads a succession of streams rather than one, because adapting the
// bitrate means relaunching the encoder and scrcpy has no opcode for it. A
// stream ending is therefore ambiguous: either it was replaced, or the session
// is over. nextStream answers that, under the same lock the swap commits with.
func (s *Session) pump() {
	defer func() {
		close(s.done)

		// Every attached viewer's scroll burst, if it has one open, is
		// flushed before its channel closes -- the same reason RemoveViewer
		// flushes on the way out. Without this, a session that ends mid-burst
		// (the device disconnects, the process shuts down) drops the tail of
		// whatever the viewer was doing, silently: the trailing timer these
		// viewers are still waiting on never gets to fire, because nothing
		// here cancels it and nothing here waits for it either -- it is
		// simply beaten to the punch.
		s.mu.Lock()
		ids := make([]uint64, 0, len(s.viewers))
		viewers := make([]*viewer, 0, len(s.viewers))
		for id, v := range s.viewers {
			ids = append(ids, id)
			viewers = append(viewers, v)
		}
		w, h := s.init.CodedWidth, s.init.CodedHeight
		s.mu.Unlock()

		for i, v := range viewers {
			s.flushViewerScroll(ids[i], v, w, h)
		}

		s.mu.Lock()
		for id, v := range s.viewers {
			close(v.out)
			delete(s.viewers, id)
		}
		s.mu.Unlock()
	}()

	// Reused across frames so the common path allocates only when a frame is
	// larger than the previous high-water mark.
	var scratch []byte

	// scrcpy restarts every session's PTS at zero, so a relaunch would hand the
	// client a timeline that jumps backwards by the length of the session so
	// far. Nothing crashes: the jitter buffer anchors its clock on the first
	// PTS it sees and never re-anchors on its own, so every later frame reads
	// as already due and presentation silently degrades to draw-on-arrival for
	// the rest of the session. Offsetting here keeps the client unchanged,
	// which matters because web/app.js has no test harness and this does.
	var ptsOffset, lastPTS time.Duration
	var lastCaptured time.Time

	var prev target.Stream
	for {
		stream := s.nextStream(prev)
		if stream == nil {
			return
		}
		prev = stream
		first := true

		for frame := range stream.Frames() {
			captured := frame.Captured
			if captured.IsZero() {
				captured = time.Now()
			}
			if first {
				first = false
				if !lastCaptured.IsZero() {
					// Advance the timeline by the wall-clock gap, so the
					// client's presentation clock stays valid across the swap
					// instead of being left a fixed amount behind it.
					gap := captured.Sub(lastCaptured)
					if gap < minRelaunchPTSGap {
						gap = minRelaunchPTSGap
					}
					ptsOffset = lastPTS + gap - frame.PTS
				}
			}
			pts := frame.PTS + ptsOffset
			lastPTS = pts
			lastCaptured = captured

			// The first parameter set is the only place the true codec string
			// can be read. Doing it here rather than trusting the container
			// metadata is what keeps hardware decode engaged on iOS.
			if frame.Config {
				if !s.codecResolved {
					s.resolveCodec(frame.Data)
				}
				// A rotation changes the picture's shape without ending the
				// stream or resending the codec meta, so this is the only
				// notice the client gets that its canvas is now the wrong way
				// round. The stream has already read the new size off the same
				// parameter set; see adbStream.noteConfig.
				s.noteSize(stream.Meta())
			}

			s.framesIn.Add(1)
			if frame.Key {
				s.keysIn.Add(1)
			}
			if frame.Config {
				s.configIn.Add(1)
			} else {
				s.mediaIn.Add(1)
			}

			sentAt := uint64(time.Now().UnixMicro())
			scratch = EncodeFrame(scratch, uint64(pts.Microseconds()), frame.Key, frame.Config, sentAt, frame.Data)

			// Each viewer needs its own copy: the scratch buffer is overwritten
			// on the next iteration, and the channel hands it off
			// asynchronously.
			msg := OutMsg{
				Data:      make([]byte, len(scratch)),
				Key:       frame.Key,
				Config:    frame.Config,
				QueuedAt:  time.Now(),
				PTSMicros: uint64(pts.Microseconds()),
			}
			copy(msg.Data, scratch)

			if frame.Config {
				cfg := msg
				s.mu.Lock()
				s.lastCfg = &cfg
				s.mu.Unlock()
			}

			s.fanTaps(target.Frame{PTS: pts, Key: frame.Key, Config: frame.Config, Data: frame.Data, Captured: captured})

			s.broadcast(msg)
		}
	}
}

// nextStream reports the stream pump should read now, or nil once there will
// not be another one.
//
// prev is the stream that just ended. If it is still the session's stream then
// nobody replaced it and it died on its own, which ends the session; ended is
// latched here so a relaunch racing this decision aborts rather than installing
// an encoder no one will read.
func (s *Session) nextStream(prev target.Stream) target.Stream {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()

	if s.closing || (prev != nil && s.stream == prev) {
		s.ended = true
		return nil
	}
	return s.stream
}

// noteSize republishes init when the stream's dimensions change under it.
//
// scrcpy sends its codec meta once and never again, so a rotation leaves every
// viewer with a canvas of the old shape: the picture is squashed into it, and
// because the client derives touch coordinates from the canvas, every tap is
// computed in a space the device no longer has. The device then discards them
// outright rather than misplacing them -- PositionMapper rejects a size that
// does not match the encoder's current one.
//
// Init is the right carrier rather than a new message type. The client already
// treats a change of codedWidth or codedHeight as grounds to resize the canvas
// and reconfigure the decoder, which is exactly what a rotation needs, and a
// viewer that joins afterwards has to be told the same thing anyway.
func (s *Session) noteSize(meta target.Meta) {
	if meta.Width <= 0 || meta.Height <= 0 {
		return
	}

	s.mu.Lock()
	if s.init.CodedWidth == meta.Width && s.init.CodedHeight == meta.Height {
		s.mu.Unlock()
		return
	}
	oldW, oldH := s.init.CodedWidth, s.init.CodedHeight
	s.init.CodedWidth, s.init.CodedHeight = meta.Width, meta.Height
	s.sendInitLocked()
	s.mu.Unlock()

	if s.onResize != nil {
		s.onResize(oldW, oldH, meta.Width, meta.Height)
	}
}

// resolveCodec derives the codec string from the stream's parameter sets and
// resends init so viewers that joined before it was known get the corrected value.
func (s *Session) resolveCodec(cfg []byte) {
	sps := h264.FindSPS(cfg)
	if sps == nil {
		// Measured with -video-codec h265: the stream carries fine end to end,
		// but an HEVC parameter set has no NAL type 7 in it, so this finds
		// nothing and the client is handed the provisional avc1 string and
		// fails in the decoder. Say so here rather than leaving the wrong
		// codec to explain itself.
		s.mu.RLock()
		codec := s.init.Codec
		s.mu.RUnlock()
		fmt.Printf("[session %s] no H.264 SPS in the parameter set; "+
			"leaving codec as %q (a non-H.264 stream needs its own parser)\n",
			s.ID, codec)
		return
	}
	codec, err := h264.CodecString(sps)
	if err != nil {
		fmt.Printf("[session %s] could not derive codec string: %v\n", s.ID, err)
		return
	}
	s.mu.Lock()
	s.init.Codec = codec
	s.codecResolved = true
	// Viewers that joined before the codec was known need the corrected init.
	s.sendInitLocked()
	s.mu.Unlock()
}

// sendInitLocked queues init to every viewer without blocking. The
// caller must hold mu.
//
// The sends happen under the lock, not after a copy of the viewer list, because
// RemoveViewer deletes a viewer from the map under the same lock and closes its
// channel after releasing it. Sending only to viewers still in the map, while
// holding mu, is what keeps a send from reaching a closed channel. A viewer
// leaving between a copy and the send turned the send into "send on closed
// channel", which panics the producer goroutine and takes the whole process
// with it. broadcast and PublishEvent hold the lock across their sends for the
// same reason.
func (s *Session) sendInitLocked() {
	for _, v := range s.viewers {
		select {
		case v.out <- s.buildInit(v.id):
		default:
		}
	}
}

// hasRoom reports whether a keyframe sent to this viewer could actually land.
// Half the buffer rather than one slot, because a keyframe that fits by itself
// and is then followed by inter-frames into a full channel re-latches
// immediately, which is the same loop one frame further on.
func hasRoom(v *viewer) bool { return len(v.out)*2 <= cap(v.out) }

func (s *Session) broadcast(msg OutMsg) {
	starved := false

	s.mu.RLock()
	for _, v := range s.viewers {
		// A viewer that fell behind is resynchronizing. Parameter sets and
		// keyframes move it forward; anything else would decode into garbage.
		if v.needsKey && !msg.Key && !msg.Config {
			// Counted, because otherwise this branch increments nothing and a
			// latched viewer looks exactly like a device that stopped sending:
			// framesOut, framesDrop and framesShed all freeze in the same
			// second. That ambiguity cost a debugging session.
			s.framesWait.Add(1)
			// Ask again once the viewer has somewhere to put the answer. The
			// edge ask below fires when the latch closes, but if that keyframe
			// arrives while the channel is still full it is dropped like
			// anything else, and nothing would ever ask a second time.
			if hasRoom(v) {
				starved = true
			}
			continue
		}

		select {
		case v.out <- msg:
			s.framesOut.Add(1)
			if msg.Key {
				v.needsKey = false
			}
		default:
			// Buffer full. Drop and mark the viewer for resync rather than
			// blocking the whole fan-out on one slow client.
			s.framesDrop.Add(1)
			// Only the edge asks. A keyframe skips the resync branch above --
			// it is the frame that ends the wait -- so it arrives here, and a
			// full channel drops it. Asking on every drop therefore asked again
			// for the thing that had just been thrown away, which the encoder
			// answered, which was thrown away in turn: a stalled viewer turned
			// the whole stream into keyframes, the largest frames there are, on
			// a link already too slow. emulator-5554 ended a 121s session with
			// drop 746 and framesOut 737 having suppressed nothing at all.
			if !v.needsKey {
				v.needsKey = true
				starved = true
			}
		}
	}
	s.mu.RUnlock()

	// A viewer marked for resync above waits for a keyframe, and scrcpy does
	// not emit them on a timer, so without asking it would wait forever. The
	// viewer loop already asks when it sheds; this is the same latch one layer
	// up, reached when the fan-out channel fills instead.
	// This request is the only thing that ends the blackout, so it takes the
	// short floor and is never discarded. An already-latched viewer reaches
	// here on nearly every frame, which is the once-per-frame control message
	// the old limiter existed to prevent -- askKeyframeSoon coalesces instead,
	// holding at most one request outstanding.
	if starved {
		s.askKeyframeSoon()
	}
}

// keyframeRequestInterval bounds how often the fan-out asks the encoder for a
// keyframe. Under sustained starvation the drop branch fires on nearly every
// frame, which would otherwise be one control message per frame.
const keyframeRequestInterval = 500 * time.Millisecond

// keyframeEdgeInterval is the floor for the ask that follows a viewer going
// dark, which is a different question from how often to repeat one. 500ms was
// a floor under every blackout in the 09:53 trace: the viewer could do nothing
// until a keyframe arrived and nothing had yet asked for one. 50ms still bounds
// the control channel at 20 messages a second, which a stall cannot exceed
// anyway.
const keyframeEdgeInterval = 50 * time.Millisecond

// askKeyframe requests a keyframe at most once per keyframeRequestInterval.
// The request runs off the producer goroutine because the control socket write
// can block for as long as its own deadline, and stalling the producer is the
// failure this is here to prevent.
func (s *Session) askKeyframe() {
	now := time.Now()
	s.keyMu.Lock()
	if !s.lastKeyReq.IsZero() && now.Sub(s.lastKeyReq) < keyframeRequestInterval {
		s.keyMu.Unlock()
		return
	}
	s.lastKeyReq = now
	s.keyMu.Unlock()

	go func() { _ = s.currentStream().RequestKeyframe() }()
}

// askKeyframeSoon requests a keyframe for a viewer that has just gone dark.
//
// Unlike askKeyframe it never discards the request, only delays it. Dropping
// one inside the window is what put a floor under the blackouts: the ask that
// would have ended it was swallowed by an unrelated earlier one, and nothing
// retried. At most one request is ever outstanding, so a run of latches
// coalesces into a single ask rather than one per frame.
func (s *Session) askKeyframeSoon() {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	if s.keyPending {
		return
	}
	if wait := keyframeEdgeInterval - time.Since(s.lastKeyReq); wait > 0 {
		s.keyPending = true
		time.AfterFunc(wait, func() {
			s.keyMu.Lock()
			s.keyPending = false
			s.lastKeyReq = time.Now()
			s.keyMu.Unlock()
			_ = s.currentStream().RequestKeyframe()
		})
		return
	}
	s.lastKeyReq = time.Now()
	go func() { _ = s.currentStream().RequestKeyframe() }()
}
