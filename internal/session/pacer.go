package session

import "time"

// targetExcess is how much queuing delay above the path's own baseline this
// server will tolerate before shedding.
//
// The threshold has to be relative, not absolute. An absolute one silently
// assumes the path is fast: on a relayed link whose baseline round trip is
// already 250ms, a fixed 300ms target reads as permanently congested, the
// controller clamps to its floor and holds it there forever, and the stream is
// starved to a couple of frames per second while latency looks "controlled".
// What matters is delay this server is causing, which is the excess over the
// best round trip the path has recently managed.
const targetExcess = 150 * time.Millisecond

// recoverExcess is the excess the stream must fall below before the pacer opens
// back up. The gap from targetExcess is deliberate hysteresis: without it the
// controller oscillates between flooding and starving.
const recoverExcess = 60 * time.Millisecond

// The baseline is the minimum over a rolling set of short slots rather than
// over a fixed number of samples, because a sample-counted window has no fixed
// duration: it is fed once per frame, so on a link delivering three frames a
// second it spans a minute while on a healthy one it spans three seconds.
//
// A trace of a phone moving from wifi onto congested 5G shows what that costs.
// The baseline stayed at the wifi figure of 7ms while the client reported 500
// to 2000ms, so every sample read as excess, `keep` sat pinned at its 0.1 floor
// for thirteen straight seconds, and it could not recover because recovery
// needs rtt below 67ms on a path whose real floor was 233ms. That is trap 5
// again, reached from the other direction: there the threshold was absolute,
// here the threshold is relative to a baseline that has stopped tracking.
//
// The slot count sets how long a genuinely worse path takes to be believed,
// and the total duration sets how much sustained congestion it takes to poison
// the estimate. Five seconds is the compromise: short enough that a handover is
// learned before a viewer gives up, long enough that a two-second burst of
// queuing cannot convince the pacer that queuing is the new floor.
const (
	baseSlot  = 500 * time.Millisecond
	baseSlots = 10
)

// Pacer decides what fraction of non-essential frames to send, from the
// latency the client actually observes.
//
// This exists because socket-level backpressure is not trustworthy. Anything
// between this server and the viewer, a reverse proxy, a relay, a kernel send
// buffer sized for throughput, will accept data long after the link has
// stopped keeping up. Every one of those buffers is invisible from here and
// unbounded in practice. The phone's own round-trip number is the one
// measurement that includes all of them.
//
// The control law is deliberately AIMD, the same shape TCP congestion control
// uses: back off fast when latency climbs, recover slowly when it does not.
// Fast recovery re-fills the very buffers that caused the problem.
type Pacer struct {
	keep  float64 // fraction of sheddable frames to send, in [minKeep, 1]
	accum float64 // fractional carry so keep applies smoothly frame to frame

	// base is the path's recent minimum round trip: propagation and relay cost
	// this server cannot do anything about. Only delay above it is ours.
	base time.Duration
	// slots holds one minimum per baseSlot of wall clock, oldest first. The
	// baseline is the minimum across them, so a burst of queuing can spoil at
	// most one slot while a path that really did get slower expires every slot
	// it was ever measured on.
	slots     [baseSlots]time.Duration
	slotStart time.Time

	// now is the clock, swappable so the slot rotation can be tested without
	// sleeping through five seconds of it.
	now func() time.Time
}

const (
	minKeep      = 0.1  // never starve the stream completely
	backoffScale = 0.06 // multiplicative reduction per sample over target
	recoverStep  = 0.02 // additive recovery per sample under the low-water mark
)

// NewPacer returns a pacer that starts fully open.
func NewPacer() *Pacer { return &Pacer{keep: 1, now: time.Now} }

// Keep reports the current send fraction, for metrics.
func (p *Pacer) Keep() float64 { return p.keep }

// Base reports the path's current baseline round trip, for metrics.
func (p *Pacer) Base() time.Duration { return p.base }

// observeBase folds a sample into the current slot and recomputes the baseline
// as the minimum across every live slot.
func (p *Pacer) observeBase(rtt time.Duration) {
	now := p.now()
	if p.slotStart.IsZero() {
		p.slotStart = now
	}
	// Rotate one slot per elapsed baseSlot. A gap longer than the whole window
	// clears it: nothing measured before then describes this path any more.
	for elapsed := now.Sub(p.slotStart); elapsed >= baseSlot; elapsed -= baseSlot {
		copy(p.slots[:], p.slots[1:])
		p.slots[len(p.slots)-1] = 0
		p.slotStart = p.slotStart.Add(baseSlot)
	}

	cur := &p.slots[len(p.slots)-1]
	if *cur == 0 || rtt < *cur {
		*cur = rtt
	}

	p.base = 0
	for _, v := range p.slots {
		if v == 0 {
			continue
		}
		if p.base == 0 || v < p.base {
			p.base = v
		}
	}

}

// Observe updates the send fraction from the latest client-measured latency.
// A zero rtt means the client has not reported yet; the pacer holds steady
// rather than guessing.
func (p *Pacer) Observe(rtt time.Duration) {
	if rtt <= 0 {
		return
	}
	p.observeBase(rtt)
	excess := rtt - p.base

	switch {
	case excess > targetExcess:
		// Scale the backoff by how far over we are, so a 3s backlog collapses
		// the stream quickly while a slightly-over one barely moves.
		over := float64(excess-targetExcess) / float64(targetExcess)
		if over > 4 {
			over = 4
		}
		p.keep *= 1 - backoffScale*(1+over)
	case excess < recoverExcess:
		p.keep += recoverStep
	}

	if p.keep < minKeep {
		p.keep = minKeep
	}
	if p.keep > 1 {
		p.keep = 1
	}
}

// Admit reports whether the next sheddable frame should be sent. Essential
// frames (parameter sets, keyframes) must bypass this entirely: dropping them
// leaves the decoder with nothing to resynchronize against.
func (p *Pacer) Admit() bool {
	p.accum += p.keep
	if p.accum >= 1 {
		p.accum -= 1
		return true
	}
	return false
}
