package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/session"
)

// The bench half of the probe: it acks frames the way a remote viewer does,
// so the server puts it on the remote track and runs the rate controller
// against it, and it scores what arrived. scripts/stream-bench runs it
// through cmd/linkshaper so a build can be judged on a replayed link instead
// of a minute of someone scrolling on cellular.

// benchConfig is set from flags in main.
type benchConfig struct {
	acks bool
	// ackDelay stands in for decode and present: the ack leaves this long
	// after the frame arrives.
	ackDelay time.Duration
	// ackHold acks frame i only once frame i+ackHold has arrived, which is
	// what a decoder that holds frames for reordering does. Safari held 8
	// on a stream whose SPS had no VUI; this reproduces that class without
	// a browser.
	ackHold int
	// jsonl gets one JSON object per frame; summary gets the scored run.
	jsonl   string
	summary string
}

var bench benchConfig

// lockedWriter serialises writes to the control stream: input and acks
// come from different goroutines, and an interleaved half-message corrupts
// the framing for the rest of the session.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// acker sends MsgFrameAck for each frame, in arrival order, ackHold frames
// late and ackDelay after its arrival.
type acker struct {
	w       io.Writer
	hold    int
	delay   time.Duration
	pending []pendingAck
	out     chan pendingAck
	sent    int
	errs    int
}

type pendingAck struct {
	pts uint64
	at  time.Time
}

func newAcker(w io.Writer, hold int, delay time.Duration) *acker {
	a := &acker{w: w, hold: hold, delay: delay, out: make(chan pendingAck, 1024)}
	go a.loop()
	return a
}

// saw is called from the collector, which is single-goroutine.
func (a *acker) saw(pts uint64, at time.Time) {
	a.pending = append(a.pending, pendingAck{pts, at})
	for len(a.pending) > a.hold {
		p := a.pending[0]
		a.pending = a.pending[1:]
		// Held frames are released by a later arrival, so their delay runs
		// from that arrival, not their own.
		if a.hold > 0 {
			p.at = at
		}
		select {
		case a.out <- p:
		default:
			a.errs++
		}
	}
}

func (a *acker) loop() {
	for p := range a.out {
		if d := time.Until(p.at.Add(a.delay)); d > 0 {
			time.Sleep(d)
		}
		b := []byte{session.MsgFrameAck}
		b = binary.BigEndian.AppendUint64(b, p.pts)
		if _, err := a.w.Write(controlMsg(b...)); err != nil {
			a.errs++
			continue
		}
		a.sent++
	}
}

// frameRec is one arrival, as written to -jsonl.
type frameRec struct {
	ArrivalMs float64 `json:"arrMs"` // since the first frame
	ArrivalUs int64   `json:"arrUs"` // unix micros
	PTS       uint64  `json:"pts"`
	Bytes     int     `json:"bytes"`
	Key       bool    `json:"key"`
}

// Summary is the scored run. Delay is relative: the smallest arrival-minus-pts
// in the run is taken as zero, so it measures queueing and jitter on top of
// the best the path did, whatever clock pts is on.
type Summary struct {
	Seconds   float64 `json:"seconds"`
	Frames    int     `json:"frames"`
	Keyframes int     `json:"keyframes"`
	Kbps      float64 `json:"kbps"`
	FPSp50    float64 `json:"fpsP50"`
	FPSp10    float64 `json:"fpsP10"`
	FPSmin    float64 `json:"fpsMin"`
	Stalls    int     `json:"stalls200"`
	StallMs   float64 `json:"stallMs"`
	MaxGapMs  float64 `json:"maxGapMs"`
	DelayP50  float64 `json:"delayP50"`
	DelayP95  float64 `json:"delayP95"`
	DelayMax  float64 `json:"delayMax"`
	LateShare float64 `json:"late200Share"` // frames more than 200 ms over the best
	AcksSent  int     `json:"acksSent"`
	AckErrs   int     `json:"ackErrs"`
	BytesP50  float64 `json:"frameBytesP50"`
	BytesMax  int     `json:"frameBytesMax"`
}

// stallGap is the arrival gap counted as a stall: long enough that a viewer
// sees the picture stop, not just drop a frame.
const stallGap = 200.0

func summarize(recs []frameRec, a *acker) Summary {
	var s Summary
	if len(recs) == 0 {
		return s
	}
	s.Frames = len(recs)
	last := recs[len(recs)-1].ArrivalMs
	s.Seconds = last / 1000
	var total int
	var sizes []float64
	for _, r := range recs {
		total += r.Bytes
		if r.Key {
			s.Keyframes++
		}
		sizes = append(sizes, float64(r.Bytes))
		s.BytesMax = max(s.BytesMax, r.Bytes)
	}
	if s.Seconds > 0 {
		s.Kbps = float64(total) * 8 / 1000 / s.Seconds
	}
	s.BytesP50 = pct(sizes, .5)

	// Frames per whole second since the first frame; the last, partial
	// second is left out so a run's end does not read as a dip.
	n := int(last / 1000)
	if n > 0 {
		buckets := make([]float64, n)
		for _, r := range recs {
			if i := int(r.ArrivalMs / 1000); i < n {
				buckets[i]++
			}
		}
		s.FPSp50, s.FPSp10 = pct(buckets, .5), pct(buckets, .1)
		s.FPSmin = pct(buckets, 0)
	}

	for i := 1; i < len(recs); i++ {
		g := recs[i].ArrivalMs - recs[i-1].ArrivalMs
		s.MaxGapMs = math.Max(s.MaxGapMs, g)
		if g > stallGap {
			s.Stalls++
			s.StallMs += g
		}
	}

	// The first frame carries pts 0 (see collector.add), so it has no delay.
	var delays []float64
	best := math.Inf(1)
	for _, r := range recs {
		if r.PTS == 0 {
			continue
		}
		d := float64(r.ArrivalUs-int64(r.PTS)) / 1000
		delays = append(delays, d)
		best = math.Min(best, d)
	}
	if len(delays) > 0 {
		late := 0
		for i := range delays {
			delays[i] -= best
			if delays[i] > stallGap {
				late++
			}
		}
		s.DelayP50, s.DelayP95 = pct(delays, .5), pct(delays, .95)
		s.DelayMax = pct(delays, 1)
		s.LateShare = float64(late) / float64(len(delays))
	}
	if a != nil {
		s.AcksSent, s.AckErrs = a.sent, a.errs
	}
	return s
}

// pct is the nearest-rank percentile of v at f in [0, 1].
func pct(v []float64, f float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[min(int(float64(len(s)-1)*f+0.5), len(s)-1)]
}

func writeBench(recs []frameRec, a *acker) {
	if bench.jsonl != "" {
		if f, err := os.Create(bench.jsonl); err == nil {
			enc := json.NewEncoder(f)
			for _, r := range recs {
				_ = enc.Encode(r)
			}
			_ = f.Close()
		}
	}
	s := summarize(recs, a)
	b, _ := json.Marshal(s)
	fmt.Printf("SUMMARY %s\n", b)
	if bench.summary != "" {
		_ = os.WriteFile(bench.summary, append(b, '\n'), 0o644)
	}
}
