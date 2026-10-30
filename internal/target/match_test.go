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

func TestPickSkipsNodesATapCannotReach(t *testing.T) {
	for _, text := range []string{"Hidden", "Behind", "Nowhere"} {
		if _, _, err := Pick(matchTree(), Match{Text: text}); !errors.Is(err, ErrNoMatch) {
			t.Errorf("%q: %v, want ErrNoMatch", text, err)
		}
	}
}
