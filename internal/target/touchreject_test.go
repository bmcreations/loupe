package target

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The line Android 16 logged for a tap `input` sent while a pointer was
// stuck down, on emulator-5554 (the Acme One's said the same with deviceId 4).
const rejectedDown = `--------- beginning of main
         1790799762.443   656   787 E InputDispatcher: Inconsistent event: MotionEvent { action=DOWN, id[0]=0, x[0]=540, y[0]=10, eventTime=773870000000ns, downTime=773870000000ns, deviceId=-1, source=TOUCHSCREEN, displayId=0, eventId=0xc00d78df}, reason: Injection on 0: Invalid DOWN event - pointers already down for device DeviceId(-1): {DeviceId(-1): {0}}`

func TestTouchRejectionReadsTheDispatchersReason(t *testing.T) {
	got, ok := touchRejection(rejectedDown)
	if !ok {
		t.Fatal("rejection not found")
	}
	want := "Invalid DOWN event - pointers already down for device DeviceId(-1): {DeviceId(-1): {0}}"
	if got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}

func TestTouchRejectionIgnoresAQuietLog(t *testing.T) {
	for _, log := range []string{"", "--------- beginning of main\n", "E InputDispatcher: channel is unrecoverably broken"} {
		if r, ok := touchRejection(log); ok {
			t.Errorf("touchRejection(%q) = %q, want none", log, r)
		}
	}
}

// `input` exits 0 for a touch the dispatcher threw away, so a tap reported
// ok while nothing happened on the device (trap 50). The rejection is the
// only sign, and it has to come back as an error.
func TestTapReportsATouchTheDeviceRejected(t *testing.T) {
	script := writeFakeADB(t)
	tgt := NewADBTarget(ADB{Path: script}, DeviceInfo{Serial: "fake-1", State: "device"}, "", "")

	t.Setenv("FAKE_ADB_DUMP", touchLogMarker+"\n"+rejectedDown)
	err := tgt.Tap(context.Background(), 540, 10)
	if !errors.Is(err, ErrTouchRejected) {
		t.Fatalf("Tap = %v, want ErrTouchRejected", err)
	}
	if !strings.Contains(err.Error(), "pointers already down") {
		t.Errorf("error %q does not carry the device's reason", err)
	}

	t.Setenv("FAKE_ADB_DUMP", touchLogMarker+"\n--------- beginning of main")
	if err := tgt.Swipe(context.Background(), 1, 2, 3, 4, 300); err != nil {
		t.Errorf("Swipe with a quiet log = %v, want nil", err)
	}
}

// A rejection that appears only in `input`'s own output, before the log
// section, is not read as one.
func TestTapReadsOnlyTheLogSection(t *testing.T) {
	script := writeFakeADB(t)
	tgt := NewADBTarget(ADB{Path: script}, DeviceInfo{Serial: "fake-1", State: "device"}, "", "")
	t.Setenv("FAKE_ADB_DUMP", rejectedDown+"\n"+touchLogMarker)
	if err := tgt.Tap(context.Background(), 1, 1); err != nil {
		t.Errorf("Tap = %v, want nil", err)
	}
}
