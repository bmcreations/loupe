package target

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestADBKeyboardWithNoStreamIsRefused pins what an Android target does
// with the switch while nothing is streaming: on is ErrNoSession, since the
// keyboard lives in a scrcpy server that is not running, and is not kept for
// a later stream -- the next phone to connect would lose its on-screen
// keyboard without asking. Off needs nothing and succeeds.
func TestADBKeyboardWithNoStreamIsRefused(t *testing.T) {
	tgt := NewADBTarget(ADB{}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	if err := tgt.SetHardwareKeyboard(context.Background(), true); !errors.Is(err, ErrNoSession) {
		t.Fatalf("on with no stream: err = %v, want ErrNoSession", err)
	}
	if tgt.hwKeyboard {
		t.Fatal("the setting was kept with no session to scope it")
	}
	if err := tgt.SetHardwareKeyboard(context.Background(), false); err != nil {
		t.Fatalf("off with no stream: %v", err)
	}
}

// TestTheLastStreamClosingClearsTheKeyboard: the setting lasts as long as a
// stream is open, including across a relaunch's overlap, and not after.
func TestTheLastStreamClosingClearsTheKeyboard(t *testing.T) {
	tgt := NewADBTarget(ADB{}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	a, b := &adbStream{}, &adbStream{}
	tgt.streams = map[*adbStream]struct{}{a: {}, b: {}}
	tgt.hwKeyboard = true
	tgt.untrack(a)
	if !tgt.hwKeyboard {
		t.Fatal("cleared while a stream was still open")
	}
	tgt.untrack(b)
	if tgt.hwKeyboard {
		t.Fatal("still set after the last stream closed")
	}
}

// TestADBTextFallsBackToADBWithNoStream: with nothing streaming there is no
// control socket, so TypeText has to take the adb route rather than fail.
// The device here is not ready, so reaching adb is the error that proves
// the route was taken.
func TestADBTextFallsBackToADBWithNoStream(t *testing.T) {
	tgt := NewADBTarget(ADB{}, DeviceInfo{Serial: "s", State: "offline"}, "", "")
	if tgt.liveStream() != nil {
		t.Fatal("a fresh target reports a live stream")
	}
	err := tgt.TypeText(context.Background(), "a")
	if err == nil || errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v; want adb's not-ready error", err)
	}
}

// TestTextSettleTime pins the wait a key takes behind typed text: none
// behind one character, which is how a phone types, and a cap for a paste.
func TestTextSettleTime(t *testing.T) {
	for n, want := range map[int]time.Duration{
		0:    0,
		1:    0,
		8:    460 * time.Millisecond,
		1000: time.Second,
	} {
		if got := textSettleTime(n); got != want {
			t.Errorf("textSettleTime(%d) = %v, want %v", n, got, want)
		}
	}
}
