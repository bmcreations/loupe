package target

import (
	"context"
)

// Clipboarder is implemented by targets whose clipboard loupe can read and
// set. An Android device or emulator reaches it through the open scrcpy
// session, so it needs one open.
type Clipboarder interface {
	ReadClipboard(ctx context.Context) (string, error)
	// WriteClipboard sets the clipboard, and with paste also pastes it into
	// whatever has focus.
	WriteClipboard(ctx context.Context, text string, paste bool) error
}

// ClipboardWatcher is implemented by streams that report the device's
// clipboard each time it changes, so a viewer can be offered what was
// copied on the device without asking for it. The channel closes with the
// stream.
type ClipboardWatcher interface {
	ClipboardChanges() <-chan string
}
