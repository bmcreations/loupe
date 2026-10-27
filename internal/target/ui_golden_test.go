package target

import (
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateGolden = flag.Bool("update-ui-golden", false, "rewrite testdata/text_field_focused.golden.json")

// TestUIWithoutComposeIsUnchanged pins /api/ui's JSON for a tree with no
// compose data. Node's compose fields are omitempty, so adding them must
// not change a byte of what existing callers see.
func TestUIWithoutComposeIsUnchanged(t *testing.T) {
	xml, err := os.ReadFile("testdata/text_field_focused.xml")
	if err != nil {
		t.Fatal(err)
	}
	ui, err := ParseUI(xml)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(ui)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/text_field_focused.golden.json"
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("JSON changed\n got %s\nwant %s", got, want)
	}
}
