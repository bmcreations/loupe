package target

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLocationIsListedOnAnEmulatorOnly(t *testing.T) {
	phone := &androidSettings{}
	if _, ok := phone.fieldByID("location"); ok {
		t.Fatal("a phone lists location")
	}
	emu := &androidSettings{emu: func(context.Context, ...string) (string, error) { return "OK", nil }}
	if _, ok := emu.fieldByID("location"); !ok {
		t.Fatal("an emulator does not list location")
	}
}

func TestLocationWritesLongitudeFirstAndReportsTheLastFix(t *testing.T) {
	var sent []string
	a := &androidSettings{emu: func(_ context.Context, args ...string) (string, error) {
		sent = append(sent, joinArgs(args))
		return "OK", nil
	}}
	if s := locationSetting(a.lastFix()); s.Value != nil {
		t.Fatalf("before any write: %+v", s)
	}
	field, _ := a.fieldByID("location")
	ctx := context.Background()
	restore, err := field.readRestore(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A value as it arrives from a JSON body.
	var v any
	if err := json.Unmarshal([]byte(`{"lat": 37.422, "lng": -122.084}`), &v); err != nil {
		t.Fatal(err)
	}
	if err := field.validate(field, v); err != nil {
		t.Fatal(err)
	}
	if err := field.write(ctx, nil, v); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != "geo fix -122.084 37.422" {
		t.Fatalf("sent %q", sent)
	}
	if s := locationSetting(a.lastFix()); s.Value != (Point{Lat: 37.422, Lng: -122.084}) {
		t.Fatalf("after write: %+v", s)
	}
	// Nothing was set before, so a rollback has nothing to send.
	if err := restore(ctx, nil); err != nil || len(sent) != 1 {
		t.Fatalf("restore: %v, sent %q", err, sent)
	}
}

func TestLocationRejectsOutOfRangeAndMalformedValues(t *testing.T) {
	a := &androidSettings{emu: func(context.Context, ...string) (string, error) { return "OK", nil }}
	field, _ := a.fieldByID("location")
	bad := []any{
		map[string]any{"lat": 91.0, "lng": 0.0},
		map[string]any{"lat": 0.0, "lng": 181.0},
		map[string]any{"lat": 0.0},
		map[string]any{"lat": "0", "lng": 0.0},
		map[string]any{"lat": 0.0, "lng": 0.0, "alt": 1.0},
		"37.4,-122.1",
	}
	for _, v := range bad {
		if err := field.validate(field, v); err == nil {
			t.Fatalf("%v passed validation", v)
		}
	}
}
