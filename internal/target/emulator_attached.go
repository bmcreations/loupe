package target

import (
	"context"
	"time"
)

// An Emulator can do everything the device it runs as can, once it runs.
// Each capability goes through the inner ADBTarget rather than the handlers
// swapping the Emulator for it, because the handlers key sessions and trace
// records on the target's ID, and the inner one's ID is the serial.
//
// While the AVD is not running, every one of these fails with the same
// "open it to boot it" error, rather than the Emulator not implementing them
// and the endpoint answering 501, which would say it never could.

var (
	_ Inspector      = (*Emulator)(nil)
	_ Screenshotter  = (*Emulator)(nil)
	_ Actuator       = (*Emulator)(nil)
	_ Rotator        = (*Emulator)(nil)
	_ Framer         = (*Emulator)(nil)
	_ DeviceSettings = (*Emulator)(nil)
	_ AppInfo        = (*Emulator)(nil)
	_ ScreenSizer    = (*Emulator)(nil)
	_ Installer      = (*Emulator)(nil)
	_ Logcatter      = (*Emulator)(nil)
	_ Crasher        = (*Emulator)(nil)
	_ Launcher       = (*Emulator)(nil)
	_ URLOpener      = (*Emulator)(nil)
)

// wakeBudget bounds the `avd start` a call makes before reaching adbd. The
// console answered in 0.03s on loupe_pause; the budget is for a busy Mac.
const wakeBudget = 10 * time.Second

// running is the device behind every delegated call, resumed first if it
// was paused for being idle, since adbd answers nothing while it is. The
// call holds off a pause until done, because an install can outlast the
// idle window and a pause mid-call would hang its adb shell.
func (e *Emulator) running() (in *ADBTarget, done func(), err error) {
	if in := e.current(); in != nil {
		release := e.hold()
		ctx, cancel := context.WithTimeout(context.Background(), wakeBudget)
		defer cancel()
		if err := e.wake(ctx, in.Info().Serial); err != nil {
			release()
			return nil, nil, err
		}
		return in, release, nil
	}
	_, err = e.Attached()
	return nil, nil, err
}

func (e *Emulator) Inspect(ctx context.Context) (*UI, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.Inspect(ctx)
}

func (e *Emulator) Screenshot(ctx context.Context) ([]byte, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.Screenshot(ctx)
}

func (e *Emulator) Tap(ctx context.Context, x, y int) error {
	in, done, err := e.running()
	if err != nil {
		return err
	}
	defer done()
	return in.Tap(ctx, x, y)
}

func (e *Emulator) Swipe(ctx context.Context, x1, y1, x2, y2 int, ms int) error {
	in, done, err := e.running()
	if err != nil {
		return err
	}
	defer done()
	return in.Swipe(ctx, x1, y1, x2, y2, ms)
}

func (e *Emulator) TypeText(ctx context.Context, s string) error {
	in, done, err := e.running()
	if err != nil {
		return err
	}
	defer done()
	return in.TypeText(ctx, s)
}

func (e *Emulator) PressKey(ctx context.Context, keycode int) error {
	in, done, err := e.running()
	if err != nil {
		return err
	}
	defer done()
	return in.PressKey(ctx, keycode)
}

func (e *Emulator) Rotate(ctx context.Context, to int) error {
	in, done, err := e.running()
	if err != nil {
		return err
	}
	defer done()
	return in.Rotate(ctx, to)
}

func (e *Emulator) Rotation(ctx context.Context) (int, error) {
	in, done, err := e.running()
	if err != nil {
		return 0, err
	}
	defer done()
	return in.Rotation(ctx)
}

func (e *Emulator) DeviceFrame(ctx context.Context) (*DeviceFrame, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.DeviceFrame(ctx)
}

func (e *Emulator) FrameAsset(ctx context.Context, name string) ([]byte, string, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, "", err
	}
	defer done()
	return in.FrameAsset(ctx, name)
}

func (e *Emulator) Settings(ctx context.Context) ([]Setting, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.Settings(ctx)
}

func (e *Emulator) ApplySettings(ctx context.Context, changes map[string]any) ([]string, []string, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, nil, err
	}
	defer done()
	return in.ApplySettings(ctx, changes)
}

func (e *Emulator) ForegroundApp(ctx context.Context) (*App, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.ForegroundApp(ctx)
}

func (e *Emulator) ScreenSize(ctx context.Context) (int, int, error) {
	in, done, err := e.running()
	if err != nil {
		return 0, 0, err
	}
	defer done()
	return in.ScreenSize(ctx)
}

func (e *Emulator) SetHardwareKeyboard(ctx context.Context, on bool) error {
	in, done, err := e.running()
	if err != nil {
		return err
	}
	defer done()
	return in.SetHardwareKeyboard(ctx, on)
}

func (e *Emulator) Install(ctx context.Context, apks []string, opts InstallOptions) (*InstallResult, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.Install(ctx, apks, opts)
}

func (e *Emulator) Crashes(ctx context.Context, q CrashQuery) ([]CrashReport, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.Crashes(ctx, q)
}

func (e *Emulator) Crash(ctx context.Context, id string) ([]byte, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.Crash(ctx, id)
}

func (e *Emulator) Logcat(ctx context.Context, q LogcatQuery) (*Logcat, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	l, err := in.Logcat(ctx, q)
	if err != nil {
		return nil, err
	}
	return e.holdLogcat(l), nil
}

func (e *Emulator) Launch(ctx context.Context, pkg, activity string) (*LaunchResult, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.Launch(ctx, pkg, activity)
}

// CheckLaunch needs no running emulator: the names are checked the way
// the ADBTarget it would delegate to checks them.
func (e *Emulator) CheckLaunch(pkg, activity string) error {
	return checkAndroidLaunch(pkg, activity)
}

func (e *Emulator) OpenURL(ctx context.Context, u string) (*LaunchResult, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.OpenURL(ctx, u)
}

// CheckURL needs no running emulator.
func (e *Emulator) CheckURL(u string) error { return CheckURL(u) }

func (e *Emulator) ComposeDump(ctx context.Context) (*ComposeDump, error) {
	in, done, err := e.running()
	if err != nil {
		return nil, err
	}
	defer done()
	return in.ComposeDump(ctx)
}
