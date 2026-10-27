package target

import "testing"

// The shape `xcrun simctl list -j devices available` printed on this machine,
// trimmed to the fields read and one device per case.
const simctlListing = `{"devices":{
  "com.apple.CoreSimulator.SimRuntime.iOS-27-0":[
    {"udid":"1651B3AF-0FCC-4297-A849-F78349FC26F3","name":"iPhone 18 Pro","state":"Shutdown","isAvailable":true,
     "deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-18-Pro"},
    {"udid":"AAAAAAAA-0000-0000-0000-000000000001","name":"iPad Pro 13-inch (M5)","state":"Booted","isAvailable":true,
     "deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPad-Pro-13-inch-M5-12GB"},
    {"udid":"AAAAAAAA-0000-0000-0000-000000000002","name":"Broken","state":"Shutdown","isAvailable":false,
     "deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-17"}
  ],
  "com.apple.CoreSimulator.SimRuntime.watchOS-27-0":[
    {"udid":"AAAAAAAA-0000-0000-0000-000000000003","name":"Apple Watch Ultra 3","state":"Shutdown","isAvailable":true,
     "deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.Apple-Watch-Ultra-3-49mm"}
  ],
  "com.apple.CoreSimulator.SimRuntime.iOS-26-5":[]
}}`

func TestParseSimctlListKeepsAvailableIOSSimulatorsOnly(t *testing.T) {
	sims, err := parseSimctlList([]byte(simctlListing))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]SimInfo{}
	for _, s := range sims {
		byName[s.Name] = s
	}
	if len(sims) != 2 {
		t.Fatalf("got %d simulators %v, want the iPhone and the iPad", len(sims), sims)
	}
	phone := byName["iPhone 18 Pro"]
	if phone.OSVersion != "27.0" || phone.Form != "phone" || phone.Booted() ||
		phone.DeviceType != "com.apple.CoreSimulator.SimDeviceType.iPhone-18-Pro" {
		t.Errorf("iPhone parsed as %+v", phone)
	}
	pad := byName["iPad Pro 13-inch (M5)"]
	if pad.Form != "tablet" || !pad.Booted() {
		t.Errorf("iPad parsed as %+v", pad)
	}
}

func TestASimulatorBootsOnOpenOnlyWhileShutDown(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u", Name: "iPhone", State: "Shutdown"}, "")
	if !s.BootsOnOpen() {
		t.Error("a shut-down simulator does not say it boots on open")
	}
	s.Update(SimInfo{UDID: "u", Name: "iPhone", State: "Booted"})
	if s.BootsOnOpen() {
		t.Error("a simulator booted from Xcode still says it boots on open")
	}
	if s.ID() != "sim:u" {
		t.Errorf("id %q", s.ID())
	}
}

func TestOnlySyntheticRefusesInput(t *testing.T) {
	if KindSynthetic.AcceptsInput() {
		t.Error("synthetic reports it accepts input")
	}
	for _, k := range []Kind{KindDevice, KindEmulator, KindSimulator} {
		if !k.AcceptsInput() {
			t.Errorf("%s reports it does not accept input", k)
		}
	}
}
