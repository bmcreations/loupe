package target

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// IOSDeviceInfo is one physical iPhone or iPad as `devicectl list devices`
// reports it, over a cable or the network.
type IOSDeviceInfo struct {
	// Identifier is CoreDevice's id for the device, the one devicectl's
	// --device takes and simcap -devices prints.
	Identifier string
	// UDID is the hardware UDID, which is what the target id is built
	// from: it is the same number Xcode and Finder show.
	UDID      string
	Name      string
	Model     string // "iPhone 17 Pro"
	OSVersion string // "27.0"
	Form      string // phone or tablet
	// Transport is "wired" over a cable and "localNetwork" over Wi-Fi, and
	// empty when the Mac cannot currently reach the device.
	Transport     string
	DeveloperMode string // devicectl's word, "enabled" when it is on
}

// Reachable reports whether the Mac can talk to the device now.
func (i IOSDeviceInfo) Reachable() bool {
	return i.Transport == "wired" || i.Transport == "localNetwork"
}

// IOSDeviceID is the target id for a physical device.
func IOSDeviceID(i IOSDeviceInfo) string {
	if i.UDID != "" {
		return "ios:" + i.UDID
	}
	return "ios:" + i.Identifier
}

// ListIOSDevices returns every physical iOS device paired with this Mac,
// reachable or not.
func ListIOSDevices(ctx context.Context) ([]IOSDeviceInfo, error) {
	dir, err := os.MkdirTemp("", "loupe-devicectl")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "devices.json")
	// --json-output is the only stable output; the table is for people.
	out, err := exec.CommandContext(ctx, "xcrun", "devicectl", "list", "devices", "--json-output", path).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("devicectl list devices: %w: %s", err, strings.TrimSpace(string(out)))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("devicectl list devices: %w", err)
	}
	return parseDevicectlList(raw)
}

// devicectlDevice is one entry of devicectl's device list. Xcode 27's
// devicectl (jsonVersion 5) moved the fields under `properties` and marks
// hardwareProperties, deviceProperties and connectionProperties deprecated,
// while still writing them, so both are read and the new one wins. The new
// shape is not the old one renamed: the name is under `state`, the OS
// version is a components object under `software`, and developerModeStatus
// is an object keyed by the status, `{"enabled": {"mode": 1}}`, where the
// old shape had the string "enabled". Captured in testdata/devicectl-list.json.
type devicectlDevice struct {
	Identifier string `json:"identifier"`
	Properties struct {
		Hardware struct {
			Reality       string `json:"reality"`
			Platform      string `json:"platform"`
			DeviceType    string `json:"deviceType"`
			MarketingName string `json:"marketingName"`
			UDID          string `json:"udid"`
		} `json:"hardware"`
		Software struct {
			OSVersionNumber struct {
				StringValue string `json:"stringValue"`
			} `json:"osVersionNumber"`
		} `json:"software"`
		Connection struct {
			State         string `json:"state"`
			PairingState  string `json:"pairingState"`
			TransportType string `json:"transportType"`
		} `json:"connection"`
		State struct {
			Name                string        `json:"name"`
			DeveloperModeStatus devModeStatus `json:"developerModeStatus"`
		} `json:"state"`
	} `json:"properties"`
	HardwareProperties struct {
		Reality       string `json:"reality"`
		Platform      string `json:"platform"`
		DeviceType    string `json:"deviceType"`
		MarketingName string `json:"marketingName"`
		UDID          string `json:"udid"`
	} `json:"hardwareProperties"`
	DeviceProperties struct {
		Name                string `json:"name"`
		OSVersionNumber     string `json:"osVersionNumber"`
		DeveloperModeStatus string `json:"developerModeStatus"`
	} `json:"deviceProperties"`
	ConnectionProperties struct {
		PairingState  string `json:"pairingState"`
		TransportType string `json:"transportType"`
		TunnelState   string `json:"tunnelState"`
	} `json:"connectionProperties"`
}

// devModeStatus reads `properties.state.developerModeStatus`, an object with
// one key naming the status, as that key. A string is taken as it is, in
// case a later devicectl flattens it, and anything else reads empty rather
// than failing the whole listing.
type devModeStatus string

func (s *devModeStatus) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) == nil {
		*s = devModeStatus(str)
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) == nil && len(obj) == 1 {
		for k := range obj {
			*s = devModeStatus(k)
		}
	}
	return nil
}

