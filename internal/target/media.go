package target

import (
	"bytes"
	"context"
	"errors"
)

// A MediaAdder puts a photo or a video into the device's library, so it shows
// in Photos (or Google Photos) the way a picture the device took would.
// Dropping a file onto an app's own drop target is not possible: nothing
// reaches a device as a drag, through adb or through HID.
type MediaAdder interface {
	// AddMedia adds the file at the host path. name is the file name it
	// gets on the device, chosen by the server with the extension Kind
	// gave it, never the caller's.
	AddMedia(ctx context.Context, path, name string, kind MediaKind) error
}

// MediaKind is what a file's leading bytes say it is.
type MediaKind struct {
	Ext   string // with no dot: png, jpg, gif, webp, heic, mp4, mov
	Video bool
}

// SniffMedia reads a file's signature. The extension a caller sent is not
// consulted, so a file is added as what it is. A file this does not know is
// refused rather than pushed, because the media store would index it as
// nothing and the caller would see no error.
func SniffMedia(head []byte) (MediaKind, bool) {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return MediaKind{Ext: "png"}, true
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return MediaKind{Ext: "jpg"}, true
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return MediaKind{Ext: "gif"}, true
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return MediaKind{Ext: "webp"}, true
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		// ISO base media: the brand says which.
		switch brand := string(head[8:12]); brand {
		case "heic", "heix", "mif1", "msf1", "heim", "heis":
			return MediaKind{Ext: "heic"}, true
		case "qt  ":
			return MediaKind{Ext: "mov", Video: true}, true
		default:
			return MediaKind{Ext: "mp4", Video: true}, true
		}
	}
	return MediaKind{}, false
}

// SniffLen is how many leading bytes SniffMedia needs.
const SniffLen = 12

// ErrNotMedia is a file SniffMedia does not recognise.
var ErrNotMedia = errors.New("not a photo or video this can add (png, jpg, gif, webp, heic, mp4, mov)")
