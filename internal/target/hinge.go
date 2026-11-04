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
		Min: hingeAngleMin, Max: hingeAngleMax, Step: hingeAngleStep,
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

// hingeCoalescer sends hinge angles to one target, newest first. A Set that
// arrives while a write is running replaces whatever was queued behind it;
// the replaced Set returns nil at once, since the angle it asked for is
// already stale and the newer one is on its way. The zero value is ready.
type hingeCoalescer struct {
	mu      sync.Mutex
	running bool
	queued  *hingeWrite
}

type hingeWrite struct {
	deg  int
	done chan struct{}
	err  error
}

// Set queues deg and waits for it to reach the device, or to be replaced by
// a newer angle, or for ctx to end. apply runs on a goroutine of its own
// under hingeWriteTimeout, one call at a time per coalescer.
func (c *hingeCoalescer) Set(ctx context.Context, deg int, apply func(ctx context.Context, deg int) error) error {
	w := &hingeWrite{deg: deg, done: make(chan struct{})}
	c.mu.Lock()
	if prev := c.queued; prev != nil {
		close(prev.done) // superseded: err stays nil
	}
	c.queued = w
	if !c.running {
		c.running = true
		go c.drain(apply)
	}
	c.mu.Unlock()
	select {
	case <-w.done:
		return w.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *hingeCoalescer) drain(apply func(ctx context.Context, deg int) error) {
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
		w.err = apply(ctx, w.deg)
		cancel()
		close(w.done)
	}
}
