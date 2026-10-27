package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// framedTarget is a stubTarget that also describes a bezel, so the two halves
// of the endpoint -- a target that can frame and one that cannot -- are the
// same type with and without the interface.
type framedTarget struct {
	stubTarget
	frame  *target.DeviceFrame
	assets map[string][]byte
	err    error
}

func (f *framedTarget) ID() string    { return "framed" }
func (f *framedTarget) Label() string { return "framed" }

func (f *framedTarget) DeviceFrame(context.Context) (*target.DeviceFrame, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.frame, nil
}

func (f *framedTarget) FrameAsset(_ context.Context, name string) ([]byte, string, error) {
	raw, ok := f.assets[name]
	if !ok {
		return nil, "", fmt.Errorf("frame asset %q is not declared by this skin", name)
	}
	return raw, "image/webp", nil
}

func frameServer(t *testing.T, tgt target.Target) *Server {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(tgt)
	return &Server{mgr: m}
}

func getFrame(t *testing.T, s *Server, path string, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// The Pixel_10 skin, as internal/target reads it off the SDK.
func pixelFrame() *target.DeviceFrame {
	return &target.DeviceFrame{
		Source: "skin", Width: 1198, Height: 2531,
		Display:      target.FrameRect{X: 59, Y: 55, Width: 1080, Height: 2424},
		CornerRadius: 87,
		Background:   "back.webp", Mask: "mask.webp",
	}
}

func TestAFrameReachesTheClientWithItsGeometryIntact(t *testing.T) {
	s := frameServer(t, &framedTarget{frame: pixelFrame()})

	rec := getFrame(t, s, "/api/frame?target=framed", s.handleFrame)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var got target.DeviceFrame
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Width != 1198 || got.Height != 2531 {
		t.Errorf("frame = %dx%d, want 1198x2531", got.Width, got.Height)
	}
	if got.Display != (target.FrameRect{X: 59, Y: 55, Width: 1080, Height: 2424}) {
		t.Errorf("display = %+v", got.Display)
	}
	if got.CornerRadius != 87 {
		t.Errorf("CornerRadius = %d, want 87", got.CornerRadius)
	}
}

// A derived frame names no assets, and the field has to be absent rather than
// an empty string, because the client decides whether to fetch art by asking
// whether the frame named any.
func TestADerivedFrameNamesNoAssets(t *testing.T) {
	derived := &target.DeviceFrame{
		Source: "derived", Width: 1284, Height: 2754,
		Display:      target.FrameRect{X: 42, Y: 42, Width: 1200, Height: 2670},
		CornerRadius: 66,
		Cutout:       &target.FrameCutout{Path: "M 0,0 L -20,0 Z", Scale: 3, DX: 600},
	}
	s := frameServer(t, &framedTarget{frame: derived})

	rec := getFrame(t, s, "/api/frame?target=framed", s.handleFrame)
	body := rec.Body.String()
	for _, field := range []string{`"background"`, `"mask"`} {
		if strings.Contains(body, field) {
			t.Errorf("derived frame carries %s: %s", field, body)
		}
	}
	if !strings.Contains(body, `"cutout"`) || !strings.Contains(body, `"dx":600`) {
		t.Errorf("cutout missing from %s", body)
	}
}

// Synthetic has no bezel, and the answer has to be 501 rather than an empty
// frame, so a client can tell "this target cannot" from "this target has none".
func TestATargetThatCannotFrameAnswers501(t *testing.T) {
	s := frameServer(t, &stubTarget{})

	for _, path := range []string{
		"/api/frame?target=stub",
		"/api/frame/asset?target=stub&file=back.webp",
	} {
		h := s.handleFrame
		if strings.Contains(path, "asset") {
			h = s.handleFrameAsset
		}
		if rec := getFrame(t, s, path, h); rec.Code != http.StatusNotImplemented {
			t.Errorf("%s: status %d, want 501", path, rec.Code)
		}
	}
}

func TestAnAssetTheSkinDidNotDeclareIsNotServed(t *testing.T) {
	s := frameServer(t, &framedTarget{
		frame:  pixelFrame(),
		assets: map[string][]byte{"back.webp": []byte("art"), "mask.webp": []byte("mask")},
	})

	for _, file := range []string{
		"layout", "../../../../etc/passwd", "back.webp/../layout", "..", "%2e%2e",
	} {
		rec := getFrame(t, s, "/api/frame/asset?target=framed&file="+file, s.handleFrameAsset)
		if rec.Code != http.StatusNotFound {
			t.Errorf("file=%q: status %d, want 404", file, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "art") {
			t.Errorf("file=%q served the asset body", file)
		}
	}

	// No ?file= at all is the caller's mistake, not a missing file.
	if rec := getFrame(t, s, "/api/frame/asset?target=framed", s.handleFrameAsset); rec.Code != http.StatusBadRequest {
		t.Errorf("missing ?file=: status %d, want 400", rec.Code)
	}
}

// Skin art is a couple of hundred kilobytes of webp over the phone's link, and
// the whole point of the ETag is that a reload does not re-fetch it.
func TestSkinArtIsRevalidatedRatherThanRefetched(t *testing.T) {
	s := frameServer(t, &framedTarget{
		frame:  pixelFrame(),
		assets: map[string][]byte{"back.webp": []byte("a couple of hundred kilobytes")},
	})

	first := getFrame(t, s, "/api/frame/asset?target=framed&file=back.webp", s.handleFrameAsset)
	if first.Code != http.StatusOK {
		t.Fatalf("status %d", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag, so every reload refetches the art")
	}
	if cc := first.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, want it private", cc)
	}
	if first.Header().Get("Content-Type") != "image/webp" {
		t.Errorf("Content-Type = %q", first.Header().Get("Content-Type"))
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/frame/asset?target=framed&file=back.webp", nil)
	r.Header.Set("If-None-Match", etag)
	s.handleFrameAsset(w, r)
	if w.Code != http.StatusNotModified {
		t.Errorf("revalidation: status %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 carried %d bytes", w.Body.Len())
	}
}

// Different art must not share a cache entry, which a constant or a
// name-derived tag would let it do.
func TestTwoSkinsDoNotShareAnETag(t *testing.T) {
	tag := func(body string) string {
		s := frameServer(t, &framedTarget{
			frame:  pixelFrame(),
			assets: map[string][]byte{"back.webp": []byte(body)},
		})
		return getFrame(t, s, "/api/frame/asset?target=framed&file=back.webp", s.handleFrameAsset).
			Header().Get("ETag")
	}
	if a, b := tag("pixel art"), tag("other art"); a == b {
		t.Errorf("two different assets share ETag %s", a)
	}
}

func TestAFrameEndpointNeedsATarget(t *testing.T) {
	s := frameServer(t, &framedTarget{frame: pixelFrame()})

	for _, path := range []string{"/api/frame", "/api/frame?target=nope"} {
		rec := getFrame(t, s, path, s.handleFrame)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 400 or 404", path, rec.Code)
		}
	}
}
