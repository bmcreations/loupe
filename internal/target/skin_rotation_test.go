package target

import (
	"fmt"
	"testing"
)

// The three layouts below are the SDK's own files, copied verbatim from
// ~/Library/Android/sdk/skins. They are the whole reason the parser is shaped
// the way it is, so a paraphrase would test nothing.

// pixel_10 and every Pixel 3 and later declare one layout and no landscape
// anywhere in the file; frame_test.go holds that one as pixel10Layout.

// pixel_2, a legacy skin: art per orientation, and a landscape layout that
// places the display on its side with "rotation 3".
const pixel2Layout = `
parts {
  device {
    display {
      width 1080
      height 1920
      x 0
      y 0
    }
  }
  portrait {
    background {
      image port_back.webp
    }
    onion {
      image port_fore.webp
    }
  }
  landscape {
    background {
      image land_back.webp
    }
    onion {
      image land_fore.webp
    }
  }
}
layouts {
  portrait {
    width 1370
    height 2534
    event EV_SW:0:1
    part1 {
      name portrait
      x 0
      y 0
    }
    part2 {
      name device
      x 140
      y 280
    }
  }
  landscape {
    width 2596
    height 1258
    event EV_SW:0:0
    part1 {
      name landscape
      x 0
      y 0
    }
    part2 {
      name device
      x 338
      y 68
      rotation 3
    }
  }
}
`

// pixel_tablet: a naturally landscape device whose only layout is named
// "landscape" and whose art is in a part named "portrait".
const pixelTabletLayout = `
parts {
  device {
    display {
      width 2560
      height 1600
      x 0
      y 0
    }
  }
  portrait {
    background {
      image back.webp
    }
    foreground {
      mask mask.webp
    }
  }
}
layouts {
  landscape {
    width 2798
    height 1837
    event EV_SW:0:1
    part1 {
      name portrait
      x 0
      y 0
    }
    part2 {
      name device
      x 119
      y 117
    }
  }
}
`

func parseLayoutOrFail(t *testing.T, src string) *skinNode {
	t.Helper()
	node, err := parseSkinLayout(src)
	if err != nil {
		t.Fatalf("parse layout: %v", err)
	}
	return node
}

func (f *DeviceFrame) describe() string {
	if f == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%dx%d display=(%d,%d %dx%d) rotation=%d bg=%s mask=%s",
		f.Width, f.Height, f.Display.X, f.Display.Y, f.Display.Width, f.Display.Height,
		f.Rotation, f.Background, f.Mask)
}

// The bezel a device gets at each rotation, for the skin this is actually used
// with. pixel_10 declares no landscape layout, so every one of these but the
// first is produced by turning the portrait one.
//
// The turn direction is the part that cannot be reasoned out from the stream:
// a rotated encoder reports 1080x480 whichever way the device went. It is
// pinned against Android's own account of where the punch hole ends up. At
// ROTATION_90 emulator-5554 reports
//
//	mDisplayCutout insets=Rect(136, 0 - 0, 0) boundingRect Rect(0, 944 - 136, 1080)
//
// in a 2424x1080 space -- the hole is bottom-left, where turning portrait art
// anticlockwise puts it. Turning it the other way puts it top-right.
//
// These display rects are turned about the 1198x2531 layout, and the client
// has to turn back.webp about the same size for its hole to land on them:
// web/orientation.test.mjs asserts the art hole against these exact rects.
// Changing one side means changing the other.
func TestTheBezelTurnsTheSameWayTheDeviceDid(t *testing.T) {
	skin := parseLayoutOrFail(t, pixel10Layout)

	for _, c := range []struct {
		rotation int
		want     string
	}{
		// Untouched: the frame the endpoint answered before any of this.
		{0, "1198x2531 display=(59,55 1080x2424) rotation=0 bg=back.webp mask=mask.webp"},
		// On its side. The 55 and 59 margins swap axes and stay put.
		{1, "2531x1198 display=(55,59 2424x1080) rotation=3 bg=back.webp mask=mask.webp"},
		// Upside down. The portrait layout still fits the stream, so nothing
		// but the art has to turn -- but it does have to turn.
		{2, "1198x2531 display=(59,52 1080x2424) rotation=2 bg=back.webp mask=mask.webp"},
		// The other side.
		{3, "2531x1198 display=(52,59 2424x1080) rotation=1 bg=back.webp mask=mask.webp"},
	} {
		got := skin.frameFor(c.rotation).describe()
		if got != c.want {
			t.Errorf("rotation %d:\n got %s\nwant %s", c.rotation, got, c.want)
		}
	}
}

