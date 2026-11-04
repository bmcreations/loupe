package target

import (
	"os"
	"testing"
)

// A trimmed dumpsys window displays from emulator-5554 (API 36, 1080x2400):
// the decoy display 18 prints first with its own frames, then display 0.
const statusBarDump = `WINDOW MANAGER DISPLAY CONTENTS (dumpsys window displays)
  Display: mDisplayId=18
  DisplayFrames w=648 h=1440 r=0
        InsetsSource id=1 type=statusBars frame=[0,0][648,40] visible=true
  Display: mDisplayId=0 (organized)
  DisplayFrames w=1080 h=2400 r=0
        InsetsSource id=25200000 type=statusBars frame=[0,0][1080,63] visible=true flags= sideHint=TOP boundingRects=null
        mSource=InsetsSource id=25200000 type=statusBars frame=[0,0][1080,63] visible=true
`

func TestStatusBarOfReadsDisplayZero(t *testing.T) {
	bar, h, ok := statusBarOf(statusBarDump)
	if !ok || bar != 63 || h != 2400 {
		t.Fatalf("statusBarOf() = %d, %d, %v; want display 0's 63, 2400, not the decoy's 40, 1440", bar, h, ok)
	}
}

func TestStatusBarOfReadsTheRotatedHeight(t *testing.T) {
	dump := `  Display: mDisplayId=0 (organized)
  DisplayFrames w=2400 h=1080 r=1
        InsetsSource id=25200000 type=statusBars frame=[0,0][2400,63] visible=true
`
	bar, h, ok := statusBarOf(dump)
	if !ok || bar != 63 || h != 1080 {
		t.Fatalf("statusBarOf() = %d, %d, %v; want 63, 1080", bar, h, ok)
	}
}

func TestStatusBarOfSkipsABarNotOnTheTopEdge(t *testing.T) {
	dump := `  Display: mDisplayId=0 (organized)
  DisplayFrames w=1080 h=2400 r=0
        InsetsSource id=1 type=statusBars frame=[0,2337][1080,2400] visible=true
        InsetsSource id=2 type=statusBars frame=[0,0][1080,84] visible=true
`
	bar, _, ok := statusBarOf(dump)
	if !ok || bar != 84 {
		t.Fatalf("statusBarOf() = %d, %v; want the top-edge bar, 84", bar, ok)
	}
}

func TestStatusBarOfReportsNothingBeforeInsetsSource(t *testing.T) {
	dump := `  Display: mDisplayId=0 (organized)
  DisplayFrames w=1080 h=1920 r=0
`
	if _, _, ok := statusBarOf(dump); ok {
		t.Fatal("statusBarOf() ok on a dump with no statusBars source")
	}
}

func TestStatusBarOfReportsNoDisplayZero(t *testing.T) {
	if _, _, ok := statusBarOf("  Display: mDisplayId=18\n  DisplayFrames w=648 h=1440 r=0\n"); ok {
		t.Fatal("statusBarOf() ok with no display 0")
	}
}

// The whole of dumpsys window displays from emulator-5554 (API 36,
// 1080x2400, 420dpi), captured 2026-11-04.
func TestStatusBarOfReadsACapturedDump(t *testing.T) {
	dump, err := os.ReadFile("testdata/dumpsys-window-displays-api36.txt")
	if err != nil {
		t.Fatal(err)
	}
	bar, h, ok := statusBarOf(string(dump))
	if !ok || bar != 63 || h != 2400 {
		t.Fatalf("statusBarOf() = %d, %d, %v; want 63, 2400", bar, h, ok)
	}
}
