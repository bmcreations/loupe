package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"time"
)

// collector reads frames off either transport, each as its 13-byte header
// and an access unit, and keeps the numbers the probe reports.
type collector struct {
	start        time.Time
	onFirst      func()
	dump         *os.File
	frames       int
	keyframes    int
	total        int
	lastPTS      uint64
	lastArrival  time.Time
	firstArrival time.Time
	ptsGaps      []float64
	arrGaps      []float64
}

func newCollector(start time.Time, dumpTo string, onFirst func()) (*collector, error) {
	c := &collector{start: start, onFirst: onFirst}
	// The dump is what web/jitter.replay.mjs reads: real arrivals, replayed
	// through the client's jitter buffer, without a browser that has to be
	// in front to paint.
	if dumpTo != "" {
		f, err := os.Create(dumpTo)
		if err != nil {
			return nil, err
		}
		c.dump = f
	}
	return c, nil
}

func (c *collector) close() {
	if c.dump != nil {
		_ = c.dump.Close()
	}
}

func (c *collector) add(b []byte) {
	payload := b[wtFrameHeaderLen:]
	if b[12]&1 != 0 {
		c.keyframes++
	}
	if c.frames == 0 {
		if c.onFirst != nil {
			c.onFirst()
		}
		// A real access unit starts with an Annex B start code. Anything
		// else means control traffic is landing on the frame path (trap 9).
		n := min(len(payload), 5)
		fmt.Printf("first frame id=%d pts=%d flags=0x%02x starts % x at %v\n",
			binary.BigEndian.Uint32(b[0:4]), binary.BigEndian.Uint64(b[4:12]), b[12], payload[:n], time.Since(c.start).Round(time.Millisecond))
	}
	// Capture spacing against delivery spacing. A source that captures
	// irregularly and one that captures evenly but ships in bursts look
	// identical downstream, and they need different fixes.
	pts := binary.BigEndian.Uint64(b[4:12])
	now := time.Now()
	// The first frame reports pts 0 and the next an absolute device
	// timestamp, so that one gap is meaningless; a session restart can
	// also move PTS backwards, where unsigned subtraction underflows into
	// a 1.8e16ms gap. Both would swamp the mean.
	if c.frames > 0 && c.lastPTS > 0 && pts >= c.lastPTS {
		c.ptsGaps = append(c.ptsGaps, float64(pts-c.lastPTS)/1000)
	}
	if c.frames > 0 {
		c.arrGaps = append(c.arrGaps, float64(now.Sub(c.lastArrival).Microseconds())/1000)
	}
	c.lastPTS, c.lastArrival = pts, now
	if c.frames == 0 {
		c.firstArrival = now
	}
	if c.dump != nil {
		fmt.Fprintf(c.dump, "%.3f %d %d\n", float64(now.Sub(c.firstArrival).Microseconds())/1000, pts, b[12])
	}
	c.frames++
	c.total += len(payload)
}

func (c *collector) report(dur time.Duration) {
	fmt.Printf("RESULT %d frames, %d keyframes, %.2f MB in %v\n",
		c.frames, c.keyframes, float64(c.total)/(1<<20), dur)
	describe("pts gap ", c.ptsGaps)
	describe("arrival gap", c.arrGaps)
}

// sendTap sends one touch, down then up 60ms later, after tapAfter.
func sendTap(control io.Writer, tap *[2]int32, tapAfter time.Duration) {
	time.Sleep(tapAfter)
	t0 := time.Now()
	_, err := control.Write(touchMsg(0, tap[0], tap[1]))
	time.Sleep(60 * time.Millisecond)
	if err == nil {
		_, err = control.Write(touchMsg(1, tap[0], tap[1]))
	}
	fmt.Printf("tap %d,%d sent in %v err=%v\n", tap[0], tap[1], time.Since(t0).Round(time.Millisecond), err)
}
