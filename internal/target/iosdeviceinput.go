package target

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A physical iPhone's input is `simcap <identifier> -phone-input`, which
// speaks the simulator input process's line protocol over CoreDevice's HID
// capabilities, so simInput drives it unchanged. Coordinates are the
// phone's screen pixels, the space devicectl's screenshot uses: 1206x2622
// on an iPhone 16 Pro, which is also what CoreDevice reports as the
// display's native size. Unlike streaming it needs no cable; CoreDevice
// HID opened over Wi-Fi as well.

// phoneInputBudget is what a phone's command gets before a swipe's own
// duration is added. A command on a cold process opens CoreDevice's HID
// capabilities (simcap waits up to 5s), reads the display size (4s) and
// validates the capability after the send (2s), 11s in all, so this is
// longer. With the simulator's 5s, Go gave up first, killed simcap mid-open,
// and CoreDevice's own error never arrived. A var so tests can shrink it.
var phoneInputBudget = 15 * time.Second

// iosWakeTimeout bounds the devicectl call that brings a phone's tunnel up.
var iosWakeTimeout = 15 * time.Second

// IOSInputDeadline is the longest one phone input call can take: a command
// that times out, the wake, and the one retry, plus a slow swipe's own
// duration. A request context shorter than this ends the call with a bare
// context error before any of those can name a cause.
func IOSInputDeadline() time.Duration {
	return 2*phoneInputBudget + iosWakeTimeout + 5*time.Second
}

// ErrPhoneUnreachable is a phone input that was not accepted: simcap replied
// `unreachable` (CoreDevice failed before anything was sent: 1001, "not
// supported by this device", a failed open, or a capability that did not
// answer), a devicectl wake and one resend did not change that, and the
// second attempt was refused the same way. Nothing reached the phone, so the
// caller may retry. The error carries CoreDevice's text and the wake's outcome.
var ErrPhoneUnreachable = errors.New("the phone is unreachable through CoreDevice")

// ErrPhoneDeliveryUnknown is a phone input where a send left the Mac and
// whether the phone got it is unknown: simcap replied `undelivered`, or did
// not reply within its budget. It is not retried, here or by a caller that
// cannot tell, because resending would act twice (a second tap, a second Home
// press, a doubled character).
var ErrPhoneDeliveryUnknown = errors.New("a send went out and whether the phone received it is unknown")

// wakeIOSDevice brings a phone's CoreDevice tunnel up. A Wi-Fi phone's
// tunnel idles to `disconnected` and every HID capability then fails with
// 1001; `devicectl device info details` brought it back by hand. A var so
// tests do not run devicectl.
var wakeIOSDevice = func(ctx context.Context, device string) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, iosWakeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "info", "details", "--device", device)
	// devicectl's children can hold the output pipe open after devicectl is
	// killed, so the whole process group goes; WaitDelay is the backstop.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		if parent.Err() != nil {
			return fmt.Errorf("cancelled (%w)", parent.Err())
		}
		return fmt.Errorf("timed out after %s", iosWakeTimeout)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// errNoPhoneInput is an input call on a device whose simcap cannot drive it.
var errNoPhoneInput = errors.New("this simcap has no -phone-input; run make simcap")

// input returns the device's input process, creating it on first use.
func (d *IOSDevice) input(ctx context.Context) (*simInput, error) {
	d.mu.Lock()
	bin, id, known := d.simcap, d.info.Identifier, d.phoneInput
	d.mu.Unlock()
	if bin == "" {
		return nil, fmt.Errorf("simcap is not built (make simcap), and it is what drives a physical iPhone's input")
	}
	if id == "" {
		return nil, fmt.Errorf("devicectl gave %s no CoreDevice identifier, which its input is opened by", d.Label())
	}
	if known == nil {
		ok, err := simcapHas(ctx, bin, "phoneinput")
		if err != nil {
			// No answer is not "absent": a cancelled request must not
			// disable the phone's input until loupe restarts.
			return nil, err
		}
		known = &ok
		d.mu.Lock()
		if d.simcap == bin {
			d.phoneInput = known
		}
		d.mu.Unlock()
	}
	if !*known {
		return nil, errNoPhoneInput
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.in == nil || d.in.udid != d.info.Identifier || d.in.bin != d.simcap {
		d.dropInputLocked()
		d.in = &simInput{bin: d.simcap, udid: d.info.Identifier, mode: "-phone-input", budget: phoneInputBudget}
	}
	return d.in, nil
}

// dropInputLocked stops the input process. Called with mu held.
func (d *IOSDevice) dropInputLocked() {
	if d.in != nil {
		go d.in.close()
		d.in = nil
	}
}

// simcapHas reports whether simcap's -caps lists capability. The error is
// set when there was no answer -- the context ended, or simcap could not be
// started -- and the caller must not cache the false that goes with it. A
// simcap that ran and exited non-zero is an answer: an old build reads -caps
// as a UDID and fails, which means the capability is absent.
func simcapHas(ctx context.Context, bin, capability string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-caps").Output()
	if ctx.Err() != nil {
		return false, fmt.Errorf("simcap -caps: %w", ctx.Err())
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return false, nil
		}
		return false, fmt.Errorf("simcap -caps: %w", err)
	}
	return slices.Contains(strings.Fields(string(out)), capability), nil
}

