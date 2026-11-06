package target

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync/atomic"
)

// Point is a location setting's value.
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// location sets an emulator's GPS fix through the console's `geo fix`. A
// phone has no equivalent without a mock-location app, so the setting is
// listed only on emulators.
//
// Android has no command that reads the fix back: `cmd location` can only
// set test providers, and dumpsys's "last location" stays null until an app
// asks for one. So the value reported is the last fix loupe wrote to this
// device, or null before the first. A fix set some other way (Android
// Studio's extended controls, a GPX route) is not seen, and rollback can
// restore only a fix loupe wrote.

// pointFromValue reads a Point out of a decoded JSON value.
func pointFromValue(v any) (Point, error) {
	switch p := v.(type) {
	case Point:
		return p, nil
	case map[string]any:
		lat, latOK := p["lat"].(float64)
		lng, lngOK := p["lng"].(float64)
		if !latOK || !lngOK || len(p) != 2 {
			return Point{}, fmt.Errorf("want {\"lat\": number, \"lng\": number}")
		}
		return Point{Lat: lat, Lng: lng}, nil
	}
	return Point{}, fmt.Errorf("want {\"lat\": number, \"lng\": number}, got %T", v)
}

func validatePoint(_ settingField, v any) error {
	p, err := pointFromValue(v)
	if err != nil {
		return err
	}
	if math.IsNaN(p.Lat) || math.IsNaN(p.Lng) || p.Lat < -90 || p.Lat > 90 || p.Lng < -180 || p.Lng > 180 {
		return fmt.Errorf("location %v,%v is outside -90..90, -180..180", p.Lat, p.Lng)
	}
	return nil
}

// setLocation sends the fix. The console takes longitude first.
func setLocation(ctx context.Context, emu emuFunc, last *atomic.Pointer[Point], p Point) error {
	lng := strconv.FormatFloat(p.Lng, 'f', -1, 64)
	lat := strconv.FormatFloat(p.Lat, 'f', -1, 64)
	if _, err := emu(ctx, "geo", "fix", lng, lat); err != nil {
		return fmt.Errorf("geo fix: %w", err)
	}
	last.Store(&p)
	return nil
}

func locationField(emu emuFunc, last *atomic.Pointer[Point]) settingField {
	return settingField{
		id:  "location",
		typ: SettingPoint,
		readRestore: func(context.Context, shellFunc) (func(ctx context.Context, sh shellFunc) error, error) {
			prev := last.Load()
			return func(ctx context.Context, _ shellFunc) error {
				if prev == nil {
					// Nothing loupe set to go back to; see the comment above.
					return nil
				}
				return setLocation(ctx, emu, last, *prev)
			}, nil
		},
		write: func(ctx context.Context, _ shellFunc, v any) error {
			p, err := pointFromValue(v)
			if err != nil {
				return err
			}
			return setLocation(ctx, emu, last, p)
		},
		validate: validatePoint,
	}
}

// locationSetting reports the last fix loupe wrote.
func locationSetting(last *atomic.Pointer[Point]) Setting {
	s := Setting{ID: "location", Type: SettingPoint, Supported: true, Applies: "immediately"}
	if p := last.Load(); p != nil {
		s.Value = *p
	}
	return s
}
