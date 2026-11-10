package target

import "slices"

// A navigation bar or tab bar can reach loupe as a container that lists no
// children while its buttons answer hit tests. In Flipcash on an iPhone 17
// simulator, iOS 27, Chats' navigation bar came through as a Group with id
// "Chat" and the tab bar as a Group labelled "Tab Bar", both childless, while
// a hit test at the bar's "+" answered new-chat-button and one along the tab
// bar answered each of the five tabs. On New Group a hit at the back chevron
// answered BackButton, whose parent is that same childless Group: the items
// name the container as their parent, and the container enumerates none.
// Why AXPTranslator returns no children for it is not established. The bars
// on New Chat, by contrast, come through flattened under the application,
// with their items.
//
// fillByHit hit-tests inside such containers and adds what answers as their
// children, so a tap by id or text finds them.

const (
	// fillStepPt is the spacing of probes along a row, in points: under the
	// narrowest bar item seen, More's 33pt on Chats.
	fillStepPt = 16
	// fillRowPt is the spacing of rows in a container taller than two rows,
	// in points: the 44pt minimum tap target.
	fillRowPt = 44
	// fillMaxProbes caps hit tests per dump. Chats' navigation bar and tab
	// bar added about 90ms to its dump, for roughly 30 probes.
	fillMaxProbes = 96
)

// fillRoles are the container classes fillByHit looks inside.
var fillRoles = map[string]bool{"Group": true, "Toolbar": true, "TabGroup": true}

// fillByHit adds children to every childless, visible container in ui from
// hit tests across it. hit takes device pixels and returns nil when the point
// hits nothing. A hit is kept when it lies inside the container, is not the
// container, and is not already in the tree. An error stops the probing and
// leaves what was added so far.
func fillByHit(ui *UI, hit func(x, y int) (*Node, error)) error {
	if ui == nil || ui.Root == nil || ui.scale <= 0 {
		return nil
	}
	seen := map[nodeKey]bool{}
	var empty []*Node
	var walk func(n *Node)
	walk = func(n *Node) {
		seen[keyOf(n)] = true
		if n != ui.Root && fillRoles[n.Class] && len(n.Children) == 0 && !n.Occluded && onScreen(n, ui) {
			empty = append(empty, n)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(ui.Root)

	step := max(1, int(fillStepPt*ui.scale))
	budget := fillMaxProbes
	add := func(c, n *Node) {
		if k := keyOf(n); !seen[k] {
			seen[k] = true
			c.Children = append(c.Children, n)
		}
	}
	for _, c := range empty {
		if budget <= 0 {
			break
		}
		// A container that answers at its centre as itself is a leaf, such
		// as a card combined into one element, and costs one probe rather
		// than a row, so a screen of them leaves budget for the bars. A bar
		// answers with an item there: New Group's centre is its title.
		budget--
		n, err := hit(c.Center[0], c.Center[1])
		if err != nil {
			return err
		}
		if n == nil || keyOf(n) == keyOf(c) {
			continue
		}
		if boundsWithin(n.Bounds, c.Bounds) {
			add(c, n)
		}
		for _, y := range fillRows(c.Bounds, ui.scale) {
			for x := c.Bounds[0] + step/2; x < c.Bounds[2] && budget > 0; {
				budget--
				n, err := hit(x, y)
				if err != nil {
					return err
				}
				next := x + step
				if n != nil && keyOf(n) != keyOf(c) && boundsWithin(n.Bounds, c.Bounds) {
					add(c, n)
					// Skip the rest of an item the probe landed on. The
					// container itself, or anything around it, is not one.
					if n.Bounds[0] <= x && n.Bounds[2] >= next {
						next = n.Bounds[2] + 1
					}
				}
				x = next
			}
		}
		// Reading order, left to right within each row, so an index in a
		// match counts the way the bar is drawn, not the order of probes.
		row := max(1, int(fillRowPt*ui.scale))
		slices.SortStableFunc(c.Children, func(a, b *Node) int {
			if d := (a.Center[1]-c.Bounds[1])/row - (b.Center[1]-c.Bounds[1])/row; d != 0 {
				return d
			}
			return a.Bounds[0] - b.Bounds[0]
		})
	}
	return nil
}

// fillRows are the y coordinates fillByHit probes in a container: its middle
// when it is at most two rows tall, as a bar is, else one every fillRowPt.
func fillRows(b [4]int, scale float64) []int {
	row := int(fillRowPt * scale)
	h := b[3] - b[1]
	if row <= 0 || h <= 2*row {
		return []int{(b[1] + b[3]) / 2}
	}
	var ys []int
	for y := b[1] + row/2; y < b[3]; y += row {
		ys = append(ys, y)
	}
	return ys
}

// nodeKey tells one element from another across a dump and a hit test, which
// return separate copies of it.
type nodeKey struct {
	class, id, text string
	bounds          [4]int
}

func keyOf(n *Node) nodeKey { return nodeKey{n.Class, n.ID, n.Text, n.Bounds} }

// boundsWithin is whether inner lies inside outer, allowing a pixel for the
// rounding of points to pixels.
func boundsWithin(inner, outer [4]int) bool {
	return inner[0] >= outer[0]-1 && inner[1] >= outer[1]-1 &&
		inner[2] <= outer[2]+1 && inner[3] <= outer[3]+1 &&
		inner[2] > inner[0] && inner[3] > inner[1]
}

// onScreen is whether n has an area and its centre is on the screen.
func onScreen(n *Node, ui *UI) bool {
	b := n.Bounds
	return b[2] > b[0] && b[3] > b[1] &&
		n.Center[0] >= 0 && n.Center[0] < ui.Width && n.Center[1] >= 0 && n.Center[1] < ui.Height
}
