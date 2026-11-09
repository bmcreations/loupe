package session

import (
	"sync"
	"time"
)

// FlowEst turns one viewer's frame acks into a queueing-delay estimate and a
// delivery rate. The browser acks each frame's pts after presenting it
// (MsgFrameAck); the writer records each frame it delivers with Sent. The
// difference between the two times is the send-to-ack time of that frame.
//
// The estimator and its constants are ported from voxl (github.com/tembo/voxl,
// MIT, commit 3eda628: src/hub.rs Client::on_ack):
//   - the base is the smallest send-to-ack time of the last 10 s, kept as one
//     minimum per second so it ages out a second at a time (voxl replaces it
//     with the current sample when it is 10 s old, which during a congested
//     stretch made the base jump from 56 to 243 ms and hid the queueing);
//   - QueueMs is an EWMA of (sample - base): 0.7 old + 0.3 new;
//   - the delivered rate is measured over windows of at least 0.5 s and
//     smoothed 0.6 old + 0.4 new, the first window setting it outright.
//
// This slice only measures. Nothing reads a FlowEst to decide what to send.
//
// Safe across the viewer's write loop (Sent) and its read loop (Ack).
type FlowEst struct {
	mu sync.Mutex

	inflight []flowEntry

	acking     bool
	lastAck    time.Time
	lastAckPTS uint64
	// ignoredAcks counts acks for a pts not in flight (see FlowStats).
	ignoredAcks uint64
	baseMs      float64
	baseBkts    []flowBucket
	queueMs     float64
	rateBps     float64
	haveRate    bool
	winStart    time.Time
	winBytes    int
	winStarted  bool
}

// flowBucket is the smallest send-to-ack time acked during one wall second.
type flowBucket struct {
	sec int64
	min float64
}

type flowEntry struct {
	pts   uint64
	bytes int
	at    time.Time
}

const (
	flowBaseMaxAge = 10 * time.Second
	flowRateWindow = 500 * time.Millisecond
	flowQueueKeep  = 0.7
	flowRateKeep   = 0.6
	// flowMaxInflight bounds the list for a viewer that never acks, which
	// still has every delivered frame recorded. An ack for an entry trimmed
	// off the front is ignored like any other unknown pts.
	flowMaxInflight = 1024
)

// FlowStats is a point-in-time reading of a FlowEst.
type FlowStats struct {
	// Acking is true from the viewer's first ack. A viewer that never acks
	// (native iOS QUIC, an old browser) stays false and must be treated
	// exactly as before acks existed.
	Acking bool
	// Backlog is frames delivered and not yet acked.
	Backlog int
	// OldestMs is how long the oldest unacked frame has been in flight.
	OldestMs float64
	// QueueMs is the smoothed send-to-ack time above BaseMs.
	QueueMs float64
	// BaseMs is the smallest send-to-ack time seen recently.
	BaseMs float64
	// DeliveredBps is the smoothed acked rate, bits per second.
	DeliveredBps float64
	// OldestPTS and NewestPTS bound the unacked frames, zero when none are.
	OldestPTS, NewestPTS uint64
	// LastAckPTS is the pts the latest ack named, matched or not.
	LastAckPTS uint64
	// IgnoredAcks counts acks for a pts not in flight. A client acking a
	// different number than the frame header carries shows up here.
	IgnoredAcks uint64
	// LastAckAgo is the time since the last ack, zero before the first.
	LastAckAgo time.Duration
}

// Sent records a frame handed to the viewer's connection. pts is the value in
// the frame header (OutMsg.PTSMicros), which is what the client acks.
func (f *FlowEst) Sent(pts uint64, bytes int, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inflight) >= flowMaxInflight {
		f.inflight = f.inflight[1:]
	}
	f.inflight = append(f.inflight, flowEntry{pts: pts, bytes: bytes, at: at})
}

