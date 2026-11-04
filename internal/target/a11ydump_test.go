package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const a11yXML = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><hierarchy rotation="0">` +
	`<node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="p" content-desc="" bounds="[0,0][1080,2400]">` +
	`<node index="0" text="" resource-id="p:id/list" class="android.widget.ScrollView" package="p" content-desc="" scrollable="true" scroll-backward="false" scroll-forward="true" bounds="[0,200][1080,2400]">` +
	`<node index="0" text="Network" resource-id="" class="android.widget.TextView" package="p" content-desc="" bounds="[0,200][1080,400]" />` +
	`</node></node></hierarchy>`

const uiautomatorXML = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><hierarchy rotation="0">` +
	`<node index="0" text="" resource-id="p:id/list" class="android.widget.ScrollView" package="p" content-desc="" scrollable="true" bounds="[0,0][1080,2400]" />` +
	`</hierarchy>`

// fakeA11yADB answers the a11y dump with dumpScript (shell lines run in
// place of app_process) and uiautomator with uiautomatorXML, and logs every
// call to the returned file.
func fakeA11yADB(t *testing.T, dumpScript string) (adb, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	for name, body := range map[string]string{"a11y.xml": a11yXML, "ui.xml": uiautomatorXML} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	adb = filepath.Join(dir, "fake-adb")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + calls + "\n" +
		"case \"$*\" in\n" +
		"*' shell ls '*) echo \"$5\" ;;\n" +
		"*app_process*) " + strings.ReplaceAll(dumpScript, "$DIR", dir) + " ;;\n" +
		"*'uiautomator dump'*) echo loupe-pid=1; echo 'UI hierchary dumped to: " + dumpPath + "' ;;\n" +
		"*'cat " + dumpPath + "'*) cat " + dir + "/ui.xml ;;\n" +
		"esac\n"
	if err := os.WriteFile(adb, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return adb, calls
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInspectReadsScrollActionsFromTheA11yDump(t *testing.T) {
	adb, calls := fakeA11yADB(t, "cat $DIR/a11y.xml")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	ui, err := tgt.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	list := find(ui.Root, func(n *Node) bool { return n.Scrollable })
	if list == nil || list.ScrollBackward == nil || list.ScrollForward == nil {
		t.Fatalf("scrollable node = %+v; want both scroll actions set", list)
	}
	if *list.ScrollBackward || !*list.ScrollForward {
		t.Fatalf("backward = %v, forward = %v; want false, true (a list at its top)", *list.ScrollBackward, *list.ScrollForward)
	}
	if c := readCalls(t, calls); strings.Contains(c, "uiautomator") {
		t.Fatalf("uiautomator ran although the a11y dump answered:\n%s", c)
	}
}

func TestInspectReportsBusyWithoutFallingBack(t *testing.T) {
	adb, calls := fakeA11yADB(t, "echo busy; exit 3")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	_, err := tgt.Inspect(context.Background())
	if !errors.Is(err, ErrUIAutomationBusy) {
		t.Fatalf("err = %v; want ErrUIAutomationBusy", err)
	}
	// uiautomator would only be refused too, after two more seconds.
	if c := readCalls(t, calls); strings.Contains(c, "uiautomator") {
		t.Fatalf("fell back to uiautomator on busy:\n%s", c)
	}
}

func TestInspectFallsBackToUIAutomatorWhenTheA11yDumpFails(t *testing.T) {
	adb, _ := fakeA11yADB(t, "echo 'java.lang.ClassNotFoundException' >&2; exit 1")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	ui, err := tgt.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ui.Root.ID != "p:id/list" || ui.Root.ScrollBackward != nil {
		t.Fatalf("root = %+v; want uiautomator's tree, with no scroll actions", ui.Root)
	}
}

func TestInspectSkipsTheA11yDumpWhenTurnedOff(t *testing.T) {
	t.Setenv("LOUPE_A11Y_DUMP", "0")
	adb, calls := fakeA11yADB(t, "cat $DIR/a11y.xml")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	if _, err := tgt.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := readCalls(t, calls); strings.Contains(c, "app_process") {
		t.Fatalf("a11y dump ran with LOUPE_A11Y_DUMP=0:\n%s", c)
	}
}

func TestInspectFallsBackWhenTheA11yDumpHangs(t *testing.T) {
	old := a11yDumpTimeout
	a11yDumpTimeout = 200 * time.Millisecond
	t.Cleanup(func() { a11yDumpTimeout = old })
	adb, calls := fakeA11yADB(t, "exec sleep 5")
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	start := time.Now()
	ui, err := tgt.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ui.Root.ID != "p:id/list" {
		t.Fatalf("root = %+v; want uiautomator's tree", ui.Root)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Inspect took %s; want it bounded by the dump timeout", d)
	}
	if !strings.Contains(readCalls(t, calls), "pkill") {
		t.Fatal("want the stuck helper killed before falling back")
	}
}
