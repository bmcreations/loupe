package target

import (
	"errors"
	"math"
	"os"
	"strings"
	"testing"
)

func readPhoneFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/phoneax/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func walkAX(n *axNode, f func(*axNode)) {
	f(n)
	for _, c := range n.Children {
		walkAX(c, f)
	}
}

func findAX(n *axNode, pred func(*axNode) bool) *axNode {
	var hit *axNode
	walkAX(n, func(x *axNode) {
		if hit == nil && pred(x) {
			hit = x
		}
	})
	return hit
}

const phoneClock = "8:54 AM"

func isClock(n *axNode) bool { return n.Role == "AXStaticText" && n.Label == phoneClock }

func near(a, b []float64, tol float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > tol {
			return false
		}
	}
	return true
}

func TestPhoneAXAdditionalData(t *testing.T) {
	root, err := decodePhoneAX(readPhoneFixture(t, "additionaldata.bplist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Frame) != 4 || root.Frame[2] != 402 {
		t.Fatalf("root frame = %v", root.Frame)
	}
	if len(root.Children) != 25 {
		t.Errorf("root children = %d, want 25", len(root.Children))
	}
	total := 0
	walkAX(root, func(*axNode) { total++ })
	if total != 26 {
		t.Errorf("nodes = %d, want 26", total)
	}
	clock := findAX(root, isClock)
	if clock == nil {
		t.Fatal("no clock node")
	}
	if !near(clock.Frame, []float64{59, 89.33, 283.67, 138}, 0.5) {
		t.Errorf("clock frame = %v", clock.Frame)
	}
	if findAX(root, func(n *axNode) bool {
		return n.Role == "AXButton" && strings.Contains(n.Label, "FLY DELTA")
	}) == nil {
		t.Error("no FLY DELTA button")
	}
}

func TestPhoneAXInitialDumpHasNoRoles(t *testing.T) {
	init := readPhoneFixture(t, "initialdump.bplist")
	add := readPhoneFixture(t, "additionaldata.bplist")

	root, err := decodePhoneAX(init)
	if err != nil {
		t.Fatal(err)
	}
	total, labeled := 0, 0
	walkAX(root, func(n *axNode) {
		total++
		if n.Label != "" && len(n.Frame) == 4 {
			labeled++
		}
	})
	if total != 26 || labeled == 0 {
		t.Errorf("nodes = %d labeled = %d", total, labeled)
	}
	if findAX(root, func(n *axNode) bool { return n.Role == "AXButton" }) != nil {
		t.Error("initial dump should not carry roles")
	}

	merged, err := decodePhoneAXMerged(init, add)
	if err != nil {
		t.Fatal(err)
	}
	if findAX(merged, func(n *axNode) bool { return n.Role == "AXButton" }) == nil {
		t.Error("merged dump lost roles")
	}
	if findAX(merged, isClock) == nil {
		t.Error("merged dump lost the clock")
	}
}

// The phone's orientation message carries no element nodes. The bridge filters
// it out, but the merge must survive one if it slips through, not abort the
// whole tree.
func TestPhoneAXMergeSkipsOrientation(t *testing.T) {
	init := readPhoneFixture(t, "initialdump.bplist")
	add := readPhoneFixture(t, "additionaldata.bplist")
	orient := readPhoneFixture(t, "orientation.bplist")

	if _, err := decodePhoneAX(orient); !errors.Is(err, errPhoneAXEmpty) {
		t.Fatalf("orientation alone: want errPhoneAXEmpty, got %v", err)
	}
	merged, err := decodePhoneAXMerged(init, orient, add)
	if err != nil {
		t.Fatalf("merge with orientation: %v", err)
	}
	if findAX(merged, func(n *axNode) bool { return n.Role == "AXButton" }) == nil {
		t.Error("orientation in the stream dropped the roles")
	}
}

// Frames arrive in the phone's order, but if an AdditionalData dump lands
// before a (role-less) InitialDump re-send, the later InitialDump must not
// clobber the roles the merge already has.
func TestPhoneAXMergeOrderKeepsRoles(t *testing.T) {
	init := readPhoneFixture(t, "initialdump.bplist")
	add := readPhoneFixture(t, "additionaldata.bplist")

	merged, err := decodePhoneAXMerged(add, init)
	if err != nil {
		t.Fatal(err)
	}
	if findAX(merged, func(n *axNode) bool { return n.Role == "AXButton" }) == nil {
		t.Error("a later role-less dump clobbered the roles")
	}
}

func TestPhoneAXToUI(t *testing.T) {
	root, err := decodePhoneAX(readPhoneFixture(t, "additionaldata.bplist"))
	if err != nil {
		t.Fatal(err)
	}
	ui, err := phoneAXToUI(root, 1206, 2622)
	if err != nil {
		t.Fatal(err)
	}
	if ui.Width != 1206 || ui.Height != 2622 || ui.scale != 3 {
		t.Fatalf("ui = %dx%d scale %v", ui.Width, ui.Height, ui.scale)
	}
	var clock *Node
	var walk func(*Node)
	walk = func(n *Node) {
		if clock == nil && n.Text == phoneClock {
			clock = n
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(ui.Root)
	if clock == nil {
		t.Fatal("clock node missing from UI")
	}
	want := [4]int{177, 268, 1028, 682}
	for i, w := range want {
		if d := clock.Bounds[i] - w; d < -1 || d > 1 {
			t.Fatalf("clock bounds = %v, want ~%v", clock.Bounds, want)
		}
	}
}
