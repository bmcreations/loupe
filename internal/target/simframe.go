package target

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A simulator's bezel is Xcode's own: the device type's profile names a
// DeviceKit chrome bundle and a framebuffer mask, both vector PDFs installed
// with Xcode under /Library/Developer/DeviceKit. It is read in place and never
// vendored, for the same reason an emulator skin is not.
//
// The chrome's composite is the whole phone, screen included, sized in points
// and centred on the screen: iPhone 17's is 436x908 around a 402x874 screen,
// 17 points of bezel on every side. The mask is the screen's own shape at
// framebuffer size, opaque where the screen shows and clear in its rounded
// corners, which is the reverse of what the client lays over the canvas, so
// cornerMask turns it around.
//
// Both are rasterised by sips at the screen's scale, so the art lands on the
// framebuffer's pixel grid the way an emulator skin does (trap 40).
//
// A device type with two panels, the iPhone Duo, names a chrome per panel in
// its capabilities' displays, and the frame is the lit panel's. The Duo's
// inner panel uses phone14, whose composite is a generic 660x924 pt phone
// smaller than the 669x951 pt screen it frames, so it is stretched (see
// nineSlice). That panel is also turned upright by simcap (nativeRotation
// 270), and its frame is turned the same way.

// deviceKitDir is where Xcode installs the chrome and the masks.
var deviceKitDir = "/Library/Developer/DeviceKit"

// The two asset names a simulator frame declares. They are fixed rather than
// read from the bundle, so the asset endpoint cannot be steered at any other
// file.
const (
	simFrameBack = "back.png"
	simFrameMask = "mask.png"
)

// simFrameAssets are the asset names for one panel. The default panel keeps
// the plain names; another panel's carry its screen ID, so an asset fetched
// after a fold still comes from the frame that named it.
func simFrameAssets(screen int) (back, mask string) {
	if screen == 0 {
		return simFrameBack, simFrameMask
	}
	return fmt.Sprintf("back-%d.png", screen), fmt.Sprintf("mask-%d.png", screen)
}

// simChrome is what the device type says about its bezel, resolved to files.
type simChrome struct {
	composite string // PDF of the whole phone
	mask      string // PDF of the screen's shape
	scale     int
	screenW   int // framebuffer pixels
	screenH   int
	radius    int // screen corner radius, in points
	// turns is the quarter turns clockwise simcap gives this panel's
	// picture to stand it upright, from the display's nativeRotation.
	turns int
	// rim is the bezel in points, left, right, top and bottom, and corner
	// the outer corner radius, both from chrome.json. Only a composite
	// smaller than its screen uses them.
	rim    [4]int
	corner int
}

// simFrameCache holds the rasterised art for one simulator. The art depends
// only on the device type, and rendering takes sips a few hundred
// milliseconds, so it is done once.
type simFrameCache struct {
	once  sync.Once
	frame *DeviceFrame
	back  []byte
	mask  []byte
	err   error
}

// DeviceFrame implements Framer from the device type's DeviceKit chrome,
// for the panel that is lit.
func (s *Simulator) DeviceFrame(ctx context.Context) (*DeviceFrame, error) {
	c := s.frameArt(ctx, s.framePanel(ctx))
	if c.err != nil {
		return nil, c.err
	}
	f := *c.frame
	return &f, nil
}

// FrameAsset serves one of the two images DeviceFrame named. The name says
// which panel's frame named it.
func (s *Simulator) FrameAsset(ctx context.Context, name string) ([]byte, string, error) {
	screen := 0
	if n, err := fmt.Sscanf(name, "back-%d.png", &screen); n != 1 || err != nil {
		if n, err := fmt.Sscanf(name, "mask-%d.png", &screen); n != 1 || err != nil {
			screen = 0
		}
	}
	if screen != 0 && !slices.Contains(s.chromePanels(ctx), screen) {
		return nil, "", fmt.Errorf("frame asset %q is not declared by this simulator", name)
	}
	c := s.frameArt(ctx, screen)
	if c.err != nil {
		return nil, "", c.err
	}
	back, mask := simFrameAssets(screen)
	switch name {
	case back:
		return c.back, "image/png", nil
	case mask:
		return c.mask, "image/png", nil
	}
	return nil, "", fmt.Errorf("frame asset %q is not declared by this simulator", name)
}

