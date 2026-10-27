package target

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// dumpPath is where uiautomator is told to write its XML. /data/local/tmp is
// writable by the shell user on every device and needs no storage permission,
// which /sdcard does on newer Android.
const dumpPath = "/data/local/tmp/loupe-ui.xml"

// UI is the on-screen view hierarchy at one instant.
//
// Coordinates here are DEVICE pixels, which is what uiautomator reports and
// what `adb shell input` consumes. They are not stream coordinates: MaxSize
// downscales the encoded video, so a node at device x=1000 is somewhere else
// entirely in a 1080-wide stream of a 1440-wide panel. Everything that crosses
// this boundary must scale, and Width/Height are here so it can.
type UI struct {
	Width    int   `json:"width"`
	Height   int   `json:"height"`
	Rotation int   `json:"rotation"`
	Root     *Node `json:"root"`
}

// Node is one view. Fields that are empty or false are omitted, because a
// hierarchy is mostly layout containers and the defaults carry no information.
type Node struct {
	// Class is the last segment of the view's class name: "FrameLayout", not
	// "android.widget.FrameLayout".
	Class string `json:"class,omitempty"`
	// ID is the full resource-id, package included. The package is kept
	// because "android:id/content" and "com.example:id/content" are different
	// views and an agent matching on the bare name would conflate them.
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
	// Desc is content-desc, which is often the only label an icon button has.
	Desc string `json:"desc,omitempty"`
	// Value is an iOS element's accessibility value: a text field's contents
	// or placeholder, a slider's position. Android has no counterpart; its
	// text fields put their contents in Text.
	Value string `json:"value,omitempty"`
	// Bounds is [x1,y1,x2,y2] in device pixels.
	Bounds [4]int `json:"bounds"`
	// Center is the point to tap, precomputed. It is the single most-used
	// derivation of Bounds, and leaving it to the caller invites the
	// off-by-one-rounding disagreements that make a tap land on a neighbour.
	Center [2]int `json:"center"`

	Clickable  bool `json:"clickable,omitempty"`
	Scrollable bool `json:"scrollable,omitempty"`
	Checkable  bool `json:"checkable,omitempty"`
	Checked    bool `json:"checked,omitempty"`
	Selected   bool `json:"selected,omitempty"`
	Focused    bool `json:"focused,omitempty"`
	// Password is uiautomator's own "password" attribute: the field's input
	// type marks it as one, independent of anything in Text or ID. It is what
	// lets /api/input redact a typed value before publishing the event that
	// carries it -- see publishAgentEvent in internal/httpapi/agent.go.
	Password bool `json:"password,omitempty"`
	// Disabled is inverted from uiautomator's "enabled" so that the common
	// case serialises to nothing.
	Disabled bool `json:"disabled,omitempty"`
	// Occluded means a tap at Center will not reach this node: something
	// else is on top there, or Center is off the screen. Only iOS simulators
	// set it, and only one way -- its absence does not prove the node is
	// visible. See markOccluded in simcap's `ax.swift`.
	Occluded bool `json:"occluded,omitempty"`

	// Composable and Source name the composable that drew this node, from
	// the Compose inspector, and its call site ("MessageList.kt:661"). Only
	// /api/ui?compose=1 sets them. See AnnotateComposables.
	Composable string `json:"composable,omitempty"`
	Source     string `json:"source,omitempty"`

	Children []*Node `json:"children,omitempty"`
}

// Interesting reports whether a node says anything an agent could act on or
// match against. A layout container with no label, no id and no behaviour is
// structure, not content.
func (n *Node) Interesting() bool {
	return n.Text != "" || n.Desc != "" || n.ID != "" || n.Value != "" ||
		n.Clickable || n.Scrollable || n.Checkable
}

// Inspector is implemented by targets that can report their view hierarchy.
// Synthetic cannot, so this is deliberately not part of Target.
type Inspector interface {
	Inspect(ctx context.Context) (*UI, error)
}

