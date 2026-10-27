package target

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// SimRuntime is one installed iOS runtime and the device types it supports.
type SimRuntime struct {
	ID      string // com.apple.CoreSimulator.SimRuntime.iOS-27-0
	Name    string // iOS 27.0
	Version string
	Types   []SimDeviceType
}

// SimDeviceType is one entry of a runtime's supportedDeviceTypes.
type SimDeviceType struct {
	ID     string
	Name   string
	Family string // iPhone or iPad
	Bundle string
}

// ParseSimRuntimes reads `simctl list -j runtimes`. Only available iOS
// runtimes are kept, and within them only iPhone and iPad types: a runtime's
// pairs come from its own supportedDeviceTypes and nowhere else (iOS 27.1
// supports exactly one, iPhone Duo).
func ParseSimRuntimes(raw []byte) ([]SimRuntime, error) {
	var doc struct {
		Runtimes []struct {
			Identifier  string `json:"identifier"`
			Name        string `json:"name"`
			Version     string `json:"version"`
			IsAvailable bool   `json:"isAvailable"`
			Platform    string `json:"platform"`
			Supported   []struct {
				Name          string `json:"name"`
				Identifier    string `json:"identifier"`
				ProductFamily string `json:"productFamily"`
				BundlePath    string `json:"bundlePath"`
			} `json:"supportedDeviceTypes"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("simctl list runtimes: %w", err)
	}
	var out []SimRuntime
	for _, r := range doc.Runtimes {
		if !r.IsAvailable || r.Platform != "iOS" || !strings.HasPrefix(r.Identifier, "com.apple.CoreSimulator.SimRuntime.iOS-") {
			continue
		}
		rt := SimRuntime{ID: r.Identifier, Name: r.Name, Version: r.Version}
		for _, t := range r.Supported {
			if t.ProductFamily != "iPhone" && t.ProductFamily != "iPad" {
				continue
			}
			rt.Types = append(rt.Types, SimDeviceType{ID: t.Identifier, Name: t.Name, Family: t.ProductFamily, Bundle: t.BundlePath})
		}
		out = append(out, rt)
	}
	return out, nil
}

// SimTypeInfo is how the picker groups and sizes a device type.
type SimTypeInfo struct {
	Group string // phone, foldable or tablet
	Size  string // `6.3" · 1206×2622`, or "" when the bundle could not be read
}

// ParseSimTypeInfo reads a device type's capabilities.plist (as JSON).
//
// Group: foldable when two or more displays are integrated and have a
// digitizer (the Duo), tablet for an iPad, else phone. Size: the diagonal is
// sqrt(w²+h²) over the display's pitch (hdpi, else main-screen-pitch). A
// device with several panels shows the largest, upright: the Duo's inner
// panel is 2007x2853 in its framebuffer and nativeRotation 270, so it reads
// 7.6" · 2853×2007, the shape its Home Screen has.
func ParseSimTypeInfo(caps []byte, family string) (SimTypeInfo, error) {
	var c struct {
		Caps struct {
			Screen struct {
				W     int     `json:"main-screen-width"`
				H     int     `json:"main-screen-height"`
				Pitch float64 `json:"main-screen-pitch"`
			} `json:"ScreenDimensionsCapability"`
			Displays []struct {
				Type     string  `json:"displayType"`
				Digitize bool    `json:"hasDigitizer"`
				Width    int     `json:"width"`
				Height   int     `json:"height"`
				HDPI     float64 `json:"hdpi"`
				Rotation int     `json:"nativeRotation"`
			} `json:"displays"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(caps, &c); err != nil {
		return SimTypeInfo{}, fmt.Errorf("device type capabilities: %w", err)
	}
	w, h, pitch := c.Caps.Screen.W, c.Caps.Screen.H, c.Caps.Screen.Pitch
	panels := 0
	best := 0
	for _, d := range c.Caps.Displays {
		if d.Type != "integrated" || !d.Digitize || d.Width <= 0 || d.Height <= 0 {
			continue
		}
		panels++
		if area := d.Width * d.Height; area > best {
			best = area
			w, h = d.Width, d.Height
			if d.Rotation%180 != 0 {
				w, h = h, w
			}
			if d.HDPI > 0 {
				pitch = d.HDPI
			}
		}
	}
	info := SimTypeInfo{Group: GroupPhone}
	switch {
	case panels >= 2:
		info.Group = GroupFoldable
	case family == "iPad":
		info.Group = GroupTablet
	}
	if w > 0 && h > 0 {
		info.Size = fmt.Sprintf("%d×%d", w, h)
		if pitch > 0 {
			diag := math.Hypot(float64(w), float64(h)) / pitch
			info.Size = fmt.Sprintf("%.1f\" · %s", diag, info.Size)
		}
	}
	return info, nil
}

var (
	simTypeMu    sync.Mutex
	simTypeCache = map[string]SimTypeInfo{}
)

// ReadSimTypeInfos returns the info for each bundle, reading each device
// type's capabilities.plist through plutil once per process. A bundle that
// cannot be read is cached as a phone/iPad with no size rather than retried
// on every request.
func ReadSimTypeInfos(ctx context.Context, types []SimDeviceType) map[string]SimTypeInfo {
	out := make(map[string]SimTypeInfo, len(types))
	var need []SimDeviceType
	simTypeMu.Lock()
	for _, t := range types {
		if v, ok := simTypeCache[t.Bundle]; ok {
			out[t.ID] = v
		} else {
			need = append(need, t)
		}
	}
	simTypeMu.Unlock()
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, t := range need {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			info := SimTypeInfo{Group: GroupPhone}
			if t.Family == "iPad" {
				info.Group = GroupTablet
			}
			if raw, err := plistJSON(ctx, filepath.Join(t.Bundle, "Contents", "Resources", "capabilities.plist")); err == nil {
				if v, err := ParseSimTypeInfo(raw, t.Family); err == nil {
					info = v
				}
			}
			// A read cut short by the caller's context says nothing about
			// the bundle; caching it would leave the type sizeless for the
			// life of the process.
			if ctx.Err() == nil {
				simTypeMu.Lock()
				simTypeCache[t.Bundle] = info
				simTypeMu.Unlock()
			}
			mu.Lock()
			out[t.ID] = info
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// ListSimRuntimes runs `xcrun simctl list -j runtimes`.
func ListSimRuntimes(ctx context.Context) ([]SimRuntime, error) {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "list", "-j", "runtimes").Output()
	if err != nil {
		return nil, fmt.Errorf("simctl list runtimes: %w", err)
	}
	return ParseSimRuntimes(out)
}
