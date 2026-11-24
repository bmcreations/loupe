package target

import (
	"context"
)

// BootStatus is a snapshot of an AVD's boot, for a caller polling a boot it
// started with `wait=0` or one that timed out. It reads no lock a boot holds.
type BootStatus struct {
	AVD string `json:"avd"`
	// State is "up", "booting" or "stopped".
	State string `json:"state"`
	// ProcessAlive is whether an emulator process for this AVD exists, one
	// this server started or not.
	ProcessAlive bool `json:"processAlive"`
	// PID is that process, or 0.
	PID int `json:"pid,omitempty"`
	// Serial is the adb serial it appears (or will appear) as.
	Serial string `json:"serial,omitempty"`
	// ADBState is adb's word for the serial ("device", "offline",
	// "unauthorized"), or "absent" when adb does not list it. Empty when
	// there is no serial yet.
	ADBState string `json:"adbState,omitempty"`
	// BootCompleted is sys.boot_completed. Read only while adb says the
	// device is ready and it is not paused, since getprop hangs on a
	// paused VM.
	BootCompleted bool `json:"bootCompleted"`
	// ElapsedMs is how long this server's boot has run, 0 when it started
	// none.
	ElapsedMs int64 `json:"elapsedMs,omitempty"`
	// LogPath is where the emulator's stdout and stderr go.
	LogPath string `json:"logPath,omitempty"`
}

// BootReporter is a target that can say how its boot is going.
type BootReporter interface {
	BootStatus(ctx context.Context) BootStatus
}
