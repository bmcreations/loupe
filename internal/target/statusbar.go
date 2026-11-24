package target

import (
	"context"
)

// StatusBarReader is implemented by targets that can say how tall their
// status bar is. A viewer whose own screen has a different status bar uses it
// to line the stream's status bar up with its own.
type StatusBarReader interface {
	// StatusBar returns the status bar's height and the display's current
	// height, both in display pixels and in the display's current rotation.
	StatusBar(ctx context.Context) (bar, height int, err error)
}