// do sends one command. CoreDevice offers no delivery receipt, so `ok` here
// means simcap sent it and the capability still answered afterwards, not
// that the phone received it (see HubHID.validate).
//
// Only an `unreachable` reply is retried. It means nothing was sent, so the
// server wakes the tunnel with devicectl and sends the command once more; a
// second `unreachable` is ErrPhoneUnreachable. An `undelivered` reply, or any
// failure after the command was written to simcap (no reply within the
// budget, the request's context ending, simcap exiting), means a send went
// out and may have landed, so it is ErrPhoneDeliveryUnknown with no wake and
// no resend, on the first attempt and on the retry alike. A failure before
// anything was written keeps its own error.
//
// The messages of the two phone errors are built from CoreDevice's reply, the
// wake's outcome and fixed words only. They leave out the command line, which
// for a typed character names the key, so a text action can return them.
func (d *IOSDevice) do(ctx context.Context, budget time.Duration, format string, a ...any) (string, error) {
	if line := fmt.Sprintf(format, a...); runnerVerb(line) && d.touchesViaRunner() {
		return d.runnerDo(ctx, budget, line)
	}
	in, err := d.input(ctx)
	if err != nil {
		return "", err
	}
	out, err := in.do(ctx, budget, format, a...)
	if err == nil {
		return out, nil
	}
	if deliveryUnknown(err) {
		return "", fmt.Errorf("%w: %v", ErrPhoneDeliveryUnknown, failureCause(err))
	}
	if !errors.Is(err, errUnreachableReply) {
		return "", failureCause(err)
	}
	first := err
	wake := "ok"
	if werr := wakeIOSDevice(ctx, d.device()); werr != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: %v (devicectl wake: %v)", ErrPhoneUnreachable, first, ctx.Err())
		}
		wake = werr.Error()
	}
	out, err = in.do(ctx, budget, format, a...)
	if err == nil {
		return out, nil
	}
	if deliveryUnknown(err) {
		return "", fmt.Errorf("%w: %v (after devicectl wake: %s)", ErrPhoneDeliveryUnknown, failureCause(err), wake)
	}
	if errors.Is(err, errUnreachableReply) {
		return "", fmt.Errorf("%w: %v (devicectl wake: %s)", ErrPhoneUnreachable, failureCause(err), wake)
	}
	return "", failureCause(err)
}

// touchesViaRunner reports whether this phone's touches go to the XCUITest
// runner: below iOS 27, where CoreDevice's digitizer answers ok and nothing
// lands. An OS version that does not parse stays on CoreDevice.
func (d *IOSDevice) touchesViaRunner() bool {
	major, _, _ := strings.Cut(d.Info().OSVersion, ".")
	v, err := strconv.Atoi(major)
	return err == nil && v < 27
}

// runnerDo sends one line to the phone's XCUITest runner, starting it when
// it is not answering.
func (d *IOSDevice) runnerDo(ctx context.Context, budget time.Duration, line string) (string, error) {
	d.mu.Lock()
	info, dir := d.info, d.xcrunner
	if info.Transport != "wired" {
		d.mu.Unlock()
		return "", ErrTouchNeedsCable
	}
	// Looked for on each touch until it is there, not once at startup: a
	// `make xcrunner` after the server started would otherwise answer
	// ErrRunnerNotBuilt until the next restart.
	if dir == "" || !xcRunnerBuilt(dir) {
		d.mu.Unlock()
		return "", ErrRunnerNotBuilt
	}
	if d.runner == nil || d.runner.udid != info.UDID || d.runner.dir != dir {
		d.dropRunnerLocked()
		d.runner = &xcRunner{udid: info.UDID, dir: dir}
	}
	x := d.runner
	d.mu.Unlock()
	return x.do(ctx, budget, "%s", line)
}

