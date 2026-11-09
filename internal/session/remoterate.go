package session

import (
	"math"
	"time"
)

// remoteRate is the delay-based data-rate cap for the remote encoder. It is a
// pure state machine: the session feeds it one reading per gate tick and sends
// 0x84 when the cap it returns changes.
//
// The rules and constants are ported from voxl (github.com/tembo/voxl, MIT,
// src/video.rs RateCtl). Back off toward the measured delivery rate when acks
// show queueing; probe upward slowly when the path is clear.
//
// Differences from voxl: the start is remoteCapStart, not 8 Mbps; the
// congestion thresholds are wider for cellular jitter (see below); the bounds
// are 0.5 and 8 Mbps; "the content uses the cap" is the remote encoder's own
// output rate rather than the best delivered rate, because the gate holds the
// encoder while the link is full and a held encoder would otherwise read as an
// idle screen forever.
type remoteRate struct {
	cap int

	lastEval, lastDec, lastInc time.Time
	// slowStart ramps x1.5 until the first congestion.
	slowStart bool
	// ceiling is the delivery rate seen at the last congestion: the path's
	// likely capacity. +Inf means unknown, as in voxl, so a forgotten ceiling
	// lets the cap recover quickly rather than crawl.
	ceiling   float64
	ceilingAt time.Time

	// produced rate, measured over windows of at least remoteRateWindow.
	prodBytes uint64
	prodAt    time.Time
	prodBps   float64
	haveProd  bool
}

const (
	// remoteCapMin is the floor. At 12:25 on cellular the phone received
	// 0.5-1.7 Mbps of a 1.5 Mbps floor: the cap could not go under the link,
	// frames queued 150-430 ms and the gate held half of every second.
	// Holding the gate does not shrink the stream, since the encoder spends
	// the same budget on fewer, larger frames; only the cap does. voxl on the
	// same link delivered 0.3-0.8 Mbps.
	// The 13:10 run, with the encoder at constant quality, showed a 500 kbps
	// ceiling smearing the picture while voxl carried 1-2.6 Mbps on the same
	// link (12:37); the limit also undershoots, holding 500 kbps to about
	// 170 kbps offline.
	// At 1 Mbps (13:58) the browser smeared while scrolling and the cap sat
	// there marked congested; voxl's default floor (min-kbps) is 2 Mbps and it
	// carried 1-2.6 Mbps on the same link.
	remoteCapMin = 2_000_000
	remoteCapMax = 8_000_000
	// remoteCapStart is where a new remote viewer starts. The 12:09 cellular
	// run carried 1.3-1.7 Mbps; starting at 4 Mbps queued 270 ms of frames on
	// every join before the first cut. Slow start reaches the ceiling on a
	// clean link in about a second.
	remoteCapStart = 2_500_000

	remoteRateEval      = 100 * time.Millisecond
	remoteRateCutEvery  = 300 * time.Millisecond
	remoteRateSlowEvery = 250 * time.Millisecond
	remoteRateStepEvery = 200 * time.Millisecond
	remoteRateProbe     = time.Second
	remoteRateForget    = 15 * time.Second
	remoteRateWindow    = 500 * time.Millisecond

	// A queue above remoteQueueCongested is congestion; above
	// remoteQueueClear the path is not clear enough to raise the cap. The
	// base is a windowed minimum, so on cellular the queue reads 15-110 ms
	// from jitter alone (12:09 run, 38-63 fps decoded). voxl's 30 ms pinned
	// the cap to the floor for the whole run; real congestion there read
	// 500-2000 ms.
	remoteQueueCongested = 100.0
	remoteQueueClear     = 50.0
	// remoteOldestSlack is how far past the base rtt the oldest unacked frame
	// may be before it counts as congestion. Jitter put it 30-200 ms over a
	// 60-70 ms base in the 12:09 run; congestion put it 300-600 ms over.
	remoteOldestSlack = 150.0
	// remoteDefaultRttMs stands in for an unmeasured base.
	remoteDefaultRttMs = 40.0
)

// Reasons for a rate decision, as traced.
const (
	rateSlowStart = "slowstart"
	rateCongested = "congested"
	rateRecover   = "recover"
	rateProbe     = "probe"
	rateIdle      = "idle"
	rateHold      = "hold"
)

// rateViewer is what the controller reads about one remote viewer.
type rateViewer struct {
	QueueMs, OldestMs, BaseMs, DeliveredBps float64
}

func (v rateViewer) congested() bool {
	rtt := v.BaseMs
	if rtt <= 0 {
		rtt = remoteDefaultRttMs
	}
	return v.QueueMs > remoteQueueCongested || v.OldestMs > rtt+remoteOldestSlack
}

// worstRateViewer picks the viewer the cap follows: a congested one with the
// lowest delivery rate, else the one with the longest queue.
func worstRateViewer(vs []rateViewer) rateViewer {
	var w rateViewer
	for i, v := range vs {
		if i == 0 {
			w = v
			continue
		}
		wc, vc := w.congested(), v.congested()
		switch {
		case vc && !wc:
			w = v
		case vc && wc:
			if v.DeliveredBps > 0 && (w.DeliveredBps <= 0 || v.DeliveredBps < w.DeliveredBps) {
				w = v
			}
		case !vc && !wc && v.QueueMs > w.QueueMs:
			w = v
		}
	}
	return w
}

