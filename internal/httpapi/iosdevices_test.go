package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target/ios"

	"github.com/bmcreations/loupe/internal/target/android"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

var iphoneWiFi = ios.DeviceInfo{
	Identifier: "11111111-AAAA-BBBB-CCCC-000000000001", UDID: "00008150-000A1B2C3D4E5F60",
	Name: "Brandon's iPhone", OSVersion: "27.0", Form: "phone", Transport: "localNetwork",
}

func TestAnIOSDeviceIsListedAvailableWhileReachable(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncIOSDevices([]ios.DeviceInfo{iphoneWiFi})

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
	m.SyncIOSDevices([]ios.DeviceInfo{away})

	got := infoFor(t, m, "ios:"+iphoneWiFi.UDID)
	if got.Available || got.State != "offline" || !strings.Contains(got.Reason, "network") || strings.Contains(got.Reason, "adb") {
		t.Fatalf("unreachable: %+v", got)
	}

	m.SyncIOSDevices([]ios.DeviceInfo{iphoneWiFi})
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
	m.SyncIOSDevices([]ios.DeviceInfo{iphoneWiFi})
	m.SyncSimulators([]ios.SimInfo{simProbe}, "")
	m.SyncDevices([]android.DeviceInfo{acmeReady}, buildADB)
	m.SyncDevices(nil, buildADB)
	m.SyncSimulators(nil, "")
	if !infoFor(t, m, "ios:"+iphoneWiFi.UDID).Available {
		t.Error("an adb or simctl sync marked the iPhone missing")
	}

	m.SyncSimulators([]ios.SimInfo{simProbe}, "")
	m.SyncIOSDevices(nil)
	if !infoFor(t, m, ios.SimulatorID(simProbe.UDID)).Available {
		t.Error("a devicectl sync marked a simulator missing")
	}
}

// A phone that cannot stream must never be what the viewer opens by
// default, so it lists after Android devices; the client's chooseTarget
// picks a device or an emulator first.
func TestIOSDevicesListAfterAndroidDevicesAndBeforeEmulators(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncSimulators([]ios.SimInfo{simProbe}, "")
	m.SyncIOSDevices([]ios.DeviceInfo{iphoneWiFi})
	m.SyncDevices([]android.DeviceInfo{acmeReady}, buildADB)

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
	m.SyncIOSDevices([]ios.DeviceInfo{iphoneWiFi})
	if _, err := m.Acquire(context.Background(), "ios:"+iphoneWiFi.UDID); err == nil || !strings.Contains(err.Error(), "simcap is not built") {
		t.Errorf("Acquire with no simcap: %v", err)
	}
	// Reaches a device listed before it and one listed after.
	m.SetIOSSimcap("/nonexistent/simcap")
	later := iphoneWiFi
	later.UDID, later.Identifier = "00008150-000A1B2C3D4E5F61", "11111111-AAAA-BBBB-CCCC-000000000002"
	m.SyncIOSDevices([]ios.DeviceInfo{iphoneWiFi, later})
	for _, d := range []ios.DeviceInfo{iphoneWiFi, later} {
		if _, err := m.Acquire(context.Background(), "ios:"+d.UDID); !errors.Is(err, ios.ErrIOSStreamNeedsCable) {
			t.Errorf("Acquire %s over Wi-Fi: %v", d.UDID, err)
		}
	}
}

// A Wi-Fi phone that drops off the network comes back on a new tunnel
// address, and simcap, still bound to the old one, waits forever on a socket
// nothing reaches. Its stream must be closed so the viewers' reconnect opens
// a fresh one; a phone that stays reachable keeps its stream.
func TestAPhoneBackFromOfflineGetsAFreshStream(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncIOSDevices([]ios.DeviceInfo{iphoneWiFi})
	id := "ios:" + iphoneWiFi.UDID
	s, err := session.Open(context.Background(), id, &stubTarget{}, target.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	m.SyncIOSDevices([]ios.DeviceInfo{iphoneWiFi})
	select {
	case <-s.Done():
		t.Fatal("a phone that stayed reachable lost its stream")
	default:
	}

	away := iphoneWiFi
	away.Transport = ""
	m.SyncIOSDevices([]ios.DeviceInfo{away})
	select {
	case <-s.Done():
		t.Fatal("the stream closed while the phone was away; a viewer has nothing to reopen onto")
	default:
	}

	m.SyncIOSDevices([]ios.DeviceInfo{iphoneWiFi})
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the phone came back and its stream is still the one bound to the old tunnel")
	}
	m.mu.Lock()
	_, still := m.sessions[id]
	m.mu.Unlock()
	if still {
		t.Error("the closed stream is still registered, so a reconnect would join it")
	}
}
