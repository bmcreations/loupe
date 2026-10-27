package session

import (
	"context"
	"sync"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// liveStream is a swapStream whose encoder can change bitrate in place, the
// way simcap's VideoToolbox session can.
type liveStream struct {
	*swapStream
	live bool

	mu  sync.Mutex
	set []int
}

func (s *liveStream) LiveBitRate() bool { return s.live }

func (s *liveStream) SetBitRate(bps int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set = append(s.set, bps)
	return nil
}

func (s *liveStream) rates() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.set...)
}

// liveTarget opens one prepared stream of any type.
type liveTarget struct {
	swapTarget
	stream target.Stream
}

func (t *liveTarget) Open(context.Context, target.Options) (target.Stream, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handles = append(t.handles, nil)
	return t.stream, nil
}

func openLive(t *testing.T, kind target.Kind, st target.Stream) (*Session, *liveTarget) {
	t.Helper()
	tgt := &liveTarget{swapTarget: swapTarget{kind: kind}, stream: st}
	s, err := Open(context.Background(), "live", tgt, target.Options{MaxFPS: 60, BitRate: 3_000_000})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, tgt
}

// A live change reaches the running encoder and nothing is reopened: the
// point is that no viewer waits for a keyframe and no second encoder runs.
func TestALiveEncoderChangesBitrateWithoutARelaunch(t *testing.T) {
	st := &liveStream{swapStream: newSwapStream(), live: true}
	s, tgt := openLive(t, target.KindSimulator, st)
	id, _ := s.AddViewer()
	// A joiner starts in the keyframe wait; start from a viewer past it.
	s.mu.Lock()
	s.viewers[id].needsKey = false
	s.mu.Unlock()

	live, err := s.changeBitRate(1_000_000)
	if err != nil || !live {
		t.Fatalf("changeBitRate = live %v, err %v; want a live change", live, err)
	}
	if got := st.rates(); len(got) != 1 || got[0] != 1_000_000 {
		t.Fatalf("encoder was set to %v, want [1000000]", got)
	}
	if got := s.CurrentBitRate(); got != 1_000_000 {
		t.Fatalf("CurrentBitRate = %d after a live change, want 1000000", got)
	}
	if n := len(tgt.handles); n != 1 {
		t.Fatalf("target opened %d times, want only the first", n)
	}
	s.mu.Lock()
	waiting := s.viewers[id].needsKey
	s.mu.Unlock()
	if waiting {
		t.Fatal("a live change put the viewer into the keyframe wait")
	}
}

// A simulator whose simcap predates the command must not be relaunched
// instead: that would run two simcaps side by side, which nothing has tested.
func TestASimulatorWithoutLiveBitrateIsNeverRelaunched(t *testing.T) {
	st := &liveStream{swapStream: newSwapStream(), live: false}
	s, tgt := openLive(t, target.KindSimulator, st)

	if s.AdaptBitRate() {
		t.Fatal("the controller started on a simulator that cannot change bitrate live")
	}
	if _, err := s.changeBitRate(1_000_000); err == nil {
		t.Fatal("changeBitRate succeeded on a simulator with no live bitrate")
	}
	if n := len(tgt.handles); n != 1 {
		t.Fatalf("target opened %d times, want only the first", n)
	}
	if got := s.CurrentBitRate(); got != 3_000_000 {
		t.Fatalf("CurrentBitRate = %d, want the launch rate kept", got)
	}
}

func TestASimulatorWithLiveBitrateAdapts(t *testing.T) {
	st := &liveStream{swapStream: newSwapStream(), live: true}
	s, _ := openLive(t, target.KindSimulator, st)
	if !s.AdaptBitRate() {
		t.Fatal("the controller refused a simulator that can change bitrate live")
	}
}

// The windows follow what a change costs on this encoder. A simulator that
// changes bitrate live must not wait out a relaunch's settle and probe
// windows, and a device, which relaunches, must keep them.
func TestTheControllerWaitsOnlyAsLongAsAChangeCosts(t *testing.T) {
	st := &liveStream{swapStream: newSwapStream(), live: true}
	sim, _ := openLive(t, target.KindSimulator, st)
	if got := sim.newBitrateAdapter().windows; got != liveWindows {
		t.Errorf("a live simulator got windows %+v, want the live ones %+v", got, liveWindows)
	}

	dev, _ := openSwap(t, newSwapStream())
	if got := dev.newBitrateAdapter().windows; got != relaunchWindows {
		t.Errorf("a device got windows %+v, want the relaunch ones %+v", got, relaunchWindows)
	}
}
