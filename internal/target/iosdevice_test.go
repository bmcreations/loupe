package target

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/devicectl-list.json is `devicectl list devices --json-output`
// from Xcode 27 (devicectl 651.13.4), redacted: identifiers, UDIDs, serial
// numbers, ECIDs and the tunnel address are replaced, and each capability
// list is cut to the entries loupe cares about. It holds a phone reachable
// over Wi-Fi, a paired phone that is out of reach, a booted simulator and a
// shut-down one.
func TestDevicectlListKeepsPairedPhysicalIOSDevicesOnly(t *testing.T) {
	raw, err := os.ReadFile("testdata/devicectl-list.json")
	if err != nil {
		t.Fatal(err)
	}
	devs, err := parseDevicectlList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want the two phones and neither simulator: %+v", len(devs), devs)
	}

	phone := devs[0]
	want := IOSDeviceInfo{
		Identifier: "11111111-AAAA-BBBB-CCCC-000000000001", UDID: "00008140-000A00000000001C",
		Name: "Brandon’s iPhone", Model: "iPhone 16 Pro", OSVersion: "26.6", Form: "phone",
		Transport: "localNetwork", DeveloperMode: "enabled",
	}
	if phone != want {
		t.Errorf("phone\n got %+v\nwant %+v", phone, want)
	}
	if !phone.Reachable() || IOSDeviceID(phone) != "ios:00008140-000A00000000001C" {
		t.Errorf("phone reachable=%v id=%s", phone.Reachable(), IOSDeviceID(phone))
	}

	// Paired, state "unavailable" and no transport: listed, not reachable.
	away := devs[1]
	if away.Name != "bmc-iphone" || away.OSVersion != "27.2" || away.Reachable() || away.DeveloperMode != "enabled" {
		t.Errorf("unavailable phone %+v", away)
	}
}

func TestDevicectlListSkipsUnpairedAndWatches(t *testing.T) {
	// Shapes not on hand to capture: an iPad, an unpaired phone and a
	// watch. The two disconnected phones were captured.
	const list = `{"result": {"devices": [
	  {"identifier": "P", "properties": {"hardware": {"reality": "physical", "platform": "iOS", "deviceType": "iPad", "udid": "UP"},
	    "connection": {"pairingState": "paired", "transportType": "wired", "state": "connected"}, "state": {"name": "Test iPad"}}},
	  {"identifier": "U", "properties": {"hardware": {"reality": "physical", "platform": "iOS", "udid": "UU"},
	    "connection": {"pairingState": "unpaired", "transportType": "wired"}}},
	  {"identifier": "W", "properties": {"hardware": {"reality": "physical", "platform": "watchOS", "udid": "UW"},
	    "connection": {"pairingState": "paired", "transportType": "localNetwork"}}},
	  {"identifier": "D", "properties": {"hardware": {"reality": "physical", "platform": "iOS", "udid": "UD"},
	    "connection": {"pairingState": "paired", "transportType": "wired", "state": "disconnected"},
	    "state": {"developerModeStatus": {"disabled": {}}}}},
	  {"identifier": "N", "properties": {"hardware": {"reality": "physical", "platform": "iOS", "udid": "UN"},
	    "connection": {"pairingState": "paired", "transportType": "localNetwork", "state": "disconnected"}}}
	]}}`
	devs, err := parseDevicectlList([]byte(list))
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 3 {
		t.Fatalf("got %+v, want the iPad and the two disconnected phones", devs)
	}
	if pad := devs[0]; pad.Form != "tablet" || !pad.Reachable() || pad.Name != "Test iPad" {
		t.Errorf("ipad %+v", pad)
	}
	// Disconnected is an idle tunnel, which CoreDevice reopens, on the
	// cable and on Wi-Fi alike.
	if desk := devs[1]; !desk.Reachable() || desk.Transport != "wired" || desk.DeveloperMode != "disabled" {
		t.Errorf("idle wired phone %+v", desk)
	}
	if idle := devs[2]; !idle.Reachable() || idle.Transport != "localNetwork" {
		t.Errorf("idle Wi-Fi phone %+v", idle)
	}
}

