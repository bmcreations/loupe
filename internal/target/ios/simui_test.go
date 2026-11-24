package ios

import (
	"errors"
	"os"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
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

	general := find(ui.Root, func(n *target.Node) bool { return n.ID == "com.apple.settings.general" })
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

	search := find(ui.Root, func(n *target.Node) bool { return n.Class == "TextField" })
	if search == nil || search.Value != "Search" || !search.Interesting() {
		t.Errorf("search field = %+v", search)
	}
}

// testdata/ax-safari-tv-search.json is simcap's `ui` output for Safari on
// tv.apple.com/search, iPhone 18 Pro simulator, iOS 27.2, 1206x2622. The page
// is a Group under the application beside Safari's toolbar: simcap finds it by
// hit test, since Safari's application never lists it. The page's main Group
// runs far below the screen and is marked occluded, which must not hide the
// links on screen inside it.
func TestParseAXTreeReachesSafariPage(t *testing.T) {
	raw, err := os.ReadFile("testdata/ax-safari-tv-search.json")
	if err != nil {
		t.Fatal(err)
	}
	ui, err := parseAXTree(raw, 1206, 2622)
	if err != nil {
		t.Fatal(err)
	}
	if back := find(ui.Root, func(n *target.Node) bool { return n.ID == "BackButton" }); back == nil {
		t.Error("Safari's toolbar is missing")
	}

	movies, _, err := target.Pick(ui.Root, target.Match{Text: "Movies"})
	if err != nil {
		t.Fatal(err)
	}
	// Frame [36,505,57,23] in points, at scale 3.
	if movies.Class != "Link" || !movies.Clickable || movies.Center != [2]int{193, 1549} {
		t.Errorf("Movies = %+v", movies)
	}

	// A heading the page also wraps in a wider heading of the same name
	// reaches the tree once.
	if _, _, err := target.Pick(ui.Root, target.Match{Text: "Search"}); err != nil {
		t.Errorf("Search heading: %v", err)
	}

	// Below the fold, so off the screen.
	if _, _, err := target.Pick(ui.Root, target.Match{Text: "Comedies"}); !errors.Is(err, target.ErrNoMatch) {
		t.Errorf("Comedies: err = %v, want noMatch", err)
	}

	// On the screen, but its centre is below the web area, between the page
	// and Safari's toolbar, where a tap does not reach it.
	if _, _, err := target.Pick(ui.Root, target.Match{Text: "Kids & Family"}); !errors.Is(err, target.ErrNoMatch) {
		t.Errorf("Kids & Family: err = %v, want noMatch", err)
	}

	// WebKit calls every node on the page focused, so simcap leaves the
	// flag off the page; a drop's field must not come from it.
	if f := target.FocusedAll(ui); len(f) > 0 {
		t.Errorf("focused = %d nodes, first %+v; want none", len(f), f[0])
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
	root := &target.Node{Class: "Application", Text: "Settings", Children: []*target.Node{
		{Class: "Group", Occluded: true, Children: []*target.Node{
			{Class: "Button", Text: "General", Clickable: true, Occluded: true},
			{Class: "Button", Text: "Camera", Clickable: true},
		}},
	}}
	got := target.Prune(root)
	if len(got) != 1 || len(got[0].Children) != 1 || got[0].Children[0].Text != "Camera" {
		t.Fatalf("pruned = %+v", got[0].Children)
	}
}
