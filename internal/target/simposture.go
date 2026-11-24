package target

import (
	"context"
	"time"
)

// SettingsGate is implemented by a target whose device settings depend on
// the device rather than its kind. HasSettings false answers 501, the same
// as a target with no DeviceSettings at all.
type SettingsGate interface {
	HasSettings(ctx context.Context) bool
}

// SimHingeOpen is the extra reply budget for a hinge that has to open
// CoreDevice first (openHinge in simcap's `hub.swift`).
const SimHingeOpen = 10 * time.Second
