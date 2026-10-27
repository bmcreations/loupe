package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	fakeUnreachable = "CoreDeviceError 1001: not supported by this device"
	fakeUndelivered = "CoreDevice: capability did not answer in 2s"
)

// fakePhoneSimcap writes a shell script that answers `-caps` with
// phoneinput and, for `<id> -phone-input`, speaks the line protocol the way
// a phone's simcap does. A tap is unreachable until the file at awake
// exists, a button is always unreachable, a swipe is undelivered once the awake file exists and unreachable before, and
// `swipe 1 1 2 2 1500` takes 1.5s and writes a `lifted` file next to the binary where a real one would lift, `key 40` replies after a second, which a shrunk budget turns into a missing
// reply, `key 41` exits without replying and `key 4` (typing "a") is always unreachable, and `tap 7 7` answers an
// undelivered error whose text runs over two lines. Every command but `route` is logged. It returns the binary, the awake
// path and the command log.
func fakePhoneSimcap(t *testing.T) (bin, awake, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "simcap")
	awake = filepath.Join(dir, "awake")
	logPath = filepath.Join(dir, "commands")
	script := `#!/bin/sh
if [ "$1" = -caps ]; then echo phoneinput; exit 0; fi
while read -r line; do
  if [ "$line" = route ]; then echo "ok hub"; continue; fi
  echo "$line" >> "` + logPath + `"
  case "$line" in
    size) echo "ok 1206 2622" ;;
    "tap 7 7") printf 'err undelivered: line one\nline two\n' ;;
    tap*) if [ -f "` + awake + `" ]; then echo ok; else echo "err unreachable: ` + fakeUnreachable + `"; fi ;;
    button*) echo "err unreachable: ` + fakeUnreachable + `" ;;
    "swipe 1 1 2 2 1500") sleep 1.5; touch "` + filepath.Join(dir, "lifted") + `"; echo ok ;;
    swipe*) if [ -f "` + awake + `" ]; then echo "err undelivered: ` + fakeUndelivered + `"; else echo "err unreachable: ` + fakeUnreachable + `"; fi ;;
    "key 40") sleep 1; echo ok ;;
    "key 41") exit 7 ;;
    "key 4") echo "err unreachable: ` + fakeUnreachable + `" ;;
    *) echo ok ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, awake, logPath
}

func phoneUnderTest(t *testing.T, bin string) *IOSDevice {
	t.Helper()
	d := NewIOSDevice(IOSDeviceInfo{Identifier: "CORE-ID", UDID: "UDID", Name: "phone"})
	d.SetSimcap(bin)
	t.Cleanup(func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.dropInputLocked()
	})
	return d
}

func shrink[T any](t *testing.T, v *T, to T) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

// wakeCounter replaces the wake seam with one that counts its calls, records
// the device it was asked for, and runs onWake.
func wakeCounter(t *testing.T, onWake func() error) (calls *int, device *string) {
	t.Helper()
	calls, device = new(int), new(string)
	old := wakeIOSDevice
	wakeIOSDevice = func(ctx context.Context, d string) error {
		*calls++
		*device = d
		return onWake()
	}
	t.Cleanup(func() { wakeIOSDevice = old })
	return calls, device
}

// (a) unreachable, then ok after the wake: one wake, the command sent twice.
func TestPhoneInputWakesTheTunnelOnceAndRetries(t *testing.T) {
	bin, awake, logPath := fakePhoneSimcap(t)
	calls, device := wakeCounter(t, func() error { return os.WriteFile(awake, nil, 0o644) })
	d := phoneUnderTest(t, bin)

	if err := d.Tap(t.Context(), 10, 20); err != nil {
		t.Fatalf("Tap after a wake: %v", err)
	}
	if *calls != 1 || *device != "CORE-ID" {
		t.Errorf("wake ran %d times for %q; want once for CORE-ID", *calls, *device)
	}
	if got := commands(t, logPath); len(got) != 2 || got[0] != "tap_10_20" || got[1] != "tap_10_20" {
		t.Errorf("commands = %v; want the tap sent twice", got)
	}
	// Awake now, so a second tap needs no wake.
	if err := d.Tap(t.Context(), 1, 2); err != nil || *calls != 1 {
		t.Errorf("second Tap = %v after %d wakes", err, *calls)
	}
}

// (b) A reply of undelivered means a send went out: no wake and no resend, or
// the action happens twice. This is the double-send the round-one patch had.
func TestPhoneInputUndeliveredIsNeverResent(t *testing.T) {
	bin, awake, logPath := fakePhoneSimcap(t)
	if err := os.WriteFile(awake, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	calls, _ := wakeCounter(t, func() error { return nil })
	d := phoneUnderTest(t, bin)

	err := d.Swipe(t.Context(), 1, 2, 3, 4, 100)
	if !errors.Is(err, ErrPhoneDeliveryUnknown) || errors.Is(err, ErrPhoneUnreachable) {
		t.Fatalf("Swipe = %v; want ErrPhoneDeliveryUnknown only", err)
	}
	if !strings.Contains(err.Error(), fakeUndelivered) {
		t.Errorf("error %q lost the cause %q", err, fakeUndelivered)
	}
	if *calls != 0 {
		t.Errorf("an undelivered command ran the wake %d times", *calls)
	}
	if got := commands(t, logPath); len(got) != 1 {
		t.Errorf("commands = %v; want the swipe sent exactly once", got)
	}
}

// (c) No reply is also a send that may have landed.
func TestPhoneInputWithNoReplyIsDeliveryUnknownAndNotRetried(t *testing.T) {
	bin, _, logPath := fakePhoneSimcap(t)
	calls, _ := wakeCounter(t, func() error { return nil })
	shrink(t, &phoneInputBudget, 200*time.Millisecond)
	d := phoneUnderTest(t, bin)

	err := d.PressKey(t.Context(), 66) // Enter, which the fake answers late
	if !errors.Is(err, ErrPhoneDeliveryUnknown) || errors.Is(err, ErrPhoneUnreachable) || !strings.Contains(err.Error(), "no reply in 200ms") {
		t.Fatalf("PressKey = %v; want ErrPhoneDeliveryUnknown naming the 200ms timeout", err)
	}
	if *calls != 0 {
		t.Errorf("a command with no reply ran the wake %d times", *calls)
	}
	if got := commands(t, logPath); len(got) != 1 {
		t.Errorf("commands = %v; want the key sent exactly once", got)
	}

	// The late `ok` belonged to the key. The process it would have come
	// from is gone, so the next command reads its own answer.
	time.Sleep(1200 * time.Millisecond)
	if w, h, err := d.ScreenSize(t.Context()); err != nil || w != 1206 || h != 2622 {
		t.Errorf("ScreenSize after a timeout = %d %d %v; want 1206 2622", w, h, err)
	}
	if got := commands(t, logPath); len(got) != 2 {
		t.Errorf("commands = %v", got)
	}
}

// (d) unreachable on both attempts: one wake, the command sent twice, and an
// error carrying CoreDevice's text and the wake's outcome.
func TestPhoneInputStillUnreachableAfterAWakeIsReported(t *testing.T) {
	bin, _, logPath := fakePhoneSimcap(t)
	calls, _ := wakeCounter(t, func() error { return errors.New("exit status 1: no tunnel") })
	d := phoneUnderTest(t, bin)

	err := d.PressButton(t.Context(), "home")
	if !errors.Is(err, ErrPhoneUnreachable) || errors.Is(err, ErrPhoneDeliveryUnknown) {
		t.Fatalf("PressButton = %v; want ErrPhoneUnreachable only", err)
	}
	for _, want := range []string{"1001", "not supported by this device", "devicectl wake: exit status 1: no tunnel"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if *calls != 1 {
		t.Errorf("wake ran %d times; want once", *calls)
	}
	if got := commands(t, logPath); len(got) != 2 {
		t.Errorf("commands = %v; want the original and exactly one retry", got)
	}
}

// A wake that succeeds is reported as such.
func TestPhoneInputUnreachableTwiceNamesASuccessfulWake(t *testing.T) {
	bin, _, _ := fakePhoneSimcap(t)
	wakeCounter(t, func() error { return nil })
	d := phoneUnderTest(t, bin)

	err := d.PressButton(t.Context(), "home")
	if !errors.Is(err, ErrPhoneUnreachable) || !strings.Contains(err.Error(), "devicectl wake: ok") {
		t.Fatalf("PressButton = %v; want ErrPhoneUnreachable naming a wake that was ok", err)
	}
}

// Unreachable first, then undelivered on the retry: the retry's send may have
// landed, so it is delivery-unknown, and nothing is sent a third time.
func TestPhoneInputUndeliveredOnTheRetryIsDeliveryUnknown(t *testing.T) {
	bin, awake, logPath := fakePhoneSimcap(t)
	calls, _ := wakeCounter(t, func() error { return os.WriteFile(awake, nil, 0o644) })
	d := phoneUnderTest(t, bin)

	err := d.Swipe(t.Context(), 1, 2, 3, 4, 100) // the fake: unreachable asleep, undelivered awake
	if !errors.Is(err, ErrPhoneDeliveryUnknown) || errors.Is(err, ErrPhoneUnreachable) {
		t.Fatalf("Swipe = %v; want ErrPhoneDeliveryUnknown only", err)
	}
	if *calls != 1 {
		t.Errorf("wake ran %d times; want once", *calls)
	}
	if got := commands(t, logPath); len(got) != 2 {
		t.Errorf("commands = %v; want exactly two sends", got)
	}
}

// A retry that times out is delivery-unknown too, not unreachable.
func TestPhoneInputNoReplyOnTheRetryIsDeliveryUnknown(t *testing.T) {
	bin, awake, logPath := fakePhoneSimcap(t)
	// key 40 is slow; make it unreachable first by using the awake file only
	// for taps, so use a tap that goes slow once awake.
	script, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(script), "tap*) if [ -f \""+awake+"\" ]; then echo ok;", "tap*) if [ -f \""+awake+"\" ]; then sleep 1; echo ok;", 1)
	if patched == string(script) {
		t.Fatal("fake simcap did not take the patch")
	}
	if err := os.WriteFile(bin, []byte(patched), 0o755); err != nil {
		t.Fatal(err)
	}
	calls, _ := wakeCounter(t, func() error { return os.WriteFile(awake, nil, 0o644) })
	shrink(t, &phoneInputBudget, 200*time.Millisecond)
	d := phoneUnderTest(t, bin)

	err = d.Tap(t.Context(), 3, 4)
	if !errors.Is(err, ErrPhoneDeliveryUnknown) || errors.Is(err, ErrPhoneUnreachable) {
		t.Fatalf("Tap = %v; want ErrPhoneDeliveryUnknown only", err)
	}
	if *calls != 1 {
		t.Errorf("wake ran %d times; want once", *calls)
	}
	if got := commands(t, logPath); len(got) != 2 {
		t.Errorf("commands = %v; want exactly two sends", got)
	}
}

// A fresh process that does not answer `route` is stopped before anything is
// sent. Left alive, its late `ok hub` would answer the next command.
func TestPhoneInputRouteTimeoutStopsTheProcessAndSendsNothing(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "simcap")
	logPath := filepath.Join(dir, "commands")
	marker := filepath.Join(dir, "slept")
	pids := filepath.Join(dir, "pids")
	script := `#!/bin/sh
