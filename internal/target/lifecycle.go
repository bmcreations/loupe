package target

import (
	"context"
)

// Stopper shuts a running virtual device down. A device that is not
// running is already stopped and returns nil.
type Stopper interface {
	Stop(ctx context.Context) error
}

// Deleter removes a virtual device for good, stopping it first.
type Deleter interface {
	Delete(ctx context.Context) error
}

// Starter boots a virtual device without opening a stream, and returns once
// it is up. A device already running returns nil.
type Starter interface {
	Boot(ctx context.Context) error
}

// Resetter throws away a virtual device's saved state, stopping it first
// without saving, so the next boot is clean.
type Resetter interface {
	ResetState(ctx context.Context) error
}