// framePanel is the screen ID whose frame to draw, or 0 for the device
// type's own chrome. Only a device type with two panels asks simcap which is
// lit, so a single-panel simulator never starts an input process to answer
// /api/frame.
func (s *Simulator) framePanel(ctx context.Context) int {
	panels := s.chromePanels(ctx)
	if len(panels) < 2 || s.requireBooted() != nil {
		return 0
	}
	// A CoreDevice lookup can hang (see the display watcher), and a bezel
	// for the wrong panel is better than none at all.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if lit := s.litDisplay(ctx); slices.Contains(panels, lit) {
		return lit
	}
	return 0
}

// chromePanels reads, once, the screen IDs the device type names a chrome
// for. A failed read is retried next time.
func (s *Simulator) chromePanels(ctx context.Context) []int {
	s.frameMu.Lock()
	p := s.framePanels
	s.frameMu.Unlock()
	if p != nil {
		return p
	}
	bundle, err := deviceTypeBundle(ctx, s.Info().DeviceType)
	if err != nil {
		return nil
	}
	caps, err := plistJSON(ctx, filepath.Join(bundle, "Contents", "Resources", "capabilities.plist"))
	if err != nil {
		return nil
	}
	p = simChromePanels(caps)
	s.frameMu.Lock()
	s.framePanels = p
	s.frameMu.Unlock()
	return p
}

func (s *Simulator) frameArt(ctx context.Context, screen int) *simFrameCache {
	s.frameMu.Lock()
	if s.frames == nil {
		s.frames = map[int]*simFrameCache{}
	}
	c := s.frames[screen]
	if c == nil {
		c = &simFrameCache{}
		s.frames[screen] = c
	}
	s.frameMu.Unlock()
	c.once.Do(func() {
		c.frame, c.back, c.mask, c.err = renderSimFrame(ctx, s.Info().DeviceType, screen)
		if c.err != nil {
			// A failure is not cached: it may be a timeout on a busy machine,
			// and the next fetch should get to try again.
			s.frameMu.Lock()
			if s.frames[screen] == c {
				delete(s.frames, screen)
			}
			s.frameMu.Unlock()
		}
	})
	return c
}

func renderSimFrame(ctx context.Context, deviceType string, screen int) (*DeviceFrame, []byte, []byte, error) {
	if deviceType == "" {
		return nil, nil, nil, fmt.Errorf("simulator has no device type")
	}
	bundle, err := deviceTypeBundle(ctx, deviceType)
	if err != nil {
		return nil, nil, nil, err
	}
	ch, err := readSimChrome(ctx, bundle, screen)
	if err != nil {
		return nil, nil, nil, err
	}
	pw, ph, err := pdfSize(ctx, ch.composite)
	if err != nil {
		return nil, nil, nil, err
	}
	frame, err := simFrameGeometry(pw, ph, ch)
	if err != nil {
		return nil, nil, nil, err
	}
	var back []byte
	if cw, chh := pw*ch.scale, ph*ch.scale; cw != frame.Width || chh != frame.Height {
		art, err := rasterise(ctx, ch.composite, cw, chh)
		if err != nil {
			return nil, nil, nil, err
		}
		back, err = nineSlice(art, frame.Width, frame.Height, ch.corner*ch.scale)
		if err != nil {
			return nil, nil, nil, err
		}
	} else if back, err = rasterise(ctx, ch.composite, frame.Width, frame.Height); err != nil {
		return nil, nil, nil, err
	}
	rawMask, err := rasterise(ctx, ch.mask, ch.screenW, ch.screenH)
	if err != nil {
		return nil, nil, nil, err
	}
	mask, err := cornerMask(back, rawMask, frame.Display)
	if err != nil {
		return nil, nil, nil, err
	}
	frame.Background, frame.Mask = simFrameAssets(screen)
	return frame.turned(ch.turns), back, mask, nil
}

