package target

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// StatusBarReader is implemented by targets that can say how tall their
// status bar is. A viewer whose own screen has a different status bar uses it
// to line the stream's status bar up with its own.
type StatusBarReader interface {
	// StatusBar returns the status bar's height and the display's current
	// height, both in display pixels and in the display's current rotation.
	StatusBar(ctx context.Context) (bar, height int, err error)
}

var _ StatusBarReader = (*ADBTarget)(nil)

// StatusBar reads display 0's status bar from one dumpsys window displays
// call, the same call ScreenGeometry makes.
func (t *ADBTarget) StatusBar(ctx context.Context) (int, int, error) {
	out, err := t.adb.Shell(ctx, t.info.Serial, "dumpsys", "window", "displays")
	if err != nil {
		return 0, 0, fmt.Errorf("read status bar: %w", err)
	}
	bar, height, ok := statusBarOf(out)
	if !ok {
		return 0, 0, fmt.Errorf("device %s reports no status bar on display 0", t.info.Serial)
	}
	return bar, height, nil
}

// statusBarOf reads display 0's status bar height and current display height
// from a dumpsys window displays dump. Both come off the block after
// afterDisplayZero, so the decoy display that prints first (emulator-5554
// lists mDisplayId=18 at 648x1440 ahead of display 0) cannot answer.
//
// The height is DisplayFrames' h, which is already rotated. The bar is the
// bottom of the first statusBars InsetsSource whose frame starts at the top
// edge: on emulator-5554 (API 36, 1080x2400) that line is
// "InsetsSource id=25200000 type=statusBars frame=[0,0][1080,63]". Androids
// before 11 print no InsetsSource, and report nothing here.
func statusBarOf(dump string) (bar, height int, ok bool) {
	rest, ok := afterDisplayZero(dump)
	if !ok {
		return 0, 0, false
	}
	const framesKey = "DisplayFrames w="
	i := strings.Index(rest, framesKey)
	if i < 0 {
		return 0, 0, false
	}
	_, h, found := strings.Cut(rest[i:], " h=")
	if !found {
		return 0, 0, false
	}
	if end := strings.IndexAny(h, " \n"); end >= 0 {
		h = h[:end]
	}
	height, err := strconv.Atoi(h)
	if err != nil || height <= 0 {
		return 0, 0, false
	}

	const barKey = "type=statusBars frame=["
	for s := rest; ; {
		j := strings.Index(s, barKey)
		if j < 0 {
			return 0, 0, false
		}
		s = s[j+len(barKey):]
		// [left,top][right,bottom]
		frame, _, found := strings.Cut(s, "]")
		if !found {
			return 0, 0, false
		}
		_, top, _ := strings.Cut(frame, ",")
		_, br, found := strings.Cut(s, "][")
		if !found {
			continue
		}
		br, _, _ = strings.Cut(br, "]")
		_, bottom, _ := strings.Cut(br, ",")
		t, err1 := strconv.Atoi(top)
		b, err2 := strconv.Atoi(bottom)
		if err1 != nil || err2 != nil || t != 0 || b <= 0 || b >= height {
			continue
		}
		return b, height, true
	}
}
