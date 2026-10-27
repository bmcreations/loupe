package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// What happened on 2026-09-27: the adb server reset every connection, no
// device list arrived, and avd:Pixel_10 -- running all along as
// emulator-5554 -- looked not running. Every open tried to boot it and the
// viewer sat on "Connecting". With adb reported down, the AVD and the device
// both say why instead, and opening one fails with that reason before any
// boot is tried.
func TestAdbDownMakesAdbTargetsUnavailableAndSaysWhy(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(newAVD("Pixel_10"))
	m.SyncDevices([]target.DeviceInfo{acmeReady}, buildADB)

	const fault = "adb track-devices: exit status 1: protocol fault (couldn't read status): Connection reset by peer"
	m.SetADBDown(fault)

	for _, id := range []string{"avd:Pixel_10", acmeReady.Serial} {
		got := infoFor(t, m, id)
		if got.Available || got.State != adbState {
			t.Errorf("%s: available=%v state=%q, want false/%q", id, got.Available, got.State, adbState)
		}
		if !strings.Contains(got.Reason, "adb is not responding") || !strings.Contains(got.Reason, "protocol fault") {
			t.Errorf("%s: reason %q does not say adb is not responding and why", id, got.Reason)
		}
	}

	_, err := m.Acquire(context.Background(), "avd:Pixel_10")
	if err == nil || !strings.Contains(err.Error(), "adb is not responding") {
		t.Fatalf("Acquire err = %v, want adb named as not responding", err)
	}
	if got := infoFor(t, m, "avd:Pixel_10").LastError; got != err.Error() {
		t.Errorf("lastError = %q, want %q", got, err.Error())
	}

	m.SetADBDown("")
	for _, id := range []string{"avd:Pixel_10", acmeReady.Serial} {
		if got := infoFor(t, m, id); !got.Available || got.State == adbState {
			t.Errorf("%s after adb recovered: available=%v state=%q", id, got.Available, got.State)
		}
	}
}
