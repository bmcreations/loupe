package session

import (
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
		if frame.Config {
			cfg := msg
			s.mu.Lock()
			s.pass.cfg = &cfg
			s.mu.Unlock()
		}
		s.broadcast(msg)
	}
}
