package session

import (
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
	"github.com/bmcreations/loupe/internal/target"
)

// The remote track is a second H.264 encode for viewers that are not on this
// machine and that ack their frames. The encoder is gated: it encodes a capture
// only while every such viewer's link has room, so each frame it makes is
// delivered and none is dropped, which means no keyframe is needed to recover
// from a drop. See docs/superpowers/plans/2026-11-09-remote-encoder.md.

const (
	// remoteGateTick is how often the gate is re-read.
	remoteGateTick = 10 * time.Millisecond
	// remoteSilence is how long a viewer may go without acking before the gate
	// stops waiting for it: a hidden tab stops acking and holding the gate
	// for it would freeze every other remote viewer.
	remoteSilence = 4 * time.Second
	// remoteSlack is how far past the path's base round trip the oldest
	// unacked frame may be before the gate holds.
	remoteSlack = 120 // ms
	// remoteMaxBacklog is the most frames in flight, unacked, the gate allows.
	remoteMaxBacklog = 8
	// DefaultRemoteCapBps is the remote encoder's data-rate cap until rate
	// control sets one.
	DefaultRemoteCapBps = 4_000_000
	// remoteKeyInterval bounds how often the fan-out asks for a remote
	// keyframe on its own account (a full viewer queue).
	remoteKeyInterval = 500 * time.Millisecond
)

// remoteTrack is the remote track's state. Fields without a note are guarded
// by Session.mu.
type remoteTrack struct {
	stream target.RemoteStream
	codec  string  // RFC 6381 from the first SPS; empty until known
	cfg    *OutMsg // most recent parameter sets, replayed to joiners

	// gate state, owned by the gate loop.
	gateStream target.RemoteStream // the stream sentOpen and sentCap describe
	sentOpen   bool
	sentCap    int // the cap the encoder holds; a new encoder starts at the default
	rate       *remoteRate

	lastKeyAsk atomic.Int64 // unix nanos of the last fan-out keyframe ask

	// Per-second trace counters.
	frames, keys   atomic.Uint64
	openNs, heldNs atomic.Int64
	capBps         atomic.Int64
	bytes          atomic.Uint64 // cumulative media bytes the encoder made
	reasonMu       sync.Mutex
	reason         string // last cap decision, or the last change within the second
	reasonChanged  bool
	gateOpen       atomic.Bool
}

// RemoteSecond is one second of the remote track, for the trace.
type RemoteSecond struct {
	// Gate is the fraction of the second the gate was open.
	Gate float64
	// HeldMs is how long it was held.
	HeldMs float64
	// Frames and Keys are the media frames and keyframes the encoder made.
	Frames, Keys uint64
	// CapBps is the encoder's data-rate cap.
	CapBps int64
	// RateReason is why rate control last decided what it did this second:
	// slowstart, congested, recover, probe, idle or hold. A decision that
	// changed the cap wins over the holds after it.
	RateReason string
	// Viewers is how many viewers were on the track.
	Viewers int
}

// SetRemoteObserver installs the callback that receives the remote track's
// per-second reading. Like SetBitrateObserver it is set before viewers attach.
func (s *Session) SetRemoteObserver(fn func(RemoteSecond)) {
	s.mu.Lock()
	s.onRemote = fn
	s.mu.Unlock()
}

// replayCfgLocked queues the remote parameter sets to a joiner, stamped now.
// The caller holds mu.
func (r *remoteTrack) replayCfgLocked(v *viewer) {
	if r.cfg == nil {
		return
	}
	cfg := *r.cfg
	cfg.QueuedAt = time.Now()
	cfg.Data = append([]byte(nil), cfg.Data...)
	binary.BigEndian.PutUint64(cfg.Data[10:], uint64(cfg.QueuedAt.UnixMicro()))
	v.out <- cfg
}

// startRemote begins reading stream's remote track, if it has one. It is
// called for each stream the session reads, since a relaunched stream is a new
// simcap with a new, held, remote encoder.
func (s *Session) startRemote(stream target.Stream) {
	if s.attachRemote(stream) {
		go s.remoteLoop()
	}
}

