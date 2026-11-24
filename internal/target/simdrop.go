package target

import (
	"context"
)

// An ImageDropper puts an image into the text field at a point, the way a
// person pastes one. Only an iOS simulator does: its pasteboard is reached
// through CoreDevice, where adb has no way to put an image on Android's.
type ImageDropper interface {
	// CanDropImage is nil when DropImage can work here at all. It is
	// ErrSimcapOutdated when the helper has no image pasteboard, and it
	// touches nothing on the device.
	CanDropImage(ctx context.Context) error
	// DropImage pastes the image file at the host path into the field at
	// x, y in device pixels. It is ErrNoPaste when no Paste was offered
	// there, and the image is then on the pasteboard and nowhere else. It
	// is ErrPasteFailed when Paste was tapped twice and the field did not
	// change either time. After a paste the pasteboard is left empty.
	DropImage(ctx context.Context, path string, x, y int) (DropResult, error)
}

// DropResult says how a drop went, for the trace.
type DropResult struct {
	// LongPressed is true when a long press was tried, because the second
	// tap raised no Paste or because the first Paste did not take.
	LongPressed bool
	// Verified is true when the field's value was read changing after
	// Paste. It is false when the field exposes no value to compare, and
	// the drop was then taken on trust.
	Verified bool
	// Retried is true when the first Paste left the field as it was and
	// the edit menu was raised and its Paste tapped a second time.
	Retried bool
	// PasteFrom is how the image was pasted: "key" for Cmd+V
	// (simpastekey.go), or where the Paste tapped last was found: "tree",
	// or "hit" when the tree had none and a hit test found it (simhit.go).
	// Empty when no Paste was found. A noPaste with the menu on screen is then
	// told apart from a timing miss by the trace alone.
	PasteFrom string
	// KeyPasteErr is why Cmd+V could not be tried, when it could not.
	KeyPasteErr string
	// ClearErr is why the pasteboard could not be emptied after the paste.
	ClearErr string
}
