package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

var iphoneWiFi = target.IOSDeviceInfo{
	Identifier: "11111111-AAAA-BBBB-CCCC-000000000001", UDID: "00008150-000A1B2C3D4E5F60",
	Name: "Brandon's iPhone", OSVersion: "27.0", Form: "phone", Transport: "localNetwork",
}

func TestAnIOSDeviceIsListedAvailableWhileReachable(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncIOSDevices([]target.IOSDeviceInfo{iphoneWiFi})

	got := infoFor(t, m, "ios:"+iphoneWiFi.UDID)
	if got.Kind != "iosDevice" || got.Label != "Brandon's iPhone" || !got.Available || got.State != "device" {
		t.Fatalf("listed as %+v", got)
	}
	if got.OS != "ios" || got.OSVersion != "27.0" || got.Form != "phone" {
		t.Errorf("described as %+v", got.Description)
	}
}

func TestAnUnreachableIOSDeviceSaysHowToReachIt(t *testing.T) {
	m := NewManager(target.Options{})
	away := iphoneWiFi
	away.Transport = ""
	m.SyncIOSDevices([]target.IOSDeviceInfo{away})

	got := infoFor(t, m, "ios:"+iphoneWiFi.UDID)
	if got.Available || got.State != "offline" || !strings.Contains(got.Reason, "network") || strings.Contains(got.Reason, "adb") {
		t.Fatalf("unreachable: %+v", got)
	}

	m.SyncIOSDevices([]target.IOSDeviceInfo{iphoneWiFi})
	if !infoFor(t, m, "ios:"+iphoneWiFi.UDID).Available {
		t.Error("back on the network and still unavailable")
	}

	m.SyncIOSDevices(nil)
	got = infoFor(t, m, "ios:"+iphoneWiFi.UDID)
	if got.Available || got.State != "missing" || !strings.Contains(got.Reason, "devicectl") {
		t.Errorf("unpaired: %+v", got)
	}
}

// Neither adb nor simctl lists a physical iPhone, so their syncs must leave
// it alone, and its sync must leave theirs alone.
func TestIOSDeviceSyncsAreIndependentOfTheOthers(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncIOSDevices([]target.IOSDeviceInfo{iphoneWiFi})
	m.SyncSimulators([]target.SimInfo{simProbe}, "")
	m.SyncDevices([]target.DeviceInfo{acmeReady}, buildADB)
	m.SyncDevices(nil, buildADB)
	m.SyncSimulators(nil, "")
	if !infoFor(t, m, "ios:"+iphoneWiFi.UDID).Available {
		t.Error("an adb or simctl sync marked the iPhone missing")
	}

	m.SyncSimulators([]target.SimInfo{simProbe}, "")
	m.SyncIOSDevices(nil)
	if !infoFor(t, m, target.SimulatorID(simProbe.UDID)).Available {
		t.Error("a devicectl sync marked a simulator missing")
	}
}

// A phone that cannot stream must never be what the viewer opens by
// default, so it lists after Android devices; the client's chooseTarget
// picks a device or an emulator first.
func TestIOSDevicesListAfterAndroidDevicesAndBeforeEmulators(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncSimulators([]target.SimInfo{simProbe}, "")
	m.SyncIOSDevices([]target.IOSDeviceInfo{iphoneWiFi})
	m.SyncDevices([]target.DeviceInfo{acmeReady}, buildADB)

	var kinds []string
	for _, ti := range m.Targets(context.Background()) {
		kinds = append(kinds, ti.Kind)
	}
	if got := strings.Join(kinds, ","); got != "device,iosDevice,simulator" {
		t.Errorf("order %s", got)
	}
}

func TestOpeningAnIOSDeviceExplainsWhy(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncIOSDevices([]target.IOSDeviceInfo{iphoneWiFi})
	if _, err := m.Acquire(context.Background(), "ios:"+iphoneWiFi.UDID); err == nil || !strings.Contains(err.Error(), "simcap is not built") {
		t.Errorf("Acquire with no simcap: %v", err)
	}
	// Reaches a device listed before it and one listed after.
	m.SetIOSSimcap("/nonexistent/simcap")
	later := iphoneWiFi
	later.UDID, later.Identifier = "00008150-000A1B2C3D4E5F61", "11111111-AAAA-BBBB-CCCC-000000000002"
	m.SyncIOSDevices([]target.IOSDeviceInfo{iphoneWiFi, later})
	for _, d := range []target.IOSDeviceInfo{iphoneWiFi, later} {
		if _, err := m.Acquire(context.Background(), "ios:"+d.UDID); !errors.Is(err, target.ErrIOSStreamNeedsCable) {
			t.Errorf("Acquire %s over Wi-Fi: %v", d.UDID, err)
		}
	}
}
