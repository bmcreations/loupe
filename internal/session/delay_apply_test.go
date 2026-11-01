package session

import (
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// The size ladder follows the windowed gates; a delay cut aims at the queue
// and must leave the picture where it is.
func TestADelayCutChangesBitrateButNotSize(t *testing.T) {
	st := &resizeStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	s, _ := openLive(t, target.KindSimulator, st)
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 664, 1440
	s.mu.Unlock()

	s.applyBitrate(800_000, BitrateReport{Gate: GateDelay, From: 3_000_000, To: 800_000})
	if got := st.rates(); len(got) != 1 || got[0] != 800_000 {
		t.Fatalf("encoder rates %v, want [800000]", got)
	}
	if len(st.sizes) != 0 {
		t.Fatalf("a delay cut resized to %v", st.sizes)
	}

	s.applyBitrate(767_000, BitrateReport{Gate: GateStarved, From: 800_000, To: 767_000})
	if len(st.sizes) == 0 {
		t.Fatal("a windowed cut no longer reaches the size ladder")
	}
}

func TestALossCutChangesBitrateButNotSize(t *testing.T) {
	st := &resizeStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	s, _ := openLive(t, target.KindSimulator, st)
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 664, 1440
	s.mu.Unlock()
	s.applyBitrate(1_900_000, BitrateReport{Gate: GateLoss, From: 3_000_000, To: 1_900_000})
	if got := st.rates(); len(got) != 1 || got[0] != 1_900_000 {
		t.Fatalf("encoder rates %v, want [1900000]", got)
	}
	if len(st.sizes) != 0 {
		t.Fatalf("a loss cut resized to %v", st.sizes)
	}
}

// The 13:24:33 cut took 4 Mbps to 1.68 Mbps and the picture to 416x904 in one
// step. The undercarried gate now cuts the rate first and steps the size only
// when the rate is at its floor.
func TestUndercarriedCutsRateWithoutResizingUntilTheFloor(t *testing.T) {
	st := &resizeStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	s, _ := openLive(t, target.KindSimulator, st)
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 664, 1440
	s.mu.Unlock()

	s.applyBitrate(1_680_000, BitrateReport{Gate: GateUndercarried, From: 3_000_000, To: 1_680_000})
	if len(st.sizes) != 0 {
		t.Fatalf("an undercarried cut above the floor resized to %v", st.sizes)
	}
	s.applyBitrate(minAdaptiveBitRate, BitrateReport{Gate: GateUndercarried, From: 400_000, To: minAdaptiveBitRate})
	if len(st.sizes) == 0 {
		t.Fatal("an undercarried cut to the floor no longer reaches the size ladder")
	}

	// A size-only step at the floor leaves the encoder's rate alone.
	st.sizes = nil
	before := len(st.rates())
	s.applyBitrate(minAdaptiveBitRate, BitrateReport{Gate: GateUndercarried, From: minAdaptiveBitRate, To: minAdaptiveBitRate})
	if len(st.rates()) != before {
		t.Errorf("a size-only step set the rate again: %v", st.rates())
	}
	if len(st.sizes) == 0 {
		t.Error("a size-only step at the floor did not resize")
	}
}

func TestStableBitRateWaitsOutTheLastChange(t *testing.T) {
	st := &liveStream{swapStream: newSwapStream(), live: true}
	s, _ := openLive(t, target.KindSimulator, st)
	now := time.Now()
	if _, ok := s.StableBitRate(now, time.Minute); ok {
		t.Fatal("a session that just opened reported a stable rate")
	}
	rate, ok := s.StableBitRate(now.Add(2*time.Minute), time.Minute)
	if !ok || rate != 3_000_000 {
		t.Fatalf("stable = %d, %v; want the opening rate", rate, ok)
	}
	s.reportBitrate(BitrateReport{Gate: GateDelay, From: 3_000_000, To: 800_000})
	s.mu.Lock()
	s.bitrateLog[len(s.bitrateLog)-1].At = now.Add(2 * time.Minute)
	s.mu.Unlock()
	if _, ok := s.StableBitRate(now.Add(2*time.Minute+10*time.Second), time.Minute); ok {
		t.Fatal("a rate changed ten seconds ago reported stable")
	}
}

// A session opened below its ceiling starts the controller there and lets it
// climb back to the ceiling by the recovery rules.
func TestASessionOpenedBelowItsCeilingClimbsToIt(t *testing.T) {
	st := &liveStream{swapStream: newSwapStream(), live: true}
	s, _ := openLive(t, target.KindSimulator, st)
	if _, err := s.changeBitRate(1_000_000); err != nil { // as if opened at 1 Mbps
		t.Fatal(err)
	}
	s.SetBitRateCeiling(4_000_000)
	a := s.newBitrateAdapter()
	if a.current != 1_000_000 || a.configured != 4_000_000 {
		t.Fatalf("adapter current %d configured %d, want 1 Mbps climbing to 4 Mbps", a.current, a.configured)
	}
	now := time.Now()
	a.Observe(now, healthyLink)
	got, _ := feed(a, now, int(liveRecoverWindow/time.Second)+1, healthyLink)
	if got != 1_500_000 {
		t.Fatalf("first climb step %d, want 1.5 Mbps", got)
	}
}
