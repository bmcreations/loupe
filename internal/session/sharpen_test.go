package session

import (
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// drive feeds a Sharpener a frame rate for a span at sharpenTick resolution and
// returns how many times it fired and the running frame total.
func drive(sh *Sharpener, now time.Time, frames uint64, fps int, span time.Duration, healthy bool) (time.Time, uint64, int) {
	fired := 0
	var carry float64
	for t := time.Duration(0); t < span; t += sharpenTick {
		carry += float64(fps) * sharpenTick.Seconds()
		n := uint64(carry)
		carry -= float64(n)
		frames += n
		now = now.Add(sharpenTick)
		if sh.Observe(now, frames, healthy) {
			fired++
		}
	}
	return now, frames, fired
}

// Motion, then a still screen that keeps trickling 2 frames a second: one
// sharpen, however long the screen stays still.
func TestAStillPeriodAfterMotionSharpensOnce(t *testing.T) {
	var sh Sharpener
	now, frames, fired := drive(&sh, time.Unix(1000, 0), 0, 60, 3*time.Second, true)
	if fired != 0 {
		t.Fatalf("sharpened %d times during motion", fired)
	}
	now, frames, fired = drive(&sh, now, frames, 2, 20*time.Second, true)
	if fired != 1 {
		t.Fatalf("%d sharpens in a 20s still period, want exactly 1", fired)
	}
	// The sharpen's own keyframe arrives as a short burst and must not read as
	// motion that earns another one.
	_, _, fired = drive(&sh, now, frames+3, 2, 10*time.Second, true)
	if fired != 0 {
		t.Fatalf("%d further sharpens with no new motion", fired)
	}
}

func TestEachStillPeriodGetsItsOwnSharpenButNotWithinTheGap(t *testing.T) {
	var sh Sharpener
	now, frames := time.Unix(1000, 0), uint64(0)
	total := 0
	for i := 0; i < 3; i++ {
		var f int
		now, frames, f = drive(&sh, now, frames, 60, 2*time.Second, true)
		total += f
		now, frames, f = drive(&sh, now, frames, 0, 6*time.Second, true)
		total += f
	}
	if total != 3 {
		t.Fatalf("%d sharpens over three motion/still cycles, want 3", total)
	}
}

func TestABlinkOfMotionDoesNotSharpen(t *testing.T) {
	var sh Sharpener
	now, frames, _ := drive(&sh, time.Unix(1000, 0), 0, 2, 3*time.Second, true)
	now, frames, _ = drive(&sh, now, frames, 20, 300*time.Millisecond, true) // ~6 frames
	_, _, fired := drive(&sh, now, frames, 0, 5*time.Second, true)
	if fired != 0 {
		t.Fatalf("sharpened %d times after six frames of motion", fired)
	}
}

func TestNoSharpenOnAnUnhealthyLink(t *testing.T) {
	var sh Sharpener
	now, frames, _ := drive(&sh, time.Unix(1000, 0), 0, 60, 3*time.Second, true)
	_, _, fired := drive(&sh, now, frames, 0, 5*time.Second, false)
	if fired != 0 {
		t.Fatalf("sharpened %d times with the link unhealthy", fired)
	}
}

// sharpenStream records the rates and keyframe requests a sharpen makes.
type sharpenStream struct {
	*liveStream
	keys int
}

func (s *sharpenStream) RequestKeyframe() error {
	s.mu.Lock()
	s.keys++
	s.mu.Unlock()
	return nil
}

func TestSharpenBoostsOneKeyframeThenRestoresTheSteadyRate(t *testing.T) {
	st := &sharpenStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	s, _ := openLive(t, target.KindSimulator, st)
	var mu sync.Mutex
	var reps []BitrateReport
	s.SetBitrateObserver(func(r BitrateReport) { mu.Lock(); reps = append(reps, r); mu.Unlock() })

	s.sharpen()
	s.sharpen() // a second while the first is in force does nothing
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(st.rates()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	got := st.rates()
	if len(got) != 2 || got[0] != 6_000_000 || got[1] != 3_000_000 {
		t.Fatalf("encoder rates %v, want [6000000 3000000] (boost, then steady)", got)
	}
	st.mu.Lock()
	keys := st.keys
	st.mu.Unlock()
	if keys != 1 {
		t.Errorf("%d keyframe requests, want 1", keys)
	}
	if s.CurrentBitRate() != 3_000_000 {
		t.Errorf("steady rate %d changed by a sharpen", s.CurrentBitRate())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reps) != 1 || reps[0].Gate != GateSharpen || reps[0].Boost != 6_000_000 {
		t.Errorf("reports %+v, want one sharpen report", reps)
	}
}

// A target that cannot take a rate live is skipped: no boost, no keyframe.
func TestSharpenSkipsAnEncoderThatCannotChangeRateLive(t *testing.T) {
	st := &sharpenStream{liveStream: &liveStream{swapStream: newSwapStream(), live: false}}
	s, _ := openLive(t, target.KindSimulator, st)
	s.sharpen()
	if len(st.rates()) != 0 || st.keys != 0 {
		t.Fatalf("sharpened an encoder with no live bitrate: rates %v, keys %d", st.rates(), st.keys)
	}
}
