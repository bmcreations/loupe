package target

import (
	"context"
	"errors"
	"fmt"
)

// ErrInvalidSetting wraps every error ApplySettings returns before it has run
// a single command: an unknown id, a value that fails its field's validate,
// or (talkBack only) a device that does not support the setting at all. The
// handler uses errors.Is against this to answer 400 rather than 409 --
// nothing on the device was touched, so there is nothing to report as failed
// and nothing to roll back.
var ErrInvalidSetting = errors.New("invalid setting")

// ErrDeviceUnreachable wraps an error ApplySettings' pre-flight talkBack
// check hit trying to reach the device itself, as opposed to
// ErrInvalidSetting's "the device answered and TalkBack is genuinely
// absent". Nothing has run by the time this can be returned, so there is
// nothing to roll back either; the handler maps it to 502 rather than the
// 400 an invalid setting gets or the 409 a partial failure gets.
var ErrDeviceUnreachable = errors.New("device unreachable")

// SettingType names the shape of a Setting's Value, so a client can build the
// right control without hardcoding per-id knowledge.
type SettingType string

const (
	SettingRange SettingType = "range"
	// SettingEnum is a setting whose Value is one of Options: posture, on a
	// foldable emulator or an iPhone Duo.
	SettingEnum SettingType = "enum"
)

// Setting is one device-wide accessibility or display toggle, reported the
// same way config.go reports a server setting: the value in effect, whether
// it can be written at all, and what applying a change means. Unlike the
// server's own settings there is no file/flag/default provenance here -- a
// device setting has exactly one source, the device -- but "supported" plays
// the same role "pinned" does there: a caller that cannot see it will claim
// success for a write that does nothing.
//
// darkMode and reduceMotion are reported as a bool over device state that
// carries more than two values. Night mode can be "auto" or "custom_schedule"
// as well as "yes"/"no", and the three animation-scale keys behind
// reduceMotion can independently hold any value, not just 0 or 1. Value
// collapses both to a bool for the client; ApplySettings' rollback keeps the
// raw strings it actually read, so a device found in one of those other
// states restores to exactly that state rather than to whichever side of the
// bool it rounded to.
type Setting struct {
	ID    string      `json:"id"`
	Type  SettingType `json:"type"`
	Value any         `json:"value"`
	// Min, Max and Step apply only to SettingRange, and are nil on any
	// other type. They are pointers so a min of 0 (hingeAngle's) is still
	// written. Step is the increment a slider should move in. Only
	// hingeAngle refuses a write between steps; fontScale takes any value
	// in range.
	Min  *float64 `json:"min,omitempty"`
	Max  *float64 `json:"max,omitempty"`
	Step *float64 `json:"step,omitempty"`
	// Options applies only to SettingEnum: the allowed values for Value.
	Options []string `json:"options,omitempty"`
	// Package is the app a per-app setting (appLanguage) applies to: the
	// one in front when this was read.
	Package string `json:"package,omitempty"`
	// Supported is false when the setting cannot be changed on this device
	// (TalkBack not installed, so far the only case), and Reason says why.
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Applies is always "immediately": every setting here is a live device
	// toggle, not a stream parameter queued for the next session the way
	// config.go's maxFps and bitRate are.
	Applies string `json:"applies"`
}

// DeviceSettings is implemented by targets that can read and change
// device-wide settings. Synthetic cannot, so this is deliberately not part of
// Target, the same way Inspector, Screenshotter and Framer are optional.
type DeviceSettings interface {
	Settings(ctx context.Context) ([]Setting, error)
	// ApplySettings changes one or more settings, atomically from the
	// caller's point of view: on any failure, every setting this call had
	// already started changing is restored to the raw state it read before
	// the call began. failed names the setting that failed and every setting
	// after it in table order, which never ran at all. notRestored names any
	// already-applied setting whose rollback write itself failed -- best
	// effort, not guaranteed -- and is reported alongside failed rather than
	// folded into it, because a setting in notRestored did change and a
	// setting in failed did not.
	ApplySettings(ctx context.Context, changes map[string]any) (failed, notRestored []string, err error)
}

func ptr(f float64) *float64 { return &f }

// RejectPostureWithHinge refuses a PUT that sets posture and hingeAngle at
// once. Both move the one hinge, so whichever ran second would win, and
// which that is would be the table's order, not the caller's.
func RejectPostureWithHinge(changes map[string]any) error {
	_, p := changes["posture"]
	_, h := changes[HingeAngleID]
	if p && h {
		return fmt.Errorf("%w: posture and %s both move the hinge; send one", ErrInvalidSetting, HingeAngleID)
	}
	return nil
}