// attachRemote is startRemote without the gate loop, which it reports whether
// the caller should start: true for the session's first remote stream.
func (s *Session) attachRemote(stream target.Stream) (first bool) {
	rs, ok := stream.(target.RemoteStream)
	if !ok {
		return false
	}
	frames := rs.Remote()
	if frames == nil {
		return false
	}
	s.mu.Lock()
	first = s.remote == nil
	if first {
		s.remote = &remoteTrack{}
		s.remote.capBps.Store(DefaultRemoteCapBps)
		s.remote.rate = newRemoteRate()
		s.remote.sentCap = DefaultRemoteCapBps
	}
	r := s.remote
	r.stream = rs
	r.cfg = nil
	for _, v := range s.viewers {
		if v.track == trackRemote {
			v.needsKey = true
		}
	}
	s.mu.Unlock()
	go s.pumpRemote(frames)
	return first
}

// pumpRemote is pump for the remote track. A remote viewer's own acks mean the
// frames are not offset onto the main track's clock.
func (s *Session) pumpRemote(frames <-chan target.Frame) {
	var scratch []byte
	resolved := false
	for frame := range frames {
		if frame.Config && !resolved {
			if sps := h264.FindSPS(frame.Data); sps != nil {
				if codec, err := h264.CodecString(sps); err == nil {
					resolved = true
					s.mu.Lock()
					s.remote.codec = codec
					s.sendInitLocked()
					s.mu.Unlock()
				} else {
					fmt.Printf("[session %s] remote codec string: %v\n", s.ID, err)
				}
			}
		}
		sentAt := uint64(time.Now().UnixMicro())
		scratch = EncodeFrame(scratch, uint64(frame.PTS.Microseconds()), frame.Key, frame.Config, sentAt, frame.Data)
		msg := OutMsg{
			Data:      make([]byte, len(scratch)),
			Key:       frame.Key,
			Config:    frame.Config,
			QueuedAt:  time.Now(),
			PTSMicros: uint64(frame.PTS.Microseconds()),
			track:     trackRemote,
		}
		copy(msg.Data, scratch)
		if !frame.Config {
			s.remote.frames.Add(1)
			s.remote.bytes.Add(uint64(len(frame.Data)))
		}
		if frame.Key {
			s.remote.keys.Add(1)
		}
		if frame.Config {
			cfg := msg
			s.mu.Lock()
			s.remote.cfg = &cfg
			s.mu.Unlock()
		}
		s.broadcast(msg)
	}
}

// remoteStream is the live remote stream, nil without one.
func (s *Session) remoteStream() target.RemoteStream {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.remote == nil {
		return nil
	}
	return s.remote.stream
}

// RequestRemoteKeyframe asks the remote encoder for a keyframe. It does not
// rate limit: callers do, in the way that fits them.
func (s *Session) RequestRemoteKeyframe() error {
	rs := s.remoteStream()
	if rs == nil {
		return target.ErrNoControl
	}
	return rs.RemoteKeyframe()
}

// askRemoteKeyframeSoon is the fan-out's ask, at most one per remoteKeyInterval.
func (s *Session) askRemoteKeyframeSoon() {
	s.mu.RLock()
	r := s.remote
	s.mu.RUnlock()
	if r == nil {
		return
	}
	now := time.Now().UnixNano()
	last := r.lastKeyAsk.Load()
	if now-last < int64(remoteKeyInterval) || !r.lastKeyAsk.CompareAndSwap(last, now) {
		return
	}
	go func() { _ = s.RequestRemoteKeyframe() }()
}

// gateViewer is what the gate reads about one remote-track viewer.
type gateViewer struct {
	Acking     bool
	LastAckAgo time.Duration
	SinceJoin  time.Duration
	OldestMs   float64
	BaseMs     float64
	Backlog    int
}

// silent reports whether the gate should stop waiting for the viewer: no ack
// for remoteSilence, or, for one that has never acked, no ack since it joined.
func (g gateViewer) silent() bool {
	if g.Acking {
		return g.LastAckAgo > remoteSilence
	}
	return g.SinceJoin > remoteSilence
}

// remoteGateOpen is the gate decision: held with no viewers, otherwise open
// unless a viewer that has acked lately has a frame in flight longer than its
// base round trip plus remoteSlack, or more than remoteMaxBacklog of them.
func remoteGateOpen(vs []gateViewer) bool {
	if len(vs) == 0 {
		return false
	}
	for _, g := range vs {
		if g.silent() {
			continue
		}
		if g.OldestMs > g.BaseMs+remoteSlack || g.Backlog > remoteMaxBacklog {
			return false
		}
	}
	return true
}

