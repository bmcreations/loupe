package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// uiNode is the part of a /api/ui node this extension reads.
type uiNode struct {
	Class      string    `json:"class"`
	ID         string    `json:"id"`
	Text       string    `json:"text"`
	Desc       string    `json:"desc"`
	Bounds     [4]int    `json:"bounds"`
	Center     [2]int    `json:"center"`
	Clickable  bool      `json:"clickable"`
	Scrollable bool      `json:"scrollable"`
	Checkable  bool      `json:"checkable"`
	Composable string    `json:"composable"`
	Children   []*uiNode `json:"children"`
}

type uiTree struct {
	Root  *uiNode   `json:"root"`
	Nodes []*uiNode `json:"nodes"`
}

func parseTree(b []byte) ([]*uiNode, error) {
	var t uiTree
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	if t.Root != nil {
		return []*uiNode{t.Root}, nil
	}
	return t.Nodes, nil
}

func walk(roots []*uiNode, f func(*uiNode)) {
	var rec func(*uiNode)
	rec = func(n *uiNode) {
		f(n)
		for _, c := range n.Children {
			rec(c)
		}
	}
	for _, r := range roots {
		rec(r)
	}
}

func interactive(n *uiNode) bool { return n.Clickable || n.Scrollable || n.Checkable }

// Fingerprint is how a screen is recognised on a later visit.
type Fingerprint struct {
	// Key is what screens are matched on.
	Key string `json:"key"`
	// Screen is the screen-level composable, when /api/ui?compose=1 named one.
	Screen string `json:"screen,omitempty"`
	// Basis says what Key was built from: "ids", "labels" or "classes".
	Basis string `json:"basis"`
	// Named is the agent's name for the screen, from a step's screen field.
	Named string `json:"named,omitempty"`
	// Base is the tree's own key under a named screen.
	Base string `json:"base,omitempty"`
}

// named keys the screen by the agent's name as well as the tree.
func (fp Fingerprint) named(name string) Fingerprint {
	h := sha256.Sum256([]byte(fp.Key + "\nnamed:" + name))
	fp.Base, fp.Key, fp.Named = fp.Key, hex.EncodeToString(h[:])[:16], name
	return fp
}

// fingerprint keys a screen on the kinds of things a person can touch on it,
// not on what they say: the set of class and resource id of each interactive
// node, so a chat list with five rows and one with six are the same screen,
// and so is the same list after a new message (docs/app-map.md, "Telling
// screens apart"). A set, not a list, for the same reason.
//
// With no resource ids on the screen the key falls back to class and
// accessibility label (desc on Android, text on iOS, label below), which splits a screen whose labels
// change. With no labels either it is classes alone, which merges more than it
// should; a merge or rename by hand fixes either.
//
// A screen-level composable (a name ending in "Screen", the outermost one) is
// added to the key when the tree has one: two screens built from the same
// components then still come apart by name.
func fingerprint(roots []*uiNode) Fingerprint {
	var ids, labels, classes []string
	var screen string
	walk(roots, func(n *uiNode) {
		if screen == "" && strings.HasSuffix(n.Composable, "Screen") {
			screen = n.Composable
		}
		if !interactive(n) {
			return
		}
		classes = append(classes, n.Class)
		if n.ID != "" {
			ids = append(ids, n.Class+"#"+n.ID)
		}
		if l := label(n); l != "" {
			labels = append(labels, n.Class+"@"+l)
		}
	})
	fp := Fingerprint{Screen: screen}
	parts := ids
	switch {
	case len(ids) > 0:
		fp.Basis = "ids"
	case len(labels) > 0:
		fp.Basis, parts = "labels", labels
	default:
		fp.Basis, parts = "classes", classes
	}
	parts = dedupe(parts)
	h := sha256.New()
	h.Write([]byte(screen + "\n"))
	for _, p := range parts {
		h.Write([]byte(p + "\n"))
	}
	fp.Key = hex.EncodeToString(h.Sum(nil))[:16]
	return fp
}

// label is a node's accessibility label. Android sets desc; an iOS tree has
// no desc and carries the label as text.
func label(n *uiNode) string {
	if n.Desc != "" {
		return n.Desc
	}
	return n.Text
}

func dedupe(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// NodeSel picks a node out of a tree: the first, in tree order, whose given
// fields all match. An empty field matches anything.
type NodeSel struct {
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
	Desc string `json:"desc,omitempty"`
}

func (s NodeSel) empty() bool { return s.ID == "" && s.Text == "" && s.Desc == "" }

func (s NodeSel) find(roots []*uiNode) *uiNode {
	var hit *uiNode
	walk(roots, func(n *uiNode) {
		if hit != nil {
			return
		}
		if (s.ID == "" || n.ID == s.ID) && (s.Text == "" || n.Text == s.Text) && (s.Desc == "" || n.Desc == s.Desc) {
			hit = n
		}
	})
	return hit
}

// suggestName is a screen's default name: its composable, or else the first
// title-ish text on it. The agent can rename it.
func suggestName(roots []*uiNode, fp Fingerprint) string {
	if fp.Named != "" {
		return fp.Named
	}
	if fp.Screen != "" {
		return fp.Screen
	}
	// A heading names the screen best. Failing that, the first short text,
	// passing over an iOS tree's Application root, which says the app's name.
	var heading, name string
	walk(roots, func(n *uiNode) {
		if n.Text == "" || len([]rune(n.Text)) > 40 || n.Class == "Application" {
			return
		}
		if heading == "" && n.Class == "Heading" {
			heading = n.Text
		}
		if name == "" {
			name = n.Text
		}
	})
	if heading != "" {
		return heading
	}
	return name
}
