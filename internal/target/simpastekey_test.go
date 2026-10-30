package target

import (
	"strings"
	"testing"
)

func TestTCCPasteGrantNamesTheBundle(t *testing.T) {
	stmt, err := tccPasteGrantSQL("com.flipcash.app.ios")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"insert or replace into access", "'kTCCServicePasteboard', 'com.flipcash.app.ios', 0, 2,"} {
		if !strings.Contains(stmt, want) {
			t.Errorf("%q lacks %q", stmt, want)
		}
	}
}

func TestTCCPasteGrantRefusesWhatIsNotABundleID(t *testing.T) {
	for _, b := range []string{"", "a'b", "com.x; drop table access", "-lead", "com x"} {
		if _, err := tccPasteGrantSQL(b); err == nil {
			t.Errorf("%q: want an error", b)
		}
	}
}

// A drop on the field that already has focus is not tapped first: in
// Flipcash a tap on the focused composer raised Paste | AutoFill, and the
// menu stayed up after Cmd+V pasted.
func TestFocusedAtIsTheFocusedFieldOnly(t *testing.T) {
	field := &Node{Class: "TextField", Focused: true, Bounds: [4]int{186, 1509, 840, 1569}}
	ui := &UI{Root: &Node{Bounds: [4]int{0, 0, 1206, 2622}, Children: []*Node{field}}}
	if !focusedAt(ui, 282, 1540) {
		t.Error("drop inside the focused field: want true")
	}
	if focusedAt(ui, 282, 900) {
		t.Error("drop outside it: want false")
	}
	field.Focused = false
	if focusedAt(ui, 282, 1540) {
		t.Error("nothing focused: want false")
	}
	if focusedAt(nil, 282, 1540) {
		t.Error("no tree: want false")
	}
}
