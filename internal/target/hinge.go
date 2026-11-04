package target

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// hingeAngle is the fold angle in degrees, 0 shut to 180 flat, on every
// target that offers posture. It moves the same hinge posture does, so a
// write to one shows on a read of the other: an emulator's device state
// follows its hinge sensor, and a Duo's posture is set by the same simcap
// `hinge` command.
//
// It is built for a slider. A drag sends a write per tick, so a write does
// not wait for the posture to settle, and writes to one target are
// coalesced: while one is going to the device, only the newest of the ones
// queued behind it is sent.
const (
	hingeAngleID   = "hingeAngle"
	hingeAngleMin  = 0
	hingeAngleMax  = 180
	hingeAngleStep = 1
)

// hingeWriteTimeout bounds one coalesced write. It runs detached from the
// request that queued it, because a later request may be the one waiting on
// it; simHingeOpen is the slowest case, a Duo whose hinge has to open
// CoreDevice first.
const hingeWriteTimeout = simHingeOpen + 5*time.Second

// hingeSetting is the row both kinds of target report.
func hingeSetting(deg float64) Setting {
	return Setting{
		ID: hingeAngleID, Type: SettingRange, Value: deg,
		Min: ptr(hingeAngleMin), Max: ptr(hingeAngleMax), Step: ptr(hingeAngleStep),
		Supported: true, Applies: "immediately",
	}
}

// parseHingeAngle checks a PUT's value: a JSON number, whole degrees, in
// range. Whole degrees is the step the GET advertises; a fraction is refused
// rather than rounded so a client that sent 90.5 is not told 90 took.
func parseHingeAngle(v any) (int, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("want a number, got %T", v)
	}
	if math.IsNaN(f) || f < hingeAngleMin || f > hingeAngleMax {
		return 0, fmt.Errorf("%s %v is outside %d-%d", hingeAngleID, f, hingeAngleMin, hingeAngleMax)
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("%s %v is not a whole number of degrees", hingeAngleID, f)
	}
	return int(f), nil
}

// hingeCoalescer sends writes that move one target's hinge, newest first:
// hingeAngle and posture alike, so the two go out in the order they arrive
// and a posture's settle wait is never cut into by a slider. A Set that
// arrives while a write is running replaces whatever was queued behind it;
// the replaced Set returns nil at once, since what it asked for is already
// stale and the newer write is on its way. The zero value is ready.
type hingeCoalescer struct {
	mu      sync.Mutex
	running bool
	queued  *hingeWrite
}

type hingeWrite struct {
	deg   int
	apply func(ctx context.Context, deg int) error
	done  chan struct{}
	err   error
	// sent is the angle going to the device in this write's place: its
	// own, or the newer write's that replaced it.
	sent int
}

// Set queues a write of deg and waits for it to reach the device, or to be
// replaced by a newer one, or for ctx to end. apply runs on a goroutine of
// its own under hingeWriteTimeout, one write at a time per coalescer. A
// posture write passes its posture's angle as deg.
//
// sent is the angle going to the device for this call: deg, or the angle of
// the write that replaced it. It is valid only when err is nil.
func (c *hingeCoalescer) Set(ctx context.Context, deg int, apply func(ctx context.Context, deg int) error) (sent int, err error) {
	w := &hingeWrite{deg: deg, apply: apply, done: make(chan struct{}), sent: deg}
	c.mu.Lock()
	if prev := c.queued; prev != nil {
		prev.sent = deg
		close(prev.done) // superseded: err stays nil
	}
	c.queued = w
	if !c.running {
		c.running = true
		go c.drain()
	}
	c.mu.Unlock()
	select {
	case <-w.done:
		return w.sent, w.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (c *hingeCoalescer) drain() {
	for {
		c.mu.Lock()
		w := c.queued
		c.queued = nil
		if w == nil {
			c.running = false
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), hingeWriteTimeout)
		w.err = w.apply(ctx, w.deg)
		cancel()
		close(w.done)
	}
}

// moveHinge sends one write through c. A nil c, an androidSettings built
// by a test, runs apply at once.
func moveHinge(ctx context.Context, c *hingeCoalescer, deg int, apply func(ctx context.Context, deg int) error) error {
	if c == nil {
		return apply(ctx, deg)
	}
	_, err := c.Set(ctx, deg, apply)
	return err
}
