package target

import (
	"context"
)

// Actuator is implemented by targets that accept input outside a live stream.
//
// This exists alongside Stream's Touch/Scroll/Key rather than replacing it,
// and the difference is the coordinate system. Stream input is in stream
// coordinates, which only make sense while something is streaming and which
// MaxSize rescales. These take DEVICE pixels, the same space Inspect reports,
// so a caller can tap what it just read without knowing whether a viewer is
// attached or what resolution the encoder negotiated.
//
// The cost is latency: each call spawns app_process on the device and lands in
// the low hundreds of milliseconds, against roughly nothing for a scrcpy
// control message. That is the right trade for a caller working from the view
// hierarchy and the wrong one for a finger on a screen.
type Actuator interface {
	Tap(ctx context.Context, x, y int) error
	Swipe(ctx context.Context, x1, y1, x2, y2 int, ms int) error
	TypeText(ctx context.Context, s string) error
	PressKey(ctx context.Context, keycode int) error
}

// Pincher is implemented by targets that can put two fingers down at once.
// Only an iOS simulator does, through CoreDevice's digitizer: `adb shell
// input` has no multi-touch.
type Pincher interface {
	// Pinch spreads (scale > 1) or closes (scale < 1) two fingers about
	// x, y in device pixels over ms milliseconds.
	Pinch(ctx context.Context, x, y int, scale float64, ms int) error
}

// HardwareKeyboarder is implemented by targets that can connect a hardware
// keyboard in place of their on-screen one. With it on, a focused field
// raises no keyboard, so a viewer can type on its own phone's keyboard
// without the device's covering half the screen. An iOS simulator does it
// the way Simulator.app's "Connect Hardware Keyboard" does (siminput.go); an
// Android device or emulator attaches a UHID keyboard through the open
// scrcpy session (adbkeyboard.go), and so needs one open.
type HardwareKeyboarder interface {
	SetHardwareKeyboard(ctx context.Context, on bool) error
}

// ScreenSizer is implemented by targets that can report their current
// on-screen size in device pixels -- the same space Inspect and Actuator
// use. It exists so an agent's input, which lands in device pixels, can be
// normalized into the [0,1] fractions the event log carries, the same space
// a viewer's stream-coordinate touch normalizes into.
type ScreenSizer interface {
	ScreenSize(ctx context.Context) (w, h int, err error)
}
