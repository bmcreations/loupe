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

	s.sharpen(nil)
	s.sharpen(nil) // a second while the first is in force does nothing
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
	s.sharpen(nil)
	if len(st.rates()) != 0 || st.keys != 0 {
		t.Fatalf("sharpened an encoder with no live bitrate: rates %v, keys %d", st.rates(), st.keys)
	}
}

// A still period after motion that is not sharpened says why, once.
func TestASkippedSharpenSaysWhyOncePerStillPeriod(t *testing.T) {
	var sh Sharpener
	now, frames := time.Unix(1000, 0), uint64(0)
	var reasons []string
	step := func(fps int, span time.Duration, healthy bool) {
		var carry float64
		for el := time.Duration(0); el < span; el += sharpenTick {
			carry += float64(fps) * sharpenTick.Seconds()
			n := uint64(carry)
			carry -= float64(n)
			frames += n
			now = now.Add(sharpenTick)
			sh.Observe(now, frames, healthy)
			if r := sh.Skipped(); r != "" {
				reasons = append(reasons, r)
			}
		}
	}
	step(2, 3*time.Second, true)
	step(60, 3*time.Second, true)
	step(0, 5*time.Second, false) // still, but the link is not healthy
	if len(reasons) != 1 || reasons[0] != "unhealthy" {
		t.Fatalf("reasons %v, want one \"unhealthy\" for a 5s still period", reasons)
	}

	// Motion again re-arms, and a still period inside sharpenMinGap of a
	// sharpen is skipped as too soon.
	reasons = nil
	step(60, 3*time.Second, true)
	step(0, 2*time.Second, true) // sharpens
	if len(reasons) != 0 {
		t.Fatalf("reasons %v for a period that sharpened", reasons)
	}
	step(60, 2*time.Second, true)
	step(0, 2*time.Second, true)
	if len(reasons) != 1 || reasons[0] != "tooSoon" {
		t.Fatalf("reasons %v, want one \"tooSoon\"", reasons)
	}
}

func TestSharpenBlockedNamesTheGate(t *testing.T) {
	st := &sharpenStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	s, _ := openLive(t, target.KindSimulator, st)
	now := time.Now()
	if got := s.sharpenBlocked(now, time.Second); got != "shed" {
		t.Errorf("recent shed: %q, want shed", got)
	}
	if got := s.sharpenBlocked(now, time.Minute); got != "rateNotHeld" {
		t.Errorf("fresh session: %q, want rateNotHeld", got)
	}
	if got := s.sharpenBlocked(now.Add(time.Hour), time.Minute); got != "rtt" {
		t.Errorf("no rtt reading: %q, want rtt", got)
	}
}

func TestASharpenReportCarriesItsInputs(t *testing.T) {
	st := &sharpenStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	s, _ := openLive(t, target.KindSimulator, st)
	var got BitrateReport
	s.SetBitrateObserver(func(r BitrateReport) { got = r })
	s.sharpen(map[string]any{"recentFrames": uint64(1), "motionFrames": uint64(34)})
	if got.Gate != GateSharpen || got.Detail["motionFrames"] != uint64(34) {
		t.Fatalf("report %+v, want a sharpen with its inputs", got)
	}
}
