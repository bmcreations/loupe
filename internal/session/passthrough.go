package session

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/hevc"
	"github.com/bmcreations/loupe/internal/target"
)

// ViewerOptions is what a viewer said about itself when it attached.
type ViewerOptions struct {
	// Decodes lists the fourccs the viewer can decode besides H.264, which
	// every viewer can. A viewer that lists the stream's passthrough codec
	// gets that track instead: a physical iPhone's HEVC, sent as the phone
	// encoded it, with no decode and H.264 re-encode in simcap. Browsers do
	// not list anything, because Safari cannot decode that HEVC.
	Decodes []string
}

// ParseDecode reads ?decode=, a comma-separated fourcc list ("h265").
func ParseDecode(q string) []string {
	var out []string
	for _, f := range strings.Split(q, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (o ViewerOptions) decodes(fourcc string) bool { return slices.Contains(o.Decodes, fourcc) }

// passTrack is the passthrough track's state. Its frames reach only viewers
// with passthrough set, and the H.264 track's only those without, so each
// viewer sees one coherent stream. Touches still map through init's
// width and height, the H.264 track's, which is the same picture.
type passTrack struct {
	fourcc string
	codec  string  // RFC 6381, from the first SPS
	cfg    *OutMsg // most recent parameter sets, replayed to joiners

	// gop is the last keyframe and every frame since, replayed to a joiner
	// after cfg. The phone keys only when its screen changes, and ignores a
	// keyframe request while it is still, so a viewer that joined a still
	// phone and waited for the next keyframe got no picture (awaitKey 73 on
	// bmc-iphone, 14:20). Nil until the first keyframe, and after an
	// overflow until the next one.
	gop      []OutMsg
	gopBytes int
	// seq numbers the track's frames. A joiner's replay ends at the frame
	// the pump last cached, which may not have been broadcast yet; broadcast
	// skips frames at or below the joiner's replayedThrough.
	seq uint64
}

// gopMaxFrames and gopMaxBytes bound the replay cache. A GOP past either is
// dropped whole, since a replay that starts after the keyframe cannot be
// decoded, and the phone is asked for a keyframe so the next GOP starts
// short. At 60fps that is a keyframe at least every 4s while the screen
// moves; the phone's own interval is about 8s.
const (
	gopMaxFrames = maxViewerQueue
	gopMaxBytes  = 8 << 20
)

// cacheLocked adds msg to the replay cache and numbers it. The caller holds
// mu. It reports whether the GOP just overflowed.
func (p *passTrack) cacheLocked(msg *OutMsg) (overflow bool) {
	p.seq++
	msg.seq = p.seq
	switch {
	case msg.Config:
		// Kept in cfg and replayed ahead of the GOP.
	case msg.Key:
		p.gop = append(p.gop[:0:0], *msg)
		p.gopBytes = len(msg.Data)
	case p.gop != nil:
		p.gop = append(p.gop, *msg)
		p.gopBytes += len(msg.Data)
		if len(p.gop) > gopMaxFrames || p.gopBytes > gopMaxBytes {
			p.gop, p.gopBytes = nil, 0
			return true
		}
	}
	return false
}

// replayLocked queues cfg and the cached GOP to a passthrough joiner whose
// channel has room for them, and reports whether the GOP went. The caller
// holds mu. Each message is stamped as sent now: the viewer loop sheds a
// frame by how long ago it was queued, and the client reads latency from
// the envelope's sent-at.
func (p *passTrack) replayLocked(v *viewer) bool {
	now := time.Now()
	restamp := func(m OutMsg) OutMsg {
		m.QueuedAt = now
		m.Data = append([]byte(nil), m.Data...)
		binary.BigEndian.PutUint64(m.Data[10:], uint64(now.UnixMicro()))
		return m
	}
	if p.cfg != nil {
		v.out <- restamp(*p.cfg)
	}
	if p.gop == nil {
		return false
	}
	for _, m := range p.gop {
		v.out <- restamp(m)
	}
	v.needsKey = false
	v.replayedThrough = p.gop[len(p.gop)-1].seq
	return true
}

// startPassthrough begins reading stream's passthrough track, if it has one.
// The track is decided once per session: a relaunched stream keeps the
// session's H.264 track going, and a passthrough viewer of the old stream
// stops receiving pictures (see the protocol doc).
func (s *Session) startPassthrough(stream target.Stream) {
	ps, ok := stream.(target.PassthroughStream)
	if !ok {
		return
	}
	frames := ps.Passthrough()
	if frames == nil {
		return
	}
	s.mu.Lock()
	s.pass = &passTrack{fourcc: ps.PassthroughCodec(), codec: "hvc1"}
	s.mu.Unlock()
	go s.pumpPassthrough(frames)
}

// pumpPassthrough is pump for the passthrough track, without the relaunch
// bookkeeping. Its PTS is on the H.264 track's clock (simcap stamps both from
// the same t0), so the stats and latency numbers compare.
func (s *Session) pumpPassthrough(frames <-chan target.Frame) {
	var scratch []byte
	resolved := false
	for frame := range frames {
		if frame.Config && !resolved {
			if sps := hevc.FindSPS(frame.Data); sps != nil {
				if codec, err := hevc.CodecString(sps); err == nil {
					resolved = true
					s.mu.Lock()
					s.pass.codec = codec
					for _, v := range s.viewers {
						if v.passthrough {
							select {
							case v.out <- s.buildInitFor(v):
							default:
							}
						}
					}
					s.mu.Unlock()
				} else {
					fmt.Printf("[session %s] passthrough codec string: %v\n", s.ID, err)
				}
			}
		}
		sentAt := uint64(time.Now().UnixMicro())
		scratch = EncodeFrame(scratch, uint64(frame.PTS.Microseconds()), frame.Key, frame.Config, sentAt, frame.Data)
		msg := OutMsg{
			Data:        make([]byte, len(scratch)),
			Key:         frame.Key,
			Config:      frame.Config,
			QueuedAt:    time.Now(),
			PTSMicros:   uint64(frame.PTS.Microseconds()),
			passthrough: true,
		}
		copy(msg.Data, scratch)
		if !frame.Config {
			s.passMediaIn.Add(1)
		}
		s.mu.Lock()
		if frame.Config {
			cfg := msg
			s.pass.cfg = &cfg
		}
		overflow := s.pass.cacheLocked(&msg)
		s.mu.Unlock()
		if overflow {
			s.askKeyframeSoon()
		}
		s.broadcast(msg)
	}
	// The track ended; a joiner would get pictures of a stream that is gone.
	s.mu.Lock()
	s.pass.gop, s.pass.gopBytes = nil, 0
	s.mu.Unlock()
}