// A turned frame is only worth anything if the screen still sits inside the
// bezel afterwards, which is the property trap 40 was about: the geometry was
// right there too and the art was placed against the wrong box.
func TestATurnedBezelStillContainsItsScreen(t *testing.T) {
	for name, src := range map[string]string{
		"pixel_10":     pixel10Layout,
		"pixel_2":      pixel2Layout,
		"pixel_tablet": pixelTabletLayout,
	} {
		skin := parseLayoutOrFail(t, src)
		dw, dh, ok := skin.displaySize()
		if !ok {
			t.Fatalf("%s: skin declares no display", name)
		}
		for rotation := 0; rotation < 4; rotation++ {
			f := skin.frameFor(rotation)
			if f == nil {
				t.Errorf("%s rotation %d: no frame", name, rotation)
				continue
			}
			d := f.Display
			if d.X < 0 || d.Y < 0 || d.X+d.Width > f.Width || d.Y+d.Height > f.Height {
				t.Errorf("%s rotation %d: screen escapes the bezel: %s", name, rotation, f.describe())
			}
			// The screen must also match the shape the stream will be, or the
			// client hides the frame and the turn bought nothing. That shape
			// is the device's own natural one, turned over on an odd
			// rotation -- a tablet's natural shape is already landscape.
			wantLandscape := landscape(dw, dh)
			if rotation%2 != 0 {
				wantLandscape = !wantLandscape
			}
			if landscape(d.Width, d.Height) != wantLandscape {
				t.Errorf("%s rotation %d: wrong shape for the stream: %s", name, rotation, f.describe())
			}
		}
	}
}

// A skin that draws its own landscape art is taken at its word rather than
// having its portrait art turned, because the two are not the same picture:
// pixel_2's landscape layout is 2596x1258 against a portrait 1370x2534, which
// is not that frame on its side.
func TestASkinWithItsOwnLandscapeArtUsesIt(t *testing.T) {
	skin := parseLayoutOrFail(t, pixel2Layout)

	portrait := skin.frameFor(0).describe()
	wantPortrait := "1370x2534 display=(140,280 1080x1920) rotation=0 bg=port_back.webp mask="
	if portrait != wantPortrait {
		t.Errorf("rotation 0:\n got %s\nwant %s", portrait, wantPortrait)
	}

	// "rotation 3" on the device part is the layout saying the screen lies on
	// its side inside this art, so the 1080x1920 display is placed as
	// 1920x1080. Reading the declared size straight through puts a portrait
	// screen in a landscape bezel.
	landscapeFrame := skin.frameFor(1).describe()
	wantLandscape := "2596x1258 display=(338,68 1920x1080) rotation=0 bg=land_back.webp mask="
	if landscapeFrame != wantLandscape {
		t.Errorf("rotation 1:\n got %s\nwant %s", landscapeFrame, wantLandscape)
	}
}

// A layout's block name is a label rather than a fixed key. pixel_tablet
// declares exactly one layout and calls it "landscape", and the art inside it
// sits in a part called "portrait".
//
// Looking up layouts.portrait by name, which is what this did before, finds
// nothing on this device and returns no frame in any orientation. Classifying
// by the shape of the display rect instead is what makes the lookup work here
// and on a skin that labels its layouts the other way round; this fixture only
// pins the first of those, because the SDK ships no skin with the second.
func TestALayoutNameIsNotAnOrientation(t *testing.T) {
	skin := parseLayoutOrFail(t, pixelTabletLayout)

	natural := skin.frameFor(0).describe()
	wantNatural := "2798x1837 display=(119,117 2560x1600) rotation=0 bg=back.webp mask=mask.webp"
	if natural != wantNatural {
		t.Errorf("rotation 0:\n got %s\nwant %s", natural, wantNatural)
	}

	// Turned onto its short edge, a tablet is the one that needs the turn.
	turned := skin.frameFor(1).describe()
	wantTurned := "1837x2798 display=(117,119 1600x2560) rotation=3 bg=back.webp mask=mask.webp"
	if turned != wantTurned {
		t.Errorf("rotation 1:\n got %s\nwant %s", turned, wantTurned)
	}
}

