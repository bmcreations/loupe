package httpapi

import "time"

// staleRunMax is how long a run of stale inter frames may go on being sent
// late before the viewer gives up on it and asks for a keyframe.
//
// A count of consecutive stale frames does not separate the two cases: after a
// keyframe write that held the serialized sender for ~300ms, the 18 frames
// queued behind it are all stale and all consecutive, and they drain in a few
// milliseconds each. What separates a backlog that is draining from one that
// is not is how long the run lasts, so the rule is time. One second is 60
// frames at the encoder's rate, well beyond a single keyframe write on the 5G
// trace (a 100 kB keyframe at 1.8 Mbps is ~450ms), and short enough that a
// link which really cannot keep up reaches needKey in a second.
const staleRunMax = time.Second

// lateWaitMax is the longest an inter frame may have been queued and still be
// sent late. It equals session's stalenessCap, the bound StalenessFor never
// exceeds: past it the picture is old enough that a keyframe is the better use
// of the link.
const lateWaitMax = 600 * time.Millisecond

// lateGate decides what the sender does with an inter frame that has waited
// longer than session.StalenessFor.
//
// The 5G trace showed every freeze as exactly one shedStale, then 12-15
// shedAwaitKey, then a forced keyframe: 422 of 2604 frames, with abandons ~0
// and the link carrying 1-3 Mbps. The suspected cause is a keyframe write that
// blocks the serialized sender, so the next inter frame is stale by the time it
// is reached, and shedding it forces another keyframe. On a transport that
// delivers every frame it writes, sending that frame late costs a moment of
// latency; shedding it costs a freeze and the keyframe that ends it.
type lateGate struct {
	// canLate is whether the transport delivers a late frame reliably. The
	// stream-per-frame transports do; a WebSocket write is committed to TCP
	// and head-of-line blocks everything behind it, so shedding early is the
	// only latency lever it has.
	canLate bool
	// runStart is when the current run of stale frames began, or zero.
	runStart time.Time
	// keyEnd is when the last key or config write returned.
	keyEnd time.Time
}

// noteKeyWritten records the end of a key or config frame's write.
func (g *lateGate) noteKeyWritten(now time.Time) { g.keyEnd = now }

// overlappedKey reports a frame queued before the last key write finished:
// it spent some of its wait behind that write.
func (g *lateGate) overlappedKey(queuedAt time.Time) bool {
	return !g.keyEnd.IsZero() && queuedAt.Before(g.keyEnd)
}

// fresh ends a stale run: a frame went out in time, or recovery began.
func (g *lateGate) fresh() { g.runStart = time.Time{} }

// sendLate reports whether a stale frame that waited wait should be sent
// rather than shed. A false answer also ends the run, because the caller
// sheds and enters needKey.
func (g *lateGate) sendLate(now time.Time, wait time.Duration) bool {
	if !g.canLate {
		return false
	}
	if g.runStart.IsZero() {
		g.runStart = now
	}
	if wait > lateWaitMax || now.Sub(g.runStart) >= staleRunMax {
		g.fresh()
		return false
	}
	return true
}

// sendTrace is the writer goroutine's per-second send measurements, folded
// into the viewer trace row. It is touched only by that goroutine.
type sendTrace struct {
	// writeMax and keyWriteMax are the longest SendFrame this second, over
	// every frame and over key/config frames alone.
	writeMax, keyWriteMax time.Duration
	// sentLate and staleOverKey are cumulative for the viewer, like shedStale
	// beside them: stale inter frames sent late, and stale frames (shed or
	// sent) whose wait overlapped a key write.
	sentLate, staleOverKey uint64
	// staleSustained counts stale runs that ended in a shed (cumulative).
	staleSustained uint64
}

func (t *sendTrace) noteWrite(keyOrConfig bool, d time.Duration) {
	if d > t.writeMax {
		t.writeMax = d
	}
	if keyOrConfig && d > t.keyWriteMax {
		t.keyWriteMax = d
	}
}

// take returns the trace fields and starts the next second's maxima.
func (t *sendTrace) take() map[string]any {
	rec := map[string]any{
		"writeMaxMs":     float64(t.writeMax.Microseconds()) / 1000,
		"keyWriteMaxMs":  float64(t.keyWriteMax.Microseconds()) / 1000,
		"sentLate":       t.sentLate,
		"staleOverKey":   t.staleOverKey,
		"staleSustained": t.staleSustained,
	}
	t.writeMax, t.keyWriteMax = 0, 0
	return rec
}
