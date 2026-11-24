package target

import (
	"context"
)

// PNGMagic is the PNG signature. screencap writes its errors to stdout and
// still exits zero on some devices, so the bytes are checked rather than
// trusted.
var PNGMagic = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// Screenshotter is implemented by targets that can produce one still frame
// without a stream. Synthetic cannot, so this is deliberately not part of
// Target, the same way Inspector is not.
type Screenshotter interface {
	Screenshot(ctx context.Context) ([]byte, error)
}
