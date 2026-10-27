package target

import (
	"os"
	"testing"

	"github.com/bmcreations/loupe/internal/composetree"
)

func acmechatComposables(t *testing.T) []*composetree.Composable {
	t.Helper()
	b, err := os.ReadFile("../composetree/testdata/acmechat-messages.pb")
	if err != nil {
		t.Fatal(err)
	}
	roots, err := composetree.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return roots
}

// inComposeView nests n the way a dump of a Compose activity does:
// DecorView's FrameLayout, then the ComposeView, both full screen.
func inComposeView(n *Node) *Node {
	full := [4]int{0, 0, 1080, 2400}
	cv := &Node{Class: "ComposeView", Bounds: full, Children: []*Node{n}}
	return &Node{Class: "FrameLayout", Bounds: full, Children: []*Node{cv}}
}

// MessageRow, Box, Column and Row all share these bounds in the fixture;
// the outermost is the one a developer wrote.
func TestAnnotateTakesTheShallowestExactMatch(t *testing.T) {
	leaf := &Node{Class: "View", Bounds: [4]int{53, 1674, 1027, 2125}}
	AnnotateComposables(inComposeView(leaf), acmechatComposables(t))
	if leaf.Composable != "MessageRow" || leaf.Source != "MessageList.kt:661" {
		t.Fatalf("leaf = %q %q, want MessageRow MessageList.kt:661", leaf.Composable, leaf.Source)
	}
}

// A node whose bounds match no composable exactly (here one pixel short,
// as semantics bounds clipped at a screen edge are) stays unnamed rather
// than taking an enclosing composable's name.
func TestAnnotateLeavesUnmatchedNodesAlone(t *testing.T) {
	n := &Node{Class: "View", Bounds: [4]int{53, 1674, 1027, 2124}}
	AnnotateComposables(inComposeView(n), acmechatComposables(t))
	if n.Composable != "" || n.Source != "" {
		t.Fatalf("node = %q %q, want unnamed", n.Composable, n.Source)
	}
}

// The FrameLayout and the ComposeView share the root composable's bounds,
// but no composable drew them; only nodes inside the ComposeView are named.
func TestAnnotateSkipsTheViewSystemAboveComposeView(t *testing.T) {
	inner := &Node{Class: "View", Bounds: [4]int{0, 0, 1080, 2400}}
	root := inComposeView(inner)
	AnnotateComposables(root, acmechatComposables(t))
	cv := root.Children[0]
	if root.Composable != "" || cv.Composable != "" {
		t.Fatalf("FrameLayout = %q, ComposeView = %q, want both unnamed", root.Composable, cv.Composable)
	}
	if inner.Composable == "" {
		t.Fatal("the full-screen node inside the ComposeView is unnamed")
	}
}

// On a device, uiautomator nests several nodes at one rectangle, as many as
// the composables there that carry semantics. Each takes the next composable
// down the chain rather than all repeating the outermost.
func TestAnnotateWalksDownStackedComposablesAtOneRectangle(t *testing.T) {
	r := [4]int{0, 0, 100, 100}
	comps := []*composetree.Composable{{
		Name: "App", Bounds: r, Children: []*composetree.Composable{{
			Name: "Scaffold", Bounds: r, Children: []*composetree.Composable{{
				Name: "Surface", Bounds: r,
				Children: []*composetree.Composable{{Name: "Text", Bounds: [4]int{10, 10, 50, 20}}},
			}},
		}},
	}}
	text := &Node{Class: "TextView", Bounds: [4]int{10, 10, 50, 20}}
	inner := &Node{Class: "View", Bounds: r, Children: []*Node{text}}
	outer := &Node{Class: "View", Bounds: r, Children: []*Node{inner}}
	cv := &Node{Class: "ComposeView", Bounds: r, Children: []*Node{outer}}
	AnnotateComposables(cv, comps)
	if outer.Composable != "App" || inner.Composable != "Scaffold" || text.Composable != "Text" {
		t.Fatalf("outer, inner, text = %q %q %q, want App Scaffold Text", outer.Composable, inner.Composable, text.Composable)
	}
}

// Two siblings at one rectangle are not stacked: each takes the outermost,
// the way the first one does.
func TestAnnotateDoesNotWalkDownForASibling(t *testing.T) {
	r := [4]int{0, 0, 100, 100}
	comps := []*composetree.Composable{{
		Name: "Root", Bounds: [4]int{0, 0, 200, 200}, Children: []*composetree.Composable{{
			Name: "Card", Bounds: r, Children: []*composetree.Composable{{Name: "Box", Bounds: r}},
		}},
	}}
	a := &Node{Class: "View", Bounds: r}
	b := &Node{Class: "View", Bounds: r}
	cv := &Node{Class: "ComposeView", Bounds: [4]int{0, 0, 200, 200}, Children: []*Node{a, b}}
	AnnotateComposables(cv, comps)
	if a.Composable != "Card" || b.Composable != "Card" {
		t.Fatalf("a, b = %q %q, want Card Card", a.Composable, b.Composable)
	}
}