// remoteRateViewers reads rate control's inputs: every remote viewer that is
// acking and not silent. The second result is whether any viewer is on the
// track at all.
func (s *Session) remoteRateViewers(now time.Time) (vs []rateViewer, any bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.viewers {
		if v.track != trackRemote {
			continue
		}
		any = true
		f := v.flow.Snapshot(now)
		g := gateViewer{Acking: f.Acking, LastAckAgo: f.LastAckAgo, SinceJoin: now.Sub(v.joined)}
		if !f.Acking || g.silent() {
			continue
		}
		vs = append(vs, rateViewer{QueueMs: f.QueueMs, OldestMs: f.OldestMs, BaseMs: f.BaseMs, DeliveredBps: f.DeliveredBps})
	}
	return vs, any
}

// remoteRateTick runs rate control once and sends 0x84 if the cap changed.
// It never touches the main encoder or its adapter.
func (s *Session) remoteRateTick(now time.Time, r *remoteTrack, rs target.RemoteStream) {
	vs, any := s.remoteRateViewers(now)
	if !any {
		// Nobody on the track: the next viewer starts from the default.
		r.rate.Reset()
	}
	bps, reason := r.rate.Tick(now, vs, r.bytes.Load())
	r.capBps.Store(int64(bps))
	r.reasonMu.Lock()
	changed := reason != rateHold && reason != rateIdle
	if changed || !r.reasonChanged {
		r.reason = reason
	}
	r.reasonChanged = r.reasonChanged || changed
	r.reasonMu.Unlock()
	if bps != r.sentCap {
		if err := rs.RemoteCap(bps); err == nil {
			r.sentCap = bps
		}
	}
}

// remoteViewers reads the gate's inputs from the remote-track viewers.
func (s *Session) remoteViewers(now time.Time) []gateViewer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []gateViewer
	for _, v := range s.viewers {
		if v.track != trackRemote {
			continue
		}
		f := v.flow.Snapshot(now)
		out = append(out, gateViewer{
			Acking: f.Acking, LastAckAgo: f.LastAckAgo, SinceJoin: now.Sub(v.joined),
			OldestMs: f.OldestMs, BaseMs: f.BaseMs, Backlog: f.Backlog,
		})
	}
	return out
}

// remoteGateTick evaluates the gate once and sends 0x82 if it changed. dt is
// the time since the last tick, which the trace's open fraction is made of.
func (s *Session) remoteGateTick(now time.Time, dt time.Duration) {
	s.mu.RLock()
	r := s.remote
	var rs target.RemoteStream
	if r != nil {
		rs = r.stream
	}
	s.mu.RUnlock()
	if rs == nil {
		return
	}
	open := remoteGateOpen(s.remoteViewers(now))
	// A new stream's encoder starts held.
	if r.gateStream != rs {
		r.gateStream, r.sentOpen, r.sentCap = rs, false, DefaultRemoteCapBps
	}
	if open != r.sentOpen {
		if err := rs.RemoteGate(open); err == nil {
			r.sentOpen = open
		}
	}
	s.remoteRateTick(now, r, rs)
	r.gateOpen.Store(r.sentOpen)
	if r.sentOpen {
		r.openNs.Add(int64(dt))
	} else {
		r.heldNs.Add(int64(dt))
	}
}

// remoteLoop runs the gate every remoteGateTick and reports each second.
func (s *Session) remoteLoop() {
	tick := time.NewTicker(remoteGateTick)
	defer tick.Stop()
	last := time.Now()
	report := last
	for {
		select {
		case <-s.done:
			return
		case now := <-tick.C:
			s.remoteGateTick(now, now.Sub(last))
			last = now
			if now.Sub(report) >= time.Second {
				report = now
				s.reportRemote()
			}
		}
	}
}

// reportRemote hands the observer the last second and starts the next.
func (s *Session) reportRemote() {
	s.mu.RLock()
	r, fn := s.remote, s.onRemote
	n := 0
	for _, v := range s.viewers {
		if v.track == trackRemote {
			n++
		}
	}
	s.mu.RUnlock()
	if r == nil {
		return
	}
	open, held := r.openNs.Swap(0), r.heldNs.Swap(0)
	rec := RemoteSecond{
		HeldMs:  float64(held) / float64(time.Millisecond),
		Frames:  r.frames.Swap(0),
		Keys:    r.keys.Swap(0),
		CapBps:  r.capBps.Load(),
		Viewers: n,
	}
	r.reasonMu.Lock()
	rec.RateReason = r.reason
	r.reason, r.reasonChanged = "", false
	r.reasonMu.Unlock()
	if total := open + held; total > 0 {
		rec.Gate = float64(open) / float64(total)
	}
	if fn != nil {
		fn(rec)
	}
}
