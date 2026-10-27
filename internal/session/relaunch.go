package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// relaunchTimeout bounds one attempt to bring a new encoder up. A device
// launch is a jar push, an adb forward and a socket dial with its own retry
// budget, so this is generous; the point is only that a wedged adb cannot
// leave the controller believing a relaunch is still in flight forever.
const relaunchTimeout = 30 * time.Second

// bitrateSampleInterval is how often the controller is fed. It matches the
// trace record, so a decision can be read back against the same rows that
// produced it.
const bitrateSampleInterval = time.Second

var (
	errSessionOver      = errors.New("session has ended")
	errRelaunchInFlight = errors.New("a relaunch is already in flight")
)

// Relaunch reopens the target at bitRate and swaps the result in underneath
// the live session.
//
// It is a relaunch rather than a control message because scrcpy has no bitrate
// opcode: opcodes run 0 to 17 and the encoder's rate is fixed at launch. The
// swap happens inside the session rather than by tearing it down and letting
// viewers reconnect, because iOS reconnect is the fragile path -- a page that
// is merely backgrounded can lose its reconnect timer entirely.
//
// The new encoder is opened before the old one is closed, so a relaunch that
// fails leaves the session exactly as it was. The two overlap for as long as
// the launch takes, which costs the device a second encoder for that window
// and is the price of not making a transient adb failure fatal to a session
// that was working.
func (s *Session) Relaunch(bitRate int) error {
	if bitRate <= 0 {
		return fmt.Errorf("relaunch: bitrate %d is not positive", bitRate)
	}

	s.streamMu.Lock()
	if s.closing || s.ended {
		s.streamMu.Unlock()
		return errSessionOver
	}
	if s.relaunching {
		s.streamMu.Unlock()
		return errRelaunchInFlight
	}
	s.relaunching = true
	opts := s.opts
	s.streamMu.Unlock()

	opts.BitRate = bitRate

	// Not the context Open was handed. That one belongs to the HTTP request
	// that started the session and is cancelled the moment that request
	// returns, which is minutes before a relaunch would use it.
	ctx, cancel := context.WithTimeout(context.Background(), relaunchTimeout)
	defer cancel()
	next, err := s.target.Open(ctx, opts)

	s.streamMu.Lock()
	s.relaunching = false
	if err != nil {
		s.streamMu.Unlock()
		return fmt.Errorf("relaunch at %d bps: %w", bitRate, err)
	}
	if s.closing || s.ended {
		s.streamMu.Unlock()
		_ = next.Close()
		return errSessionOver
	}
	prev := s.stream
	s.stream = next
	s.opts = opts
	s.streamMu.Unlock()

	// Closing the old stream is what ends pump's inner loop, and it happens
	// only after the swap is committed. That ordering is what lets pump tell a
	// replaced stream from one that died: it re-reads the session's stream and
	// finds a different one.
	_ = prev.Close()

	s.watchClipboard(next)
	s.watchRelayed(next)
	s.resync()
	return nil
}

// resync puts every viewer back into the keyframe wait after a swap.
//
// A viewer's queue still holds frames from the old encoder, and the new one
// starts with its own parameter sets and keyframe. Marking them here means the
// fan-out suppresses the inter-frames in between rather than handing the
// decoder pictures that reference a GOP it no longer has. Nothing needs to ask
// for the keyframe: a fresh scrcpy session emits one as its first picture.
//
// codecResolved is cleared because the codec string is derived from the SPS,
// and H.264 level is a function of bitrate as well as resolution, so a
// relaunch can legitimately change it.
func (s *Session) resync() {
	s.mu.Lock()
	s.codecResolved = false
	for _, v := range s.viewers {
		v.needsKey = true
	}
	s.mu.Unlock()
}

// CurrentBitRate reports the bitrate the live encoder was launched with.
func (s *Session) CurrentBitRate() int {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return s.opts.BitRate
}

