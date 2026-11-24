package target

import (
	"context"
)

// Describer reports what a target is, for the device drawer. It is optional:
// a target that cannot say omits the fields rather than guessing them.
type Describer interface {
	Describe(ctx context.Context) (Description, error)
}

// Description is what a target says about itself. Every field is optional --
// an AVD that has not booted yet has none of them -- so a caller reports what
// it has and omits the rest rather than guessing.
type Description struct {
	Form      string `json:"form,omitempty"` // phone, foldable, tablet, pattern
	OS        string `json:"os,omitempty"`   // android, ios, synthetic
	OSVersion string `json:"osVersion,omitempty"`
	APILevel  int    `json:"apiLevel,omitempty"`
	// Transport is how the device reaches this Mac: usb, wifi, emulator or
	// simulator. Omitted for synthetic, and for anything that has not
	// described itself.
	Transport string `json:"transport,omitempty"`
}
