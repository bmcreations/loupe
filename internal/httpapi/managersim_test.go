package httpapi

import (
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// The 5s poll can begin its simctl listing before `simctl create` returns and
// apply it after the creator's own sync. That stale listing must not mark the
// new simulator missing.
func TestStaleSimulatorListingDoesNotMarkANewSimulatorMissing(t *testing.T) {
	m := NewManager(target.Options{})
	pollStart := time.Now()
	creatorStart := pollStart.Add(time.Second)
	fresh := target.SimInfo{UDID: "AAAA-1111", Name: "Fresh", OSVersion: "27.0", State: "Shutdown"}

	m.SyncSimulatorsAt(creatorStart, []target.SimInfo{fresh}, "simcap")
	m.SyncSimulatorsAt(pollStart, nil, "simcap") // listed before the create returned

	if state := m.statePresence("sim:AAAA-1111"); state == "missing" {
		t.Fatal("a listing that began before the last applied one marked the simulator missing")
	}
	// A listing that began later still applies.
	m.SyncSimulatorsAt(creatorStart.Add(time.Second), nil, "simcap")
	if state := m.statePresence("sim:AAAA-1111"); state != "missing" {
		t.Errorf("a newer listing without it did not mark it missing: %q", state)
	}
}

func (m *Manager) statePresence(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.presence[id]
}

// A simctl listing that began before a delete's Unregister still names the
// simulator. Applied afterwards, it must not register it again.
func TestAListingFromBeforeUnregisterDoesNotBringTheSimulatorBack(t *testing.T) {
	m := NewManager(target.Options{})
	info := target.SimInfo{UDID: "DEAD-0001", Name: "Doomed", OSVersion: "27.0", State: "Shutdown"}
	m.SyncSimulatorsAt(time.Now(), []target.SimInfo{info}, "simcap")
	pollStart := time.Now()
	time.Sleep(time.Millisecond)
	m.Unregister("sim:DEAD-0001")
	m.SyncSimulatorsAt(pollStart, []target.SimInfo{info}, "simcap")
	if _, ok := m.Target("sim:DEAD-0001"); ok {
		t.Fatal("a listing from before the delete re-registered the simulator")
	}
	// A listing that began afterwards is the truth again.
	time.Sleep(time.Millisecond)
	m.SyncSimulatorsAt(time.Now(), []target.SimInfo{info}, "simcap")
	if _, ok := m.Target("sim:DEAD-0001"); !ok {
		t.Error("a listing from after the delete was ignored")
	}
}

// The AVD rescan after a create has the same race: a scan that read the
// deleted AVD's .ini before Unregister must not register it afterwards, while
// one that began later (a recreate under the same name) does.
func TestRegisterScannedSkipsATargetUnregisteredAfterTheScanBegan(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&describingTarget{id: "avd:doomed"})
	scanStart := time.Now()
	time.Sleep(time.Millisecond)
	m.Unregister("avd:doomed")
	if m.RegisterScanned(&describingTarget{id: "avd:doomed"}, scanStart) {
		t.Fatal("a scan from before the delete re-registered the AVD")
	}
	if _, ok := m.Target("avd:doomed"); ok {
		t.Fatal("target is back")
	}
	time.Sleep(time.Millisecond)
	if !m.RegisterScanned(&describingTarget{id: "avd:doomed"}, time.Now()) {
		t.Error("a scan from after the delete was refused")
	}
	if m.RegisterScanned(&describingTarget{id: "avd:doomed"}, time.Now()) {
		t.Error("RegisterScanned replaced a registered target")
	}
}