// AdaptBitRate runs the bitrate controller for this session until it ends, and
// reports whether it started one.
//
// Synthetic targets adapt like a device: ffmpeg is launched with BitRate as its
// -b:v, -maxrate and -bufsize, and a relaunch starts a new ffmpeg at the new
// rate. Measured through wtprobe on a spare port, 4 Mbps delivered 3.34 Mbps
// and 500 kbps delivered 0.40 Mbps, both at 61fps. That makes synthetic the
// one target the controller can be driven through without hardware.
//
// The return value exists so that a refusal is visible -- a controller that
// silently did not run is indistinguishable from one that ran and found
// nothing to do.
//
// A simulator runs only when its stream can change bitrate live. A relaunch
// would open a second simcap beside the live one, and that overlap has not
// been run: cmd/overlaptest proved it for scrcpy, and nothing has for
// CoreSimulator.
func (s *Session) AdaptBitRate() bool {
	if s.target.Kind() == target.KindSimulator && !s.liveBitRate() {
		return false
	}
	go s.adaptLoop(s.newBitrateAdapter())
	return true
}

// newBitrateAdapter picks the adapter whose windows match what a change will
// cost on this session's encoder. It is decided once: a stream that changes
// bitrate live does so for the life of the session, and one that does not is
// never swapped for one that does -- a simulator is never relaunched, and a
// device's relaunch opens another scrcpy server.
func (s *Session) newBitrateAdapter() *BitrateAdapter {
	if s.liveBitRate() {
		return NewLiveBitrateAdapter(s.CurrentBitRate())
	}
	return NewBitrateAdapter(s.CurrentBitRate())
}

// liveBitRate reports whether the current stream can change bitrate without a
// relaunch.
func (s *Session) liveBitRate() bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	lb, ok := s.stream.(target.LiveBitRater)
	return ok && lb.LiveBitRate()
}

// changeBitRate applies a controller decision: live when the encoder can take
// it, by relaunch otherwise. It reports which, for the trace.
//
// A live change skips resync. The encoder keeps its GOP, so no viewer has
// anything to wait for, and none of the counters the controller reads are
// disturbed by a restart.
func (s *Session) changeBitRate(bitRate int) (live bool, err error) {
	s.streamMu.Lock()
	if s.closing || s.ended {
		s.streamMu.Unlock()
		return false, errSessionOver
	}
	if lb, ok := s.stream.(target.LiveBitRater); ok && lb.LiveBitRate() && !s.relaunching {
		// Under streamMu, so a relaunch cannot swap the stream between the
		// write and the record of what it is running at.
		err := lb.SetBitRate(bitRate)
		if err == nil {
			s.opts.BitRate = bitRate
		}
		s.streamMu.Unlock()
		if err != nil {
			return true, fmt.Errorf("live bitrate %d bps: %w", bitRate, err)
		}
		return true, nil
	}
	s.streamMu.Unlock()
	if s.target.Kind() == target.KindSimulator {
		return false, fmt.Errorf("bitrate %d bps: simulators change bitrate live or not at all", bitRate)
	}
	return false, s.Relaunch(bitRate)
}

func (s *Session) adaptLoop(a *BitrateAdapter) {
	tick := time.NewTicker(bitrateSampleInterval)
	defer tick.Stop()

	sampler := newBitrateSampler()
	for {
		select {
		case <-s.done:
			return
		case now := <-tick.C:
			sample, ok := sampler.next(s, now)
			if !ok {
				// No viewer was attached for the whole interval, so there is
				// no measurement of what the link carried. Start the window
				// over rather than folding in a zero, which reads identically
				// to a link that carried nothing.
				s.reportBitrate(a.report(now, GateNoViewer))
				a.Reset(now)
				continue
			}

			next, rep := a.Observe(now, sample)
			if next == 0 {
				s.reportBitrate(rep)
				continue
			}
			was := s.CurrentBitRate()
			live, err := s.changeBitRate(next)
			rep.Live = live
			if err != nil {
				rep.Err = err.Error()
				s.reportBitrate(rep)
				fmt.Printf("[session %s] bitrate %d -> %d failed: %v\n", s.ID, was, next, err)
				continue
			}
			if live {
				edge, err := s.resizeForBitRate(next)
				rep.MaxSize = edge
				if err != nil {
					fmt.Printf("[session %s] maxsize %d failed: %v\n", s.ID, edge, err)
				}
			}
			s.reportBitrate(rep)
			how := "relaunch"
			if live {
				how = "live"
			}
			fmt.Printf("[session %s] bitrate %d -> %d (%s)\n", s.ID, was, next, how)
		}
	}
}

// bitrateSampler turns the session's cumulative counters into the per-interval
// deltas the controller reads.
type bitrateSampler struct {
	frames uint64
	shed   uint64
	await  uint64
	seen   map[uint64]uint64
}

