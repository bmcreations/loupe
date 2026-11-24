package target

import (
	"context"
	"errors"
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
	HingeAngleID   = "hingeAngle"
	HingeAngleMin  = 0
	HingeAngleMax  = 180
	HingeAngleStep = 1
)

// hingeWriteTimeout bounds one coalesced write. It runs detached from the
// request that queued it, because a later request may be the one waiting on
// it; SimHingeOpen is the slowest case, a Duo whose hinge has to open
// CoreDevice first.
const hingeWriteTimeout = SimHingeOpen + 5*time.Second

// hingeSetting is the row both kinds of target report.
func hingeSetting(deg float64) Setting {
	return Setting{
		ID: HingeAngleID, Type: SettingRange, Value: deg,
		Min: ptr(HingeAngleMin), Max: ptr(HingeAngleMax), Step: ptr(HingeAngleStep),
		Supported: true, Applies: "immediately",
	}
}

// HingeSetting is the hingeAngle row for deg, for a PUT that answers with
// that row alone.
func HingeSetting(deg float64) Setting { return hingeSetting(deg) }

// HingeWriter is a DeviceSettings that can write hingeAngle alone and say
// which angle went to the device, so a PUT can answer without reading every
// setting back. sent is the PUT's own angle, or that of the newer write
// that ran in its place. Errors are ApplySettings': ErrInvalidSetting before
// anything is sent, ErrDeviceUnreachable, or the failure of the write that
// ran.
type HingeWriter interface {
	WriteHingeAngle(ctx context.Context, v any) (sent int, err error)
}

// ParseHingeAngle checks a PUT's value: a JSON number, whole degrees, in
// range. Whole degrees is the step the GET advertises; a fraction is refused
// rather than rounded so a client that sent 90.5 is not told 90 took.
func ParseHingeAngle(v any) (int, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("want a number, got %T", v)
	}
	if math.IsNaN(f) || f < HingeAngleMin || f > HingeAngleMax {
		return 0, fmt.Errorf("%s %v is outside %d-%d", HingeAngleID, f, HingeAngleMin, HingeAngleMax)
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("%s %v is not a whole number of degrees", HingeAngleID, f)
	}
	return int(f), nil
}

// ErrStillSending is a hinge write whose caller stopped waiting before it
// reached the device. The write runs on, detached, so it may still land:
// unlike a failure there is nothing rolled back, and a GET says where the
// hinge ended.
var ErrStillSending = errors.New("still sending")

// HingeCoalescer sends writes that move one target's hinge, newest first:
// hingeAngle and posture alike, so the two go out in the order they arrive
// and a posture's settle wait is never cut into by a slider. A Set that
// arrives while a write is running replaces whatever was queued behind it.
// The replaced Set waits for the write that runs in its place and answers
// with that write's angle and error, so it never reports an angle that was
// not sent. The zero value is ready.
type HingeCoalescer struct {
	mu      sync.Mutex
	running bool
	queued  *hingeWrite
}

// Queued reports the angle waiting behind the write in flight, if any.
func (c *HingeCoalescer) Queued() (deg int, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queued == nil {
		return 0, false
	}
	return c.queued.deg, true
}

type hingeWrite struct {
	deg   int
	apply func(ctx context.Context, deg int) error
	done  chan struct{}
	err   error
	// sent is the angle that went to the device in this write's place: its
	// own, or that of the newer write that replaced it.
	sent int
	// replaced are the queued writes this one replaced, directly or through
	// the writes it replaced. They finish when this one does.
	replaced []*hingeWrite
}

// Set queues a write of deg and waits for it, or the newer write that
// replaced it, to finish, or for ctx to end. apply runs on a goroutine of
// its own under hingeWriteTimeout, one write at a time per coalescer. A
// posture write passes its posture's angle as deg.
//
// sent is the angle that went to the device for this call: deg, or the
// angle of the write that ran in its place. err is that write's. sent is
// valid only when err is nil.
func (c *HingeCoalescer) Set(ctx context.Context, deg int, apply func(ctx context.Context, deg int) error) (sent int, err error) {
	w := &hingeWrite{deg: deg, apply: apply, done: make(chan struct{}), sent: deg}
	c.mu.Lock()
	if prev := c.queued; prev != nil {
		// prev never runs. Its callers wait for w instead.
		w.replaced = append(prev.replaced, prev)
		prev.replaced = nil
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
		return 0, fmt.Errorf("%w: %w", ErrStillSending, ctx.Err())
	}
}

func (c *HingeCoalescer) drain() {
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
		err := w.apply(ctx, w.deg)
		cancel()
		for _, r := range append(w.replaced, w) {
			r.sent, r.err = w.deg, err
			close(r.done)
		}
	}
}

// MoveHinge sends one write through c. A nil c, an androidSettings built
// by a test, runs apply at once.
func MoveHinge(ctx context.Context, c *HingeCoalescer, deg int, apply func(ctx context.Context, deg int) error) error {
	if c == nil {
		return apply(ctx, deg)
	}
	_, err := c.Set(ctx, deg, apply)
	if errors.Is(err, ErrStillSending) {
		// The rollback table restores behind it through the same
		// coalescer, so the PUT's 409 and rollback still hold.
		return errors.New(err.Error())
	}
	return err
}
