package target

import (
	"errors"
	"fmt"
	"strings"
)

// Match picks a node out of the view hierarchy by what it says rather than
// where it is, so a caller can tap "Sign in" without reading coordinates off
// /api/ui first. Every field set must hold for a node to match.
type Match struct {
	// ID is a resource-id on Android or an accessibility identifier on iOS.
	// A bare Android name ("send") matches any package's ":id/send".
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
	Desc string `json:"desc,omitempty"`
	// Contains matches Text and Desc as case-insensitive substrings instead
	// of exactly. ID always matches exactly.
	Contains bool `json:"contains,omitempty"`
	// Index picks one of several matches, in tree order, counting from 0.
	// Without it more than one match is ErrAmbiguousMatch.
	Index *int `json:"index,omitempty"`
	// WaitMs keeps dumping the hierarchy until the node appears, up to this
	// long. Zero looks once.
	WaitMs int `json:"waitMs,omitempty"`
}

// MaxMatchWaitMs bounds Match.WaitMs.
const MaxMatchWaitMs = 20_000

// ErrNoMatch is a Match no on-screen node satisfies.
var ErrNoMatch = errors.New("no node matches")

// ErrAmbiguousMatch is a Match with no Index that more than one node
// satisfies.
var ErrAmbiguousMatch = errors.New("more than one node matches")

// Empty reports whether m names nothing to match on.
func (m Match) Empty() bool {
	return m.ID == "" && m.Text == "" && m.Desc == ""
}

// String is m as a caller wrote it, for errors and traces.
func (m Match) String() string {
	var parts []string
	if m.ID != "" {
		parts = append(parts, fmt.Sprintf("id=%q", m.ID))
	}
	if m.Text != "" {
		parts = append(parts, fmt.Sprintf("text=%q", m.Text))
	}
	if m.Desc != "" {
		parts = append(parts, fmt.Sprintf("desc=%q", m.Desc))
	}
	if m.Contains {
		parts = append(parts, "contains")
	}
	if m.Index != nil {
		parts = append(parts, fmt.Sprintf("index=%d", *m.Index))
	}
	return strings.Join(parts, " ")
}

// FindNodes returns every node under root that m matches, in tree order.
//
// Nodes with no area and nodes simcap marked occluded are left out: a tap at
// their center would land on something else. Uiautomator often stacks a
// Compose node and its host at one rectangle with the same label, so a
// match with the bounds of one already found is the same target and is
// dropped rather than making the match ambiguous.
func FindNodes(root *Node, m Match) []*Node {
	if root == nil || m.Empty() {
		return nil
	}
	var out []*Node
	seen := map[[4]int]bool{}
	var walk func(n *Node)
	walk = func(n *Node) {
		b := n.Bounds
		if m.matches(n) && b[2] > b[0] && b[3] > b[1] && !n.Occluded && !seen[b] {
			seen[b] = true
			out = append(out, n)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	return out
}

// Pick resolves m against root to the one node it names.
func Pick(root *Node, m Match) (*Node, []*Node, error) {
	found := FindNodes(root, m)
	switch {
	case len(found) == 0:
		return nil, nil, fmt.Errorf("%w: %s", ErrNoMatch, m)
	case m.Index != nil:
		if *m.Index < 0 || *m.Index >= len(found) {
			return nil, found, fmt.Errorf("%w: %s; %d match", ErrNoMatch, m, len(found))
		}
		return found[*m.Index], found, nil
	case len(found) > 1:
		return nil, found, fmt.Errorf("%w: %s matches %d nodes; pass index", ErrAmbiguousMatch, m, len(found))
	}
	return found[0], found, nil
}

func (m Match) matches(n *Node) bool {
	if m.ID != "" && n.ID != m.ID && !strings.HasSuffix(n.ID, ":id/"+m.ID) {
		return false
	}
	if m.Text != "" && !m.label(n.Text, m.Text) {
		return false
	}
	if m.Desc != "" && !m.label(n.Desc, m.Desc) {
		return false
	}
	return true
}

func (m Match) label(have, want string) bool {
	if m.Contains {
		return strings.Contains(strings.ToLower(have), strings.ToLower(want))
	}
	return have == want
}

// Leaf is n without its children, for a response that names one node.
func (n *Node) Leaf() *Node {
	c := *n
	c.Children = nil
	return &c
}