// simFrameGeometry places the screen inside the composite. The composite is
// centred on the screen, so the bezel is half the difference on each axis; an
// odd or negative difference means that assumption does not hold for this
// chrome, and no frame is better than a screen drawn off its hole.
//
// A composite smaller than the screen is the exception: phone14 is drawn for
// no screen in particular, and DeviceKit stretches it with chrome.json's
// sizing as the bezel. The frame is then the screen plus that rim.
func simFrameGeometry(compositeW, compositeH int, ch simChrome) (*DeviceFrame, error) {
	if ch.scale <= 0 || ch.screenW <= 0 || ch.screenH <= 0 {
		return nil, fmt.Errorf("device type reports no screen size")
	}
	w, h := compositeW*ch.scale, compositeH*ch.scale
	if (w < ch.screenW || h < ch.screenH) && ch.rim[0] > 0 && ch.rim[1] > 0 && ch.rim[2] > 0 && ch.rim[3] > 0 {
		l, r, t, b := ch.rim[0]*ch.scale, ch.rim[1]*ch.scale, ch.rim[2]*ch.scale, ch.rim[3]*ch.scale
		return &DeviceFrame{
			Source:       "skin",
			Width:        ch.screenW + l + r,
			Height:       ch.screenH + t + b,
			Display:      FrameRect{X: l, Y: t, Width: ch.screenW, Height: ch.screenH},
			CornerRadius: ch.radius * ch.scale,
			Background:   simFrameBack,
			Mask:         simFrameMask,
		}, nil
	}
	dx, dy := w-ch.screenW, h-ch.screenH
	if dx < 0 || dy < 0 || dx%2 != 0 || dy%2 != 0 {
		return nil, fmt.Errorf("chrome %dx%d pt does not centre a %dx%d screen at %dx",
			compositeW, compositeH, ch.screenW, ch.screenH, ch.scale)
	}
	return &DeviceFrame{
		Source:       "skin",
		Width:        w,
		Height:       h,
		Display:      FrameRect{X: dx / 2, Y: dy / 2, Width: ch.screenW, Height: ch.screenH},
		CornerRadius: ch.radius * ch.scale,
		Background:   simFrameBack,
		Mask:         simFrameMask,
	}, nil
}

// deviceTypeBundle finds the .simdevicetype bundle for an identifier. The
// bundle is named for the product, not the identifier, so simctl is asked.
func deviceTypeBundle(ctx context.Context, id string) (string, error) {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "list", "-j", "devicetypes").Output()
	if err != nil {
		return "", fmt.Errorf("simctl list devicetypes: %w", err)
	}
	return parseDeviceTypeBundle(out, id)
}

