package target

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A simulator's one device setting is posture, and only on a device type
// with two panels, the iPhone Duo. simcap folds it the way Device Hub's hinge
// slider does, through CoreDevice, and reads the posture back from which
// panel is lit: the outer one is closed, the inner one opened.
//
// There is no halfOpened. The hinge takes any angle, but the lit panel only
// says folded or not, so a half-open Duo would read back as opened and the
// control would disagree with what it was just told.
//
// hingeAngle sends any angle, 0-180, for a slider. simcap's hinge is a
// one-way HID report with nothing to read back, so the angle a GET reports
// is the last one this server sent (simHinge), checked against the lit
// panel; see hingeReadback.

var (
	_ DeviceSettings = (*Simulator)(nil)
	_ SettingsGate   = (*Simulator)(nil)
)

// SettingsGate is implemented by a target whose device settings depend on
// the device rather than its kind. HasSettings false answers 501, the same
// as a target with no DeviceSettings at all.
type SettingsGate interface {
	HasSettings(ctx context.Context) bool
}

// simPostureOptions are the postures a Duo can be put in, and the hinge
// angle each sends.
var simPostureOptions = []string{"closed", "opened"}

var simHingeDegrees = map[string]int{"closed": 0, "opened": 180}

// simPostureSettle bounds the wait for the lit panel to follow the hinge.
// A fold and an unfold each took about a second on loupe-duo.
const simPostureSettle = 4 * time.Second

// simHingeOpen is the extra reply budget for a hinge that has to open
// CoreDevice first (openHinge in simcap's `hub.swift`).
const simHingeOpen = 10 * time.Second

// HasSettings reports whether the simulator is booted and has two panels.
// The panel count comes from the device type and is read once.
func (s *Simulator) HasSettings(ctx context.Context) bool {
	if s.requireBooted() != nil {
		return false
	}
	if n := s.panels.Load(); n > 0 {
		return n >= 2
	}
	out, err := s.input().do(ctx, 0, "panels")
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || n <= 0 {
		// An older simcap answers err for an unknown command; nothing is
		// cached so a rebuilt one is asked again.
		return false
	}
	s.panels.Store(int32(n))
	return n >= 2
}

