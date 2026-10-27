package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

var (
	simProbe = target.SimInfo{UDID: "98F85F5F-A448-4F5A-A511-45529A267273", Name: "loupe-probe", State: "Shutdown", OSVersion: "27.0", Form: "phone"}
	simPro   = target.SimInfo{UDID: "1651B3AF-0FCC-4297-A849-F78349FC26F3", Name: "iPhone 18 Pro", State: "Shutdown", OSVersion: "27.0", Form: "phone"}
)

func TestASimulatorIsListedAndFollowsItsBootState(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncSimulators([]target.SimInfo{simProbe}, "/nonexistent/simcap")

	id := target.SimulatorID(simProbe.UDID)
	got := infoFor(t, m, id)
	if got.Kind != "simulator" || got.Label != "loupe-probe" || !got.Available || !got.BootsOnOpen {
		t.Fatalf("listed as %+v", got)
	}
	if got.OS != "ios" || got.OSVersion != "27.0" {
		t.Errorf("described as os=%q version=%q", got.OS, got.OSVersion)
	}

	booted := simProbe
	booted.State = "Booted"
	m.SyncSimulators([]target.SimInfo{booted}, "/nonexistent/simcap")
	if infoFor(t, m, id).BootsOnOpen {
		t.Error("a simulator booted from Xcode still reads as booting on open")
	}
}

func TestADeletedSimulatorIsKeptAndSaysWhy(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncSimulators([]target.SimInfo{simProbe, simPro}, "")
	m.SyncSimulators([]target.SimInfo{simPro}, "")

	got := infoFor(t, m, target.SimulatorID(simProbe.UDID))
	if got.Available || got.State != "missing" {
		t.Fatalf("after deleting: available=%v state=%q", got.Available, got.State)
	}
	if !strings.Contains(got.Reason, "simctl") || strings.Contains(got.Reason, "adb") {
		t.Errorf("reason %q blames the wrong tool", got.Reason)
	}
	if !infoFor(t, m, target.SimulatorID(simPro.UDID)).Available {
		t.Error("the simulator still listed became unavailable")
	}
}

// adb never lists a simulator, so an adb sync must leave them alone.
func TestAnAdbSyncDoesNotMarkSimulatorsMissing(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncSimulators([]target.SimInfo{simProbe}, "")
	m.SyncDevices([]target.DeviceInfo{acmeReady}, buildADB)
	m.SyncDevices(nil, buildADB)

	if !infoFor(t, m, target.SimulatorID(simProbe.UDID)).Available {
		t.Error("an adb sync marked a simulator missing")
	}
}

// Ids are UDIDs, so sorting by id alone scatters simulators at random.
func TestSimulatorsListAfterDevicesRunningFirstThenByName(t *testing.T) {
	m := NewManager(target.Options{})
	booted := simPro
	booted.State = "Booted"
	other := target.SimInfo{UDID: "00000000-0000-0000-0000-000000000000", Name: "iPhone 17", State: "Shutdown"}
	m.SyncSimulators([]target.SimInfo{simProbe, other, booted}, "")
	m.SyncDevices([]target.DeviceInfo{acmeReady}, buildADB)

	var ids []string
	for _, ti := range m.Targets(context.Background()) {
		ids = append(ids, ti.Label)
	}
	want := []string{"Acme One (ACME00000000001)", "iPhone 18 Pro", "iPhone 17", "loupe-probe"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("order %v, want %v", ids, want)
	}
}
