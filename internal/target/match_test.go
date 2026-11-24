package target

import (
	"errors"
	"testing"
)

func matchTree() *Node {
	return &Node{Class: "FrameLayout", Bounds: [4]int{0, 0, 1080, 2400}, Children: []*Node{
		// A Compose host and its node at one rectangle, both labelled.
		{Class: "View", Text: "Sign in", Bounds: [4]int{100, 100, 500, 200}, Clickable: true, Children: []*Node{
			{Class: "TextView", Text: "Sign in", Bounds: [4]int{100, 100, 500, 200}},
		}},
		{Class: "ImageButton", ID: "com.acme:id/send", Desc: "Send", Bounds: [4]int{900, 2200, 1000, 2300}},
		{Class: "ImageButton", ID: "android:id/send", Bounds: [4]int{0, 2200, 100, 2300}},
		{Class: "TextView", Text: "Reply", Bounds: [4]int{0, 300, 500, 400}},
		{Class: "TextView", Text: "Reply all", Bounds: [4]int{0, 400, 500, 500}},
		{Class: "TextView", Text: "Hidden", Bounds: [4]int{0, 500, 0, 600}},
		{Class: "Button", Text: "Behind", Bounds: [4]int{0, 600, 500, 700}, Occluded: true},
	}}
}

func TestPickFindsByText(t *testing.T) {
	n, _, err := Pick(matchTree(), Match{Text: "Sign in"})
	if err != nil {
		t.Fatal(err)
	}
	if n.Class != "View" {
		t.Fatalf("picked %s, want the outer node of the stacked pair", n.Class)
	}
}

func TestPickTakesABareIDFromAnyPackage(t *testing.T) {
	_, found, err := Pick(matchTree(), Match{ID: "send"})
	if !errors.Is(err, ErrAmbiguousMatch) || len(found) != 2 {
		t.Fatalf("err %v found %d, want two ambiguous matches", err, len(found))
	}
	n, _, err := Pick(matchTree(), Match{ID: "com.acme:id/send"})
	if err != nil || n.Desc != "Send" {
		t.Fatalf("full id picked %+v, %v", n, err)
	}
}

func TestPickByDescAndID(t *testing.T) {
	n, _, err := Pick(matchTree(), Match{ID: "send", Desc: "Send"})
	if err != nil || n.ID != "com.acme:id/send" {
		t.Fatalf("picked %+v, %v", n, err)
	}
}

func TestContainsIsCaseInsensitiveAndIndexPicks(t *testing.T) {
	_, found, err := Pick(matchTree(), Match{Text: "reply", Contains: true})
	if !errors.Is(err, ErrAmbiguousMatch) || len(found) != 2 {
		t.Fatalf("err %v found %d", err, len(found))
	}
	one := 1
	n, _, err := Pick(matchTree(), Match{Text: "reply", Contains: true, Index: &one})
	if err != nil || n.Text != "Reply all" {
		t.Fatalf("index 1 picked %+v, %v", n, err)
	}
	five := 5
	if _, _, err := Pick(matchTree(), Match{Text: "reply", Contains: true, Index: &five}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("an index past the end gave %v", err)
	}
}

func TestExactTextDoesNotMatchAPrefix(t *testing.T) {
	n, _, err := Pick(matchTree(), Match{Text: "Reply"})
	if err != nil || n.Text != "Reply" {
		t.Fatalf("picked %+v, %v", n, err)
	}
}

// labelTree is a Flutter or React Native screen on Android: the controls
// carry their label in content-desc and have no text, and one text node
// shares a label with a desc-only icon.
func labelTree() *Node {
	return &Node{Class: "FrameLayout", Bounds: [4]int{0, 0, 1080, 2400}, Children: []*Node{
		{Class: "View", ID: "send", Bounds: [4]int{922, 2190, 1049, 2316}, Children: []*Node{
			{Class: "Button", Desc: "Send", Bounds: [4]int{922, 2190, 1049, 2316}, Clickable: true},
		}},
		{Class: "Button", Desc: "Scan QR code", Bounds: [4]int{912, 139, 1038, 265}, Clickable: true},
		{Class: "Button", Desc: "Scan later", Bounds: [4]int{700, 139, 900, 265}, Clickable: true},
		{Class: "TextView", Text: "Search", Bounds: [4]int{0, 300, 500, 400}},
		{Class: "ImageButton", Desc: "Search", Bounds: [4]int{900, 300, 1000, 400}},
		{Class: "Button", Text: "Archive", Desc: "Delete", Bounds: [4]int{0, 500, 500, 600}},
		{Class: "Button", Desc: "Gone", Bounds: [4]int{0, 700, 0, 800}},
		{Class: "Button", Desc: "Covered", Bounds: [4]int{0, 800, 500, 900}, Occluded: true},
	}}
}

