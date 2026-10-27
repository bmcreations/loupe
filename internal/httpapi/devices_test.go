package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

func buildADB(d target.DeviceInfo) target.Target {
	return target.NewADBTarget(target.ADB{}, d, "", "")
}

var (
	acmeReady = target.DeviceInfo{Serial: "ACME00000000001", State: "device", Model: "Acme One"}
	acmeAuth  = target.DeviceInfo{Serial: "ACME00000000001", State: "unauthorized"}
	emulator  = target.DeviceInfo{Serial: "emulator-5554", State: "device", Model: "sdk_gphone16k_arm64", Emulator: true}
)

func infoFor(t *testing.T, m *Manager, id string) TargetInfo {
	t.Helper()
	for _, ti := range m.Targets(context.Background()) {
		if ti.ID == id {
			return ti
		}
	}
	t.Fatalf("target %q is not listed", id)
	return TargetInfo{}
}

// What happened on 2026-09-23: the Acme One dropped off adb overnight and the
// phone kept reconnecting to it, each attempt failing at the jar push with
// "failed to get feature set: device 'ACME00000000001' not found".
func TestADeviceAdbStopsListingIsKeptAndSaysWhy(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncDevices([]target.DeviceInfo{acmeReady, emulator}, buildADB)
	if !infoFor(t, m, acmeReady.Serial).Available {
		t.Fatal("a ready device is listed unavailable")
	}

	m.SyncDevices([]target.DeviceInfo{emulator}, buildADB)

	got := infoFor(t, m, acmeReady.Serial)
	if got.Available || got.State != "missing" {
		t.Fatalf("after unplugging: available=%v state=%q, want false/missing", got.Available, got.State)
	}
	if !strings.Contains(got.Reason, "Acme One") || !strings.Contains(got.Reason, "not connected") {
		t.Errorf("reason %q does not name the device and say it is not connected", got.Reason)
	}
	if !infoFor(t, m, emulator.Serial).Available {
		t.Error("the device still attached became unavailable")
	}

	m.SyncDevices([]target.DeviceInfo{acmeReady, emulator}, buildADB)
	if !infoFor(t, m, acmeReady.Serial).Available {
		t.Error("plugging the device back in did not make it available")
	}
}

func TestAcquiringAMissingDeviceFailsWithoutTouchingAdb(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncDevices([]target.DeviceInfo{acmeReady}, buildADB)
	m.SyncDevices(nil, buildADB)

	// The zero ADB has no jar and no device, so reaching adb would fail with
	// something about a push, not about the device being gone.
	_, err := m.Acquire(context.Background(), acmeReady.Serial)
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("Acquire err = %v, want the device named as not connected", err)
	}
	if got := infoFor(t, m, acmeReady.Serial).LastError; got != err.Error() {
		t.Errorf("lastError = %q, want %q", got, err.Error())
	}
}

// An ADBTarget keeps the state it was built with and refuses to open when
// that was not "device", so a device that is authorized after startup has to
// be rebuilt or it stays unopenable for the life of the process.
func TestADeviceThatBecomesAuthorizedIsRebuilt(t *testing.T) {
	m := NewManager(target.Options{})
	m.SyncDevices([]target.DeviceInfo{acmeAuth}, buildADB)

	got := infoFor(t, m, acmeAuth.Serial)
	if got.Available || !strings.Contains(got.Reason, "USB debugging") {
		t.Fatalf("unauthorized: available=%v reason=%q", got.Available, got.Reason)
	}

	m.SyncDevices([]target.DeviceInfo{acmeReady}, buildADB)
	tgt, _ := m.Target(acmeReady.Serial)
	if info := tgt.(*target.ADBTarget).Info(); info != acmeReady {
		t.Errorf("target still carries %+v, want %+v", info, acmeReady)
	}
	if got := infoFor(t, m, acmeReady.Serial); !got.Available || got.Label != acmeReady.Label() {
		t.Errorf("after authorizing: available=%v label=%q", got.Available, got.Label)
	}
}

func newAVD(name string) *target.Emulator {
	return target.NewEmulator(target.ADB{Path: "/nonexistent/loupe-test-adb"}, target.EmulatorConfig{AVD: name}, "", "")
}

func named(d target.DeviceInfo, avd string) target.DeviceInfo {
	d.AVD = avd
	return d
}

func TestARunningAVDIsNotListedTwice(t *testing.T) {
	m := NewManager(target.Options{})
	e := newAVD("pixel_10")
	m.Register(e)
	m.SyncDevices([]target.DeviceInfo{named(emulator, "pixel_10")}, buildADB)

	if e.Serial() != emulator.Serial {
		t.Fatalf("AVD attached as %q, want %q", e.Serial(), emulator.Serial)
	}
	for _, info := range m.Targets(context.Background()) {
		if info.ID == emulator.Serial {
			t.Error("the AVD's serial was listed as a second target")
		}
	}
}