// Inspect dumps the current view hierarchy.
//
// This deliberately does not go through the live stream. uiautomator reads the
// accessibility tree, which is available whether or not anything is streaming,
// and asking the video path for it would couple a text query to an encoder.
func (t *ADBTarget) Inspect(ctx context.Context) (*UI, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}

	// uiautomator writes the file itself and prints only a status line, so the
	// dump and the read are two commands. Errors arrive on stdout as plain
	// prose rather than a non-zero exit, which is why the output is inspected
	// instead of trusted.
	//
	// The echo is for the one failure that prints nothing at all. `exec` keeps
	// the pid, so $$ -- expanded by sh on the device -- is uiautomator's own,
	// and its crash can be read back out of logcat by that pid.
	out, err := t.adb.Shell(ctx, t.info.Serial,
		"echo "+dumpPIDPrefix+"$$; exec uiautomator dump "+dumpPath)
	pid, out := splitDumpPID(out)
	if err != nil {
		if pid > 0 {
			crash, _ := t.adb.Shell(ctx, t.info.Serial,
				"logcat", "-d", "-b", "crash", "--pid="+strconv.Itoa(pid))
			if dumpCrashIsUIAutomationConflict(crash) {
				return nil, fmt.Errorf("uiautomator dump: %w", ErrUIAutomationBusy)
			}
		}
		return nil, fmt.Errorf("uiautomator dump: %w: %s", err, strings.TrimSpace(out))
	}
	if !strings.Contains(out, dumpPath) {
		return nil, fmt.Errorf("uiautomator dump did not produce a hierarchy: %s", strings.TrimSpace(out))
	}

	raw, err := t.adb.ExecOut(ctx, t.info.Serial, "cat", dumpPath)
	if err != nil {
		return nil, err
	}
	return ParseUI(raw)
}

// ErrUIAutomationBusy is a dump refused because another UiAutomation client
// already holds the device. Android allows one at a time, and a Maestro driver
// (dev.mobile.maestro) holds it for as long as `maestro mcp` or a flow runs.
//
// uiautomator reports this by crashing: the IllegalStateException goes to
// logcat, nothing reaches stdout or stderr, and adb exits 137 because the
// runtime SIGKILLs the process after the uncaught exception. Without the
// logcat read it is a bare "exit status 137:".
var ErrUIAutomationBusy = errors.New("another UiAutomation client (e.g. Maestro) holds this device")

const dumpPIDPrefix = "loupe-pid="

// splitDumpPID takes the pid line off the front of the dump's output and
// returns the rest as uiautomator printed it. The pid is 0 when the line is
// missing, which is the device shell failing before echo ran.
func splitDumpPID(out string) (int, string) {
	line, rest, _ := strings.Cut(out, "\n")
	v, ok := strings.CutPrefix(strings.TrimSpace(line), dumpPIDPrefix)
	if !ok {
		return 0, out
	}
	pid, err := strconv.Atoi(v)
	if err != nil {
		return 0, out
	}
	return pid, rest
}

// dumpCrashIsUIAutomationConflict reads the crash buffer for the dump's pid.
// Both halves of the message are required: "already registered!" alone is
// thrown by other registries, and the pid filter already ties it to this
// process.
func dumpCrashIsUIAutomationConflict(crash string) bool {
	for _, line := range strings.Split(crash, "\n") {
		if strings.Contains(line, "IllegalStateException: UiAutomationService") &&
			strings.Contains(line, "already registered") {
			return true
		}
	}
	return false
}

// xmlHierarchy and xmlNode mirror uiautomator's output. Every attribute is a
// string there, including the booleans.
type xmlHierarchy struct {
	XMLName  xml.Name  `xml:"hierarchy"`
	Rotation string    `xml:"rotation,attr"`
	Nodes    []xmlNode `xml:"node"`
}

type xmlNode struct {
	Class      string    `xml:"class,attr"`
	ResID      string    `xml:"resource-id,attr"`
	Text       string    `xml:"text,attr"`
	Desc       string    `xml:"content-desc,attr"`
	Bounds     string    `xml:"bounds,attr"`
	Clickable  string    `xml:"clickable,attr"`
	Scrollable string    `xml:"scrollable,attr"`
	Checkable  string    `xml:"checkable,attr"`
	Checked    string    `xml:"checked,attr"`
	Selected   string    `xml:"selected,attr"`
	Focused    string    `xml:"focused,attr"`
	Password   string    `xml:"password,attr"`
	Enabled    string    `xml:"enabled,attr"`
	Nodes      []xmlNode `xml:"node"`
}

