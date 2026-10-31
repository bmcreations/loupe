package httpapi

import (
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// An AVD created outside loupe is picked up by the first lookup that names
// it, and the scan is throttled so a typo'd id does not rescan per request.
func TestTargetRescansAVDsOnAnUnknownAVDID(t *testing.T) {
	m := NewManager(target.Options{})
	scans := 0
	m.SetAVDRescan(func() {
		scans++
		if scans >= 1 {
			m.Register(&fakeAgentTarget{id: "avd:made_outside", screenW: 1, screenH: 1})
		}
	})
	if _, ok := m.Target("dev-unknown"); ok || scans != 0 {
		t.Fatalf("non-avd id: ok %v scans %d, want a plain miss with no scan", ok, scans)
	}
	if _, ok := m.Target("avd:made_outside"); !ok || scans != 1 {
		t.Fatalf("avd id: ok %v scans %d, want found after one scan", ok, scans)
	}
	if _, ok := m.Target("avd:typo"); ok || scans != 1 {
		t.Fatalf("second miss inside the throttle: ok %v scans %d, want no rescan", ok, scans)
	}
	m.mu.Lock()
	m.rescanAt = time.Now().Add(-avdRescanEvery)
	m.mu.Unlock()
	if _, ok := m.Target("avd:typo"); ok || scans != 2 {
		t.Fatalf("after the throttle: ok %v scans %d, want a second scan", ok, scans)
	}
}
