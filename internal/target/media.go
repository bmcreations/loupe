package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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

// AddMedia pushes the file into Pictures or Movies on shared storage and
// asks the media scanner for it.
//
// Measured on 21 October with adb push to /sdcard/Pictures: on API 34
// (loupe_headless) the file was in content://media/external/images/media at
// once, because shared storage is FUSE and the media provider sees the write.
// On API 29 (loupe_api29) it was not, and the MEDIA_SCANNER_SCAN_FILE
// broadcast indexed it; `content call --method scan_file` threw a
// NullPointerException there. So the broadcast is sent on every level: on 34
// it finds the file already indexed.
func (t *ADBTarget) AddMedia(ctx context.Context, path, name string, kind MediaKind) error {
	if !t.info.Ready() {
		return fmt.Errorf("%w: device %s is in state %q", ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	dir := "/sdcard/Pictures"
	if kind.Video {
		dir = "/sdcard/Movies"
	}
	dest := dir + "/" + name
	if out, err := exec.CommandContext(ctx, t.adb.bin(), "-s", t.info.Serial, "push", path, dest).CombinedOutput(); err != nil {
		return fmt.Errorf("adb push: %w: %s", err, lastLine(strings.TrimSpace(string(out))))
	}
	scan := "am broadcast -a android.intent.action.MEDIA_SCANNER_SCAN_FILE -d " + shellQuote("file://"+dest)
	if out, err := exec.CommandContext(ctx, t.adb.bin(), "-s", t.info.Serial, "shell", scan).CombinedOutput(); err != nil {
		return fmt.Errorf("media scan: %w: %s", err, lastLine(strings.TrimSpace(string(out))))
	}
	return nil
}

// AddMedia goes through `simctl addmedia`, which puts photos and videos in
// the simulator's Photos library. It needs the simulator booted.
func (s *Simulator) AddMedia(ctx context.Context, path, name string, kind MediaKind) error {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "addmedia", s.UDID(), path).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("simctl addmedia: %w", err)
		}
		return fmt.Errorf("simctl addmedia: %w: %s", err, lastLine(msg))
	}
	return nil
}

func (e *Emulator) AddMedia(ctx context.Context, path, name string, kind MediaKind) error {
	in, done, err := e.running()
	if err != nil {
		return err
	}
	defer done()
	return in.AddMedia(ctx, path, name, kind)
}

// ErrNotMedia is a file SniffMedia does not recognise.
var ErrNotMedia = errors.New("not a photo or video this can add (png, jpg, gif, webp, heic, mp4, mov)")

var (
	_ MediaAdder = (*ADBTarget)(nil)
	_ MediaAdder = (*Simulator)(nil)
	_ MediaAdder = (*Emulator)(nil)
)