func TestTextFallsBackToTheDescOfANodeWithNoText(t *testing.T) {
	n, _, err := Pick(labelTree(), Match{Text: "Send"})
	if err != nil || n.Class != "Button" || n.Desc != "Send" {
		t.Fatalf("picked %+v, %v", n, err)
	}
}

func TestTextPrefersATextNodeOverADescOnlyNode(t *testing.T) {
	n, found, err := Pick(labelTree(), Match{Text: "Search"})
	if err != nil || n.Class != "TextView" || len(found) != 1 {
		t.Fatalf("picked %+v of %d, %v; want the TextView alone", n, len(found), err)
	}
}

// contains widens what each pass accepts, not when the second one runs: the
// icon whose desc holds "sear" stays out while a text node holds it.
func TestContainsTextPrefersATextNodeOverADescOnlyNode(t *testing.T) {
	n, found, err := Pick(labelTree(), Match{Text: "sear", Contains: true})
	if err != nil || n.Class != "TextView" || len(found) != 1 {
		t.Fatalf("picked %+v of %d, %v; want the TextView alone", n, len(found), err)
	}
}

func TestFindNodesViaSaysWhichPassFoundThem(t *testing.T) {
	if found, inDesc := FindNodesVia(labelTree(), Match{Text: "Search"}); len(found) != 1 || inDesc {
		t.Fatalf("text=Search: %d found, inDesc %v; want the TextView by its text", len(found), inDesc)
	}
	if found, inDesc := FindNodesVia(labelTree(), Match{Text: "Send"}); len(found) != 1 || !inDesc {
		t.Fatalf("text=Send: %d found, inDesc %v; want the button by its desc", len(found), inDesc)
	}
	if found, inDesc := FindNodesVia(labelTree(), Match{Desc: "Send"}); len(found) != 1 || inDesc {
		t.Fatalf("desc=Send: %d found, inDesc %v; a desc= match is not the fallback", len(found), inDesc)
	}
	if found, inDesc := FindNodesVia(labelTree(), Match{Text: "Nothing"}); len(found) != 0 || inDesc {
		t.Fatalf("text=Nothing: %d found, inDesc %v; want none", len(found), inDesc)
	}
}

func TestTextDoesNotFallBackToTheDescOfANodeWithText(t *testing.T) {
	if _, _, err := Pick(labelTree(), Match{Text: "Delete"}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("text=Delete gave %v, want ErrNoMatch: the node reads Archive", err)
	}
}

func TestTextFallbackHonoursContainsIndexIDAndDesc(t *testing.T) {
	_, found, err := Pick(labelTree(), Match{Text: "scan", Contains: true})
	if !errors.Is(err, ErrAmbiguousMatch) || len(found) != 2 {
		t.Fatalf("contains: err %v found %d, want two", err, len(found))
	}
	one := 1
	if n, _, err := Pick(labelTree(), Match{Text: "scan", Contains: true, Index: &one}); err != nil || n.Desc != "Scan later" {
		t.Fatalf("index 1 picked %+v, %v", n, err)
	}
	if _, _, err := Pick(labelTree(), Match{Text: "Send", ID: "send"}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("id=send text=Send gave %v; the node with the id has no label", err)
	}
	if n, _, err := Pick(labelTree(), Match{Text: "Send", Desc: "Send"}); err != nil || n.Class != "Button" {
		t.Fatalf("text=Send desc=Send picked %+v, %v", n, err)
	}
}

func TestTextFallbackSkipsNodesATapCannotReach(t *testing.T) {
	for _, text := range []string{"Gone", "Covered"} {
		if _, _, err := Pick(labelTree(), Match{Text: text}); !errors.Is(err, ErrNoMatch) {
			t.Errorf("%q: %v, want ErrNoMatch", text, err)
		}
	}
}

func TestPickSkipsNodesATapCannotReach(t *testing.T) {
	for _, text := range []string{"Hidden", "Behind", "Nowhere"} {
		if _, _, err := Pick(matchTree(), Match{Text: text}); !errors.Is(err, ErrNoMatch) {
			t.Errorf("%q: %v, want ErrNoMatch", text, err)
		}
	}
}
