package session

import (
	"context"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

func TestSizeForBitRate(t *testing.T) {
	const phone = 1206.0 / 2622 // an iPhone 17's framebuffer
	cases := []struct {
		name          string
		bps, max, cur int
		want          int
	}{
		{"full rate keeps the configured size", 8_000_000, 1080, 1080, 1080},
		{"the cut in the scroll trace steps down", 1_600_000, 1080, 1080, 900},
		{"a deeper cut steps further", 1_100_000, 1080, 1080, 720},
		{"the 26 October cut stops at the floor", 775_493, 1080, 1080, 540},
		{"the floor bottoms out at 540", 250_000, 1080, 1080, 540},
		{"a rate that would just fit a larger rung does not step up", 2_000_000, 1080, 720, 720},
		{"a rate with margin steps up", 3_000_000, 1080, 720, 900},
		{"back to full rate returns to the configured size", 8_000_000, 1080, 540, 1080},
		{"a smaller configured size is the top rung", 8_000_000, 720, 720, 720},
		{"no configured size leaves the stream alone", 500_000, 0, 2622, 0},
		{"a current size between rungs steps down to one", 1_200_000, 1080, 1000, 720},
		{"an SPS rounded off a rung stays on it", 2_000_000, 1080, 718, 720},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sizeForBitRate(c.bps, c.max, 60, phone, c.cur); got != c.want {
				t.Errorf("sizeForBitRate(%d, max %d, cur %d) = %d, want %d", c.bps, c.max, c.cur, got, c.want)
			}
		})
	}
}

// resizeStream is a liveStream that can also change its size in place, as a
// simcap with the resize capability can.
type resizeStream struct {
	*liveStream
	sizes []int
}

func (s *resizeStream) LiveResize() bool { return true }

func (s *resizeStream) SetMaxSize(px int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes = append(s.sizes, px)
	return nil
}

// A live cut to a rate too low for the configured size moves the encoder to a
// smaller rung, and recovering moves it back to the configured size, which
// goes out as 0 so simcap returns to its own -maxsize.
func TestALiveCutResizesAndRecoveryRestores(t *testing.T) {
	st := &resizeStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	tgt := &liveTarget{swapTarget: swapTarget{kind: target.KindSimulator}, stream: st}
	s, err := Open(context.Background(), "resize", tgt, target.Options{MaxFPS: 60, MaxSize: 1080, BitRate: 8_000_000})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 496, 1080
	s.mu.Unlock()

	if edge, err := s.resizeForBitRate(1_600_000); err != nil || edge != 900 {
		t.Fatalf("resize at 1.6 Mbps = %d, %v; want 900", edge, err)
	}
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 414, 900
	s.mu.Unlock()
	if edge, err := s.resizeForBitRate(8_000_000); err != nil || edge != 1080 {
		t.Fatalf("resize at 8 Mbps = %d, %v; want 1080", edge, err)
	}
	st.mu.Lock()
	got := append([]int(nil), st.sizes...)
	st.mu.Unlock()
	if len(got) != 2 || got[0] != 900 || got[1] != 0 {
		t.Fatalf("encoder sizes = %v, want [900 0]", got)
	}
}

// A stream that cannot resize is left alone.
func TestAStreamWithoutResizeIsNotResized(t *testing.T) {
	st := &liveStream{swapStream: newSwapStream(), live: true}
	s, _ := openLive(t, target.KindSimulator, st)
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 496, 1080
	s.mu.Unlock()
	if edge, err := s.resizeForBitRate(500_000); err != nil || edge != 0 {
		t.Fatalf("resize = %d, %v; want 0 for a stream without LiveResizer", edge, err)
	}
}

// A recording ends on a size change, so the size holds while one runs.
func TestARecordingHoldsTheSize(t *testing.T) {
	st := &resizeStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	tgt := &liveTarget{swapTarget: swapTarget{kind: target.KindSimulator}, stream: st}
	s, err := Open(context.Background(), "resize", tgt, target.Options{MaxFPS: 60, MaxSize: 1080, BitRate: 8_000_000})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 496, 1080
	s.mu.Unlock()
	s.AddTap(8)
	if edge, err := s.resizeForBitRate(500_000); err != nil || edge != 0 {
		t.Fatalf("resize while recording = %d, %v; want 0", edge, err)
	}
}

// A simulator opened at its native size steps down from it on a cut, and
// returns to it, sent as 0, once the rate is back to the one it opened with.
func TestANativeSizeStepsDownAndBack(t *testing.T) {
	st := &resizeStream{liveStream: &liveStream{swapStream: newSwapStream(), live: true}}
	tgt := &liveTarget{swapTarget: swapTarget{kind: target.KindSimulator}, stream: st}
	s, err := Open(context.Background(), "resize", tgt, target.Options{MaxFPS: 60, BitRate: 8_000_000})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 1206, 2622
	s.mu.Unlock()

	if edge, err := s.resizeForBitRate(1_600_000); err != nil || edge != 900 {
		t.Fatalf("resize at 1.6 Mbps = %d, %v; want 900", edge, err)
	}
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 414, 900
	s.mu.Unlock()
	if edge, err := s.resizeForBitRate(5_500_000); err != nil || edge != 1080 {
		t.Fatalf("resize at 5.5 Mbps = %d, %v; want 1080", edge, err)
	}
	s.mu.Lock()
	s.init.CodedWidth, s.init.CodedHeight = 496, 1080
	s.mu.Unlock()
	if edge, err := s.resizeForBitRate(8_000_000); err != nil || edge != 2622 {
		t.Fatalf("resize at 8 Mbps = %d, %v; want the native 2622", edge, err)
	}
	st.mu.Lock()
	got := append([]int(nil), st.sizes...)
	st.mu.Unlock()
	if len(got) != 3 || got[0] != 900 || got[1] != 1080 || got[2] != 0 {
		t.Fatalf("encoder sizes = %v, want [900 1080 0]", got)
	}
}