// ParseUI turns uiautomator XML into a hierarchy. It is exported so the parse
// can be tested against captured output without a device attached.
func ParseUI(b []byte) (*UI, error) {
	var h xmlHierarchy
	if err := xml.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("parse hierarchy: %w", err)
	}
	if len(h.Nodes) == 0 {
		return nil, fmt.Errorf("parse hierarchy: no nodes; the screen may be off or locked")
	}

	rot, _ := strconv.Atoi(h.Rotation)
	ui := &UI{Rotation: rot, Root: convert(h.Nodes[0])}

	// A hierarchy can have several roots when more than one window is showing,
	// a dialog over an activity being the usual case. The screen is the union,
	// and taking only the first root would under-report it.
	for _, n := range h.Nodes[1:] {
		if c := convert(n); c != nil {
			ui.Root.Children = append(ui.Root.Children, c)
			ui.Width = max(ui.Width, c.Bounds[2])
			ui.Height = max(ui.Height, c.Bounds[3])
		}
	}
	ui.Width = max(ui.Width, ui.Root.Bounds[2])
	ui.Height = max(ui.Height, ui.Root.Bounds[3])
	return ui, nil
}

func convert(x xmlNode) *Node {
	n := &Node{
		Class:      shortClass(x.Class),
		ID:         x.ResID,
		Text:       x.Text,
		Desc:       x.Desc,
		Bounds:     parseBounds(x.Bounds),
		Clickable:  x.Clickable == "true",
		Scrollable: x.Scrollable == "true",
		Checkable:  x.Checkable == "true",
		Checked:    x.Checked == "true",
		Selected:   x.Selected == "true",
		Focused:    x.Focused == "true",
		Password:   x.Password == "true",
		Disabled:   x.Enabled == "false",
	}
	n.Center = [2]int{
		(n.Bounds[0] + n.Bounds[2]) / 2,
		(n.Bounds[1] + n.Bounds[3]) / 2,
	}
	for _, c := range x.Nodes {
		n.Children = append(n.Children, convert(c))
	}
	return n
}

// FocusedAll returns every node in ui marked focused, in depth-first order.
// A dump can carry more than one: a WebView or Compose host can mark itself
// focused alongside the leaf field actually receiving keystrokes, and a
// dialog's window can leave a focused node behind it in another window. A
// caller deciding whether a password field is focused must check all of
// them, not just the first or the deepest.
func FocusedAll(ui *UI) []*Node {
	if ui == nil {
		return nil
	}
	var out []*Node
	collectFocused(ui.Root, &out)
	return out
}

func collectFocused(n *Node, out *[]*Node) {
	if n == nil {
		return
	}
	if n.Focused {
		*out = append(*out, n)
	}
	for _, c := range n.Children {
		collectFocused(c, out)
	}
}

// Prune drops nodes that carry no information, lifting their children into
// place. Layout containers are the bulk of any hierarchy and mean nothing to a
// caller looking for something to tap; a node is kept if it is Interesting
// itself or if it is the only thing holding several kept subtrees together.
func Prune(n *Node) []*Node {
	if n == nil {
		return nil
	}
	var kids []*Node
	for _, c := range n.Children {
		kids = append(kids, Prune(c)...)
	}
	// An occluded node is not something to tap, but its children carry their
	// own verdicts: a container can be covered at its centre and not at its
	// edges.
	if n.Occluded {
		return kids
	}
	if n.Interesting() {
		out := *n
		out.Children = kids
		return []*Node{&out}
	}
	// Keeping a dull node with two or more kept children preserves the
	// grouping that tells a list row from the list. Collapsing a chain with a
	// single child only removes a level of indirection.
	if len(kids) > 1 {
		out := *n
		out.Children = kids
		return []*Node{&out}
	}
	return kids
}

func shortClass(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// parseBounds reads uiautomator's "[x1,y1][x2,y2]".
func parseBounds(s string) [4]int {
	var b [4]int
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == '[' || r == ']' || r == ','
	})
	if len(f) != 4 {
		return b
	}
	for i := range b {
		b[i], _ = strconv.Atoi(f[i])
	}
	return b
}
