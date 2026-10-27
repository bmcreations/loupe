package main

import (
	"math/rand"
	"testing"
	"time"
)

func at(ms int) time.Time {
	return time.Unix(0, 0).Add(time.Duration(ms) * time.Millisecond)
}

// The point of the harness is that a 400 kbps cap actually caps at 400 kbps.
// If the model is wrong the controller is being tuned against a link nobody
// has, which is worse than having no harness.
func TestTheRateLimitIsTheRateItClaims(t *testing.T) {
	l := &Link{Rate: 400_000, MaxQueue: 10 * time.Second}

	// 1200-byte datagrams, the QUIC size pinned by trap 11.
	const size = 1200
	const n = 200
	var last time.Time
	for i := 0; i < n; i++ {
		f := l.Schedule(at(0), size)
		if f.Drop {
			t.Fatalf("datagram %d dropped: %s", i, f.Reason)
		}
		last = f.Release
	}

	bits := float64(n * size * 8)
	secs := last.Sub(at(0)).Seconds()
	got := bits / secs
	if got < 396_000 || got > 404_000 {
		t.Fatalf("drained at %.0f bps, want ~400000", got)
	}
}

// Unbounded queueing is trap 4's shape: nothing is lost, everything is late,
// and every counter upstream looks healthy. The harness has to be able to
// produce it, because that is the failure the pacer exists to catch.
func TestAnUnboundedQueueDelaysRatherThanDrops(t *testing.T) {
	l := &Link{Rate: 100_000}
	for i := 0; i < 500; i++ {
		if f := l.Schedule(at(0), 1200); f.Drop {
			t.Fatalf("datagram %d dropped from an unbounded queue: %s", i, f.Reason)
		}
	}
	if got := l.Backlog(at(0)); got < 40*time.Second {
		t.Fatalf("backlog %v, want the queue to have grown without bound", got)
	}
}

func TestABoundedQueueTailDropsOnceItIsFull(t *testing.T) {
	l := &Link{Rate: 100_000, MaxQueue: 200 * time.Millisecond}

	accepted, dropped := 0, 0
	for i := 0; i < 500; i++ {
		if l.Schedule(at(0), 1200).Drop {
			dropped++
			continue
		}
		accepted++
	}
	if dropped == 0 {
		t.Fatal("nothing was dropped, so MaxQueue did not bound anything")
	}
	// 200ms at 100 kbps is 20000 bits, about two 1200-byte datagrams, and the
	// check admits one more because it is made before the packet is charged.
	if accepted > 5 {
		t.Fatalf("accepted %d into a 200ms queue at 100 kbps, want a handful", accepted)
	}
	if got := l.Backlog(at(0)); got > 400*time.Millisecond {
		t.Fatalf("backlog grew to %v despite a 200ms bound", got)
	}
}

// A queue that drains is a queue that accepts again. Without this the shaper
// would model a link that fails once and stays failed, and recovery -- the
// half of self-healing that matters -- could never be observed through it.
func TestADrainedQueueAcceptsAgain(t *testing.T) {
	l := &Link{Rate: 100_000, MaxQueue: 200 * time.Millisecond}
	for i := 0; i < 50; i++ {
		l.Schedule(at(0), 1200)
	}
	if !l.Schedule(at(0), 1200).Drop {
		t.Fatal("a full queue accepted a datagram")
	}
	if f := l.Schedule(at(5000), 1200); f.Drop {
		t.Fatalf("a queue with five seconds to drain still dropped: %s", f.Reason)
	}
}

func TestDelayIsAddedAfterSerialisation(t *testing.T) {
	plain := &Link{Rate: 400_000}
	delayed := &Link{Rate: 400_000, Delay: 150 * time.Millisecond}

	a := plain.Schedule(at(0), 1200).Release
	b := delayed.Schedule(at(0), 1200).Release
	if got := b.Sub(a); got != 150*time.Millisecond {
		t.Fatalf("delay contributed %v, want 150ms", got)
	}
}

