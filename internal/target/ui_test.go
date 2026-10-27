package target

import (
	"os"
	"testing"
)

// A trimmed but structurally faithful uiautomator dump: one decor root, a
// couple of layout containers carrying nothing, a labelled button, an icon
// button whose only label is content-desc, and a disabled control.
const sampleDump = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
 <node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="com.example.app" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,0][1080,2400]">
  <node index="0" text="" resource-id="android:id/content" class="android.widget.LinearLayout" package="com.example.app" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,100][1080,2400]">
   <node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="com.example.app" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,100][1080,300]">
    <node index="0" text="Sign in" resource-id="com.example.app:id/login_button" class="android.widget.Button" package="com.example.app" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[40,140][1040,260]" />
   </node>
   <node index="1" text="" resource-id="" class="androidx.appcompat.widget.AppCompatImageButton" package="com.example.app" content-desc="Settings" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[900,300][1020,420]" />
   <node index="2" text="Continue" resource-id="" class="android.widget.Button" package="com.example.app" content-desc="" checkable="false" checked="false" clickable="true" enabled="false" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[40,500][1040,620]" />
  </node>
 </node>
</hierarchy>`

func TestParseUIReadsGeometryAndLabels(t *testing.T) {
	ui, err := ParseUI([]byte(sampleDump))
	if err != nil {
		t.Fatalf("ParseUI: %v", err)
	}
	if ui.Width != 1080 || ui.Height != 2400 {
		t.Errorf("screen = %dx%d, want 1080x2400", ui.Width, ui.Height)
	}

	btn := find(ui.Root, func(n *Node) bool { return n.ID == "com.example.app:id/login_button" })
	if btn == nil {
		t.Fatal("login button not found; resource-id is how an agent addresses a view")
	}
	if btn.Text != "Sign in" {
		t.Errorf("text = %q, want %q", btn.Text, "Sign in")
	}
	if btn.Class != "Button" {
		t.Errorf("class = %q, want the last segment %q", btn.Class, "Button")
	}
	if !btn.Clickable {
		t.Error("clickable = false, want true")
	}
	if btn.Bounds != [4]int{40, 140, 1040, 260} {
		t.Errorf("bounds = %v, want [40 140 1040 260]", btn.Bounds)
	}
	// The tap point is the whole reason this endpoint exists.
	if btn.Center != [2]int{540, 200} {
		t.Errorf("center = %v, want [540 200]", btn.Center)
	}
}

func TestParseUIKeepsContentDescAndDisabledState(t *testing.T) {
	ui, err := ParseUI([]byte(sampleDump))
	if err != nil {
		t.Fatalf("ParseUI: %v", err)
	}

	icon := find(ui.Root, func(n *Node) bool { return n.Desc == "Settings" })
	if icon == nil {
		t.Fatal("icon button not found; content-desc is often an icon's only label")
	}
	if icon.Text != "" {
		t.Errorf("text = %q, want empty", icon.Text)
	}

	cont := find(ui.Root, func(n *Node) bool { return n.Text == "Continue" })
	if cont == nil {
		t.Fatal("Continue not found")
	}
	// Reporting a disabled control as tappable is the failure that wastes an
	// agent's turn: the tap lands and nothing happens.
	if !cont.Disabled {
		t.Error("Disabled = false on enabled=\"false\"")
	}
}

func TestPruneDropsLayoutButKeepsGrouping(t *testing.T) {
	ui, err := ParseUI([]byte(sampleDump))
	if err != nil {
		t.Fatalf("ParseUI: %v", err)
	}
	roots := Prune(ui.Root)

	var dull int
	for _, r := range roots {
		walk(r, func(n *Node) {
			if !n.Interesting() && len(n.Children) < 2 {
				dull++
			}
		})
	}
	if dull != 0 {
		t.Errorf("%d uninteresting single-child nodes survived pruning", dull)
	}

	// The unlabelled FrameLayout wrapping the button has one kept child, so it
	// collapses; the LinearLayout has three and must not.
	if got := countNodes(roots); got != 4 {
		t.Errorf("pruned to %d nodes, want 4 (content group + 3 controls)", got)
	}
	for _, want := range []string{"Sign in", "Continue"} {
		found := false
		for _, r := range roots {
			walk(r, func(n *Node) {
				if n.Text == want {
					found = true
				}
			})
		}
		if !found {
			t.Errorf("pruning lost %q", want)
		}
	}
}

func TestPruneDoesNotMutateTheSourceTree(t *testing.T) {
	ui, err := ParseUI([]byte(sampleDump))
	if err != nil {
		t.Fatalf("ParseUI: %v", err)
	}
	before := countNodes([]*Node{ui.Root})
	Prune(ui.Root)
	if after := countNodes([]*Node{ui.Root}); after != before {
		t.Errorf("full tree went from %d to %d nodes; Prune must copy, because "+
			"the same parse serves both the full and the pruned response", before, after)
	}
}

func TestParseUIRejectsAnEmptyHierarchy(t *testing.T) {
	// What a locked or screen-off device produces.
	if _, err := ParseUI([]byte(`<hierarchy rotation="0" />`)); err == nil {
		t.Fatal("want an error for an empty hierarchy, got nil")
	}
}

// The three fixtures below back the password-redaction tests in
// internal/httpapi/agent_test.go, which need ParseUI (and FocusedAll) to read
// the "password" and "focused" attributes correctly off real uiautomator
// output rather than off the hand-trimmed sampleDump above.
//
// testdata/password_field_focused.xml is a real dump captured from
// emulator-5554: Settings > Network & internet > Internet > Add network,
// with Security set to WPA/WPA2-Personal and the Password field tapped so it
// carries focus. ACME00000000001 was never touched.
//
// testdata/text_field_focused.xml and testdata/no_node_focused.xml are hand
// edits of that same capture: the first flips the focused node's
// password="true" to "false" and renames its resource-id/hint so it reads as
// an ordinary text field; the second flips that node's focused="true" to
// "false" so nothing in the dump is focused at all. Neither required a
// second device session.

func TestParseUIReadsThePasswordAttribute(t *testing.T) {
	raw, err := os.ReadFile("testdata/password_field_focused.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ui, err := ParseUI(raw)
	if err != nil {
		t.Fatalf("ParseUI: %v", err)
	}
	nodes := FocusedAll(ui)
	if len(nodes) != 1 {
		t.Fatalf("FocusedAll = %d nodes; want 1", len(nodes))
	}
	n := nodes[0]
	if !n.Password {
		t.Errorf("Password = false on the focused field; want true")
	}
	if n.ID != "com.android.settings:id/password" {
		t.Errorf("ID = %q, want the password field's resource-id", n.ID)
	}
}

func TestParseUITreatsAnOrdinaryFocusedFieldAsNotPassword(t *testing.T) {
	raw, err := os.ReadFile("testdata/text_field_focused.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ui, err := ParseUI(raw)
	if err != nil {
		t.Fatalf("ParseUI: %v", err)
	}
	nodes := FocusedAll(ui)
	if len(nodes) != 1 {
		t.Fatalf("FocusedAll = %d nodes; want 1", len(nodes))
	}
	if nodes[0].Password {
		t.Errorf("Password = true on an ordinary text field; want false")
	}
}

func TestFocusedAllReportsEmptyWhenNothingIsFocused(t *testing.T) {
	raw, err := os.ReadFile("testdata/no_node_focused.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ui, err := ParseUI(raw)
	if err != nil {
		t.Fatalf("ParseUI: %v", err)
	}
	if nodes := FocusedAll(ui); len(nodes) != 0 {
		t.Errorf("FocusedAll = %+v, want none", nodes)
	}
}

// TestFocusedAllReturnsEveryFocusedNodeAncestorDescendant covers a host that
// marks itself focused above the leaf actually receiving keystrokes -- a
// WebView or a Compose host is the plausible real case. Both document orders
// are checked: a caller reading only the first focused node in document
// order, or only the deepest, must not silently drop the other one.
func TestFocusedAllReturnsEveryFocusedNodeAncestorDescendant(t *testing.T) {
	const dump = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
 <node index="0" text="" resource-id="host" class="android.webkit.WebView" package="com.example.app" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="true" focused="true" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,0][1080,2400]">
  <node index="0" text="" resource-id="leaf" class="android.widget.EditText" package="com.example.app" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="true" scrollable="false" long-clickable="false" password="true" selected="false" bounds="[40,140][1040,260]" />
 </node>
</hierarchy>`
	ui, err := ParseUI([]byte(dump))
	if err != nil {
		t.Fatalf("ParseUI: %v", err)
	}
	nodes := FocusedAll(ui)
	if len(nodes) != 2 {
		t.Fatalf("FocusedAll = %d nodes; want 2 (host and leaf)", len(nodes))
	}
	var sawHost, sawLeafPassword bool
	for _, n := range nodes {
		if n.ID == "host" && !n.Password {
			sawHost = true
		}
		if n.ID == "leaf" && n.Password {
			sawLeafPassword = true
		}
	}
	if !sawHost || !sawLeafPassword {
		t.Fatalf("FocusedAll = %+v; want both the non-password host and the password leaf", nodes)
	}
}

