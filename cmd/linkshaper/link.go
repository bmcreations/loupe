package main

import (
	"math/rand"
	"time"
)

// Link is the impairment model: it decides when a datagram is allowed out, or
// that it is not allowed out at all.
//
// It is separated from the sockets for the same reason bitrate.go is separated
// from relaunch.go -- one decides and the other acts, and only the first is
// cheap to test. A Link holds no socket and no clock; the caller passes the
// time in, so a queue that takes four seconds to drain can be exercised in
// microseconds.
//
// Not safe for concurrent use. One Link models one direction of one flow.
type Link struct {
	// Rate is the serialisation rate in bits per second. Zero means the link
	// is not rate limited.
	Rate int
	// Delay is added one-way propagation, applied after serialisation.
	Delay time.Duration
	// Jitter spreads Delay uniformly over +/- Jitter. Delivery is still
	// ordered: a datagram never leaves before the one in front of it, which is
	// what a real queue does and what QUIC's reordering thresholds expect.
	Jitter time.Duration
	// Loss is the independent drop probability, 0 to 1.
	Loss float64
	// MaxQueue bounds the backlog by the time it would take to drain. Past
	// this, arrivals are tail-dropped. Zero means unbounded, which models the
	// bufferbloat case -- nothing is lost and everything is late, which is the
	// shape trap 4 hid behind a proxy.
	MaxQueue time.Duration

	Rand *rand.Rand

	// nextFree is when the serialiser finishes with everything already
	// accepted. The backlog is nextFree - now, which is the only state a
	// work-conserving queue needs.
	nextFree time.Time
	// lastRelease is what the no-reorder clamp measures against. It cannot be
	// nextFree: that advances per datagram, so flooring a jittered release at
	// its own nextFree still lets it land before the datagram in front of it,
	// which is reordering with extra steps.
	lastRelease time.Time
}

// Fate is what the Link decided about one datagram.
type Fate struct {
	// Release is when it may be written. Zero when Drop is set.
	Release time.Time
	// Drop is set when it was lost, and Reason says which model dropped it.
	Drop   bool
	Reason string
}

// Schedule decides one datagram of size bytes arriving at now.
func (l *Link) Schedule(now time.Time, size int) Fate {
	if l.Loss > 0 && l.rnd().Float64() < l.Loss {
		// Charged nothing: a datagram lost on the wire never occupied the
		// queue it was lost after.
		return Fate{Drop: true, Reason: "loss"}
	}

	start := now
	if l.nextFree.After(start) {
		start = l.nextFree
	}
	if l.MaxQueue > 0 && start.Sub(now) > l.MaxQueue {
		// Tail drop. The backlog is already longer than the queue can hold, so
		// this datagram would be admitted only to arrive uselessly late.
		return Fate{Drop: true, Reason: "queue"}
	}

	service := l.service(size)
	l.nextFree = start.Add(service)

	release := l.nextFree.Add(l.Delay)
	if l.Jitter > 0 {
		spread := time.Duration(l.rnd().Int63n(int64(2*l.Jitter)+1)) - l.Jitter
		release = release.Add(spread)
	}
	// Ordered delivery: jitter may compress the gap to zero but never invert
	// it. QUIC treats reordering past a threshold as loss, so a model that
	// reorders is measuring packet loss while claiming to measure jitter.
	if release.Before(l.lastRelease) {
		release = l.lastRelease
	}
	l.lastRelease = release
	return Fate{Release: release}
}

// Backlog reports how long the queue would take to drain at now.
func (l *Link) Backlog(now time.Time) time.Duration {
	if l.nextFree.After(now) {
		return l.nextFree.Sub(now)
	}
	return 0
}

func (l *Link) service(size int) time.Duration {
	if l.Rate <= 0 {
		return 0
	}
	return time.Duration(float64(size) * 8 / float64(l.Rate) * float64(time.Second))
}

func (l *Link) rnd() *rand.Rand {
	if l.Rand == nil {
		l.Rand = rand.New(rand.NewSource(1))
	}
	return l.Rand
}