func TestIOSDeviceStreamsOverWifiOnlyFromIOS27(t *testing.T) {
	// A simcap that lists -phone-wifi and writes nothing, so an Open that
	// gets as far as starting it fails reading the stream header.
	bin := filepath.Join(t.TempDir(), "simcap")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$1\" = -caps ] && { echo phonewifi; exit 0; }\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		os, transport string
		needsCable    bool
	}{
		{"26.6", "localNetwork", true},
		{"", "localNetwork", true},
		{"27.2", "localNetwork", false},
	} {
		d := NewIOSDevice(IOSDeviceInfo{Identifier: "X", UDID: "U", Name: "p", OSVersion: c.os, Transport: c.transport})
		d.SetSimcap(bin)
		_, err := d.Open(t.Context(), Options{})
		if err == nil {
			t.Fatalf("iOS %q opened against a simcap that writes nothing", c.os)
		}
		if errors.Is(err, ErrIOSStreamNeedsCable) != c.needsCable {
			t.Errorf("iOS %q over Wi-Fi: %v", c.os, err)
		}
	}
	d := NewIOSDevice(IOSDeviceInfo{Identifier: "X", UDID: "U", Name: "p", OSVersion: "27.2"})
	d.SetSimcap(bin)
	if _, err := d.Open(t.Context(), Options{}); err == nil || !strings.Contains(err.Error(), "out of reach") {
		t.Errorf("Open with no transport: %v", err)
	}
}

func TestIOSDeviceStreamsOnlyOnACableAndChecksLaunchNames(t *testing.T) {
	d := NewIOSDevice(IOSDeviceInfo{Identifier: "X", UDID: "U", Name: "p", Transport: "localNetwork"})
	if _, err := d.Open(t.Context(), Options{}); err == nil || !strings.Contains(err.Error(), "simcap is not built") {
		t.Errorf("Open without simcap: %v", err)
	}
	d.SetSimcap("/nonexistent/simcap")
	if _, err := d.Open(t.Context(), Options{}); !errors.Is(err, ErrIOSStreamNeedsCable) {
		t.Errorf("Open over Wi-Fi: %v", err)
	}
	if KindIOSDevice.AcceptsInput() {
		t.Error("a target with no input route says it accepts input")
	}
	if err := d.CheckLaunch("com.apple.Preferences", ".Main"); !errors.Is(err, ErrBadLaunchName) {
		t.Errorf("an activity: %v", err)
	}
	if err := d.CheckLaunch("com.apple; rm -rf /", ""); !errors.Is(err, ErrBadLaunchName) {
		t.Errorf("a shell payload: %v", err)
	}
	if err := d.CheckLaunch("com.apple.Preferences", ""); err != nil {
		t.Errorf("Settings: %v", err)
	}
}

func TestDevicectlLaunchOfAMissingAppReadsNotInstalled(t *testing.T) {
	// devicectl 651.13.4 against an iPhone on iOS 26.6.
	const out = `ERROR: The application failed to launch. (com.apple.dt.CoreDeviceError error 10002 (0x2712))
       BundleIdentifier = com.example.notinstalled.loupe
       NSLocalizedFailureReason = The requested application com.example.notinstalled.loupe is not installed.
       NSLocalizedRecoverySuggestion = Provide a valid bundle identifier.`
	if !devicectlNotInstalled([]byte(out)) {
		t.Error("a missing app is not read as ErrAppNotInstalled")
	}
	if devicectlNotInstalled([]byte("ERROR: The application failed to launch. (com.apple.dt.CoreDeviceError error 10002 (0x2712))\n       NSLocalizedFailureReason = The device is locked.")) {
		t.Error("a locked device reads as not installed")
	}
}
