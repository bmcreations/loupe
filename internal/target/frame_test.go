package target

import (
	"os"
	"path/filepath"
	"testing"
)

// pixel10Layout is the layout file the SDK installs at skins/pixel_10, copied
// verbatim. Every figure the tests assert comes from here or from a device,
// never from a constant in the code under test.
const pixel10Layout = `parts {
  device {
    display {
      width 1080
      height 2424
      x 0
      y 0
      corner_radius 87
    }
  }
  portrait {
    background {
      image back.webp
    }
    foreground {
      mask mask.webp
      cutout hole
    }
  }
}
layouts {
  portrait {
    width 1198
    height 2531
    event EV_SW:0:1
    part1 {
      name portrait
      x 0
      y 0
    }
    part2 {
      name device
      x 59
      y 55
    }
  }
}
`

func TestASkinLayoutYieldsTheBezelTheSDKDescribes(t *testing.T) {
	node, err := parseSkinLayout(pixel10Layout)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := node.frame()
	if f == nil {
		t.Fatal("no frame from a layout that describes one")
	}

	if f.Source != "skin" {
		t.Errorf("Source = %q, want skin", f.Source)
	}
	if f.Width != 1198 || f.Height != 2531 {
		t.Errorf("frame = %dx%d, want 1198x2531", f.Width, f.Height)
	}
	if f.Display != (FrameRect{X: 59, Y: 55, Width: 1080, Height: 2424}) {
		t.Errorf("display = %+v, want {59 55 1080 2424}", f.Display)
	}
	if f.CornerRadius != 87 {
		t.Errorf("CornerRadius = %d, want 87", f.CornerRadius)
	}
	if f.Background != "back.webp" || f.Mask != "mask.webp" {
		t.Errorf("assets = %q/%q, want back.webp/mask.webp", f.Background, f.Mask)
	}

	// The display has to sit inside the bezel with room on every side, which
	// is the one relationship that makes the numbers a frame rather than four
	// unrelated integers.
	if f.Display.X <= 0 || f.Display.Y <= 0 {
		t.Errorf("display is flush with the top-left corner: %+v", f.Display)
	}
	if right := f.Display.X + f.Display.Width; right >= f.Width {
		t.Errorf("display right edge %d is outside the frame width %d", right, f.Width)
	}
	if bottom := f.Display.Y + f.Display.Height; bottom >= f.Height {
		t.Errorf("display bottom edge %d is outside the frame height %d", bottom, f.Height)
	}
}

// The device part is found by name because its block number is not fixed. A
// skin that numbers it differently has to parse the same.
func TestTheDisplayOffsetIsFoundByNameRatherThanByBlockNumber(t *testing.T) {
	renumbered := `parts {
  device {
    display { width 1080
      height 2424
    }
  }
}
layouts {
  portrait {
    width 1198
    height 2531
    part7 {
      name device
      x 59
      y 55
    }
  }
}
`
	node, err := parseSkinLayout(renumbered)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := node.frame()
	if f == nil {
		t.Fatal("no frame when the device part is part7")
	}
	if f.Display.X != 59 || f.Display.Y != 55 {
		t.Errorf("display offset = %d,%d, want 59,55", f.Display.X, f.Display.Y)
	}
}

func TestALayoutWithNoDeviceDisplayYieldsNoFrame(t *testing.T) {
	for name, src := range map[string]string{
		"no display block": "layouts {\n portrait {\n width 1198\n height 2531\n part1 {\n name device\n x 59\n y 55\n }\n }\n}\n",
		"no portrait layout": `parts {
  device {
    display {
      width 1080
      height 2424
    }
  }
}
`,
		"no device part": `parts {
  device {
    display {
      width 1080
      height 2424
    }
  }
}
layouts {
  portrait {
    width 1198
    height 2531
    part1 {
      name portrait
      x 0
      y 0
    }
  }
}
`,
	} {
		node, err := parseSkinLayout(src)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if f := node.frame(); f != nil {
			t.Errorf("%s: got a frame %+v, want none", name, f)
		}
	}
}

