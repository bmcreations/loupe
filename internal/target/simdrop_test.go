package target

import "testing"

func TestJudgePaste(t *testing.T) {
	read := func(v string) fieldRead { return fieldRead{value: v, ok: true, images: 0} }
	withImages := func(v string, n int) fieldRead { return fieldRead{value: v, ok: true, images: n} }
	for _, c := range []struct {
		name          string
		before, after fieldRead
		want          pasteVerdict
	}{
		{"an attachment appeared", read("field:"), read("field: ￼"), pasteTook},
		{"a second attachment", read("field: ￼"), read("field: ￼￼"), pasteTook},
		{"an empty field took it", read(""), read("￼"), pasteTook},
		{"shown some other way", read("0 photos"), read("1 photo"), pasteTook},
		{"nothing changed", read("field:"), read("field:"), pasteMissing},
		{"nothing changed after one", read("field: ￼"), read("field: ￼"), pasteMissing},
		{"no value either side", read(""), read(""), pasteUnknown},
		{"no field before", fieldRead{images: -1}, read("field: ￼"), pasteUnknown},
		{"no field after", read("field:"), fieldRead{images: -1}, pasteUnknown},
		{"a chip beside the field", withImages("Message", 1), withImages("Message", 2), pasteTook},
		{"a chip, field not read", fieldRead{images: 1}, fieldRead{images: 2}, pasteTook},
		{"an image went away", withImages("Message", 2), withImages("Message", 1), pasteMissing},
	} {
		if got := judgePaste(c.before, c.after); got != c.want {
			t.Errorf("%s: judgePaste(%q, %q) = %d; want %d", c.name, c.before.value, c.after.value, got, c.want)
		}
	}
}

func TestReadFieldPrefersTheFocusedNode(t *testing.T) {
	field := &Node{Class: "TextArea", Value: "field:", Bounds: [4]int{60, 360, 1110, 1410}}
	label := &Node{Class: "StaticText", Text: "DropTarget", Bounds: [4]int{60, 180, 1110, 300}}
	root := &Node{Class: "Application", Bounds: [4]int{0, 0, 1206, 2622}, Children: []*Node{label, field}}
	ui := &UI{Root: root}

	if got := readField(ui, 585, 885); !got.ok || got.value != "field:" {
		t.Errorf("unfocused, at the field: %+v; want the field's value", got)
	}
	if got := readField(ui, 585, 240); !got.ok || got.value != "" {
		t.Errorf("unfocused, at the label: %+v; want the label, which has no value", got)
	}
	if got := readField(ui, 5000, 5000); got.ok {
		t.Errorf("off the screen: %+v; want nothing read", got)
	}
	// Once the taps focused the field, a keyboard over the point does not
	// change which node is read.
	field.Focused = true
	root.Children = append(root.Children, &Node{Class: "Keyboard", Bounds: [4]int{0, 800, 1206, 2622}})
	if got := readField(ui, 585, 885); !got.ok || got.value != "field:" {
		t.Errorf("focused, under the keyboard: %+v; want the field's value", got)
	}
	if got := readField(nil, 1, 1); got.ok {
		t.Errorf("no tree: %+v; want nothing read", got)
	}
}

func TestTreeAppSkipsTheHomeScreen(t *testing.T) {
	home := &UI{Root: &Node{Class: "Application"}}
	app := &UI{Root: &Node{Class: "Application", Text: "Target"}}
	if _, ok := treeApp(home); ok {
		t.Error("home screen tree counted as an app")
	}
	if _, ok := treeApp(nil); ok {
		t.Error("nil tree counted as an app")
	}
	if name, ok := treeApp(app); !ok || name != "Target" {
		t.Errorf("treeApp(app) = %q, %v", name, ok)
	}
}

func TestAXPrefOn(t *testing.T) {
	for out, want := range map[string]bool{"1\n": true, "true": true, "0\n": false, "": false,
		"The domain/default pair of (com.apple.Accessibility, X) does not exist\n": false} {
		if got := axPrefOn(out); got != want {
			t.Errorf("axPrefOn(%q) = %v; want %v", out, got, want)
		}
	}
}
