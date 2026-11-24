package target

import (
	"context"
	"time"
)

// StateSaver saves a running virtual device's state so its next boot resumes
// it. Optional, as Stopper is: only an Emulator has a snapshot to save into.
type StateSaver interface {
	SaveState(ctx context.Context) (bool, error)
}

// WarmSaver saves a running emulator's state as the one every later boot
// starts from, whatever KeepState says.
type WarmSaver interface {
	SaveWarm(ctx context.Context) error
}

// StartStater reports when a start state was last saved on purpose.
type StartStater interface {
	StartSavedAt() time.Time
}
