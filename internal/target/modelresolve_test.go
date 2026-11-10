package target

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeModelADB answers getprop ro.product.model with model and logs each call.
func fakeModelADB(t *testing.T, model string) (adb ADB, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "fake-adb")
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\n" +
		"case \"$*\" in *'getprop ro.product.model') echo '" + model + "' ;; esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return ADB{Path: bin}, calls
}

func callCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// The line is what adb printed for a Pixel 9 reserved through Firebase and
// attached by `android device remote connect` on 10 November 2026.
func TestModelResolverNamesADeviceListedWithoutAModel(t *testing.T) {
	listed, err := parseDevices("List of devices attached\nlocalhost:65090        device transport_id:21\n")
	if err != nil || len(listed) != 1 {
		t.Fatalf("parseDevices = %v, %v", listed, err)
	}
	if got := listed[0].Label(); got != "localhost:65090" {
		t.Fatalf("unresolved label = %q, want the bare serial", got)
	}

	adb, calls := fakeModelADB(t, "Pixel 9")
	r := NewModelResolver(adb)
	got := r.Resolve(context.Background(), listed)
	if got[0].Label() != "Pixel 9 (localhost:65090)" {
		t.Errorf("label = %q, want %q", got[0].Label(), "Pixel 9 (localhost:65090)")
	}
	if listed[0].Model != "" {
		t.Errorf("Resolve changed its argument: Model = %q", listed[0].Model)
	}

	// The same connection is not asked again.
	r.Resolve(context.Background(), listed)
	if n := callCount(t, calls); n != 1 {
		t.Errorf("getprop ran %d times for one connection, want 1", n)
	}

	// The port reused by another reservation is a new connection.
	listed[0].TransportID = "22"
	r.Resolve(context.Background(), listed)
	if n := callCount(t, calls); n != 2 {
		t.Errorf("getprop ran %d times after the transport changed, want 2", n)
	}
}

func TestModelResolverLeavesOtherDevicesAlone(t *testing.T) {
	adb, calls := fakeModelADB(t, "Wrong")
	devices := []DeviceInfo{
		{Serial: "R5CT", State: "device", Model: "SM_S921U", TransportID: "1"},
		{Serial: "emulator-5554", State: "device", Emulator: true, TransportID: "2"},
		{Serial: "localhost:1", State: "offline", TransportID: "3"},
		{Serial: "localhost:2", State: "unauthorized", TransportID: "4"},
	}
	got := NewModelResolver(adb).Resolve(context.Background(), devices)
	for i, d := range got {
		if d.Model != devices[i].Model {
			t.Errorf("%s: Model = %q, want %q", d.Serial, d.Model, devices[i].Model)
		}
	}
	if n := callCount(t, calls); n != 0 {
		t.Errorf("getprop ran %d times, want 0", n)
	}
}
