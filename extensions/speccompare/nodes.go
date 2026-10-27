package main

import "encoding/json"

// uiNode is the part of a /api/ui node this extension reads.
type uiNode struct {
	Class    string    `json:"class"`
	ID       string    `json:"id"`
	Text     string    `json:"text"`
	Desc     string    `json:"desc"`
	Bounds   Rect      `json:"bounds"`
	Children []*uiNode `json:"children"`
}

// uiTree is /api/ui's body: the full tree has a root, ?interactive=1 a list.
type uiTree struct {
	Root  *uiNode   `json:"root"`
	Nodes []*uiNode `json:"nodes"`
}

// NodeRef names the node a region belongs to.
type NodeRef struct {
	ID     string `json:"id,omitempty"`
	Text   string `json:"text,omitempty"`
	Desc   string `json:"desc,omitempty"`
	Class  string `json:"class,omitempty"`
	Bounds Rect   `json:"bounds"`
}

func parseTree(b []byte) ([]*uiNode, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var t uiTree
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	if t.Root != nil {
		return []*uiNode{t.Root}, nil
	}
	return t.Nodes, nil
}

// nodeFor picks the node a region is about: the deepest one whose bounds
// hold the whole region, so "send_contact_row differs" rather than "the
// screen differs". A region that straddles two siblings falls back to their
// parent, which is the honest answer. Nodes with nothing to name them (no
// id, text or desc) are skipped in favour of a named ancestor.
func nodeFor(roots []*uiNode, r Rect) *NodeRef {
	var best *uiNode
	bestDepth := -1
	var walk func(n *uiNode, depth int)
	walk = func(n *uiNode, depth int) {
		if !n.Bounds.contains(r) {
			return
		}
		if named(n) && depth > bestDepth {
			best, bestDepth = n, depth
		}
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	for _, n := range roots {
		walk(n, 0)
	}
	if best == nil {
		return nil
	}
	return &NodeRef{ID: best.ID, Text: best.Text, Desc: best.Desc, Class: best.Class, Bounds: best.Bounds}
}

func named(n *uiNode) bool { return n.ID != "" || n.Text != "" || n.Desc != "" }

// label is how a region's box is captioned on the overlay.
func (n *NodeRef) label() string {
	switch {
	case n == nil:
		return ""
	case n.Text != "":
		return n.Text
	case n.Desc != "":
		return n.Desc
	}
	id := n.ID
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			return id[i+1:]
		}
	}
	return id
}
