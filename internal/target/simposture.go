package target

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
	return []Setting{{
		ID: "posture", Type: SettingEnum, Value: p, Options: simPostureOptions,
		Supported: true, Applies: "immediately",
	}}, nil
}

// ApplySettings implements DeviceSettings. It returns once the lit panel has
// followed the hinge, so a GET straight after reads the new posture.
func (s *Simulator) ApplySettings(ctx context.Context, changes map[string]any) (failed, notRestored []string, err error) {
	if len(changes) == 0 {
		return nil, nil, fmt.Errorf("%w: no changes given", ErrInvalidSetting)
	}
	var want string
	for id, v := range changes {
		if id != "posture" {
			return nil, nil, fmt.Errorf("%w: unknown setting %q", ErrInvalidSetting, id)
		}
		str, ok := v.(string)
		if _, known := simHingeDegrees[str]; !ok || !known {
			return nil, nil, fmt.Errorf("%w: posture: %v is not one of %v", ErrInvalidSetting, v, simPostureOptions)
		}
		want = str
	}
	if !s.HasSettings(ctx) {
		return nil, nil, fmt.Errorf("%w: posture: %s has one panel", ErrInvalidSetting, s.Label())
	}
	// An input process on the legacy route opens CoreDevice for the hinge
	// alone, which simcap gives ten seconds.
	if _, err := s.input().do(ctx, simHingeOpen, "hinge %d", simHingeDegrees[want]); err != nil {
		return []string{"posture"}, nil, err
	}
	deadline := time.Now().Add(simPostureSettle)
	for {
		got, err := s.readPosture(ctx)
		if err != nil {
			return []string{"posture"}, nil, err
		}
		if got == want {
			return nil, nil, nil
		}
		if time.Now().After(deadline) {
			return []string{"posture"}, nil, fmt.Errorf("posture: still %s %s after the hinge moved", got, simPostureSettle)
		}
		select {
		case <-ctx.Done():
			return []string{"posture"}, nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}
