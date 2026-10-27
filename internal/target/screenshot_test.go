package target

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Trimmed from Hinge_Pixel_Fold half opened: the inner panel lit, the outer off.
const foldDumpsysDisplay = `  DisplayDeviceInfo{"Built-in Screen": uniqueId="local:4619827259835644672", 2208 x 1840, modeId 1, touch INTERNAL, rotation 0, type INTERNAL, state ON, committedState ON, frameRateOverride }
    mPhysicalDisplayId=4619827259835644672
  DisplayDeviceInfo{"Built-in Screen": uniqueId="local:4619827551948147201", 1080 x 2092, modeId 2, touch INTERNAL, rotation 0, type INTERNAL, state OFF, committedState OFF, frameRateOverride }
    mPhysicalDisplayId=4619827551948147201
`

func TestLitPhysicalDisplayPicksThePanelThatIsOn(t *testing.T) {
	id, ok := litPhysicalDisplay(foldDumpsysDisplay)
	if !ok || id != "4619827259835644672" {
		t.Fatalf("got %q, %v; want the inner panel", id, ok)
	}
	folded := strings.NewReplacer("state ON, committedState ON", "state X", "state OFF, committedState OFF", "state ON, committedState ON").
		Replace(foldDumpsysDisplay)
	if id, ok := litPhysicalDisplay(folded); !ok || id != "4619827551948147201" {
		t.Fatalf("folded: got %q, %v; want the outer panel", id, ok)
	}
	both := strings.ReplaceAll(foldDumpsysDisplay, "state OFF", "state ON")
	if _, ok := litPhysicalDisplay(both); ok {
		t.Error("two lit panels gave an answer; want none")
	}
}

func TestScreenshotOnAFoldableNamesTheLitDisplay(t *testing.T) {
	adb, argv := fakeADB(t, `case "$*" in
*"screencap -d 4619827259835644672 -p"*) printf '\211PNG\r\n\032\nbody' ;;
*"screencap -p"*) echo '[Warning] Multiple displays were found, but no display id was specified!' ;;
*"dumpsys display"*) cat <<'X'
`+foldDumpsysDisplay+`X
;;
esac`)
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "emulator-5556", State: "device"}, "", "")
	png, err := tgt.Screenshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(png, pngMagic) {
		t.Fatalf("got %q; want the PNG from the lit display", png)
	}
	if calls := readArgv(t, argv); len(calls) != 3 {
		t.Errorf("adb calls = %q; want screencap, dumpsys, screencap -d", calls)
	}
}