func parseDeviceTypeBundle(raw []byte, id string) (string, error) {
	var doc struct {
		DeviceTypes []struct {
			Identifier string `json:"identifier"`
			BundlePath string `json:"bundlePath"`
		} `json:"devicetypes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("simctl list devicetypes: %w", err)
	}
	for _, t := range doc.DeviceTypes {
		if t.Identifier == id {
			return t.BundlePath, nil
		}
	}
	return "", fmt.Errorf("device type %s is not installed", id)
}

// readSimChrome reads the profile and capabilities plists of a device type
// bundle and resolves the chrome and mask they name, for the panel with
// screen ID `screen`, or for the device type as a whole when it is 0.
func readSimChrome(ctx context.Context, bundle string, screen int) (simChrome, error) {
	res := filepath.Join(bundle, "Contents", "Resources")
	profile, err := plistJSON(ctx, filepath.Join(res, "profile.plist"))
	if err != nil {
		return simChrome{}, err
	}
	caps, err := plistJSON(ctx, filepath.Join(res, "capabilities.plist"))
	if err != nil {
		return simChrome{}, err
	}
	return parseSimChrome(profile, caps, deviceKitDir, screen)
}

// simDisplay is one entry of a device type's capabilities displays.
type simDisplay struct {
	ScreenID int    `json:"screenID"`
	Chrome   string `json:"chromeIdentifier"`
	Mask     string `json:"framebufferMaskIdentifier"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Scale    int    `json:"scale"`
	Radius   int    `json:"cornerRadiusUL"`
	Rotation int    `json:"nativeRotation"`
}

// simChromePanels is the screen IDs of the displays that name a chrome:
// one panel on an iPhone, two on a Duo, never a TV-out or a CarPlay screen.
func simChromePanels(caps []byte) []int {
	var c struct {
		Caps struct {
			Displays []simDisplay `json:"displays"`
		} `json:"capabilities"`
	}
	out := []int{}
	if json.Unmarshal(caps, &c) != nil {
		return out
	}
	for _, d := range c.Caps.Displays {
		if d.Chrome != "" && d.Mask != "" && d.ScreenID > 0 {
			out = append(out, d.ScreenID)
		}
	}
	return out
}

func parseSimChrome(profile, caps []byte, kit string, screen int) (simChrome, error) {
	var p struct {
		Chrome string `json:"chromeIdentifier"`
		Mask   string `json:"framebufferMask"`
	}
	if err := json.Unmarshal(profile, &p); err != nil {
		return simChrome{}, fmt.Errorf("device type profile: %w", err)
	}
	var c struct {
		Caps struct {
			Screen struct {
				Scale  int `json:"main-screen-scale"`
				Width  int `json:"main-screen-width"`
				Height int `json:"main-screen-height"`
			} `json:"ScreenDimensionsCapability"`
			// Displays carries the corner radius, per corner, in points,
			// and on a device type with two panels each one's chrome.
			Displays []simDisplay `json:"displays"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(caps, &c); err != nil {
		return simChrome{}, fmt.Errorf("device type capabilities: %w", err)
	}
	var panel *simDisplay
	if screen != 0 {
		for i, d := range c.Caps.Displays {
			if d.ScreenID == screen && d.Chrome != "" && d.Mask != "" {
				panel = &c.Caps.Displays[i]
			}
		}
		if panel == nil || panel.Scale <= 0 || panel.Width <= 0 || panel.Height <= 0 {
			return simChrome{}, fmt.Errorf("device type names no chrome for screen %d", screen)
		}
		if panel.Rotation%90 != 0 {
			return simChrome{}, fmt.Errorf("screen %d has a rotation of %d", screen, panel.Rotation)
		}
		p.Chrome, p.Mask = panel.Chrome, panel.Mask
	}
	// com.apple.dt.devicekit.chrome.phone11 lives in phone11.devicechrome.
	const prefix = "com.apple.dt.devicekit.chrome."
	if !strings.HasPrefix(p.Chrome, prefix) || p.Mask == "" {
		return simChrome{}, fmt.Errorf("device type names no DeviceKit chrome")
	}
	name := strings.TrimPrefix(p.Chrome, prefix)
	if strings.ContainsAny(name, "/.") || strings.ContainsAny(p.Mask, "/.") {
		return simChrome{}, fmt.Errorf("device type chrome %q is not a plain name", p.Chrome)
	}
	chromeRes := filepath.Join(kit, "Chrome", name+".devicechrome", "Contents", "Resources")
	var cj struct {
		Images struct {
			Composite string `json:"composite"`
			Sizing    struct {
				L int `json:"leftWidth"`
				R int `json:"rightWidth"`
				T int `json:"topHeight"`
				B int `json:"bottomHeight"`
			} `json:"sizing"`
		} `json:"images"`
		Paths struct {
			Border struct {
				Radius int `json:"cornerRadiusX"`
			} `json:"simpleOutsideBorder"`
		} `json:"paths"`
	}
	raw, err := os.ReadFile(filepath.Join(chromeRes, "chrome.json"))
	if err != nil {
		return simChrome{}, fmt.Errorf("read chrome: %w", err)
	}
	if err := json.Unmarshal(raw, &cj); err != nil {
		return simChrome{}, fmt.Errorf("chrome.json: %w", err)
	}
	if cj.Images.Composite == "" || strings.Contains(cj.Images.Composite, "/") {
		return simChrome{}, fmt.Errorf("chrome %s declares no composite", name)
	}
	s := c.Caps.Screen
	ch := simChrome{
		composite: filepath.Join(chromeRes, cj.Images.Composite+".pdf"),
		mask:      filepath.Join(kit, "FramebufferMasks", p.Mask+".pdf"),
		scale:     s.Scale,
		screenW:   s.Width,
		screenH:   s.Height,
	}
	z := cj.Images.Sizing
	ch.rim = [4]int{z.L, z.R, z.T, z.B}
	ch.corner = cj.Paths.Border.Radius
	if panel != nil {
		ch.scale, ch.screenW, ch.screenH = panel.Scale, panel.Width, panel.Height
		ch.radius = panel.Radius
		ch.turns = panel.Rotation / 90 % 4
	} else if d := c.Caps.Displays; len(d) > 0 {
		ch.radius = d[0].Radius
	}
	return ch, nil
}

func plistJSON(ctx context.Context, path string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "plutil", "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return out, nil
}

// pdfSize is a PDF's page size in points, which is what sips reports as its
// pixel size.
func pdfSize(ctx context.Context, path string) (int, int, error) {
	out, err := exec.CommandContext(ctx, "sips", "-g", "pixelWidth", "-g", "pixelHeight", path).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("sips %s: %w", filepath.Base(path), err)
	}
	return parseSipsSize(string(out))
}

func parseSipsSize(out string) (int, int, error) {
	var w, h float64
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			continue
		}
		switch k {
		case "pixelWidth":
			w = f
		case "pixelHeight":
			h = f
		}
	}
	if w <= 0 || h <= 0 || w != float64(int(w)) || h != float64(int(h)) {
		return 0, 0, fmt.Errorf("sips reported %gx%g", w, h)
	}
	return int(w), int(h), nil
}

// rasterise renders a PDF to a PNG of exactly w x h. sips redraws the vector
// at the target size rather than scaling a 1x bitmap, so the edges are sharp.
func rasterise(ctx context.Context, pdf string, w, h int) ([]byte, error) {
	dir, err := os.MkdirTemp("", "loupe-frame-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "out.png")
	cmd := exec.CommandContext(ctx, "sips", "-s", "format", "png",
		"-z", strconv.Itoa(h), strconv.Itoa(w), pdf, "--out", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sips %s: %w: %s", filepath.Base(pdf), err, bytes.TrimSpace(b))
	}
	return os.ReadFile(out)
}

// nineSlice stretches art to w x h, keeping a `corner` px square at each
// corner as drawn and stretching the edges between them. phone14's composite
// is 1980x2772 at 3x and the Duo's inner frame 2109x2955, about 6.5% longer
// on each axis; the edges are a plain rim apart from a hinge mark mid-height,
// which this stretches by the same 6.5%.
func nineSlice(raw []byte, w, h, corner int) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode chrome: %w", err)
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if corner <= 0 || 2*corner >= sw || 2*corner >= sh || 2*corner >= w || 2*corner >= h {
		return nil, fmt.Errorf("chrome corner %d does not fit %dx%d to %dx%d", corner, sw, sh, w, h)
	}
	axis := func(d, sd int) []int {
		m := make([]int, d)
		for i := range m {
			switch {
			case i < corner:
				m[i] = i
			case i >= d-corner:
				m[i] = sd - (d - i)
			default:
				m[i] = corner + (i-corner)*(sd-2*corner)/(d-2*corner)
			}
		}
		return m
	}
	xs, ys := axis(w, sw), axis(h, sh)
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+xs[x], b.Min.Y+ys[y]))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// cornerMask builds what covers the screen's corners: the bezel art cropped to
// the display rect, kept only where the screen's shape is clear.
//
// Plain black corners are what an emulator skin's mask.webp holds, and they
// work there because a Pixel's bezel is black where it meets the screen.
// DeviceKit's chrome has a grey rim that curves inside the display rect, so
// black corners cut square notches out of it; drawing the bezel's own pixels
// there puts the rim back. The screen's corners reach past the phone's outer
// curve, so this cannot hide the video alone: the client also clips the canvas
// to CornerRadius.
func cornerMask(back, screen []byte, display FrameRect) ([]byte, error) {
	art, err := png.Decode(bytes.NewReader(back))
	if err != nil {
		return nil, fmt.Errorf("decode bezel: %w", err)
	}
	shape, err := png.Decode(bytes.NewReader(screen))
	if err != nil {
		return nil, fmt.Errorf("decode mask: %w", err)
	}
	sb := shape.Bounds()
	if sb.Dx() != display.Width || sb.Dy() != display.Height {
		return nil, fmt.Errorf("mask is %dx%d, display is %dx%d", sb.Dx(), sb.Dy(), display.Width, display.Height)
	}
	dst := image.NewNRGBA(image.Rect(0, 0, display.Width, display.Height))
	for y := 0; y < display.Height; y++ {
		for x := 0; x < display.Width; x++ {
			_, _, _, in := shape.At(sb.Min.X+x, sb.Min.Y+y).RGBA()
			if in == 0xffff {
				continue // screen shows here
			}
			// Where the art is itself clear the corner is past the phone's
			// outer curve, and the client's clip on the canvas hides the video
			// there, so clear is right.
			c := color.NRGBAModel.Convert(art.At(display.X+x, display.Y+y)).(color.NRGBA)
			c.A = uint8(uint32(c.A) * (0xffff - in) / 0xffff)
			dst.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