// readPosture asks simcap which posture the lit panel means. simcap answers
// none until CoreDevice has reported a lit panel, one half-second poll after
// the input process starts on a quiet Mac. A second was not enough with the
// load average at 370: the first read of a fresh process answered none.
func (s *Simulator) readPosture(ctx context.Context) (string, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, err := s.input().do(ctx, 0, "posture")
		if err != nil {
			return "", err
		}
		if out != "none" || time.Now().After(deadline) {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// simHinge is the last hinge angle this server sent a simulator, and when.
type simHinge struct {
	mu  sync.Mutex
	set bool
	deg int
	at  time.Time
}

func (h *simHinge) record(deg int) {
	h.mu.Lock()
	h.set, h.deg, h.at = true, deg, time.Now()
	h.mu.Unlock()
}

// reset forgets the last angle, for a reboot or a hinge moved elsewhere.
func (h *simHinge) reset() {
	h.mu.Lock()
	h.set = false
	h.mu.Unlock()
}

func (h *simHinge) last() (deg int, at time.Time, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deg, h.at, h.set
}

// simPanelContradicts reports whether a lit panel cannot go with a hinge
// angle. On the iPhone Duo simulator the panel is not a function of the
// angle: it has hysteresis and depends on the path. Sweeps there read
// closed at 90 (coming down from 180 to 40, then up), and opened at 30
// (coming down by 5s from 180) and at 20 (coming up from 10), but a jump
// from 180 to 30 or 28 closed it. Neither panel was seen outside these
// bounds: closed at 100 or more coming up from 0, opened at 10 or less.
// Only those ends say the hinge moved some other way.
func simPanelContradicts(posture string, deg int) bool {
	switch posture {
	case "closed":
		return deg >= simOpenedAbove
	case "opened":
		return deg <= simClosedBelow
	}
	return false
}

const (
	simOpenedAbove = 135
	simClosedBelow = 5
)

// hingeReadback is the angle a GET reports, given the posture the lit panel
// reads. The last angle sent stands unless the panel contradicts it
// (simPanelContradicts) once simPostureSettle has passed since it was sent,
// which means the hinge was moved some other way: Device Hub, or another
// server. Then, and before anything was sent, the angle is the one that
// posture sends. ok is false when there is neither.
func hingeReadback(posture string, deg int, at time.Time, sent bool, now time.Time) (float64, bool) {
	if sent && (!simPanelContradicts(posture, deg) || now.Sub(at) < simPostureSettle) {
		return float64(deg), true
	}
	if d, known := simHingeDegrees[posture]; known {
		return float64(d), true
	}
	return 0, false
}

// Settings implements DeviceSettings.
func (s *Simulator) Settings(ctx context.Context) ([]Setting, error) {
	if !s.HasSettings(ctx) {
		return nil, nil
	}
	p, err := s.readPosture(ctx)
	if err != nil {
		return nil, err
	}
	if p == "none" {
		p = ""
	}
	out := []Setting{{
		ID: "posture", Type: SettingEnum, Value: p, Options: simPostureOptions,
		Supported: true, Applies: "immediately",
	}}
	deg, at, sent := s.hingeSent.last()
	now := time.Now()
	if d, ok := hingeReadback(p, deg, at, sent, now); ok {
		out = append(out, hingeSetting(d))
	}
	// Once the panel has said the hinge moved elsewhere, the stored angle
	// is forgotten. Kept, it would come back as soon as the hinge moved
	// again to a panel that no longer contradicts it.
	if sent && simPanelContradicts(p, deg) && now.Sub(at) >= simPostureSettle {
		s.hingeSent.reset()
	}
	return out, nil
}

// ApplySettings implements DeviceSettings. A posture change returns once the
// lit panel has followed the hinge, so a GET straight after reads the new
// posture. A hingeAngle change returns once simcap has sent it, without that
// wait, because a slider sends one per tick.
func (s *Simulator) ApplySettings(ctx context.Context, changes map[string]any) (failed, notRestored []string, err error) {
	if len(changes) == 0 {
		return nil, nil, fmt.Errorf("%w: no changes given", ErrInvalidSetting)
	}
	if err := rejectPostureWithHinge(changes); err != nil {
		return nil, nil, err
	}
	var want string
	hinge := -1
	for id, v := range changes {
		switch id {
		case "posture":
			str, ok := v.(string)
			if _, known := simHingeDegrees[str]; !ok || !known {
				return nil, nil, fmt.Errorf("%w: posture: %v is not one of %v", ErrInvalidSetting, v, simPostureOptions)
			}
			want = str
		case hingeAngleID:
			deg, err := parseHingeAngle(v)
			if err != nil {
				return nil, nil, fmt.Errorf("%w: %s", ErrInvalidSetting, err)
			}
			hinge = deg
		default:
			return nil, nil, fmt.Errorf("%w: unknown setting %q", ErrInvalidSetting, id)
		}
	}
	if !s.HasSettings(ctx) {
		return nil, nil, fmt.Errorf("%w: %s has one panel", ErrInvalidSetting, s.Label())
	}
	if hinge >= 0 {
		if _, err := s.hinge.Set(ctx, hinge, s.sendHinge); err != nil {
			return []string{hingeAngleID}, nil, err
		}
		return nil, nil, nil
	}
	// The posture goes through the same coalescer as hingeAngle, settle wait
	// and all, so a slider angle queued behind it waits for it, and one
	// queued before it is replaced by it. A posture replaced by a newer
	// angle answers as a replaced angle does: with that angle's write.
	if _, err := s.hinge.Set(ctx, simHingeDegrees[want], func(ctx context.Context, deg int) error {
		return s.setPosture(ctx, want, deg)
	}); err != nil {
		return []string{"posture"}, nil, err
	}
	return nil, nil, nil
}

// WriteHingeAngle implements HingeWriter.
func (s *Simulator) WriteHingeAngle(ctx context.Context, v any) (int, error) {
	deg, err := parseHingeAngle(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrInvalidSetting, err)
	}
	if !s.HasSettings(ctx) {
		return 0, fmt.Errorf("%w: %s has one panel", ErrInvalidSetting, s.Label())
	}
	return s.hinge.Set(ctx, deg, s.sendHinge)
}

// setPosture sends a posture's angle and waits for the lit panel to follow.
func (s *Simulator) setPosture(ctx context.Context, want string, deg int) error {
	if err := s.sendHinge(ctx, deg); err != nil {
		return err
	}
	deadline := time.Now().Add(simPostureSettle)
	for {
		got, err := s.readPosture(ctx)
		if err != nil {
			return err
		}
		if got == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("posture: still %s %s after the hinge moved", got, simPostureSettle)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// sendHinge moves the hinge and records the angle for hingeReadback. An
// input process on the legacy route opens CoreDevice for the hinge alone,
// which simcap gives ten seconds.
func (s *Simulator) sendHinge(ctx context.Context, deg int) error {
	if _, err := s.input().do(ctx, simHingeOpen, "hinge %d", deg); err != nil {
		return err
	}
	s.hingeSent.record(deg)
	return nil
}