// An agent or bookmark that named the serial before AVDs had ids of their
// own still reaches the AVD, and its session, rather than a 404.
func TestARunningAVDAnswersToItsSerial(t *testing.T) {
	m := NewManager(target.Options{})
	e := newAVD("pixel_10")
	m.Register(e)
	m.SyncDevices([]target.DeviceInfo{named(emulator, "pixel_10")}, buildADB)

	got, ok := m.Target(emulator.Serial)
	if !ok || got != target.Target(e) {
		t.Fatalf("Target(%q) = %v, %v; want the AVD", emulator.Serial, got, ok)
	}
}

// Reading the name costs a getprop an emulator cannot answer until it is
// ready, so it is listed by serial first. Once named, that entry has to go,
// not linger as "missing" beside the AVD it was all along.
func TestASerialListedBeforeItsNameWasKnownIsRemoved(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(newAVD("pixel_10"))
	m.SyncDevices([]target.DeviceInfo{emulator}, buildADB)
	if _, ok := m.targets[emulator.Serial]; !ok {
		t.Fatal("an unnamed emulator should be listed by serial")
	}
	m.SyncDevices([]target.DeviceInfo{named(emulator, "pixel_10")}, buildADB)
	if _, ok := m.targets[emulator.Serial]; ok {
		t.Error("the serial entry survived its emulator being named")
	}
}

func TestAnAVDThatStopsIsDetached(t *testing.T) {
	m := NewManager(target.Options{})
	e := newAVD("pixel_10")
	m.Register(e)
	m.SyncDevices([]target.DeviceInfo{named(emulator, "pixel_10")}, buildADB)
	m.SyncDevices(nil, buildADB)

	if e.Serial() != "" || !e.BootsOnOpen() {
		t.Errorf("after stopping: serial=%q bootsOnOpen=%v, want detached", e.Serial(), e.BootsOnOpen())
	}
	if _, ok := m.Target(emulator.Serial); ok {
		t.Error("a stopped AVD still answers to the serial it ran as")
	}
	info := infoFor(t, m, e.ID())
	if !info.Available {
		t.Errorf("a stopped AVD is unavailable (%q); opening it boots it", info.Reason)
	}
}

// An emulator running an AVD with no target -- one from another AVD home --
// is still a device that streams.
func TestAnEmulatorOfAnUnknownAVDIsListedBySerial(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(newAVD("pixel_10"))
	m.SyncDevices([]target.DeviceInfo{named(emulator, "someone_elses")}, buildADB)
	if _, ok := m.targets[emulator.Serial]; !ok {
		t.Error("an emulator of an unregistered AVD was not listed")
	}
}

func TestTargetsAdbDoesNotTrackAreAlwaysAvailable(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	m.SyncDevices(nil, buildADB)
	if got := infoFor(t, m, "stub"); !got.Available || got.State != "" {
		t.Errorf("stub: available=%v state=%q", got.Available, got.State)
	}
}

// failOnce fails its first Open and succeeds afterwards, which is a device
// that was mid-boot on the first attempt.
type failOnce struct {
	stubTarget
	failed bool
}

func (f *failOnce) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	if !f.failed {
		f.failed = true
		return nil, errors.New("connect control socket: dial tcp: refused")
	}
	return f.stubTarget.Open(ctx, opts)
}

func TestTheLastErrorClearsOnceATargetOpens(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&failOnce{})

	if _, err := m.Acquire(context.Background(), "stub"); err == nil {
		t.Fatal("first Acquire succeeded")
	}
	if got := infoFor(t, m, "stub").LastError; !strings.Contains(got, "refused") {
		t.Fatalf("lastError = %q after a failed open", got)
	}

	s, err := m.Acquire(context.Background(), "stub")
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	defer s.Close()
	if got := infoFor(t, m, "stub").LastError; got != "" {
		t.Errorf("lastError = %q after a successful open, want empty", got)
	}
}

// The list came from a map, so two calls a moment apart disagreed on order,
// and a client that polls it rebuilt its picker every time -- closing the
// select under a finger on iOS.
func TestTheTargetListHasAFixedOrder(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&target.Synthetic{})
	m.SyncDevices([]target.DeviceInfo{
		emulator,
		acmeReady,
		{Serial: "emulator-5556", State: "device", Emulator: true},
		{Serial: "AAAA", State: "device"},
	}, buildADB)

	want := []string{"AAAA", acmeReady.Serial, emulator.Serial, "emulator-5556", "synthetic"}
	for i := 0; i < 50; i++ {
		var got []string
		for _, ti := range m.Targets(context.Background()) {
			got = append(got, ti.ID)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("call %d: order %v, want %v", i, got, want)
		}
	}
}
