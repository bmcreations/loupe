package android

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

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
func (t *Device) AddMedia(ctx context.Context, path, name string, kind target.MediaKind) error {
	if !t.info.Ready() {
		return fmt.Errorf("%w: device %s is in state %q", target.ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	dir := "/sdcard/Pictures"
	if kind.Video {
		dir = "/sdcard/Movies"
	}
	dest := dir + "/" + name
	if out, err := exec.CommandContext(ctx, t.adb.bin(), "-s", t.info.Serial, "push", path, dest).CombinedOutput(); err != nil {
		return fmt.Errorf("adb push: %w: %s", err, target.LastLine(strings.TrimSpace(string(out))))
	}
	scan := "am broadcast -a android.intent.action.MEDIA_SCANNER_SCAN_FILE -d " + shellQuote("file://"+dest)
	if out, err := exec.CommandContext(ctx, t.adb.bin(), "-s", t.info.Serial, "shell", scan).CombinedOutput(); err != nil {
		return fmt.Errorf("media scan: %w: %s", err, target.LastLine(strings.TrimSpace(string(out))))
	}
	return nil
}

func (e *Emulator) AddMedia(ctx context.Context, path, name string, kind target.MediaKind) error {
	in, done, err := e.running()
	if err != nil {
		return err
	}
	defer done()
	return in.AddMedia(ctx, path, name, kind)
}

var (
	_ target.MediaAdder = (*Device)(nil)
	_ target.MediaAdder = (*Emulator)(nil)
)
