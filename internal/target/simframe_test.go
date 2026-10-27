package target

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// The profile and capabilities of iPhone 17 on Xcode 27, trimmed to the keys
// read, in the shape plutil -convert json prints them.
const (
	iPhone17Profile = `{"chromeIdentifier":"com.apple.dt.devicekit.chrome.phone11",
		"framebufferMask":"4E5532ED-1470-47D1-BDF4-7AA90C26957A"}`
	iPhone17Caps = `{"capabilities":{
		"ScreenDimensionsCapability":{"main-screen-scale":3,"main-screen-width":1206,"main-screen-height":2622},
		"displays":[{"cornerRadiusUL":62,"cornerRadiusUR":62,"cornerRadiusLL":62,"cornerRadiusLR":62}]}}`
)

func fakeDeviceKit(t *testing.T) string {
	t.Helper()
	kit := t.TempDir()
	res := filepath.Join(kit, "Chrome", "phone11.devicechrome", "Contents", "Resources")
	if err := os.MkdirAll(res, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(res, "chrome.json"),
		[]byte(`{"images":{"composite":"PhoneComposite"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return kit
}

func TestParseSimChromeResolvesTheChromeAndMask(t *testing.T) {
	kit := fakeDeviceKit(t)
	ch, err := parseSimChrome([]byte(iPhone17Profile), []byte(iPhone17Caps), kit, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := simChrome{
		composite: filepath.Join(kit, "Chrome", "phone11.devicechrome", "Contents", "Resources", "PhoneComposite.pdf"),
		mask:      filepath.Join(kit, "FramebufferMasks", "4E5532ED-1470-47D1-BDF4-7AA90C26957A.pdf"),
		scale:     3, screenW: 1206, screenH: 2622, radius: 62,
	}
	if ch != want {
		t.Fatalf("got %+v\nwant %+v", ch, want)
	}
}

func TestParseSimChromeRefusesAPathInTheProfile(t *testing.T) {
	kit := fakeDeviceKit(t)
	for _, profile := range []string{
		`{"chromeIdentifier":"com.apple.dt.devicekit.chrome.../../etc","framebufferMask":"m"}`,
		`{"chromeIdentifier":"com.apple.dt.devicekit.chrome.phone11","framebufferMask":"../x"}`,
		`{"chromeIdentifier":"something.else","framebufferMask":"m"}`,
	} {
		if _, err := parseSimChrome([]byte(profile), []byte(iPhone17Caps), kit, 0); err == nil {
			t.Errorf("%s: accepted", profile)
		}
	}
}

// Measured on iPhone 17: a 436x908 pt composite around a 402x874 pt screen,
// which at 3x is 51 px of bezel on every side.
func TestSimFrameGeometryCentresTheScreen(t *testing.T) {
	f, err := simFrameGeometry(436, 908, simChrome{scale: 3, screenW: 1206, screenH: 2622, radius: 62})
	if err != nil {
		t.Fatal(err)
	}
	if f.Width != 1308 || f.Height != 2724 {
		t.Errorf("frame %dx%d, want 1308x2724", f.Width, f.Height)
	}
	if f.Display != (FrameRect{X: 51, Y: 51, Width: 1206, Height: 2622}) {
		t.Errorf("display %+v", f.Display)
	}
	if f.CornerRadius != 186 || f.Source != "skin" || f.Background != simFrameBack || f.Mask != simFrameMask {
		t.Errorf("frame %+v", f)
	}
}

func TestSimFrameGeometryRefusesAChromeThatDoesNotCentreTheScreen(t *testing.T) {
	ch := simChrome{scale: 3, screenW: 1206, screenH: 2622}
	for _, size := range [][2]int{{400, 908}, {436, 800}} {
		if _, err := simFrameGeometry(size[0], size[1], ch); err == nil {
			t.Errorf("%v: accepted", size)
		}
	}
	// An odd difference cannot be split evenly across two sides.
	if _, err := simFrameGeometry(11, 11, simChrome{scale: 1, screenW: 10, screenH: 10}); err == nil {
		t.Error("odd bezel accepted")
	}
}

func TestParseSipsSize(t *testing.T) {
	out := "/Library/Developer/DeviceKit/x.pdf\n  pixelWidth: 436.000\n  pixelHeight: 908.000\n"
	w, h, err := parseSipsSize(out)
	if err != nil || w != 436 || h != 908 {
		t.Fatalf("got %d %d %v", w, h, err)
	}
	if _, _, err := parseSipsSize("  pixelWidth: 436.5\n  pixelHeight: 908\n"); err == nil {
		t.Error("fractional size accepted")
	}
}

func TestParseDeviceTypeBundle(t *testing.T) {
	raw := []byte(`{"devicetypes":[
		{"identifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-17","bundlePath":"/P/iPhone 17.simdevicetype"}]}`)
	got, err := parseDeviceTypeBundle(raw, "com.apple.CoreSimulator.SimDeviceType.iPhone-17")
	if err != nil || got != "/P/iPhone 17.simdevicetype" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := parseDeviceTypeBundle(raw, "nope"); err == nil {
		t.Error("unknown device type found")
	}
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The screen's shape is opaque over the screen. What covers the canvas must be
// clear there and the bezel's own pixels in the corners, including where the
// bezel is itself clear.
func TestCornerMaskShowsTheBezelOnlyOutsideTheScreen(t *testing.T) {
	grey := color.NRGBA{R: 90, G: 90, B: 90, A: 255}
	back := image.NewNRGBA(image.Rect(0, 0, 5, 3))
	back.SetNRGBA(1, 1, grey) // display (1,1): a corner, over the grey rim
	// display (2,1) is a corner over clear art; (3,1) is screen.
	shape := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	shape.SetNRGBA(2, 0, color.NRGBA{A: 255})

	out, err := cornerMask(encodePNG(t, back), encodePNG(t, shape), FrameRect{X: 1, Y: 1, Width: 3, Height: 1})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	at := func(x int) color.NRGBA { return color.NRGBAModel.Convert(img.At(x, 0)).(color.NRGBA) }
	if got := at(0); got != grey {
		t.Errorf("rim corner %v, want the bezel's %v", got, grey)
	}
	if got := at(1); got.A != 0 {
		t.Errorf("clear-art corner %v, want clear", got)
	}
	if got := at(2); got.A != 0 {
		t.Errorf("screen %v, want clear", got)
	}
}

func TestCornerMaskRefusesAMaskOfTheWrongSize(t *testing.T) {
	back := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 4, 4)))
	shape := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 2, 2)))
	if _, err := cornerMask(back, shape, FrameRect{Width: 3, Height: 3}); err == nil {
		t.Error("accepted")
	}
}

// The iPhone Duo's displays on Xcode 27, trimmed: the outer panel, the TV-out
// that names no chrome, and the inner panel, whose framebuffer is portrait
// under a Home Screen drawn landscape.
const duoCaps = `{"capabilities":{
	"ScreenDimensionsCapability":{"main-screen-scale":3,"main-screen-width":1398,"main-screen-height":2034},
	"displays":[
		{"screenID":1,"chromeIdentifier":"com.apple.dt.devicekit.chrome.phone15","framebufferMaskIdentifier":"OUTER",
		 "width":1398,"height":2034,"scale":3,"cornerRadiusUL":8,"nativeRotation":0},
		{"screenID":2,"width":1920,"height":1080,"scale":1},
		{"screenID":3,"chromeIdentifier":"com.apple.dt.devicekit.chrome.phone14","framebufferMaskIdentifier":"BF0DC480",
		 "width":2007,"height":2853,"scale":3,"cornerRadiusUL":55,"nativeRotation":270}]}}`

func TestSimChromePanelsListsOnlyPanelsWithChrome(t *testing.T) {
	got := simChromePanels([]byte(duoCaps))
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("panels %v, want [1 3]", got)
	}
	if got := simChromePanels([]byte(iPhone17Caps)); len(got) != 0 {
		t.Errorf("iPhone 17 panels %v, want none", got)
	}
}

func TestParseSimChromeReadsTheLitPanel(t *testing.T) {
	kit := t.TempDir()
	res := filepath.Join(kit, "Chrome", "phone14.devicechrome", "Contents", "Resources")
	if err := os.MkdirAll(res, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(res, "chrome.json"), []byte(`{"images":{"composite":"C",
		"sizing":{"leftWidth":17,"rightWidth":17,"topHeight":17,"bottomHeight":17}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err := parseSimChrome([]byte(iPhone17Profile), []byte(duoCaps), kit, 3)
	if err != nil {
		t.Fatal(err)
	}
	if ch.screenW != 2007 || ch.screenH != 2853 || ch.scale != 3 || ch.radius != 55 || ch.turns != 3 {
		t.Errorf("chrome %+v", ch)
	}
	if ch.mask != filepath.Join(kit, "FramebufferMasks", "BF0DC480.pdf") || ch.rim != [4]int{17, 17, 17, 17} {
		t.Errorf("chrome %+v", ch)
	}
	// The TV-out names no chrome, and neither does a screen that is not there.
	for _, screen := range []int{2, 9} {
		if _, err := parseSimChrome([]byte(iPhone17Profile), []byte(duoCaps), kit, screen); err == nil {
			t.Errorf("screen %d accepted", screen)
		}
	}
}

// phone14's composite is 660x924 pt, smaller than the 669x951 pt inner panel,
// so the frame is the panel plus chrome.json's rim, then turned upright.
func TestSimFrameGeometrySizesASmallChromeByItsRim(t *testing.T) {
	ch := simChrome{scale: 3, screenW: 2007, screenH: 2853, radius: 55, rim: [4]int{17, 17, 17, 17}, turns: 3}
	f, err := simFrameGeometry(660, 924, ch)
	if err != nil {
		t.Fatal(err)
	}
	if f.Width != 2109 || f.Height != 2955 || f.Display != (FrameRect{X: 51, Y: 51, Width: 2007, Height: 2853}) {
		t.Errorf("frame %+v", f)
	}
	up := f.turned(ch.turns)
	if up.Width != 2955 || up.Height != 2109 || up.Display.Width != 2853 || up.Display.Height != 2007 {
		t.Errorf("turned %+v", up)
	}
}

func TestNineSliceKeepsCornersAndStretchesEdges(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	src.Set(0, 0, color.NRGBA{255, 0, 0, 255})
	src.Set(9, 9, color.NRGBA{0, 0, 255, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	out, err := nineSlice(buf.Bytes(), 16, 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 16 || b.Dy() != 20 {
		t.Fatalf("size %v", b)
	}
	if r, _, _, _ := img.At(0, 0).RGBA(); r>>8 != 255 {
		t.Error("top-left corner moved")
	}
	if _, _, b, _ := img.At(15, 19).RGBA(); b>>8 != 255 {
		t.Error("bottom-right corner moved")
	}
	if _, err := nineSlice(buf.Bytes(), 16, 20, 5); err == nil {
		t.Error("a corner half the art accepted")
	}
}
