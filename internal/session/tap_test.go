package session

import (
	"bytes"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

func TestNewTapReceivesSentFramesUntilRemoved(t *testing.T) {
	tap, send := NewTap(2)
	send(target.Frame{Config: true, Data: []byte{1}})
	send(target.Frame{Key: true, Data: []byte{2}})
	if f := <-tap.C; !f.Config {
		t.Fatalf("first frame %+v, want the config", f)
	}
	if f := <-tap.C; !f.Key {
		t.Fatalf("second frame %+v, want the key", f)
	}
	send(target.Frame{Data: []byte{3}})
	send(target.Frame{Data: []byte{4}})
	send(target.Frame{Data: []byte{5}})
	select {
	case <-tap.Dropped:
	default:
		t.Fatal("Dropped not closed after overflow")
	}
	tap.Remove()
	tap.Remove()
	send(target.Frame{Data: []byte{6}}) // after Remove: a no-op, not a panic
}

func tapFrame(i int) target.Frame {
	f := target.Frame{PTS: time.Duration(i) * 16 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, byte(i)}}
	switch i {
	case 0:
		f.Config, f.Data = true, []byte{0, 0, 0, 1, 0x67, 1, 2, 3}
	case 1:
		f.Key, f.Data = true, []byte{0, 0, 0, 1, 0x65, 9}
	}
	return f
}

func TestTapSeesEveryFrameIncludingOnesAViewerShed(t *testing.T) {
	st := newSwapStream()
	st.frames = make(chan target.Frame, 64)
	s, _ := openSwap(t, st)
	s.AddViewer() // nobody reads it, so its queue sheds
	tap := s.AddTap(64)
	defer tap.Remove()
	for i := 0; i < 40; i++ {
		st.frames <- tapFrame(i)
	}
	var last time.Duration
	for i := 0; i < 40; i++ {
		select {
		case f := <-tap.C:
			if i > 0 && f.PTS <= last {
				t.Fatalf("frame %d pts %v not after %v", i, f.PTS, last)
			}
			last = f.PTS
			if i == 0 && !f.Config {
				t.Fatal("first frame is not the config")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("tap got %d of 40 frames", i)
		}
	}
	select {
	case <-tap.Dropped:
		t.Fatal("tap dropped")
	default:
	}
}

func TestTapReplaysTheLastConfigFirst(t *testing.T) {
	st := newSwapStream()
	s, _ := openSwap(t, st)
	st.frames <- tapFrame(0)
	st.frames <- tapFrame(1)
	waitFor(t, "two frames in", func() bool { return s.FrameCount() == 2 })
	tap := s.AddTap(8)
	defer tap.Remove()
	select {
	case f := <-tap.C:
		if !f.Config || !bytes.Equal(f.Data, tapFrame(0).Data) {
			t.Fatalf("first frame = %+v, want the raw config", f)
		}
	case <-time.After(time.Second):
		t.Fatal("no replay")
	}
}

func TestFullTapClosesDroppedAndNeverBlocksThePump(t *testing.T) {
	st := newSwapStream()
	s, _ := openSwap(t, st)
	_, ch := s.AddViewer()
	tap := s.AddTap(2)
	defer tap.Remove()
	go func() {
		for i := 0; i < 10; i++ {
			st.frames <- tapFrame(i)
		}
	}()
	if got := len(drainPTS(ch, 10)); got != 10 {
		t.Fatalf("viewer got %d frames", got)
	}
	select {
	case <-tap.Dropped:
	case <-time.After(time.Second):
		t.Fatal("Dropped not closed")
	}
	// Nothing more arrives once it dropped: it holds only what fit.
	if n := len(tap.C); n != 2 {
		t.Fatalf("tap holds %d frames, want its capacity of 2", n)
	}
	st.frames <- tapFrame(10)
	waitFor(t, "extra frame in", func() bool { return s.FrameCount() == 11 })
	if n := len(tap.C); n != 2 {
		t.Fatalf("tap grew to %d after Dropped", n)
	}
}

func TestRemovingATapMidStreamIsSafe(t *testing.T) {
	st := newSwapStream()
	s, _ := openSwap(t, st)
	tap := s.AddTap(4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			select {
			case st.frames <- tapFrame(i):
			case <-time.After(time.Second):
				return
			}
		}
	}()
	time.Sleep(5 * time.Millisecond)
	tap.Remove()
	tap.Remove()
	<-done
	for len(tap.C) > 0 {
		<-tap.C
	}
	st.frames <- tapFrame(300)
	st.frames <- tapFrame(301)
	waitFor(t, "frames in", func() bool { return s.FrameCount() >= 202 })
	if n := len(tap.C); n != 0 {
		t.Fatalf("removed tap received %d frames", n)
	}
	s.mu.RLock()
	n := len(s.taps)
	s.mu.RUnlock()
	if n != 0 {
		t.Fatalf("%d taps left", n)
	}
}

func TestHoldsCount(t *testing.T) {
	s, _ := openSwap(t, newSwapStream())
	r1 := s.Hold()
	_ = s.Hold()
	if s.Holds() != 2 {
		t.Fatalf("holds = %d", s.Holds())
	}
	r1()
	r1()
	if s.Holds() != 1 {
		t.Fatalf("holds = %d, want 1", s.Holds())
	}
}

func TestBitrateChangesAreLogged(t *testing.T) {
	s, _ := openSwap(t, newSwapStream())
	s.reportBitrate(BitrateReport{})
	s.reportBitrate(BitrateReport{From: 3_000_000, To: 1_500_000})
	s.reportBitrate(BitrateReport{From: 1_500_000, To: 750_000, Err: "relaunch failed"})
	got := s.BitrateChanges()
	if len(got) != 1 || got[0].From != 3_000_000 || got[0].To != 1_500_000 {
		t.Fatalf("changes = %+v", got)
	}
}
