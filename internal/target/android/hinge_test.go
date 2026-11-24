package android

import (
	"encoding/json"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// A range's min and max are on the wire even at zero; a bool has neither.
func TestSettingJSONKeepsARangesZeroMin(t *testing.T) {
	b, err := json.Marshal([]target.Setting{target.HingeSetting(0), {ID: "darkMode", Type: SettingBool, Value: true, Supported: true, Applies: "immediately"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":"hingeAngle","type":"range","value":0,"min":0,"max":180,"step":1,"supported":true,"applies":"immediately"},` +
		`{"id":"darkMode","type":"bool","value":true,"supported":true,"applies":"immediately"}]`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}
