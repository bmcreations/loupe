// Package awake keeps the Mac from idle-sleeping while someone is using it
// through loupe.
//
// A sleeping Mac pauses the server, every emulator and every simulator, and
// the phone is left retrying against nothing. Waking one remotely is not
// something to rely on, so the only dependable answer is not to fall asleep
// while a viewer or an agent is active. Once nobody is, the assertion goes and
// the Mac sleeps on its own schedule again.
//
// It prevents idle sleep only. Closing the lid still sleeps the Mac, and the
// display is free to turn off, which costs the stream nothing.
package awake

import (
	"sync"
	"time"
)

// Assert takes the power assertion and returns the function that drops it.
type Assert func() (release func(), err error)

// Keeper holds one assertion for as long as anything wants the Mac awake:
// open holds (viewers) or an unexpired linger (recent agent input).
type Keeper struct {
	assert Assert
	note   func(on bool, err error)
	now    func() time.Time

	mu      sync.Mutex
	holds   int
	until   time.Time
	timer   *time.Timer
	release func()
}

// New returns a Keeper that takes assertions through assert. note, if not
// nil, is told each time the assertion is taken or dropped, and of a failure
// to take it.
func New(assert Assert, note func(on bool, err error)) *Keeper {
	return &Keeper{assert: assert, note: note, now: time.Now}
}

// Hold keeps the Mac awake until the returned function is called. Calling it
// more than once is harmless.
func (k *Keeper) Hold() (done func()) {
	if k == nil {
		return func() {}
	}
	k.mu.Lock()
	k.holds++
	k.updateLocked()
	k.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			k.holds--
			k.updateLocked()
			k.mu.Unlock()
		})
	}
}

// Touch keeps the Mac awake for at least d from now. An agent drives the
// device in separate requests with gaps between them, so each one extends a
// window rather than holding for its own few hundred milliseconds.
func (k *Keeper) Touch(d time.Duration) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if u := k.now().Add(d); u.After(k.until) {
		k.until = u
		if k.timer != nil {
			k.timer.Stop()
		}
		k.timer = time.AfterFunc(d, func() {
			k.mu.Lock()
			k.updateLocked()
			k.mu.Unlock()
		})
	}
	k.updateLocked()
}

// Active reports whether the assertion is held.
func (k *Keeper) Active() bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.release != nil
}

func (k *Keeper) updateLocked() {
	want := k.holds > 0 || k.now().Before(k.until)
	switch {
	case want && k.release == nil:
		rel, err := k.assert()
		if err != nil {
			if k.note != nil {
				k.note(false, err)
			}
			return
		}
		k.release = rel
		if k.note != nil {
			k.note(true, nil)
		}
	case !want && k.release != nil:
		k.release()
		k.release = nil
		if k.note != nil {
			k.note(false, nil)
		}
	}
}
