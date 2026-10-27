package target

import (
	"strconv"
	"strings"

	"github.com/bmcreations/loupe/internal/composetree"
)

// AnnotateComposables names each node in root after the composable whose
// bounds equal the node's exactly.
//
// Several composables often share one rectangle: MessageRow wraps a Box, a
// Column and a Row of the same size. The outermost is the name a developer
// wrote, so the shallowest match wins. A node with no exact match stays
// unnamed. Taking the
// smallest enclosing composable instead would label a text node with its
// parent row's name, which misleads more often than an empty field does.
//
// uiautomator often stacks several nodes at one rectangle, a full-screen
// screen above all. Each uiautomator node is a semantics node, so it anchors
// on a composable with semantics at its rectangle, and takes the name of the
// outermost composable between that anchor and the anchor of the nearest
// named node above it. Composition nesting is not layout nesting: a sibling
// branch, such as a bottom bar's AnimatedContent, can sit full screen too.
// So a node with named nodes below it anchors on the lowest composable at
// its rectangle that is an ancestor of all their anchors, and stays unnamed
// when there is none, as the host Views above the root composable do. A
// node with none below it takes the shallowest composable with semantics at
// its rectangle, unless that composable is an ancestor of another node's
// anchor, in which case it stays unnamed. A reply that sets no semantics flags falls back to the
// shallowest composable at the rectangle below the nearest match.
//
// Only nodes inside a ComposeView are named. The DecorView's layouts and
// the ComposeView itself are full screen like the root composable, but
// nothing in the composition drew them. The match is on the suffix so an
// app's own AbstractComposeView subclass counts too.
func AnnotateComposables(root *Node, comps []*composetree.Composable) {
	parent := map[*composetree.Composable]*composetree.Composable{}
	depth := map[*composetree.Composable]int{}
	byBounds := map[[4]int][]*composetree.Composable{}
	flagged := false
	var index func(*composetree.Composable, []*composetree.Composable, int)
	index = func(up *composetree.Composable, cs []*composetree.Composable, d int) {
		for _, c := range cs {
			byBounds[c.Bounds] = append(byBounds[c.Bounds], c)
			depth[c] = d
			flagged = flagged || c.Semantics
			if up != nil {
				parent[c] = up
			}
			index(c, c.Children, d+1)
		}
	}
	index(nil, comps, 0)
	if !flagged {
		annotateBySize(root, comps, byBounds)
		return
	}

	isAncestor := func(a, c *composetree.Composable) bool {
		for p := parent[c]; p != nil; p = parent[p] {
			if p == a {
				return true
			}
		}
		return false
	}

	// anchors fills anchor bottom-up and returns the anchors of the nearest
	// anchored nodes at or below n. A node in banned is left unanchored.
	anchor := map[*Node]*composetree.Composable{}
	leaf := map[*Node]bool{}
	banned := map[*Node]bool{}
	var anchors func(n *Node, inCompose bool) []*composetree.Composable
	anchors = func(n *Node, inCompose bool) []*composetree.Composable {
		var below []*composetree.Composable
		childIn := inCompose || strings.HasSuffix(n.Class, "ComposeView")
		for _, ch := range n.Children {
			below = append(below, anchors(ch, childIn)...)
		}
		if !inCompose || banned[n] {
			return below
		}
		var best *composetree.Composable
		for _, c := range byBounds[n.Bounds] {
			if !c.Semantics {
				continue
			}
			ok := true
			for _, a := range below {
				if !isAncestor(c, a) {
					ok = false
					break
				}
			}
			// Lowest common ancestor when something below is named,
			// otherwise the shallowest match.
			if ok && (best == nil || len(below) > 0 && depth[c] > depth[best] || len(below) == 0 && depth[c] < depth[best]) {
				best = c
			}
		}
		if best == nil {
			return below
		}
		anchor[n] = best
		leaf[n] = len(below) == 0
		return []*composetree.Composable{best}
	}
	// A leaf's semantics node has no children, so a leaf anchored on an
	// ancestor of another node's anchor matched the wrong composable, and
	// its parent could not anchor around it. Unanchor such leaves and redo.
	for range 8 {
		clear(anchor)
		clear(leaf)
		anchors(root, false)
		again := false
		for n, a := range anchor {
			if !leaf[n] {
				continue
			}
			for _, o := range anchor {
				if isAncestor(a, o) {
					banned[n] = true
					again = true
					break
				}
			}
		}
		if !again {
			break
		}
	}

	// outermost climbs from a while the parent shares its rectangle,
	// stopping short of scope and of any composable with semantics of its
	// own, which belongs to a node of its own.
	outermost := func(a, scope *composetree.Composable) *composetree.Composable {
		top := a
		for {
			p := parent[top]
			if p == nil || p == scope || p.Bounds != a.Bounds || p.Semantics {
				return top
			}
			top = p
		}
	}
	var name func(n *Node, scope *composetree.Composable)
	name = func(n *Node, scope *composetree.Composable) {
		if a := anchor[n]; a != nil {
			setComposable(n, outermost(a, scope))
			scope = a
		}
		for _, ch := range n.Children {
			name(ch, scope)
		}
	}
	name(root, nil)
}

func setComposable(n *Node, c *composetree.Composable) {
	n.Composable = c.Name
	if c.File != "" && c.Line > 0 {
		n.Source = c.File + ":" + strconv.Itoa(c.Line)
	}
}

// annotateBySize is the rule for replies without semantics flags: each node
// takes the shallowest composable at its rectangle below the nearest match
// above it.
func annotateBySize(root *Node, comps []*composetree.Composable, byBounds map[[4]int][]*composetree.Composable) {
	below := func(scope *composetree.Composable, bounds [4]int) *composetree.Composable {
		level := comps
		if scope != nil {
			level = scope.Children
		}
		for len(level) > 0 {
			var next []*composetree.Composable
			for _, c := range level {
				if c.Bounds == bounds {
					return c
				}
				next = append(next, c.Children...)
			}
			level = next
		}
		return nil
	}
	onPath := map[*composetree.Composable]bool{}
	var walk func(n *Node, inCompose bool, scope *composetree.Composable)
	walk = func(n *Node, inCompose bool, scope *composetree.Composable) {
		if n == nil {
			return
		}
		var match *composetree.Composable
		if inCompose {
			if scope != nil {
				match = below(scope, n.Bounds)
			}
			if match == nil {
				for _, c := range byBounds[n.Bounds] {
					if !onPath[c] {
						match = c
						break
					}
				}
			}
		}
		if match != nil {
			setComposable(n, match)
			scope = match
			onPath[match] = true
			defer delete(onPath, match)
		}
		inCompose = inCompose || strings.HasSuffix(n.Class, "ComposeView")
		for _, ch := range n.Children {
			walk(ch, inCompose, scope)
		}
	}
	walk(root, false, nil)
}