// Ack handles the client's ack of pts: it pops every entry up to and including
// it. A pts that was never recorded (a shed frame, a trimmed entry, an ack
// from before a Reset) is ignored, not read as acking everything.
func (f *FlowEst) Ack(pts uint64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAckPTS = pts
	idx := -1
	for i, e := range f.inflight {
		if e.pts == pts {
			idx = i
			break
		}
	}
	if idx < 0 {
		f.ignoredAcks++
		return
	}
	acked := f.inflight[idx]
	bytes := 0
	for _, e := range f.inflight[:idx+1] {
		bytes += e.bytes
	}
	f.inflight = f.inflight[idx+1:]
	f.acking = true
	f.lastAck = at

	sample := float64(at.Sub(acked.at)) / float64(time.Millisecond)
	f.noteBase(sample, at)
	f.queueMs = flowQueueKeep*f.queueMs + (1-flowQueueKeep)*(sample-f.baseMs)

	// The window opens on an ack and counts the bytes acked after it, so idle
	// time before the first ack of a burst is not read as a slow link.
	if !f.winStarted {
		f.winStart = at
		f.winBytes = 0
		f.winStarted = true
		return
	}
	f.winBytes += bytes
	if dt := at.Sub(f.winStart); dt >= flowRateWindow {
		rate := float64(f.winBytes) * 8 / dt.Seconds()
		if f.haveRate {
			rate = flowRateKeep*f.rateBps + (1-flowRateKeep)*rate
		}
		f.rateBps = rate
		f.haveRate = true
		f.winStart = at
		f.winBytes = 0
	}
}

// noteBase folds sample into the per-second minima and recomputes baseMs as
// the minimum over the last flowBaseMaxAge. Called with f.mu held.
func (f *FlowEst) noteBase(sample float64, at time.Time) {
	sec := at.Unix()
	if n := len(f.baseBkts); n > 0 && f.baseBkts[n-1].sec == sec {
		if sample < f.baseBkts[n-1].min {
			f.baseBkts[n-1].min = sample
		}
	} else {
		f.baseBkts = append(f.baseBkts, flowBucket{sec: sec, min: sample})
	}
	f.baseMs = f.windowMin(at)
}

// windowMin is the smallest bucket minimum within flowBaseMaxAge of now, and
// drops older buckets. Called with f.mu held.
func (f *FlowEst) windowMin(now time.Time) float64 {
	oldest := now.Unix() - int64(flowBaseMaxAge/time.Second) + 1
	keep := f.baseBkts[:0]
	for _, b := range f.baseBkts {
		if b.sec >= oldest {
			keep = append(keep, b)
		}
	}
	f.baseBkts = keep
	if len(keep) == 0 {
		return f.baseMs
	}
	m := keep[0].min
	for _, b := range keep[1:] {
		if b.min < m {
			m = b.min
		}
	}
	return m
}

// Reset forgets what is in flight. The encoder restarts its pts when it is
// rebuilt (resize, rotate, relaunch), so an ack for the old stream must not
// match a new entry; the writer calls this whenever it sends a config frame.
// The path measurements (base, queue, rate) describe the link, not the stream,
// and are kept, as is Acking.
func (f *FlowEst) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inflight = nil
	f.winStarted = false
	f.winBytes = 0
}

// Snapshot reads the estimator at now.
func (f *FlowEst) Snapshot(now time.Time) FlowStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := FlowStats{
		Acking:       f.acking,
		Backlog:      len(f.inflight),
		QueueMs:      f.queueMs,
		BaseMs:       f.windowMin(now),
		DeliveredBps: f.rateBps,
		LastAckPTS:   f.lastAckPTS,
		IgnoredAcks:  f.ignoredAcks,
	}
	if len(f.inflight) > 0 {
		s.OldestMs = float64(now.Sub(f.inflight[0].at)) / float64(time.Millisecond)
		s.OldestPTS = f.inflight[0].pts
		s.NewestPTS = f.inflight[len(f.inflight)-1].pts
	}
	if f.acking {
		s.LastAckAgo = now.Sub(f.lastAck)
	}
	return s
}
