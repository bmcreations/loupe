package httpapi

import (
	"context"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// pauseCheckEvery is how often idle emulators are looked for. It adds at
// most that much to the idle setting, which is minutes.
const pauseCheckEvery = 30 * time.Second

// pauseBudget bounds one `avd stop`, and the process-table read before it.
const pauseBudget = 10 * time.Second

// SetEmulatorPause sets how long a headless emulator goes unused before it
// is paused, or 0 for never. The loop reads it on each pass, so it applies
// to emulators already running.
func (m *Manager) SetEmulatorPause(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emulatorPause = d
}

// SetClaimed sets the check for a live claim on a target.
func (m *Manager) SetClaimed(f func(id string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claimed = f
}

// WakeEmulator resumes the emulator behind id if it is paused and restarts
// its idle clock. It does nothing for any other target.
func (m *Manager) WakeEmulator(ctx context.Context, id string) {
	t, ok := m.Target(id)
	if !ok {
		return
	}
	if e, ok := t.(*target.Emulator); ok {
		_ = e.Resume(ctx)
	}
}

// SetEmulatorShutdown sets how long a headless emulator goes unused before
// it is shut down, or 0 for never. Like the pause, it applies at once.
func (m *Manager) SetEmulatorShutdown(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emulatorShutdown = d
}

// PauseIdleEmulators pauses idle headless emulators, and shuts down ones
// idle for longer, until ctx ends.
func (m *Manager) PauseIdleEmulators(ctx context.Context) {
	t := time.NewTicker(pauseCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.pauseIdle(ctx)
		}
	}
}

// pauseIdle is one pass. An emulator is paused or shut down only when it
// runs headless, so one with its window on the Mac, which someone may be
// looking at, never is; when no session is open on it, which covers viewers
// and recordings; when no agent holds a claim on it; and when nothing else has reached it for the setting. An
// emulator with a session has its clock restarted instead, so nothing
// happens to it the moment the session closes. Both settings count from the
// last use, so a paused emulator keeps ageing towards the shutdown.
func (m *Manager) pauseIdle(ctx context.Context) {
	m.mu.Lock()
	pause, shutdown := m.emulatorPause, m.emulatorShutdown
	var candidates []*target.Emulator
	for id, t := range m.targets {
		e, ok := t.(*target.Emulator)
		if !ok || e.Serial() == "" {
			continue
		}
		if _, open := m.sessions[id]; open {
			e.MarkUsed()
			continue
		}
		// An agent's claim holds the emulator awake: its raw adb install or
		// shell reaches the device without passing through loupe, and hangs
		// on a paused VM. Like a session, it restarts the clock, so the
		// pause comes a full period after the claim lapses.
		if m.claimed != nil && m.claimed(id) {
			e.MarkUsed()
			continue
		}
		// A shutdown or delete in flight is about to kill it anyway.
		if ended, ok := m.shutAt[id]; ok && ended.pending {
			continue
		}
		candidates = append(candidates, e)
	}
	m.mu.Unlock()
	if (pause <= 0 && shutdown <= 0) || len(candidates) == 0 {
		return
	}
	now := time.Now()
	var toStop, toPause []*target.Emulator
	for _, e := range candidates {
		if e.InUse() {
			continue
		}
		idle := e.IdleFor(now)
		switch {
		case shutdown > 0 && idle >= shutdown:
			toStop = append(toStop, e)
		case pause > 0 && idle >= pause && !e.Paused():
			toPause = append(toPause, e)
		}
	}
	if len(toStop) == 0 && len(toPause) == 0 {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, pauseBudget)
	headless := headlessAVDs(pctx)
	for _, e := range toPause {
		if !headless[e.AVD()] {
			continue
		}
		// PauseIfIdle checks again under the lock a wake takes, so a call
		// that arrived since the check above is not paused under.
		_, _ = e.PauseIfIdle(pctx, pause)
	}
	cancel()
	for _, e := range toStop {
		if headless[e.AVD()] {
			m.stopIdle(ctx, e, shutdown)
		}
	}
}

// stopIdle shuts down one idle emulator. It claims the target as a picker
// Shut Down does, so the two cannot stop it at once, and arms the reopen
// guard so an open during the kill is refused rather than booting into it.
// The guard is lifted as soon as the stop returns: its grace is there to
// turn away a viewer reconnecting after a shutdown, and an idle emulator had
// no viewer, so the next open boots it from its snapshot.
func (m *Manager) stopIdle(ctx context.Context, e *target.Emulator, idle time.Duration) {
	id := e.ID()
	armed, ok := m.armIdleStop(id)
	if !ok {
		return
	}
	defer m.lifeEnd(id)
	defer m.ClearEnded(id, armed)
	sctx, cancel := context.WithTimeout(ctx, deviceLifeTimeout)
	defer cancel()
	// StopIfIdle checks again under the lock a wake takes.
	_, _ = e.StopIfIdle(sctx, idle)
}

// armIdleStop claims the target and arms its reopen guard, in the same
// critical section that checks nothing has opened it since the pass looked:
// no session, no shutdown or delete running, no guard already set.
func (m *Manager) armIdleStop(id string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, open := m.sessions[id]; open {
		return time.Time{}, false
	}
	if _, ok := m.shutAt[id]; ok {
		return time.Time{}, false
	}
	if m.lifeBusy[id] {
		return time.Time{}, false
	}
	now := time.Now()
	m.lifeBusy[id] = true
	m.shutAt[id] = endedAt{armed: now, at: now, reason: "shutDown", pending: true}
	return now, true
}
