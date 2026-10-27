package target

import (
	"os"
	"testing"
)

// testdata/ax-settings.json is simcap's `ui` output for the Settings root
// screen on an iPhone 17 simulator, iOS 27.0, whose framebuffer is 1206x2622.
func TestParseAXTreeScalesPointsToPixels(t *testing.T) {
	raw, err := os.ReadFile("testdata/ax-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	ui, err := parseAXTree(raw, 1206, 2622)
	if err != nil {
		t.Fatal(err)
	}
	if ui.Root.Class != "Application" || ui.Root.Text != "Settings" {
		t.Fatalf("root = %s %q", ui.Root.Class, ui.Root.Text)
	}
	if ui.Root.Bounds != [4]int{0, 0, 1206, 2622} {
		t.Errorf("root bounds = %v, want the whole framebuffer", ui.Root.Bounds)
	}

	general := find(ui.Root, func(n *Node) bool { return n.ID == "com.apple.settings.general" })
	if general == nil {
		t.Fatal("no General row")
	}
	// Frame [16,380.33,370,52] in points, at scale 3.
	if general.Bounds != [4]int{48, 1141, 1158, 1297} || general.Center != [2]int{603, 1219} {
		t.Errorf("General bounds %v center %v", general.Bounds, general.Center)
	}
	if !general.Clickable || general.Text != "General" || general.Class != "Button" {
		t.Errorf("General = %+v", general)
	}

	search := find(ui.Root, func(n *Node) bool { return n.Class == "TextField" })
	if search == nil || search.Value != "Search" || !search.Interesting() {
		t.Errorf("search field = %+v", search)
	}
}

func TestParseAXTreeCarriesItsFlags(t *testing.T) {
	raw := []byte(`{"role":"AXApplication","frame":[0,0,402,874],"children":[
		{"role":"AXTextField","subrole":"AXSecureTextField","focused":true,"frame":[0,0,10,10]},
		{"role":"AXCheckBox","subrole":"AXSwitch","value":"1","frame":[0,0,10,10]},
		{"role":"AXButton","label":"Off","disabled":true,"occluded":true,"frame":[0,0,10,10]}]}`)
	ui, err := parseAXTree(raw, 1206, 2622)
	if err != nil {
		t.Fatal(err)
	}
	pw, sw, off := ui.Root.Children[0], ui.Root.Children[1], ui.Root.Children[2]
	if !pw.Password || !pw.Focused {
		t.Errorf("secure field = %+v", pw)
	}
	if !sw.Checkable || !sw.Checked {
		t.Errorf("switch = %+v", sw)
	}
	if !off.Disabled || !off.Occluded {
		t.Errorf("disabled button = %+v", off)
	}
}

func TestParseAXTreeRejectsARootWithoutAFrame(t *testing.T) {
	if _, err := parseAXTree([]byte(`{"role":"AXApplication"}`), 1206, 2622); err == nil {
		t.Fatal("want an error: nothing to derive the scale from")
	}
}

func TestPruneDropsOccludedNodesAndKeepsTheirChildren(t *testing.T) {
	root := &Node{Class: "Application", Text: "Settings", Children: []*Node{
		{Class: "Group", Occluded: true, Children: []*Node{
			{Class: "Button", Text: "General", Clickable: true, Occluded: true},
			{Class: "Button", Text: "Camera", Clickable: true},
		}},
	}}
	got := Prune(root)
	if len(got) != 1 || len(got[0].Children) != 1 || got[0].Children[0].Text != "Camera" {
		t.Fatalf("pruned = %+v", got[0].Children)
	}
}