// TestFocusedAllReturnsEveryFocusedNodeSiblingWindows covers a dialog over an
// activity, where uiautomator reports one focused node per window. The
// non-password window is listed first here; the redaction test in
// internal/httpapi/agent_test.go covers the reverse order.
func TestFocusedAllReturnsEveryFocusedNodeSiblingWindows(t *testing.T) {
	const dump = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
 <node index="0" text="" resource-id="behind" class="android.widget.EditText" package="com.example.app" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="true" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,0][1080,200]" />
 <node index="1" text="" resource-id="dialog_password" class="android.widget.EditText" package="com.example.app" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="true" scrollable="false" long-clickable="false" password="true" selected="false" bounds="[100,900][980,1020]" />
</hierarchy>`
	ui, err := ParseUI([]byte(dump))
	if err != nil {
		t.Fatalf("ParseUI: %v", err)
	}
	nodes := FocusedAll(ui)
	if len(nodes) != 2 {
		t.Fatalf("FocusedAll = %d nodes; want 2 (one per window)", len(nodes))
	}
	var sawPassword bool
	for _, n := range nodes {
		if n.ID == "dialog_password" && n.Password {
			sawPassword = true
		}
	}
	if !sawPassword {
		t.Fatalf("FocusedAll = %+v; want the dialog's password field included", nodes)
	}
}

func TestParseBounds(t *testing.T) {
	if got := parseBounds("[0,100][1080,2400]"); got != [4]int{0, 100, 1080, 2400} {
		t.Errorf("parseBounds = %v", got)
	}
	if got := parseBounds("garbage"); got != [4]int{} {
		t.Errorf("parseBounds(garbage) = %v, want zero", got)
	}
}

func find(n *Node, pred func(*Node) bool) *Node {
	if n == nil {
		return nil
	}
	if pred(n) {
		return n
	}
	for _, c := range n.Children {
		if got := find(c, pred); got != nil {
			return got
		}
	}
	return nil
}

func walk(n *Node, fn func(*Node)) {
	if n == nil {
		return
	}
	fn(n)
	for _, c := range n.Children {
		walk(c, fn)
	}
}

func countNodes(ns []*Node) int {
	total := 0
	for _, n := range ns {
		walk(n, func(*Node) { total++ })
	}
	return total
}

// The crash buffer for a dump refused while a Maestro driver held
// emulator-5554, captured with `logcat -d -b crash --pid=`. Nothing reached
// stdout or stderr; adb exited 137.
func TestDumpCrashIsUIAutomationConflict(t *testing.T) {
	crash, err := os.ReadFile("testdata/uiautomator-dump-conflict.crash.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !dumpCrashIsUIAutomationConflict(string(crash)) {
		t.Fatal("captured UiAutomation conflict not recognised")
	}

	other := `09-24 22:32:24.264 25170 25170 E AndroidRuntime: FATAL EXCEPTION: main
09-24 22:32:24.264 25170 25170 E AndroidRuntime: java.lang.IllegalStateException: Receiver already registered!`
	for name, log := range map[string]string{"empty": "", "other crash": other} {
		if dumpCrashIsUIAutomationConflict(log) {
			t.Errorf("%s: read as a UiAutomation conflict", name)
		}
	}
}

func TestSplitDumpPID(t *testing.T) {
	cases := []struct {
		name, out, rest string
		pid             int
	}{
		// adb shell over a pty ends lines with \r\n.
		{"dumped", "loupe-pid=25170\r\nUI hierchary dumped to: /data/local/tmp/loupe-ui.xml\r\n",
			"UI hierchary dumped to: /data/local/tmp/loupe-ui.xml\r\n", 25170},
		{"killed with no output", "loupe-pid=25170\n", "", 25170},
		{"no pid line", "sh: exec: not found\n", "sh: exec: not found\n", 0},
		{"empty", "", "", 0},
	}
	for _, c := range cases {
		pid, rest := splitDumpPID(c.out)
		if pid != c.pid || rest != c.rest {
			t.Errorf("%s: got (%d, %q), want (%d, %q)", c.name, pid, rest, c.pid, c.rest)
		}
	}
}