// A uiautomator node is a semantics node, so when the reply marks which
// composables carry semantics, each stacked node anchors on the next one of
// those and is named for the outermost composable between it and the anchor
// above. Theme wrappers without semantics never take a node of their own.
func TestAnnotateAnchorsStackedNodesOnSemantics(t *testing.T) {
	full := [4]int{0, 0, 100, 100}
	row := [4]int{0, 10, 100, 30}
	c := func(name string, sem bool, b [4]int, kids ...*composetree.Composable) *composetree.Composable {
		return &composetree.Composable{Name: name, Semantics: sem, Bounds: b, Children: kids}
	}
	comps := []*composetree.Composable{
		c("App", false, full,
			c("Theme", false, full,
				c("Box", true, full,
					c("ChatScreen", false, full,
						c("Scaffold", true, full,
							c("MessageList", false, full,
								c("LazyColumn", true, full,
									c("MessageRow", false, row,
										c("Row", true, row))))))))),
	}
	rowNode := &Node{Class: "View", Bounds: row}
	list := &Node{Class: "View", Bounds: full, Children: []*Node{rowNode}}
	scaffold := &Node{Class: "View", Bounds: full, Children: []*Node{list}}
	box := &Node{Class: "View", Bounds: full, Children: []*Node{scaffold}}
	cv := &Node{Class: "ComposeView", Bounds: full, Children: []*Node{box}}
	AnnotateComposables(cv, comps)
	got := []string{box.Composable, scaffold.Composable, list.Composable, rowNode.Composable}
	want := []string{"App", "ChatScreen", "MessageList", "MessageRow"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %q, want %q", got, want)
		}
	}
}

// The shape acmechat's chat list showed on emulator-5560: six Views stacked
// full screen, four semantics composables in a chain, and AnimatedContent
// with semantics at the same rectangle in a sibling branch of the
// composition. Composition nesting is not layout nesting, so a stacked View
// takes the lowest semantics composable that is an ancestor of what its
// children matched, and the Views above the chain stay unnamed.
func TestAnnotateAnchorsOnAncestorsOfWhatChildrenMatched(t *testing.T) {
	full := [4]int{0, 0, 1080, 2400}
	rowB := [4]int{0, 283, 1080, 482}
	titleB := [4]int{483, 187, 596, 258}
	c := func(name string, sem bool, b [4]int, kids ...*composetree.Composable) *composetree.Composable {
		return &composetree.Composable{Name: name, Semantics: sem, Bounds: b, Children: kids}
	}
	rowC := c("ChatSwipeRow", false, rowB, c("Row", true, rowB))
	lazy := c("LazyColumn", true, full, rowC)
	scaffold := c("Scaffold", true, full, c("Title", true, titleB), c("ChatList", false, full, lazy))
	nav := c("NavBox", true, full, c("ChatsScreen", false, full, scaffold))
	sibling := c("BottomBarContainer", false, full, c("AnimatedContent", true, full, c("Spacer", false, full)))
	stl := c("SharedTransitionLayout", false, full, c("Decorator", false, full, nav), sibling)
	comps := []*composetree.Composable{c("Provide", false, full, c("Box", true, full, stl))}
	row := &Node{Class: "View", Bounds: rowB}
	v := make([]*Node, 6)
	v[5] = &Node{Class: "View", Bounds: full, Children: []*Node{row}}
	v[4] = &Node{Class: "View", Bounds: full, Children: []*Node{v[5], {Class: "TextView", Bounds: titleB}}}
	for i := 3; i >= 0; i-- {
		v[i] = &Node{Class: "View", Bounds: full, Children: []*Node{v[i+1]}}
	}
	// An empty full-screen overlay beside the screen, as acmechat had three
	// of. Its shallowest match is the root Box, an ancestor of every other
	// anchor, which a leaf's semantics node cannot be.
	overlay := &Node{Class: "View", Bounds: full}
	v[2].Children = append(v[2].Children, overlay)
	cv := &Node{Class: "ComposeView", Bounds: full, Children: []*Node{v[0]}}
	AnnotateComposables(cv, comps)
	var got []string
	for _, n := range v {
		got = append(got, n.Composable)
	}
	want := []string{"", "", "Provide", "SharedTransitionLayout", "ChatsScreen", "ChatList"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stack = %q, want %q", got, want)
		}
	}
	if overlay.Composable != "" {
		t.Fatalf("overlay = %q, want unnamed", overlay.Composable)
	}
	if row.Composable != "ChatSwipeRow" {
		t.Fatalf("row = %q, want ChatSwipeRow", row.Composable)
	}
}