func parseDevicectlList(raw []byte) ([]IOSDeviceInfo, error) {
	var doc struct {
		Result struct {
			Devices []devicectlDevice `json:"devices"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("devicectl list devices: %w", err)
	}
	var out []IOSDeviceInfo
	for _, d := range doc.Result.Devices {
		p := d.Properties
		hw, old := p.Hardware, d.HardwareProperties
		reality := cmp.Or(hw.Reality, old.Reality)
		transport := cmp.Or(p.Connection.TransportType, d.ConnectionProperties.TransportType)
		// Xcode 27 lists simulators here too, as reality "simulated" and
		// transport "sameMachine". simctl owns those.
		if reality != "physical" || transport == "sameMachine" {
			continue
		}
		// iPadOS reports platform iOS too. A watch or a TV has no touch
		// screen for the viewer to drive.
		if cmp.Or(hw.Platform, old.Platform) != "iOS" {
			continue
		}
		// An unpaired device is one the Mac has seen and cannot do anything
		// with until someone trusts it at the device.
		if cmp.Or(p.Connection.PairingState, d.ConnectionProperties.PairingState) != "paired" {
			continue
		}
		// A device that has dropped off the network keeps its entry and
		// loses its transport. "unavailable" is believed too, since the
		// transport may outlive the connection. "disconnected" is not: it
		// is an idle tunnel, which CoreDevice brings up when something
		// uses it. bmc-iphone read so on Wi-Fi a minute after streaming
		// over it, and Brandon's iPhone read so on the cable within 10s
		// of the last devicectl call while usbmuxd still reached its
		// lockdownd; out of reach, a phone reads unavailable with no
		// transport.
		if p.Connection.State == "unavailable" {
			transport = ""
		}
		form := "phone"
		if strings.EqualFold(cmp.Or(hw.DeviceType, old.DeviceType), "iPad") {
			form = "tablet"
		}
		out = append(out, IOSDeviceInfo{
			Identifier:    d.Identifier,
			UDID:          cmp.Or(hw.UDID, old.UDID),
			Name:          cmp.Or(p.State.Name, d.DeviceProperties.Name, cmp.Or(hw.MarketingName, old.MarketingName)),
			Model:         cmp.Or(hw.MarketingName, old.MarketingName),
			OSVersion:     cmp.Or(p.Software.OSVersionNumber.StringValue, d.DeviceProperties.OSVersionNumber),
			Form:          form,
			Transport:     transport,
			DeveloperMode: cmp.Or(string(p.State.DeveloperModeStatus), d.DeviceProperties.DeveloperModeStatus),
		})
	}
	return out, nil
}

// IOSDevice is a physical iPhone or iPad paired with this Mac. It is listed,
// described, launched into and screenshotted through devicectl, over a cable
// or the network alike. On a cable it streams through `simcap -phone` and the
// screen-capture device QuickTime records from. Over Wi-Fi there is no such
// device, and it streams through `simcap -phone-wifi`, which receives the
// mirrored stream CoreDevice negotiates; the phone refuses that before
// iOS 27. Input goes through CoreDevice HID on either, through
// `simcap -phone-input` (iosdeviceinput.go).
type IOSDevice struct {
	mu     sync.Mutex
	info   IOSDeviceInfo
	simcap string

	// in is the input process, made on first use for the identifier it
	// was made with; phoneInput is whether simcap has -phone-input, asked
	// once per simcap.
	in         *simInput
	phoneInput *bool
	// phoneWifi is whether simcap has -phone-wifi, asked once per simcap.
	phoneWifi *bool

	// xcrunner is the built XCUITest runner's products directory, or "";
	// runner is its connection, made on the first touch below iOS 27.
	xcrunner string
	runner   *xcRunner
}

// NewIOSDevice returns a target for info.
func NewIOSDevice(info IOSDeviceInfo) *IOSDevice { return &IOSDevice{info: info} }

func (d *IOSDevice) ID() string    { return IOSDeviceID(d.Info()) }
func (d *IOSDevice) Kind() Kind    { return KindIOSDevice }
func (d *IOSDevice) Label() string { return d.Info().Name }

// Info is what devicectl last said about this device.
func (d *IOSDevice) Info() IOSDeviceInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.info
}

// Update records a fresh listing.
func (d *IOSDevice) Update(info IOSDeviceInfo) {
	d.mu.Lock()
	if info.Identifier != d.info.Identifier {
		d.dropInputLocked()
	}
	if info.Transport != "wired" || info.UDID != d.info.UDID {
		d.dropRunnerLocked()
	}
	d.info = info
	d.mu.Unlock()
}

// device is what devicectl's --device takes. CoreDevice's identifier is its
// own; the UDID is accepted too, and is the fallback.
func (d *IOSDevice) device() string {
	i := d.Info()
	return cmp.Or(i.Identifier, i.UDID)
}

// SetSimcap records the simcap that streams this device, or "" when none is
// built.
func (d *IOSDevice) SetSimcap(bin string) {
	d.mu.Lock()
	if bin != d.simcap {
		d.dropInputLocked()
		d.phoneInput = nil
		d.phoneWifi = nil
	}
	d.simcap = bin
	d.mu.Unlock()
}

// ErrIOSStreamNeedsCable is an Open of a physical iOS device over Wi-Fi that
// cannot stream there: the phone is before iOS 27, or simcap predates
// -phone-wifi.
var ErrIOSStreamNeedsCable = fmt.Errorf("this iPhone streams only over a cable; launch and screenshots still work over Wi-Fi through /api/input and /api/screenshot")

// Open implements Target with `simcap <udid> -phone` on a cable and
// `simcap <identifier> -phone-wifi` over Wi-Fi, whose output is the same wire
// format a simulator's is.
func (d *IOSDevice) Open(ctx context.Context, opts Options) (Stream, error) {
	d.mu.Lock()
	info, bin := d.info, d.simcap
	d.mu.Unlock()
	if bin == "" {
		return nil, fmt.Errorf("simcap is not built (make simcap), and it is what streams a physical iPhone")
	}
	var st *simStream
	var err error
	switch info.Transport {
	case "wired":
		st, err = openSimcap(ctx, bin, info.UDID, info.Name, opts, "-phone", info.Name)
	case "localNetwork":
		if !d.streamsOverWifi(ctx) {
			return nil, fmt.Errorf("%w (Wi-Fi streaming needs iOS 27 and a simcap with -phone-wifi; this is iOS %s)", ErrIOSStreamNeedsCable, info.OSVersion)
		}
		st, err = openSimcap(ctx, bin, info.Identifier, info.Name, opts, "-phone-wifi", info.Name)
	default:
		return nil, fmt.Errorf("%s is paired but out of reach: devicectl lists it with no transport", info.Name)
	}
	if err != nil {
		return nil, err
	}
	// The phone's encoder is simcap's, which takes a bitrate on stdin.
	st.live = true
	// A stream without input still streams: an older simcap, or a phone
	// CoreDevice has not listed yet, answers every touch with ErrNoControl.
	if in, err := d.input(ctx); err == nil {
		st.sim, st.in = d, in
		st.route = d.do
		if d.touchesViaRunner() {
			// One finger: the runner has no second. Started now, so the
			// first touch does not wait out the install.
			st.oneFinger = true
			_, _ = d.runnerDo(ctx, 0, "ping")
		}
	}
	return st, nil
}

// streamsOverWifi reports whether a Wi-Fi Open can work: the phone refused
// CoreDevice's media stream on iOS 26.6 with "Remote control requires iOS
// 27.0", and an older simcap reads -phone-wifi as a simulator's UDID.
func (d *IOSDevice) streamsOverWifi(ctx context.Context) bool {
	d.mu.Lock()
	info, bin, known := d.info, d.simcap, d.phoneWifi
	d.mu.Unlock()
	major, _, _ := strings.Cut(info.OSVersion, ".")
	if v, err := strconv.Atoi(major); err != nil || v < 27 || info.Identifier == "" {
		return false
	}
	if known == nil {
		ok, err := simcapHas(ctx, bin, "phonewifi")
		if err != nil {
			return false
		}
		known = &ok
		d.mu.Lock()
		if d.simcap == bin {
			d.phoneWifi = known
		}
		d.mu.Unlock()
	}
	return *known
}

// Describe implements Describer from the listing, with no subprocess.
func (d *IOSDevice) Describe(context.Context) (Description, error) {
	i := d.Info()
	transport := map[string]string{"wired": "usb", "localNetwork": "wifi"}[i.Transport]
	return Description{OS: "ios", OSVersion: i.OSVersion, Form: i.Form, Transport: transport}, nil
}

// Launch implements Launcher with `devicectl device process launch`, which
// needs Developer Mode on the device and fails on one that is locked.
// A bundle the device lacks fails with CoreDeviceError 10002, the code for
// any failed launch, whose NSLocalizedFailureReason reads "The requested
// application <bundle> is not installed.", so the reason is what is matched.
func (d *IOSDevice) Launch(ctx context.Context, bundle, activity string) (*LaunchResult, error) {
	if err := d.CheckLaunch(bundle, activity); err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "process", "launch",
		"--device", d.device(), bundle).CombinedOutput()
	if err != nil {
		if devicectlNotInstalled(out) {
			return nil, fmt.Errorf("%w: %s", ErrAppNotInstalled, bundle)
		}
		return nil, fmt.Errorf("devicectl launch %s: %w: %s", bundle, err, strings.TrimSpace(string(out)))
	}
	return &LaunchResult{}, nil
}

func devicectlNotInstalled(out []byte) bool {
	return strings.Contains(string(out), "is not installed.")
}

// CheckLaunch implements Launcher.
func (d *IOSDevice) CheckLaunch(bundle, activity string) error {
	if activity != "" {
		return fmt.Errorf("%w: an iOS app has no activities to name", ErrBadLaunchName)
	}
	if !bundleIDRE.MatchString(bundle) {
		return fmt.Errorf("%w: %q is not a bundle ID", ErrBadLaunchName, bundle)
	}
	return nil
}

// Screenshot implements Screenshotter with `devicectl device capture
// screenshot`, which Xcode 26.6 added for devices and simulators alike.
func (d *IOSDevice) Screenshot(ctx context.Context) ([]byte, error) {
	dir, err := os.MkdirTemp("", "loupe-iosshot")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "shot.png")
	out, err := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "capture", "screenshot",
		"--device", d.device(), "--destination", path).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("devicectl screenshot: %v: %s", err, strings.TrimSpace(string(out)))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < len(pngMagic) || string(raw[:len(pngMagic)]) != string(pngMagic) {
		return nil, fmt.Errorf("devicectl screenshot did not write a PNG (%d bytes)", len(raw))
	}
	return raw, nil
}
