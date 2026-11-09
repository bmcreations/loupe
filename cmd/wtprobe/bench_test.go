package main

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// A steady 60 fps run with one 300 ms hole scores one stall, and delay is
// measured from the best frame, whatever clock pts is on.
func TestSummarizeStallAndDelay(t *testing.T) {
	var recs []frameRec
	base := int64(1_000_000_000)
	arr := 0.0
	for i := range 180 {
		if i == 90 {
			arr += 300
		} else if i > 0 {
			arr += 1000.0 / 60
		}
		pts := uint64(5_000_000 + i*16667) // a different clock from arrival
		if i == 0 {
			pts = 0
		}
		extra := int64(0)
		if i >= 90 {
			extra = 280_000 // frames after the hole arrive 280 ms late
		}
		recs = append(recs, frameRec{ArrivalMs: arr, ArrivalUs: base + int64(i*16667) + extra, PTS: pts, Bytes: 1000})
	}
	s := summarize(recs, nil)
	if s.Stalls != 1 || s.MaxGapMs < 299 {
		t.Fatalf("stalls %d max gap %.0f, want 1 and 300", s.Stalls, s.MaxGapMs)
	}
	if s.DelayP50 > 1 && s.DelayP50 < 279 || s.DelayMax < 279 || s.DelayMax > 281 {
		t.Fatalf("delay p50 %.0f max %.0f, want max 280", s.DelayP50, s.DelayMax)
	}
	if s.LateShare < .45 || s.LateShare > .55 {
		t.Fatalf("late share %.2f, want about half", s.LateShare)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }

// With a hold of 2, frame 1 is acked only when frame 3 arrives.
func TestAckerHold(t *testing.T) {
	var w syncBuf
	a := newAcker(&w, 2, 0)
	now := time.Now()
	for pts := uint64(1); pts <= 3; pts++ {
		a.saw(pts, now)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := w.b.Len()
		w.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	w.mu.Lock()
	b := w.b.Bytes()
	w.mu.Unlock()
	// One framed ack: u32 length 9, 0x1A, pts 1.
	if len(b) != 13 || binary.BigEndian.Uint32(b) != 9 || b[4] != 0x1A || binary.BigEndian.Uint64(b[5:]) != 1 {
		t.Fatalf("got % x, want one ack for pts 1", b)
	}
}
