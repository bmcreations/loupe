package target

import (
	"context"
)

// ComposeInspector is implemented by targets that can read the composable
// tree out of the foreground app. Only Android targets can, and only for a
// debuggable app, so this is not part of Target.
type ComposeInspector interface {
	ComposeDump(ctx context.Context) (*ComposeDump, error)
}

// ComposeDump is a live connection to the compose agent in one app process.
type ComposeDump struct {
	Package string
	// Fetch asks the agent for every window's composables once. The tree is
	// short for a moment after attach; composetree.Stable polls it.
	Fetch func(context.Context) ([][]byte, error)
}
