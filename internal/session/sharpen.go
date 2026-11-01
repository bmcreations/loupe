package session

import (
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// Sharpening: when motion stops, send one keyframe at a higher bitrate so text
// that was encoded at motion quality becomes crisp, then return to the steady
// rate.
//
// Only an encoder that changes bitrate while it runs can do this
// (target.LiveBitRater, a simulator's simcap). scrcpy fixes the rate at launch,
// so a device would need a relaunch for one frame, and a plain keyframe at the
// steady rate is no sharper than the frame it replaces; a device is skipped.
const (
	// sharpenStill is how long the screen must have been still, and
	// sharpenStillFrames the most frames that may arrive in that time. A
	// static simulator screen still trickles 2 frames a second (the 5G
	// trace), so "still" is at most one frame in 400ms rather than none.
	sharpenStill       = 400 * time.Millisecond
	sharpenStillFrames = 1

	// sharpenMotionLookback and sharpenMotionFrames say what "after motion"
	// means: at least this many frames in the stretch just before the still
	// period, so a blink of the screen does not sharpen.
	sharpenMotionLookback = 2 * time.Second
	sharpenMotionFrames   = 10

	// sharpenRearmFrames is how many frames inside one sharpenStill window
	// count as motion again. Together with sharpenQuiet it is what makes one
	// still period produce one sharpen: the keyframe's own frames arrive
	// during the quiet time, and the trickle of a static screen never reaches
	// this.
	sharpenRearmFrames = 3
	sharpenQuiet       = 2 * time.Second

	// sharpenMinGap bounds how often a long run of short pauses may sharpen.
	sharpenMinGap = 5 * time.Second

	// sharpenHold is how long the boosted rate stays before the steady rate
	// is restored. simprobe measured VideoToolbox following a new average
	// bitrate within a second.
	sharpenHold = 800 * time.Millisecond

	// sharpenBoost multiplies the steady rate for the one frame, up to
	// sharpenMaxBitRate. On a 1 Mbps link the steady rate is already cut to
	// about 800 kbps, so the extra is a single keyframe of a few tens of
	// kilobytes, and the health gate (below) keeps it off a link that is
	// queueing.
	sharpenBoost      = 2.0
	sharpenMaxBitRate = 8_000_000

	// sharpenStableFor is how long the steady rate must have held, so a link
	// that is still being cut or climbed gets no extra frame.
	sharpenStableFor = 20 * time.Second

	// sharpenShedQuiet is how long nothing may have been shed (pacer, stale,
	// abandon, keyframe wait) before a sharpen is allowed.
	sharpenShedQuiet = 3 * time.Second

	sharpenTick = 100 * time.Millisecond
)

type sharpenPoint struct {
	at     time.Time
	frames uint64
}

// Sharpener decides when a still period follows motion. It is pure, like the
// BitrateAdapter: it holds no clock and touches no stream.
type Sharpener struct {
	hist      []sharpenPoint
	armed     bool
	quietEnd  time.Time
	lastFire  time.Time
	fireCount int

	// skip is why the last Observe saw a still period after motion and did
	// not sharpen, reported once per still period ("" otherwise): "unhealthy"
	// or "tooSoon". skipNoted is set once it has been, until motion re-arms.
	skip      string
	skipNoted bool
	// recent and motion are the frame counts the last decision read.
	recent, motion uint64
}

// Skipped reports why the last Observe declined a detected still period, or
// "" when it did not, or has already said so for this period.
func (h *Sharpener) Skipped() string { return h.skip }

// framesSince is how many frames arrived after t, reading the history point at
// or before t (or the oldest one when the history is shorter).
func (h *Sharpener) framesSince(t time.Time, now uint64) uint64 {
	base := h.hist[0].frames
	for _, p := range h.hist {
		if p.at.After(t) {
			break
		}
		base = p.frames
	}
	return now - base
}

// Observe folds one reading of the cumulative frame count in and reports
// whether to sharpen now. healthy is the caller's verdict that the link is
// good enough to afford one larger frame.
func (h *Sharpener) Observe(now time.Time, frames uint64, healthy bool) bool {
	h.skip = ""
	h.hist = append(h.hist, sharpenPoint{now, frames})
	cut := now.Add(-(sharpenMotionLookback + sharpenStill + time.Second))
	for len(h.hist) > 1 && h.hist[1].at.Before(cut) {
		h.hist = h.hist[1:]
	}

	stillEdge := now.Add(-sharpenStill)
	recent := h.framesSince(stillEdge, frames)
	if !now.After(h.quietEnd) {
		return false
	}
	if recent >= sharpenRearmFrames {
		h.armed = true
		h.skipNoted = false
		return false
	}
	if !h.armed || recent > sharpenStillFrames {
		return false
	}
	// Enough history to have seen the stillness: the oldest point must be at
	// least sharpenStill old, or "no frames" is just "just started looking".
	if now.Sub(h.hist[0].at) < sharpenStill {
		return false
	}
	motion := h.framesSince(now.Add(-sharpenMotionLookback), frames) - recent
	if motion < sharpenMotionFrames {
		return false
	}
	h.recent, h.motion = recent, motion
	if !healthy {
		h.noteSkip("unhealthy")
		return false
	}
	if !h.lastFire.IsZero() && now.Sub(h.lastFire) < sharpenMinGap {
		h.noteSkip("tooSoon")
		return false
	}
	h.armed = false
	h.skipNoted = false
	h.lastFire = now
	h.quietEnd = now.Add(sharpenQuiet)
	h.fireCount++
	return true
}

func (h *Sharpener) noteSkip(why string) {
	if !h.skipNoted {
		h.skipNoted = true
		h.skip = why
	}
}

// sharpenLoop runs the sharpener for a session whose encoder takes a bitrate
// live. It samples at sharpenTick, much finer than the controller's second,
// because stillness is judged over a few hundred milliseconds.
func (s *Session) sharpenLoop() {
	tick := time.NewTicker(sharpenTick)
	defer tick.Stop()
	var sh Sharpener
	var lastShed uint64
	var shedAt time.Time
	for {
		select {
		case <-s.done:
			return
		case now := <-tick.C:
			b := s.ShedBreakdown()
			total := b.Pacer + b.Stale + b.Abandon + b.AwaitKey
			if total != lastShed || shedAt.IsZero() {
				lastShed, shedAt = total, now
			}
			why := s.sharpenBlocked(now, now.Sub(shedAt))
			if sh.Observe(now, s.framesIn.Load(), why == "") {
				s.sharpen(map[string]any{"recentFrames": sh.recent, "motionFrames": sh.motion})
			} else if skip := sh.Skipped(); skip != "" {
				// One record per still period that was not sharpened, which
				// is what tells a skipped sharpen from one never considered.
				if skip == "unhealthy" {
					skip = why
				}
				s.reportBitrate(BitrateReport{Gate: GateSharpenSkipped, Current: s.CurrentBitRate(),
					Detail: map[string]any{"why": skip, "recentFrames": sh.recent, "motionFrames": sh.motion}})
			}
		}
	}
}

// sharpenBlocked is the gate that keeps the extra frame off a struggling link:
// every viewer's current round trip near baseline and fresh, nothing shed for
// sharpenShedQuiet, no viewer awaiting a keyframe, and the steady rate stable.
// It returns what blocked the sharpen, or "" when none did.
func (s *Session) sharpenBlocked(now time.Time, shedQuiet time.Duration) string {
	if shedQuiet < sharpenShedQuiet {
		return "shed"
	}
	if _, ok := s.StableBitRate(now, sharpenStableFor); !ok {
		return "rateNotHeld"
	}
	cur, base := s.linkRTT(now)
	if cur <= 0 || !linkHealthy(cur, base) {
		return "rtt"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.viewers) == 0 {
		return "noViewer"
	}
	for _, v := range s.viewers {
		if v.needsKey {
			return "awaitKey"
		}
	}
	return ""
}

// sharpen raises the encoder's rate, asks for a keyframe, and restores the
// steady rate after sharpenHold. The restore reads the session's rate when it
// runs, so a controller change made in the meantime is the one that stands.
func (s *Session) sharpen(detail map[string]any) {
	s.streamMu.Lock()
	lb, ok := s.stream.(target.LiveBitRater)
	if !ok || !lb.LiveBitRate() || s.closing || s.ended || s.relaunching || s.sharpening {
		s.streamMu.Unlock()
		return
	}
	steady := s.opts.BitRate
	boost := int(sharpenBoost * float64(steady))
	if boost > sharpenMaxBitRate {
		boost = sharpenMaxBitRate
	}
	if boost <= steady || lb.SetBitRate(boost) != nil {
		s.streamMu.Unlock()
		return
	}
	s.sharpening = true
	stream := s.stream
	s.streamMu.Unlock()

	_ = stream.RequestKeyframe()
	s.reportBitrate(BitrateReport{Gate: GateSharpen, Current: steady, Boost: boost, Detail: detail})

	time.AfterFunc(sharpenHold, func() {
		s.streamMu.Lock()
		defer s.streamMu.Unlock()
		s.sharpening = false
		if s.closing || s.ended || s.stream != stream {
			return
		}
		_ = lb.SetBitRate(s.opts.BitRate)
	})
}