if [ "$1" = -caps ]; then echo phoneinput; exit 0; fi
echo $$ >> "` + pids + `"
while read -r line; do
  if [ "$line" = route ]; then
    if [ -f "` + marker + `" ]; then echo "ok hub"; else touch "` + marker + `"; sleep 1; echo "ok hub"; fi
    continue
  fi
  echo "$line" >> "` + logPath + `"
  case "$line" in
    size) echo "ok 1206 2622" ;;
    *) echo ok ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	calls, _ := wakeCounter(t, func() error { return nil })
	shrink(t, &simInputRouteBudget, 200*time.Millisecond)
	d := phoneUnderTest(t, bin)

	err := d.Tap(t.Context(), 1, 2)
	if err == nil || !errors.Is(err, errSimInputRouteTimeout) || !strings.Contains(err.Error(), "no reply to route in 200ms") {
		t.Fatalf("Tap = %v; want an error naming the route timeout", err)
	}
	if errors.Is(err, ErrPhoneDeliveryUnknown) || errors.Is(err, ErrPhoneUnreachable) {
		t.Errorf("Tap = %v; nothing was sent, so neither phone error applies", err)
	}
	if *calls != 0 {
		t.Errorf("wake ran %d times", *calls)
	}
	if got := commands(t, logPath); len(got) != 0 {
		t.Fatalf("commands = %v; the tap must not be written after a route timeout", got)
	}

	// The timed-out process is stopped, not abandoned: its stdin is closed
	// and it exits. Without the stop it blocks in read forever.
	first := processIDs(t, pids)
	if len(first) != 1 {
		t.Fatalf("launched %v; want one process so far", first)
	}
	requireExited(t, first[0])

	time.Sleep(1200 * time.Millisecond)
	if w, h, err := d.ScreenSize(t.Context()); err != nil || w != 1206 || h != 2622 {
		t.Errorf("ScreenSize after a route timeout = %d %d %v; want 1206 2622", w, h, err)
	}
	if got := commands(t, logPath); len(got) != 1 || got[0] != "size" {
		t.Errorf("commands = %v; want only size", got)
	}
	if all := processIDs(t, pids); len(all) != 2 || all[0] == all[1] {
		t.Errorf("launched %v; want the stopped process then a fresh one", all)
	}
}