// QUIC treats reordering past a threshold as loss, so a model that reorders is
// measuring packet loss while claiming to measure jitter.
func TestJitterNeverReorders(t *testing.T) {
	l := &Link{Rate: 1_000_000, Delay: 50 * time.Millisecond,
		Jitter: 40 * time.Millisecond, Rand: rand.New(rand.NewSource(7))}

	var prev time.Time
	for i := 0; i < 2000; i++ {
		f := l.Schedule(at(0), 1200)
		if f.Drop {
			t.Fatalf("datagram %d dropped unexpectedly", i)
		}
		if i > 0 && f.Release.Before(prev) {
			t.Fatalf("datagram %d releases at %v, before its predecessor at %v",
				i, f.Release, prev)
		}
		prev = f.Release
	}
}

func TestJitterActuallyVaries(t *testing.T) {
	l := &Link{Delay: 50 * time.Millisecond, Jitter: 20 * time.Millisecond,
		Rand: rand.New(rand.NewSource(3))}

	// Arrivals are spaced wider than the jitter can close, so the no-reorder
	// clamp never binds and every sample reaches the release time unmodified.
	// Feeding a burst that all arrives at one instant would measure the clamp
	// instead: it flattens such a burst into a monotonic staircase, correctly.
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		arrival := at(50 * i)
		seen[l.Schedule(arrival, 1200).Release.Sub(arrival)] = true
	}
	if len(seen) < 20 {
		t.Fatalf("%d distinct delays over 200 samples, want jitter to vary", len(seen))
	}
}

// The clamp is what makes a simultaneous burst come out ordered, and it is
// worth pinning separately from the jitter it constrains.
func TestASimultaneousBurstLeavesInOrder(t *testing.T) {
	l := &Link{Rate: 400_000, Delay: 50 * time.Millisecond,
		Jitter: 30 * time.Millisecond, Rand: rand.New(rand.NewSource(13))}

	var prev time.Time
	for i := 0; i < 500; i++ {
		f := l.Schedule(at(0), 1200)
		if f.Drop {
			t.Fatalf("datagram %d dropped unexpectedly", i)
		}
		if i > 0 && f.Release.Before(prev) {
			t.Fatalf("datagram %d releases before its predecessor", i)
		}
		prev = f.Release
	}
}

func TestLossIsRoughlyTheRateAsked(t *testing.T) {
	l := &Link{Loss: 0.10, Rand: rand.New(rand.NewSource(11))}

	dropped := 0
	const n = 20000
	for i := 0; i < n; i++ {
		if l.Schedule(at(0), 1200).Drop {
			dropped++
		}
	}
	frac := float64(dropped) / n
	if frac < 0.09 || frac > 0.11 {
		t.Fatalf("dropped %.3f, want ~0.10", frac)
	}
}

// A datagram lost on the wire never occupied the queue it was lost after.
// Charging it would make a lossy link quietly slower than the rate asked for.
func TestALostDatagramDoesNotOccupyTheQueue(t *testing.T) {
	l := &Link{Rate: 100_000, Loss: 1.0, Rand: rand.New(rand.NewSource(5))}
	for i := 0; i < 100; i++ {
		if f := l.Schedule(at(0), 1200); !f.Drop {
			t.Fatal("a link with Loss 1.0 delivered a datagram")
		}
	}
	if got := l.Backlog(at(0)); got != 0 {
		t.Fatalf("backlog %v after 100 lost datagrams, want 0", got)
	}
}

func TestAnUnshapedLinkPassesEverythingThrough(t *testing.T) {
	l := &Link{}
	for i := 0; i < 100; i++ {
		f := l.Schedule(at(0), 1200)
		if f.Drop {
			t.Fatal("an unshaped link dropped a datagram")
		}
		if !f.Release.Equal(at(0)) {
			t.Fatalf("release %v, want no added delay", f.Release)
		}
	}
}
