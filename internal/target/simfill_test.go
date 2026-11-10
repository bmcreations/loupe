package target

import (
	"errors"
	"os"
	"testing"
)

// newGroupHits are simcap's `hit` replies along New Group's navigation bar,
// in points, from the same screen as testdata/ax-flipcash-new-group.json.
var newGroupHits = []axNode{
	{Role: "AXButton", ID: "BackButton", Label: "Back", Frame: []float64{16, 62, 44, 44}},
	{Role: "AXHeading", Label: "New Group", Frame: []float64{147.33333333333334, 71.66666666666666, 107.33333333333334, 24.666666666666671}},
	{Role: "AXButton", ID: "new-group-create-button", Label: "Create", Disabled: true, Frame: []float64{307.3333333333333, 66, 74.66666666666669, 36}},
}

// fakeHit answers a point in device pixels with the first of hits whose frame
// holds it, else the deepest node of ui that does, as the simulator answers
// with the bar itself between its items.
func fakeHit(ui *UI, hits []axNode, calls *int) func(x, y int) (*Node, error) {
	return func(x, y int) (*Node, error) {
		*calls++
		px, py := float64(x)/ui.scale, float64(y)/ui.scale
		for i := range hits {
			f := hits[i].Frame
			if px >= f[0] && px < f[0]+f[2] && py >= f[1] && py < f[1]+f[3] {
				return axToNode(&hits[i], ui.scale), nil
			}
		}
		var deepest *Node
		var walk func(n *Node)
		walk = func(n *Node) {
			b := n.Bounds
			if x >= b[0] && x < b[2] && y >= b[1] && y < b[3] {
				deepest = n
			}
			for _, c := range n.Children {
				walk(c)
			}
		}
		walk(ui.Root)
		if deepest == nil {
			return nil, nil
		}
		c := *deepest
		c.Children = nil
		return &c, nil
	}
}

func loadNewGroup(t *testing.T) *UI {
	t.Helper()
	raw, err := os.ReadFile("testdata/ax-flipcash-new-group.json")
	if err != nil {
		t.Fatal(err)
	}
	ui, err := parseAXTree(raw, 1206, 2622)
	if err != nil {
		t.Fatal(err)
	}
	return ui
}

// On Flipcash's New Group screen the navigation bar dumps as a childless
// Group, so its back button and Create were in no tree an agent could match.
func TestFillByHitAddsTheItemsOfAChildlessNavigationBar(t *testing.T) {
	ui := loadNewGroup(t)
	if n := FindNodes(ui.Root, Match{ID: "BackButton"}); len(n) != 0 {
		t.Fatalf("the fixture already has BackButton: %+v", n)
	}
	calls := 0
	if err := fillByHit(ui, fakeHit(ui, newGroupHits, &calls)); err != nil {
		t.Fatal(err)
	}

	bar := find(ui.Root, func(n *Node) bool { return n.Class == "Group" && n.ID == "New Group" })
	if bar == nil {
		t.Fatal("no navigation bar")
	}
	if len(bar.Children) != 3 {
		t.Fatalf("bar children = %d, want Back, the title and Create: %+v", len(bar.Children), bar.Children)
	}

	back, _, err := Pick(ui.Root, Match{ID: "BackButton"})
	if err != nil {
		t.Fatal(err)
	}
	// Frame [16,62,44,44] in points, at scale 3.
	if back.Bounds != [4]int{48, 186, 180, 318} || back.Center != [2]int{114, 252} || !back.Clickable {
		t.Errorf("Back = %+v", back)
	}
	if create, _, err := Pick(ui.Root, Match{Text: "Create"}); err != nil || !create.Disabled {
		t.Errorf("Create = %+v, %v", create, err)
	}
	if _, _, err := Pick(ui.Root, Match{Text: "New Group"}); err != nil {
		t.Errorf("title: %v", err)
	}
	// ?interactive=1 keeps them.
	kept := 0
	for _, n := range Prune(ui.Root) {
		if find(n, func(n *Node) bool { return n.ID == "BackButton" || n.ID == "new-group-create-button" }) != nil {
			kept++
		}
	}
	if kept == 0 {
		t.Error("Prune dropped the bar's buttons")
	}
	// One row of 16pt steps across 402pt is 26 probes; landing on an item
	// skips the rest of it.
	if calls > 26 {
		t.Errorf("%d hit tests for one bar", calls)
	}
}

func TestFillByHitLeavesContainersWithChildrenAndCoveredOnesAlone(t *testing.T) {
	raw := []byte(`{"role":"AXApplication","frame":[0,0,402,874],"children":[
		{"role":"AXGroup","frame":[0,62,402,54],"children":[{"role":"AXButton","label":"Back","frame":[16,62,44,44]}]},
		{"role":"AXGroup","occluded":true,"frame":[0,791,402,83]},
		{"role":"AXGroup","frame":[0,900,402,54]},
		{"role":"AXButton","label":"Leaf","frame":[0,300,402,54]}]}`)
	ui, err := parseAXTree(raw, 1206, 2622)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := fillByHit(ui, func(x, y int) (*Node, error) { calls++; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("%d hit tests; a bar with children, a covered bar and one off the screen need none", calls)
	}
}

func TestFillByHitKeepsWhatItFoundBeforeAnError(t *testing.T) {
	ui := loadNewGroup(t)
	calls := 0
	ok := fakeHit(ui, newGroupHits, &calls)
	broken := errors.New("input process gone")
	err := fillByHit(ui, func(x, y int) (*Node, error) {
		if calls >= 2 {
			return nil, broken
		}
		return ok(x, y)
	})
	if !errors.Is(err, broken) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := Pick(ui.Root, Match{ID: "BackButton"}); err != nil {
		t.Errorf("BackButton, found by the second probe: %v", err)
	}
}

func TestFillByHitStopsAtItsBudget(t *testing.T) {
	// A childless group the size of the screen: rows every 44pt across it
	// would be about 500 probes.
	raw := []byte(`{"role":"AXApplication","frame":[0,0,402,874],"children":[{"role":"AXGroup","frame":[0,0,402,874]}]}`)
	ui, err := parseAXTree(raw, 1206, 2622)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := fillByHit(ui, func(x, y int) (*Node, error) { calls++; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if calls != fillMaxProbes {
		t.Errorf("%d hit tests, want the cap %d", calls, fillMaxProbes)
	}
}