// processIDs reads the pids a fake simcap appended to path, one per launch.
func processIDs(t *testing.T, path string) []int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var ids []int
	for _, f := range strings.Fields(string(raw)) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("pid %q: %v", f, err)
		}
		ids = append(ids, n)
	}
	return ids
}

// requireExited fails unless pid has exited within a few seconds.
func requireExited(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A request context that ends while the reply to a written command is pending
// leaves the command's effect unknown: 503 phoneDeliveryUnknown, one send, no
// wake. It used to surface as a bare context error and a codeless 502.
func TestPhoneInputContextEndingAfterTheSendIsDeliveryUnknown(t *testing.T) {
	bin, _, logPath := fakePhoneSimcap(t)
	calls, _ := wakeCounter(t, func() error { return nil })
	d := phoneUnderTest(t, bin)
	// Start the process and cache simcap's capabilities first, so the short
	// context below runs out waiting for the reply and not for startup.
	if _, _, err := d.ScreenSize(t.Context()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := d.PressKey(ctx, 66) // Enter, which the fake answers after a second
	if !errors.Is(err, ErrPhoneDeliveryUnknown) || errors.Is(err, ErrPhoneUnreachable) {
		t.Fatalf("PressKey = %v; want ErrPhoneDeliveryUnknown only", err)
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("error %q does not name the context ending", err)
	}
	if *calls != 0 {
		t.Errorf("wake ran %d times", *calls)
	}
	if got := commands(t, logPath); len(got) != 2 || got[1] != "key_40" {
		t.Errorf("commands = %v; want size, then the key sent exactly once", got)
	}
}

// simcap dying after a command was written is the same: the send may have
// landed.
func TestPhoneInputSimcapExitingAfterTheSendIsDeliveryUnknown(t *testing.T) {
	bin, _, logPath := fakePhoneSimcap(t)
	calls, _ := wakeCounter(t, func() error { return nil })
	d := phoneUnderTest(t, bin)

	err := d.PressKey(t.Context(), 111) // Escape, on which the fake exits
	if !errors.Is(err, ErrPhoneDeliveryUnknown) || errors.Is(err, ErrPhoneUnreachable) {
		t.Fatalf("PressKey = %v; want ErrPhoneDeliveryUnknown only", err)
	}
	if !strings.Contains(err.Error(), errSimcapExited.Error()) {
		t.Errorf("error %q does not say simcap exited", err)
	}
	if *calls != 0 {
		t.Errorf("wake ran %d times", *calls)
	}
	if got := commands(t, logPath); len(got) != 1 {
		t.Errorf("commands = %v; want the key sent exactly once", got)
	}
}

// What a phone input error carries is CoreDevice's text, the wake's result
// and fixed words: never the command line, which for a typed character is
// "key <usage>", so a text action can return it without echoing its payload.
func TestPhoneInputErrorsDoNotCarryTheCommand(t *testing.T) {
	bin, awake, _ := fakePhoneSimcap(t)
	d := phoneUnderTest(t, bin)
	if _, _, err := d.ScreenSize(t.Context()); err != nil { // cache -caps
		t.Fatal(err)
	}

	t.Run("no reply", func(t *testing.T) {
		wakeCounter(t, func() error { return nil })
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		err := d.PressKey(ctx, 66)
		if !errors.Is(err, ErrPhoneDeliveryUnknown) || strings.Contains(err.Error(), "key 40") || strings.Contains(err.Error(), "simcap input") {
			t.Errorf("error %q names the command", err)
		}
	})
	t.Run("waiting for the old helper", func(t *testing.T) {
		// "no reply" left its helper finishing; the wait for it ends with
		// the context, and nothing was sent.
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		err := d.TypeText(ctx, "a")
		if err == nil || strings.Contains(err.Error(), "key ") || strings.Contains(err.Error(), "simcap input") {
			t.Errorf("error %q names the typed key", err)
		}
	})
	t.Run("typed text, no reply", func(t *testing.T) {
		wakeCounter(t, func() error { return nil })
		d := phoneUnderTest(t, bin)
		if _, _, err := d.ScreenSize(t.Context()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		// "\n" is key 40, which the fake answers after a second.
		err := d.TypeText(ctx, "\n")
		if !errors.Is(err, ErrPhoneDeliveryUnknown) || strings.Contains(err.Error(), "key ") || strings.Contains(err.Error(), "simcap input") {
			t.Errorf("error %q names the typed key", err)
		}
	})
	t.Run("typed text, unreachable twice", func(t *testing.T) {
		wakeCounter(t, func() error { return nil })
		_ = os.Remove(awake)
		err := d.TypeText(t.Context(), "a")
		if !errors.Is(err, ErrPhoneUnreachable) || !strings.Contains(err.Error(), fakeUnreachable) || !strings.Contains(err.Error(), "devicectl wake: ok") ||
			strings.Contains(err.Error(), "key ") || strings.Contains(err.Error(), "simcap input") {
			t.Errorf("error %q; want CoreDevice's text and the wake, without the typed key", err)
		}
	})
	t.Run("unreachable twice", func(t *testing.T) {
		wakeCounter(t, func() error { return nil })
		_ = os.Remove(awake)
		err := d.PressButton(t.Context(), "home")
		if !errors.Is(err, ErrPhoneUnreachable) || !strings.Contains(err.Error(), fakeUnreachable) || !strings.Contains(err.Error(), "devicectl wake: ok") || strings.Contains(err.Error(), "button") {
			t.Errorf("error %q; want CoreDevice's text and the wake, without the command", err)
		}
	})
}

// A reply whose text runs over two lines must not shift the replies after it:
// the second line is part of the first error, and the next command gets its own.
func TestPhoneInputAMultiLineErrorDoesNotShiftLaterReplies(t *testing.T) {
	bin, awake, logPath := fakePhoneSimcap(t)
	if err := os.WriteFile(awake, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	calls, _ := wakeCounter(t, func() error { return nil })
	shrink(t, &simInputContinuationGrace, 2*time.Second) // so a slow scheduler cannot split the reply
	d := phoneUnderTest(t, bin)

	err := d.Tap(t.Context(), 7, 7)
	if !errors.Is(err, ErrPhoneDeliveryUnknown) || errors.Is(err, ErrPhoneUnreachable) {
		t.Fatalf("Tap = %v; want ErrPhoneDeliveryUnknown only", err)
	}
	if !strings.Contains(err.Error(), "line one") || !strings.Contains(err.Error(), "line two") {
		t.Errorf("error %q lost a line of the cause", err)
	}
	// "line two" must not have been taken as this command's reply.
	if err := d.Tap(t.Context(), 1, 2); err != nil {
		t.Fatalf("the next Tap = %v; want its own ok", err)
	}
	if *calls != 0 {
		t.Errorf("an undelivered command ran the wake %d times", *calls)
	}
	if got := commands(t, logPath); len(got) != 2 || got[0] != "tap_7_7" || got[1] != "tap_1_2" {
		t.Errorf("commands = %v; want each tap sent exactly once", got)
	}
}

func TestAssembleRepliesFoldsOnlyIntoAnErrAndNeverCountsAContinuation(t *testing.T) {
	shrink(t, &simInputContinuationGrace, time.Minute) // closing in ends the hold
	in, out := make(chan string), make(chan string, 8)
	go assembleReplies(in, out)
	for _, l := range []string{
		"stray before any reply", // nothing waiting: dropped
		"ok 1 2",
		"after an ok", // not an err: dropped
		"err first",
		"second",
		"third",
		"okay is a continuation, not an ok",
		"ok",
		"err",
		"x",
	} {
		in <- l
	}
	close(in)
	var got []string
	for r := range out {
		got = append(got, r)
	}
	want := []string{"ok 1 2", "err first second third okay is a continuation, not an ok", "ok", "err x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("replies = %q; want %q", got, want)
	}
}

// The wake runs devicectl in its own process group and kills the group when
// its context ends. The test owns the timing: the shim records its pids before
// it blocks, the test waits for them, and only then cancels. A fixed timeout
// raced the shim's start (a fresh script can take over 200ms to exec when the
// machine is busy), so the wake sometimes timed out before the shim ran at all.
func TestPhoneInputARealWakeThatHangsIsBounded(t *testing.T) {
	shim := t.TempDir()
	argsPath := filepath.Join(shim, "args")
	pidPath := filepath.Join(shim, "pids")
	// No exec: the shim keeps a grandchild of its own, as devicectl does. The
	// pids file is moved into place last, so its existence means the shim is
	// running, its args are recorded and the sleep is its child.
	body := "#!/bin/sh\necho \"$@\" > \"" + argsPath + "\"\nsleep 30 &\necho \"$$ $!\" > \"" + pidPath + ".tmp\"\nmv \"" + pidPath + ".tmp\" \"" + pidPath + "\"\nwait\n"
	if err := os.WriteFile(filepath.Join(shim, "xcrun"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	shrink(t, &iosWakeTimeout, time.Minute)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- wakeIOSDevice(ctx, "CORE-ID") }()

	var shell, sleeper int
	ready := time.After(20 * time.Second)
	for shell == 0 {
		select {
		case err := <-done:
			if errors.Is(err, syscall.EPERM) {
				t.Skipf("this environment forbids the wake's setpgid: %v", err)
			}
			t.Fatalf("the wake ended before the shim was running: %v", err)
		case <-ready:
			t.Fatal("the shim never recorded its pids")
		case <-time.After(10 * time.Millisecond):
		}
		if raw, err := os.ReadFile(pidPath); err == nil {
			if _, err := fmt.Sscan(string(raw), &shell, &sleeper); err != nil {
				t.Fatalf("recorded pids %q: %v", raw, err)
			}
		}
	}

	began := time.Now()
	cancel()
	select {
	case err := <-done:
		// The WaitDelay backstop is 2s; a group kill returns well before it.
		if took := time.Since(began); took > 1500*time.Millisecond {
			t.Errorf("the wake took %v to end after its context did; the group kill did not reach the grandchild", took)
		}
		if err == nil || !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "cancelled") {
			t.Errorf("wake = %v; want an error saying it was cut short", err)
		}
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-shell, syscall.SIGKILL)
		t.Fatal("the wake did not return after its context ended")
	}

	if got := readTrim(t, argsPath); got != "devicectl device info details --device CORE-ID" {
		t.Errorf("xcrun args = %q", got)
	}
	// The group id is the shim's pid: both the shell and its sleep must be gone.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(-shell, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-shell, syscall.SIGKILL)
			t.Fatalf("process group %d (shell %d, sleep %d) outlived the wake", shell, shell, sleeper)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// Once a character has landed, a later unreachable one is not "nothing reached
// the phone": a caller who retried on ErrPhoneUnreachable would type the
// landed characters twice.
func TestPhoneTextUnreachableAfterACharacterLandedIsDeliveryUnknown(t *testing.T) {
	bin, _, logPath := fakePhoneSimcap(t)
	wakeCounter(t, func() error { return nil })
	d := phoneUnderTest(t, bin)

	// "b" is key 5, which lands; "a" is key 4, which the fake never reaches.
	err := d.TypeText(t.Context(), "ba")
	if !errors.Is(err, ErrPhoneDeliveryUnknown) || errors.Is(err, ErrPhoneUnreachable) {
		t.Fatalf("TypeText = %v; want ErrPhoneDeliveryUnknown only", err)
	}
	if strings.Contains(err.Error(), "key ") {
		t.Errorf("error %q names a typed key", err)
	}
	if got := commands(t, logPath); len(got) != 3 || got[0] != "key_5" {
		t.Errorf("commands = %v; want key 5 once, then key 4 sent and retried", got)
	}
}

// A request cancelled while simcap -caps runs is no answer, so it must not be
// cached as "this simcap has no -phone-input".
func TestPhoneInputCapsAskedAgainAfterACancelledRequest(t *testing.T) {
	bin, awake, logPath := fakePhoneSimcap(t)
	if err := os.WriteFile(awake, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := phoneUnderTest(t, bin)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.Tap(cancelled, 10, 20); !errors.Is(err, context.Canceled) {
		t.Fatalf("Tap with a cancelled context = %v; want context.Canceled", err)
	}
	if err := d.Tap(t.Context(), 10, 20); err != nil {
		t.Fatalf("Tap after the cancelled one: %v", err)
	}
	if got := commands(t, logPath); len(got) != 1 || got[0] != "tap_10_20" {
		t.Errorf("commands = %v; want the second tap to reach simcap", got)
	}
}

// A request that gives up on a slow swipe stops the helper, and the helper
// must still lift: the kill waits for the gesture, not a fixed second.
func TestPhoneInputStopLetsAPlayingSwipeFinish(t *testing.T) {
	bin, awake, _ := fakePhoneSimcap(t)
	if err := os.WriteFile(awake, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lifted := filepath.Join(filepath.Dir(bin), "lifted")
	d := phoneUnderTest(t, bin)

	// Warm the caps cache and the helper, so the 100ms below lands in the swipe
	// and not in `simcap -caps`.
	if err := d.Tap(t.Context(), 9, 9); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := d.Swipe(ctx, 1, 1, 2, 2, 1500); !errors.Is(err, ErrPhoneDeliveryUnknown) {
		t.Fatalf("Swipe = %v; want ErrPhoneDeliveryUnknown", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(lifted); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper was killed before the swipe lifted")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Two helpers must never drive one phone at once: a command after a cancelled
// swipe waits for the old helper to lift and exit before starting a new one.
func TestPhoneInputNextCommandWaitsForTheCancelledSwipeToLift(t *testing.T) {
	bin, awake, logPath := fakePhoneSimcap(t)
	if err := os.WriteFile(awake, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lifted := filepath.Join(filepath.Dir(bin), "lifted")
	d := phoneUnderTest(t, bin)

	// Warm the caps cache and the helper, so the 100ms below lands in the swipe
	// and not in `simcap -caps`.
	if err := d.Tap(t.Context(), 9, 9); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_ = d.Swipe(ctx, 1, 1, 2, 2, 1500)
	if err := d.Tap(t.Context(), 3, 4); err != nil {
		t.Fatalf("Tap after a cancelled swipe: %v", err)
	}
	if _, err := os.Stat(lifted); err != nil {
		t.Fatal("the next tap ran before the cancelled swipe lifted")
	}
	if got := commands(t, logPath); got[len(got)-1] != "tap_3_4" {
		t.Errorf("commands = %v; want the tap last", got)
	}
}

// Waiting for the old helper honours the caller's context, and a command that
// gave up while waiting was never sent.
func TestPhoneInputWaitForTheOldHelperEndsWithTheContext(t *testing.T) {
	bin, awake, logPath := fakePhoneSimcap(t)
	if err := os.WriteFile(awake, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wakeCounter(t, func() error { return nil })
	d := phoneUnderTest(t, bin)
	if err := d.Tap(t.Context(), 9, 9); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_ = d.Swipe(ctx, 1, 1, 2, 2, 1500)

	waitCtx, cancelWait := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancelWait()
	start := time.Now()
	err := d.Tap(waitCtx, 3, 4)
	if took := time.Since(start); took > time.Second {
		t.Errorf("Tap took %s; want it to give up with its context", took)
	}
	if err == nil || errors.Is(err, ErrPhoneDeliveryUnknown) {
		t.Errorf("Tap = %v; want an error saying nothing was sent", err)
	}
	for _, c := range commands(t, logPath) {
		if c == "tap_3_4" {
			t.Errorf("commands = %v; the tap was sent while the old helper ran", commands(t, logPath))
		}
	}
}
