package target

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Groups a device is offered under. TVs, watches, cars and desktops have none
// and are left out.
const (
	GroupPhone    = "phone"
	GroupFoldable = "foldable"
	GroupTablet   = "tablet"
)

// TabletDiagonalIn is where a plain screen becomes a tablet. Phones in the SDK
// run up to 6.9in (pixel_9_pro_xl is 6.8; this SDK's lists stop at the Pixel 9); the smallest
// tablet is nexus_7 at 7.0in. A foldable is decided by its hinge first, since
// pixel_fold is 7.58in.
const TabletDiagonalIn = 7.0

// AVDDevice is one hardware profile from the SDK's device definitions, the
// thing `avdmanager create avd -d` takes.
type AVDDevice struct {
	ID         string
	Name       string
	Group      string // GroupPhone, GroupFoldable or GroupTablet
	DiagonalIn float64
	Width      int
	Height     int
	// MinAPI is the oldest API level the definition runs on, 0 for any.
	MinAPI int
}

// Size is how the picker shows a device: `6.2" · 1080×2424`.
func (d AVDDevice) Size() string {
	s := ""
	if d.DiagonalIn > 0 {
		s = fmt.Sprintf("%.1f\"", d.DiagonalIn)
	}
	if d.Width > 0 && d.Height > 0 {
		if s != "" {
			s += " · "
		}
		s += fmt.Sprintf("%d×%d", d.Width, d.Height)
	}
	return s
}

// validDeviceID drops the legacy ids ("7.6in Foldable", "Nexus7", "2.7in
// QVGA"): they are not shell-safe, and avdmanager's own names for them are
// not what its skins directory uses.
var validDeviceID = regexp.MustCompile(`^[a-z0-9_]+$`)

// deviceXML is the d: namespace's shape, read by local name.
type deviceXML struct {
	Devices []struct {
		Deprecated string `xml:"deprecated,attr"`
		Name       string `xml:"name"`
		ID         string `xml:"id"`
		TagID      string `xml:"tag-id"`
		Hardware   struct {
			Screen struct {
				Diagonal   float64 `xml:"diagonal-length"`
				Dimensions struct {
					X int `xml:"x-dimension"`
					Y int `xml:"y-dimension"`
				} `xml:"dimensions"`
				FoldableRegion *struct{} `xml:"foldable-region"`
			} `xml:"screen"`
			Hinge *struct {
				Count int `xml:"count"`
			} `xml:"hinge"`
		} `xml:"hardware"`
		Software struct {
			APILevel string `xml:"api-level"`
		} `xml:"software"`
	} `xml:"device"`
}

// ParseAVDDevices reads one of sdklib's device definition files and returns
// the devices a phone-family image can use. Anything with a tag-id (TV, Wear,
// automotive, desktop) is dropped, as is a deprecated or legacy-named one.
func ParseAVDDevices(r io.Reader) ([]AVDDevice, error) {
	var doc deviceXML
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("device definitions: %w", err)
	}
	var out []AVDDevice
	for _, d := range doc.Devices {
		id := strings.TrimSpace(d.ID)
		if d.Deprecated == "true" || strings.TrimSpace(d.TagID) != "" || !validDeviceID.MatchString(id) {
			continue
		}
		scr := d.Hardware.Screen
		dev := AVDDevice{ID: id, Name: strings.TrimSpace(d.Name), DiagonalIn: scr.Diagonal, Width: scr.Dimensions.X, Height: scr.Dimensions.Y}
		switch {
		case (d.Hardware.Hinge != nil && d.Hardware.Hinge.Count >= 1) || scr.FoldableRegion != nil:
			dev.Group = GroupFoldable
		case scr.Diagonal >= TabletDiagonalIn:
			dev.Group = GroupTablet
		default:
			dev.Group = GroupPhone
		}
		if floor, _, _ := strings.Cut(strings.TrimSpace(d.Software.APILevel), "-"); floor != "" {
			dev.MinAPI, _ = strconv.Atoi(floor)
		}
		out = append(out, dev)
	}
	return out, nil
}

// avdDeviceFiles are the definition files inside sdklib.core.jar.
var avdDeviceFiles = []string{"nexus.xml", "devices.xml"}

var (
	avdDevicesMu    sync.Mutex
	avdDevicesCache = map[string][]AVDDevice{}
)

// AVDDevices reads the device definitions out of the SDK's command-line tools
// jar, once per process per SDK. The jar is
// cmdline-tools/<version>/lib/sdklib/sdklib.core.jar; avdmanager reads the
// same files, so the ids here are the ids `-d` accepts. tv.xml, wear.xml,
// automotive.xml and desktop.xml are not opened, since every device in them
// carries a tag-id.
func AVDDevices(sdk string) ([]AVDDevice, error) {
	avdDevicesMu.Lock()
	defer avdDevicesMu.Unlock()
	if d, ok := avdDevicesCache[sdk]; ok {
		return d, nil
	}
	jars, _ := filepath.Glob(filepath.Join(sdk, "cmdline-tools", "*", "lib", "sdklib", "sdklib.core.jar"))
	slices.Sort(jars)
	slices.Reverse(jars) // "latest" sorts after numbered versions, which is the one to prefer
	var firstErr error
	for _, jar := range jars {
		devs, err := readDeviceJar(jar)
		if err != nil {
			firstErr = err
			continue
		}
		avdDevicesCache[sdk] = devs
		return devs, nil
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no cmdline-tools sdklib.core.jar under %s", sdk)
	}
	return nil, firstErr
}

func readDeviceJar(jar string) ([]AVDDevice, error) {
	zr, err := zip.OpenReader(jar)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var all []AVDDevice
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "com/android/sdklib/devices/") || !slices.Contains(avdDeviceFiles, filepath.Base(f.Name)) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		devs, err := ParseAVDDevices(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		all = append(all, devs...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%s holds no device definitions", jar)
	}
	return all, nil
}

// AVDManagerBinary finds avdmanager: the explicit path, then every
// cmdline-tools/*/bin in the SDK with "latest" first. It runs without
// JAVA_HOME. "" when there is none.
func AVDManagerBinary(sdk, explicit string) string {
	cands := []string{explicit}
	if sdk != "" {
		cands = append(cands, filepath.Join(sdk, "cmdline-tools", "latest", "bin", "avdmanager"))
		others, _ := filepath.Glob(filepath.Join(sdk, "cmdline-tools", "*", "bin", "avdmanager"))
		slices.Sort(others)
		slices.Reverse(others)
		cands = append(cands, others...)
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}
