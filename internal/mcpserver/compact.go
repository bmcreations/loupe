package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// compactHeader explains the line format once per answer, so each node can
// leave its keys out.
const compactHeader = "One line per node, indented under its parent: Class \"text\" desc=\"…\" value=\"…\" id=… @x,y and flags. Tap a match on text, desc or id copied verbatim from here, or tap x,y at the @ point (device pixels)."

// compactScreen turns /api/ui's JSON into one line per node an agent could
// tap, match or read. Layout containers that carry nothing but an id are
// dropped and their children lifted to their parent's depth; an id-only
// leaf stays, because it may be the only handle a field or icon has. On the
// Seeker's Google TV search screen this cut 154 nodes and 21 KB of JSON to
// a few KB of text, which every later model turn rereads.
//
// Anything that fails to parse comes back as it was, so a server change in
// the JSON never costs the agent the screen.
func compactScreen(b []byte) string {
	var s struct {
		Width    int            `json:"width"`
		Height   int            `json:"height"`
		Rotation int            `json:"rotation"`
		Nodes    []*target.Node `json:"nodes"`
	}
	if err := json.Unmarshal(b, &s); err != nil || s.Nodes == nil {
		return string(b)
	}
	var w strings.Builder
	fmt.Fprintf(&w, "screen %dx%d rotation %d. %s", s.Width, s.Height, s.Rotation, compactHeader)
	pkg := commonIDPrefix(s.Nodes)
	if pkg != "" {
		fmt.Fprintf(&w, " Ids without a package are %s; a match takes either form.", strings.TrimSuffix(pkg, ":id/"))
	}
	w.WriteByte('\n')
	for _, n := range s.Nodes {
		writeCompact(&w, n, 0, pkg)
	}
	return w.String()
}

// commonIDPrefix is the "package:id/" most of the tree's ids share, so each
// line can drop it. match already takes a bare name, matching it against
// the end of the resource-id.
func commonIDPrefix(nodes []*target.Node) string {
	count := map[string]int{}
	var walk func([]*target.Node)
	walk = func(ns []*target.Node) {
		for _, n := range ns {
			if i := strings.Index(n.ID, ":id/"); i > 0 {
				count[n.ID[:i+len(":id/")]]++
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	best, most := "", 0
	for p, c := range count {
		if c > most || (c == most && p < best) {
			best, most = p, c
		}
	}
	return best
}

func keepCompact(n *target.Node) bool {
	if n.Text != "" || n.Desc != "" || n.Value != "" || n.Clickable || n.Scrollable || n.Checkable {
		return true
	}
	return n.ID != "" && len(n.Children) == 0
}

func writeCompact(w *strings.Builder, n *target.Node, depth int, pkg string) {
	next := depth
	if keepCompact(n) {
		w.WriteString(strings.Repeat("  ", depth))
		w.WriteString(compactLine(n, pkg))
		w.WriteByte('\n')
		next = depth + 1
	}
	for _, c := range n.Children {
		writeCompact(w, c, next, pkg)
	}
}

func compactLine(n *target.Node, pkg string) string {
	parts := []string{n.Class}
	if n.Class == "" {
		parts[0] = "Node"
	}
	if n.Text != "" {
		parts = append(parts, fmt.Sprintf("%q", n.Text))
	}
	if n.Desc != "" {
		parts = append(parts, "desc="+fmt.Sprintf("%q", n.Desc))
	}
	if n.Value != "" {
		parts = append(parts, "value="+fmt.Sprintf("%q", n.Value))
	}
	if n.ID != "" {
		id := n.ID
		if pkg != "" {
			id = strings.TrimPrefix(id, pkg)
		}
		parts = append(parts, "id="+id)
	}
	parts = append(parts, fmt.Sprintf("@%d,%d", n.Center[0], n.Center[1]))
	flag := func(on bool, name string) {
		if on {
			parts = append(parts, name)
		}
	}
	flag(n.Clickable, "click")
	if n.Scrollable {
		dirs := ""
		if n.ScrollBackward != nil && *n.ScrollBackward {
			dirs += "back"
		}
		if n.ScrollForward != nil && *n.ScrollForward {
			if dirs != "" {
				dirs += ","
			}
			dirs += "fwd"
		}
		if dirs == "" {
			parts = append(parts, "scroll")
		} else {
			parts = append(parts, "scroll:"+dirs)
		}
	}
	flag(n.Checkable && !n.Checked, "checkable")
	flag(n.Checked, "checked")
	flag(n.Selected, "selected")
	flag(n.Focused, "focused")
	flag(n.Password, "password")
	flag(n.Disabled, "disabled")
	flag(n.Occluded, "occluded")
	return strings.Join(parts, " ")
}