func newRemoteRate() *remoteRate {
	r := &remoteRate{}
	r.Reset()
	return r
}

// Reset returns to the start state, for a session with no remote viewers.
func (r *remoteRate) Reset() {
	*r = remoteRate{cap: remoteCapStart, slowStart: true, ceiling: math.Inf(1)}
}

// Cap is the current cap in bits per second.
func (r *remoteRate) Cap() int { return r.cap }

// produced folds the encoder's cumulative output bytes into a smoothed rate.
func (r *remoteRate) produced(now time.Time, total uint64) {
	if r.prodAt.IsZero() {
		r.prodAt, r.prodBytes = now, total
		return
	}
	if dt := now.Sub(r.prodAt); dt >= remoteRateWindow {
		rate := float64(total-r.prodBytes) * 8 / dt.Seconds()
		if r.haveProd {
			rate = 0.6*r.prodBps + 0.4*rate
		}
		r.prodBps, r.haveProd = rate, true
		r.prodAt, r.prodBytes = now, total
	}
}

// Tick takes one reading and returns the cap and why. vs is every non-silent
// acking remote viewer; none means there is nothing to adapt to. producedTotal
// is the remote encoder's cumulative output in bytes.
func (r *remoteRate) Tick(now time.Time, vs []rateViewer, producedTotal uint64) (int, string) {
	r.produced(now, producedTotal)
	if len(vs) == 0 {
		return r.cap, rateIdle
	}
	if !r.lastEval.IsZero() && now.Sub(r.lastEval) < remoteRateEval {
		return r.cap, rateHold
	}
	r.lastEval = now

	w := worstRateViewer(vs)
	congested := w.congested()
	clear := w.QueueMs <= remoteQueueClear
	cap := float64(r.cap)

	since := func(t time.Time) time.Duration {
		if t.IsZero() {
			return time.Hour
		}
		return now.Sub(t)
	}

	// pushed says the stream was using at least half the cap. The remote
	// encoder runs at constant quality under the cap, so it often spends far
	// less; a queue reading then comes from the path, not from our rate, and
	// cutting a cap the encoder is not using only thins the next burst. In
	// the 13:10 cellular run the cap sat at the floor while the phone
	// received 0.4-0.6 Mbps of a 0.5 Mbps cap and 14 kbps when idle.
	pushed := (r.haveProd && r.prodBps >= cap*0.5) || w.DeliveredBps >= cap*0.5

	switch {
	case congested && !pushed:
		return r.cap, rateHold
	case congested && since(r.lastDec) >= remoteRateCutEvery:
		t := cap * 0.85
		// Delivery says something about capacity only when the link was
		// being pushed: congestion from one big frame on an idle screen
		// would otherwise pin the ceiling at a few kbps.
		if w.DeliveredBps > 0 && w.DeliveredBps >= cap*0.5 {
			r.ceiling, r.ceilingAt = w.DeliveredBps, now
			if d := w.DeliveredBps * 0.85; d < t {
				t = d
			}
		}
		r.setCap(t)
		r.lastDec = now
		r.slowStart = false
		return r.cap, rateCongested
	case congested:
		return r.cap, rateHold
	case !clear || !r.haveProd || r.prodBps < cap*0.5:
		// Raise only while the content uses the cap: an idle screen says
		// nothing about the path, and an inflated cap overshoots the next
		// burst.
		if r.haveProd && r.prodBps < cap*0.5 {
			return r.cap, rateIdle
		}
		return r.cap, rateHold
	case r.slowStart:
		if since(r.lastInc) >= remoteRateSlowEvery {
			r.setCap(cap * 1.5)
			r.lastInc = now
			return r.cap, rateSlowStart
		}
	case since(r.lastDec) >= time.Second:
		// Paths change: forget a capacity that has not been confirmed lately.
		if since(r.ceilingAt) >= remoteRateForget {
			r.ceiling = math.Inf(1)
		}
		// Recover quickly to just under the known capacity, then probe past
		// it gently so a busy link is not driven into the queue every
		// couple of seconds.
		switch {
		case cap < r.ceiling*0.9 && since(r.lastInc) >= remoteRateStepEvery:
			t := cap*1.06 + 100_000
			if m := r.ceiling * 0.9; t > m {
				t = m
			}
			r.setCap(t)
			r.lastInc = now
			return r.cap, rateRecover
		case cap >= r.ceiling*0.9 && since(r.lastInc) >= remoteRateProbe:
			r.setCap(cap * 1.02)
			r.lastInc = now
			return r.cap, rateProbe
		}
	}
	return r.cap, rateHold
}

func (r *remoteRate) setCap(bps float64) {
	if bps < remoteCapMin {
		bps = remoteCapMin
	}
	if bps > remoteCapMax {
		bps = remoteCapMax
	}
	r.cap = int(bps)
}