func newBitrateSampler() *bitrateSampler {
	return &bitrateSampler{seen: map[uint64]uint64{}}
}

// next reports one interval, or false when no viewer was attached for all of
// it and there is therefore nothing measuring the link.
func (b *bitrateSampler) next(s *Session, now time.Time) (BitrateSample, bool) {
	// framesIn is read before the shed counters, so a frame that arrives and
	// is suppressed between the two loads counts as suppressed without
	// counting as produced. That makes awaitFrac read a few percent high for
	// one interval and correct itself on the next, which is why the trace
	// shows occasional values just over 1.0 (1.02, 1.05) on a starved link.
	// It is noise around the ratio rather than drift, and the starvation gate
	// reads a ten-second average against a threshold of 0.50, so it is not
	// worth ordering the loads to remove.
	frames := s.framesIn.Load()
	shedBy := s.ShedBreakdown()
	// Keyframe waits stay out of Shed: they are the consequence of a shed, not
	// independent evidence of one, and counting them there lets a single
	// dropped frame argue for a change sixteen times over. They are carried
	// separately because sustained across a whole window they are the one
	// thing that shows a link too slow to deliver a keyframe at all, which is
	// the state Shed alone reads as healthy.
	//
	// Abandon does belong here. A frame reset mid-flight consumed link time
	// and arrived nowhere, which is the link failing to carry what it was
	// offered -- the same thing staleness measures, decided a moment later.
	shed := shedBy.Pacer + shedBy.Stale + shedBy.Abandon
	await := shedBy.AwaitKey

	sample := BitrateSample{
		Frames:   frames - b.frames,
		Shed:     shed - b.shed,
		AwaitKey: await - b.await,
	}
	b.frames, b.shed, b.await = frames, shed, await

	bytes, ok := s.intervalBytes(b.seen, now.Add(-bitrateSampleInterval))
	if !ok {
		return BitrateSample{}, false
	}
	sample.Bytes = bytes
	return sample, true
}

// intervalBytes reports how many payload bytes reached the slowest viewer that
// was attached for the whole interval, and whether any viewer qualified.
//
// The minimum rather than the sum, because the encoder is shared: with two
// viewers on two links, what the encoder must fit inside is the slower of them,
// and summing would read two half-speed links as one fast one. seen carries
// each viewer's running total between calls and is pruned here.
func (s *Session) intervalBytes(seen map[uint64]uint64, since time.Time) (uint64, bool) {
	type reading struct {
		total uint64
		whole bool
	}

	s.mu.RLock()
	cur := make(map[uint64]reading, len(s.viewers))
	for id, v := range s.viewers {
		cur[id] = reading{total: v.sent.Load(), whole: !v.joined.After(since)}
	}
	s.mu.RUnlock()

	var low uint64
	found := false
	for id, r := range cur {
		if !r.whole {
			continue
		}
		delta := r.total - seen[id]
		if !found || delta < low {
			low, found = delta, true
		}
	}

	for id := range seen {
		if _, ok := cur[id]; !ok {
			delete(seen, id)
		}
	}
	for id, r := range cur {
		seen[id] = r.total
	}
	return low, found
}

// SetBitrateObserver installs a callback that receives every interval the
// controller evaluates, acted on or not.
//
// This has to be set before AdaptBitRate, which is why it is a setter rather
// than an argument: the manager publishes the session before starting the
// controller, so the two calls are already separated.
func (s *Session) SetBitrateObserver(fn func(BitrateReport)) { s.onBitrate = fn }

// SetResizeObserver installs the hook that records a change of picture size.
// Like SetBitrateObserver it is set once, before any viewer is attached.
func (s *Session) SetResizeObserver(fn func(oldW, oldH, newW, newH int)) { s.onResize = fn }

func (s *Session) reportBitrate(rep BitrateReport) {
	if rep.To != 0 && rep.Err == "" {
		s.mu.Lock()
		s.bitrateLog = append(s.bitrateLog, BitrateChange{At: time.Now(), From: rep.From, To: rep.To})
		if len(s.bitrateLog) > 256 {
			s.bitrateLog = s.bitrateLog[len(s.bitrateLog)-256:]
		}
		s.mu.Unlock()
	}
	if s.onBitrate == nil {
		return
	}
	s.onBitrate(rep)
}