func TestAnUnbalancedSkinLayoutIsRejected(t *testing.T) {
	for name, src := range map[string]string{
		"closes too often": "parts {\n}\n}\n",
		"never closes":     "parts {\n  device {\n",
		"unnamed block":    "{\n}\n",
	} {
		if _, err := parseSkinLayout(src); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

// Measured on emulator-5554: 1080x2424 at density 420, a punch-hole anchored
// left. The path is in pixels already, so nothing scales.
func TestAPixelAnchoredCutoutIsPlacedWithoutScaling(t *testing.T) {
	const spec = "M 128,83 A 44,44 0 0 1 84,127 44,44 0 0 1 40,83 44,44 0 0 1 84,39 44,44 0 0 1 128,83 Z @left"

	c := parseCutoutSpec(spec, 1080, 420)
	if c == nil {
		t.Fatal("no cutout from a spec that carries one")
	}
	if c.Scale != 1 {
		t.Errorf("Scale = %v, want 1 with no @dp", c.Scale)
	}
	if c.DX != 0 {
		t.Errorf("DX = %d, want 0 for @left", c.DX)
	}
	if want := "M 128,83 A 44,44 0 0 1 84,127 44,44 0 0 1 40,83 44,44 0 0 1 84,39 44,44 0 0 1 128,83 Z"; c.Path != want {
		t.Errorf("Path = %q, want the spec without its suffix", c.Path)
	}

	// The hole is centred at x=84 r=44 on the device, so it has to land inside
	// the left half of a 1080-wide display and clear of the edge.
	if left := c.DX + int(40*c.Scale); left <= 0 || left >= 540 {
		t.Errorf("hole left edge lands at %d, off the top-left of a 1080 display", left)
	}
}

// Measured on ACME00000000001: 1200x2670 at density 480, a 40x26dp notch with
// no anchor, so it is centred and scaled by 480/160 = 3.
func TestADpCutoutScalesByDensityAndCentres(t *testing.T) {
	const spec = "M 0,0 L -20, 0 L -20, 26 L 20, 26 L 20, 0 Z @dp"

	c := parseCutoutSpec(spec, 1200, 480)
	if c == nil {
		t.Fatal("no cutout from a spec that carries one")
	}
	if c.Scale != 3 {
		t.Errorf("Scale = %v, want 3 at density 480", c.Scale)
	}

	// What matters is where the notch ends up, not the two factors separately.
	// 40dp wide at density 480 is 120px, centred on a 1200px display, so the
	// notch spans 540..660 and is 78px deep.
	left := c.DX + int(-20*c.Scale)
	right := c.DX + int(20*c.Scale)
	depth := int(26 * c.Scale)
	if left != 540 || right != 660 {
		t.Errorf("notch spans %d..%d, want 540..660", left, right)
	}
	if depth != 78 {
		t.Errorf("notch depth = %d, want 78", depth)
	}
	if mid := (left + right) / 2; mid != 600 {
		t.Errorf("notch centre = %d, want the display centre 600", mid)
	}
}

func TestACutoutAnchoredRightEndsAtTheDisplayEdge(t *testing.T) {
	c := parseCutoutSpec("M 0,0 L -20,0 L -20,26 L 0,26 Z @dp @right", 1200, 480)
	if c == nil {
		t.Fatal("no cutout")
	}
	if c.DX != 1200 {
		t.Errorf("DX = %d, want the display width 1200 for @right", c.DX)
	}
}

func TestACutoutWithNoPathIsNotDrawn(t *testing.T) {
	for _, spec := range []string{"", "   ", "@dp", "@left"} {
		if c := parseCutoutSpec(spec, 1080, 420); c != nil {
			t.Errorf("spec %q gave a cutout %+v, want none", spec, c)
		}
	}
}

// A density the device would not report must not scale the path to nothing.
func TestAnImplausibleDensityFallsBackToTheFrameworkBaseline(t *testing.T) {
	for _, density := range []int{0, -1} {
		c := parseCutoutSpec("M 0,0 L 10,0 Z @dp", 1080, density)
		if c == nil {
			t.Fatalf("density %d: no cutout", density)
		}
		if c.Scale != 1 {
			t.Errorf("density %d: Scale = %v, want 1 from the 160dpi baseline", density, c.Scale)
		}
	}
}

// The asset endpoint's only guard. A name the skin did not declare must be
// refused whatever it looks like.
func TestAFrameOnlyServesTheAssetsItsSkinDeclared(t *testing.T) {
	f := &DeviceFrame{Background: "back.webp", Mask: "mask.webp"}

	for _, name := range []string{"back.webp", "mask.webp"} {
		if !f.declaresAsset(name) {
			t.Errorf("refused %q, which the skin declares", name)
		}
	}
	for _, name := range []string{
		"", "layout", "back.webp ", "./back.webp", "back.webp/../layout",
		"../../../../etc/passwd", "/etc/passwd", "..", "BACK.WEBP",
	} {
		if f.declaresAsset(name) {
			t.Errorf("served %q, which the skin does not declare", name)
		}
	}

	// A derived frame declares no assets at all, so it serves none.
	derived := &DeviceFrame{Source: "derived"}
	for _, name := range []string{"", "back.webp", "mask.webp"} {
		if derived.declaresAsset(name) {
			t.Errorf("a derived frame served %q", name)
		}
	}
}

func TestAnAVDWithNoSkinIsNotTreatedAsHavingOne(t *testing.T) {
	dir := t.TempDir()

	write := func(body string) string {
		path := filepath.Join(dir, "config.ini")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	for name, body := range map[string]string{
		"no skin at all":  "skin.path=_no_skin\nhw.lcd.density=420\n",
		"empty":           "skin.path=\n",
		"relative to sdk": "skin.path=pixel_10\n",
		"absent":          "hw.lcd.density=420\n",
	} {
		if got, err := skinPathFromConfig(write(body)); err == nil {
			t.Errorf("%s: resolved to %q, want an error", name, got)
		}
	}

	if got, err := skinPathFromConfig(write("skin.path = /Users/x/sdk/skins/pixel_10\n")); err != nil {
		t.Errorf("absolute skin path: %v", err)
	} else if got != "/Users/x/sdk/skins/pixel_10" {
		t.Errorf("skin path = %q, want the absolute path with whitespace trimmed", got)
	}

	if _, err := skinPathFromConfig(filepath.Join(dir, "missing.ini")); err == nil {
		t.Error("a missing config.ini resolved to a skin")
	}
}

// TestTheFirstNonEmptyCutoutSpecWins is built from a real Acme One dump, in which
// a blank display is reported ahead of the one carrying the notch. Reading the
// first match alone returns nothing and the frame loses its cutout.
func TestTheFirstNonEmptyCutoutSpecWins(t *testing.T) {
	const dump = `  Display 0:
      mDisplayCutout=DisplayCutout{cutoutPathParserInfo={CutoutPathParserInfo{displayWidth=0 cutoutSpec={} rotation={0}}}}
  Display 2:
      mDisplayCutout=DisplayCutout{cutoutPathParserInfo={CutoutPathParserInfo{displayWidth=1200 cutoutSpec={M 0,0 L -20, 0 L -20, 26 L 20, 26 L 20, 0 Z @dp} rotation={0}}}}
`
	spec, ok := firstCutoutSpec(dump)
	if !ok {
		t.Fatal("no cutout found in a dump that declares one")
	}
	if want := "M 0,0 L -20, 0 L -20, 26 L 20, 26 L 20, 0 Z @dp"; spec != want {
		t.Fatalf("spec = %q, want %q", spec, want)
	}
}

// TestADumpWithNoCutoutReportsNone keeps the other direction: a device that
// genuinely declares nothing must not be given a cutout.
func TestADumpWithNoCutoutReportsNone(t *testing.T) {
	for _, dump := range []string{
		"",
		"cutoutSpec={}",
		"cutoutSpec={}\ncutoutSpec={   }\n",
		"mDisplayCutout=DisplayCutout{insets=Rect(0, 0 - 0, 0)}",
	} {
		if spec, ok := firstCutoutSpec(dump); ok {
			t.Fatalf("dump %q yielded cutout %q, want none", dump, spec)
		}
	}
}
