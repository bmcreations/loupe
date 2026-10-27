package session

import (
	"github.com/bmcreations/loupe/internal/target"
)

// A simulator's picture on a slow link: the scroll trace of 2026-10-26 had the
// adapter cut sim:77763E2D from 8 to 1.6 Mbps, which at 497x1080 and 60fps is
// 0.05 bits a pixel. A fast scroll changes nearly every pixel, and at that
// rate the encoder could only send blocks. Fewer pixels at the same rate give
// each one more bits, so a stream that can resize live (target.LiveResizer)
// steps down a ladder as the bitrate falls and back up as it recovers.
const (
	// targetBitsPerPixel is the rate a rung must give each pixel of each
	// frame to be chosen. At 0.07, 8 Mbps keeps a phone at 1080, the 1.6
	// Mbps of the scroll trace drops it to 900 and 775 kbps to 540. It was
	// 0.1, and on 26 October a phone on the internet path had a simulator cut
	// to 775 kbps by its opening keyframes and shown at 168x360, too coarse
	// to read. It is an estimate for screen content, not a measured threshold.
	targetBitsPerPixel = 0.07

	// upSizeMargin is how far above targetBitsPerPixel a larger rung must
	// land before the stream steps up to it. A step up sends a keyframe at
	// the larger size just as the adapter has decided the link is clean, and
	// one that only just fit could shed and send it straight back down.
	upSizeMargin = 1.5
)

// sizeRungs are the longest edges the ladder steps through, largest first.
// The configured size is always the top rung, whatever its value. 540 is the
// floor: below it a phone's text stops being legible at any bitrate, and a
// blocky picture you can read beats a clean one you cannot.
var sizeRungs = []int{1080, 900, 720, 540}

// sizeForBitRate picks the longest edge for a stream running at bps, given
// the size it was opened with (maxSize), its frame rate, its short edge over
// its long edge (aspect) and the longest edge it encodes at now (cur). It
// returns maxSize for the top rung and 0 when maxSize is 0: a stream asked
// for its native size is left alone.
func sizeForBitRate(bps, maxSize, fps int, aspect float64, cur int) int {
	if maxSize <= 0 || bps <= 0 || fps <= 0 || aspect <= 0 {
		return maxSize
	}
	rungs := []int{maxSize}
	for _, r := range sizeRungs {
		if r < maxSize {
			rungs = append(rungs, r)
		}
	}
	fits := func(edge int, margin float64) bool {
		pixels := aspect * float64(edge) * float64(edge)
		return float64(bps)/(float64(fps)*pixels) >= targetBitsPerPixel*margin
	}
	// cur is read off the SPS, which rounds to even dimensions, so 720 can
	// come back as 718. Snap it to the nearest rung, or a size that only
	// differs by rounding would be sent as a change.
	if cur <= 0 || cur > maxSize {
		cur = maxSize
	}
	near := rungs[0]
	for _, r := range rungs {
		if abs(r-cur) < abs(near-cur) {
			near = r
		}
	}
	cur = near
	if !fits(cur, 1) {
		for _, r := range rungs {
			if r < cur && fits(r, 1) {
				return r
			}
		}
		return rungs[len(rungs)-1]
	}
	for _, r := range rungs {
		if r <= cur {
			break
		}
		if fits(r, upSizeMargin) {
			return r
		}
	}
	return cur
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// resizeForBitRate moves a live-resizable stream to the rung for bps and
// reports the longest edge it asked for, 0 when the stream cannot resize. It runs after a live bitrate change: the
// resize follows the rate, never leads it.
func (s *Session) resizeForBitRate(bps int) (int, error) {
	s.mu.Lock()
	w, h := s.init.CodedWidth, s.init.CodedHeight
	recording := len(s.taps) > 0
	s.mu.Unlock()
	// A tap is a recorder, and the mp4 writer ends a recording on a size
	// change (mp4.ErrResized), so the size holds while one runs.
	if w <= 0 || h <= 0 || recording {
		return 0, nil
	}
	long, short := max(w, h), min(w, h)

	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.closing || s.ended || s.relaunching {
		return 0, nil
	}
	lr, ok := s.stream.(target.LiveResizer)
	if !ok || !lr.LiveResize() {
		return 0, nil
	}
	// Simulators open at their native size (MaxSize 0), which is the top
	// rung. It is remembered the first time through, before any step down
	// has shrunk the SPS, and it holds while the rate is at least the one
	// the session opened with: native at full rate is what a viewer on a
	// clean link asked for, even where the ladder would pick a smaller rung.
	top := s.opts.MaxSize
	if top <= 0 {
		if s.nativeEdge == 0 {
			s.nativeEdge = long
		}
		top = s.nativeEdge
		if s.openBitRate > 0 && bps >= s.openBitRate {
			return top, lr.SetMaxSize(0)
		}
	}
	fps := s.opts.MaxFPS
	if fps <= 0 {
		fps = target.DefaultMaxFPS
	}
	edge := sizeForBitRate(bps, top, fps, float64(short)/float64(long), long)
	px := edge
	if px == top {
		px = 0
	}
	return edge, lr.SetMaxSize(px)
}