// SetXCRunner records the built runner's products directory, or "" when it
// is not built.
func (d *IOSDevice) SetXCRunner(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dir != d.xcrunner {
		d.dropRunnerLocked()
	}
	d.xcrunner = dir
}

// dropRunnerLocked forgets the runner and stops an xcodebuild this server
// started for it. Called with mu held.
func (d *IOSDevice) dropRunnerLocked() {
	if d.runner != nil {
		go d.runner.close()
		d.runner = nil
	}
}

// deliveryUnknown reports whether a send may have reached the phone: simcap
// said `undelivered`, or the command was written and no reply followed.
func deliveryUnknown(err error) bool {
	return errors.Is(err, errUndeliveredReply) || errors.Is(err, errSimInputTimeout) || sentUnanswered(err)
}

// Tap implements Actuator, in screen pixels.
func (d *IOSDevice) Tap(ctx context.Context, x, y int) error {
	_, err := d.do(ctx, 0, "tap %d %d", x, y)
	return err
}

// Swipe implements Actuator.
func (d *IOSDevice) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error {
	_, err := d.do(ctx, time.Duration(ms)*time.Millisecond, "swipe %d %d %d %d %d", x1, y1, x2, y2, ms)
	return err
}

// TypeText implements Actuator one keystroke per character, on the US
// layout a simulator's text uses. Every character is checked before any is
// sent.
func (d *IOSDevice) TypeText(ctx context.Context, text string) error {
	keys := make([]iosKey, 0, len(text))
	for _, r := range text {
		k, ok := charToIOS[r]
		if !ok {
			return fmt.Errorf("cannot type %q on an iPhone: only US keyboard characters are mapped", r)
		}
		keys = append(keys, k)
	}
	for i, k := range keys {
		if _, err := d.do(ctx, 0, "%s", k.command()); err != nil {
			// Earlier characters landed, so a retry would type them twice.
			if i > 0 && errors.Is(err, ErrPhoneUnreachable) {
				cause := strings.TrimPrefix(err.Error(), ErrPhoneUnreachable.Error()+": ")
				return fmt.Errorf("%w: %d of %d characters typed, then %s", ErrPhoneDeliveryUnknown, i, len(keys), cause)
			}
			return err
		}
	}
	return nil
}

// PressKey implements Actuator with the simulator's keycode table.
func (d *IOSDevice) PressKey(ctx context.Context, keycode int) error {
	k, ok := androidToIOS[int32(keycode)]
	if !ok {
		return fmt.Errorf("keycode %d has no iOS equivalent", keycode)
	}
	if k.button != "" {
		return d.PressButton(ctx, k.button)
	}
	_, err := d.do(ctx, 0, "%s", k.command())
	return err
}

// PressButton implements ButtonPresser: home and lock are CoreDevice's
// consumer usages, and the app switcher is two Home presses.
func (d *IOSDevice) PressButton(ctx context.Context, name string) error {
	if !slices.Contains(iosButtons, name) {
		return fmt.Errorf("%w %q; want %s", ErrUnknownButton, name, strings.Join(iosButtons, ", "))
	}
	_, err := d.do(ctx, 0, "button %s", name)
	return err
}

// Buttons implements ButtonPresser.
func (d *IOSDevice) Buttons() []string { return iosButtons }

// ScreenSize implements ScreenSizer from CoreDevice's display info.
func (d *IOSDevice) ScreenSize(ctx context.Context) (int, int, error) {
	out, err := d.do(ctx, 0, "size")
	if err != nil {
		return 0, 0, err
	}
	var w, h int
	if _, err := fmt.Sscanf(out, "%d %d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("simcap size %q", out)
	}
	return w, h, nil
}

// keyContext implements simcapInputOwner. A phone has no Device Hub hold
// to check.
func (d *IOSDevice) keyContext(ctx context.Context) (context.Context, error) { return ctx, nil }

var (
	_ Actuator      = (*IOSDevice)(nil)
	_ ButtonPresser = (*IOSDevice)(nil)
	_ ScreenSizer   = (*IOSDevice)(nil)
)