// The asset guard has to cover every layout, not the one frame being drawn.
// A skin with its own landscape art names a different file per orientation,
// and checking only the current frame refuses the other one -- which is the
// art the browser needs at the moment the device turns.
func TestTheAssetGuardCoversEveryLayout(t *testing.T) {
	skin := parseLayoutOrFail(t, pixel2Layout)

	for _, name := range []string{"port_back.webp", "land_back.webp"} {
		if !skin.declaresAsset(name) {
			t.Errorf("refused %q, which the skin declares", name)
		}
	}
	for _, name := range []string{
		"", "layout", "back.webp", "land_back.webp ", "./land_back.webp",
		"land_back.webp/../layout", "../../../../etc/passwd", "/etc/passwd",
		"..", "LAND_BACK.WEBP",
	} {
		if skin.declaresAsset(name) {
			t.Errorf("served %q, which the skin does not declare", name)
		}
	}
}

// Real output from `dumpsys window displays` on emulator-5554, trimmed to the
// lines that mention a rotation.
//
// Three things here look like the answer and are not, and the ordering is the
// one the device actually emits. mDisplayId=1084 comes first: a 1dpi 1080x480
// surface that stays at ROTATION_0 while the screen turns, so a parser reading
// the first rotation reports a portrait device however it is held. That is
// also the entry trap 42 caught reporting cur=1080x480 against /api/ui's
// 2424x1080, which is why that trap wrote the whole dump off.
func TestTheDisplayRotationIsReadOffDisplayZero(t *testing.T) {
	const dump = `  Display: mDisplayId=1084
    init=1080x480 1dpi mMinSizeOfResizeableTaskDp=220 cur=1080x480 app=1080x480 rng=480x480-1080x1080
  overrideConfig={0.0 ?mcc0mnc ?localeList ?layoutDir sw76800dp w172800dp h76800dp 1dpi xlrg long compactNeeded land ?uimode ?night finger qwerty/v/v dpad/v winConfig={ mBounds=Rect(0, 0 - 1080, 480) mAppBounds=Rect(0, 0 - 1080, 480) mMaxBounds=Rect(0, 0 - 1080, 480) mDisplayRotation=ROTATION_0 mWindowingMode=fullscreen mActivityType=undefined mAlwaysOnTop=undefined mRotation=ROTATION_0} ?fontWeightAdjustment}
    mLandscapeRotation=ROTATION_90 mSeascapeRotation=ROTATION_270
    mPortraitRotation=ROTATION_0 mUpsideDownRotation=ROTATION_180
  Display: mDisplayId=0 (organized)
    init=1080x2424 420dpi mMinSizeOfResizeableTaskDp=220 cur=%s
  overrideConfig={1.0 310mcc260mnc [en_US] ldltr sw411dp w923dp h411dp 420dpi nrml long compactNeeded land finger qwerty/v/v dpad/v winConfig={ mBounds=Rect(0, 0 - 2424, 1080) mDisplayRotation=%s mWindowingMode=fullscreen mActivityType=undefined mAlwaysOnTop=undefined mRotation=%s} s.1075 fontWeightAdjustment=0}
    mSupportAutoRotation=true`

	for _, c := range []struct {
		reported string
		size     string
		want     int
	}{
		{"ROTATION_0", "1080x2424", 0},
		{"ROTATION_90", "2424x1080", 1},
		{"ROTATION_180", "1080x2424", 2},
		{"ROTATION_270", "2424x1080", 3},
	} {
		got, ok := displayRotation(fmt.Sprintf(dump, c.size, c.reported, c.reported))
		if !ok || got != c.want {
			t.Errorf("%s: got (%d, %v), want (%d, true)", c.reported, got, ok, c.want)
		}
	}

	// 1084 is not display 0, and neither is any other id that starts with one.
	// Matching the id without its end reads 1084's rotation for 90 and 270,
	// which is the half of this that a device at rest cannot show.
	if _, ok := displayRotation("Display: mDisplayId=1084\n mDisplayRotation=ROTATION_90"); ok {
		t.Error("answered for display 1084, which is not the one being streamed")
	}

	// The constants on their own are not a rotation. This is the case that
	// separates the field name from the substring "Rotation=ROTATION_": a
	// parser matching the looser one reads ROTATION_90 here and reports a
	// device on its side.
	const constantsOnly = `  Display: mDisplayId=0
    mLandscapeRotation=ROTATION_90 mSeascapeRotation=ROTATION_270
    mPortraitRotation=ROTATION_0 mUpsideDownRotation=ROTATION_180`
	if got, ok := displayRotation(constantsOnly); ok {
		t.Errorf("read a rotation of %d off the device's rotation constants", got)
	}

	// A dump with only virtual displays states no rotation either.
	if got, ok := displayRotation("mDisplayId=0 mDisplayRotation=undefined mRotation=undefined"); ok {
		t.Errorf("read a rotation of %d off a display that declares none", got)
	}
}
