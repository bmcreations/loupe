package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	pngcodec "image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/camfeed"
	"github.com/bmcreations/loupe/internal/target"
)

// cameraTarget is a CameraFeeder that keeps what it was handed.
type cameraTarget struct {
	stubTarget
	err     error
	got     []byte
	cleared bool
}

func (f *cameraTarget) SetCamera(ctx context.Context, png []byte) (target.CameraApplies, error) {
	f.got = png
	return target.CameraOnOpen, f.err
}

func (f *cameraTarget) ClearCamera(ctx context.Context) (target.CameraApplies, error) {
	f.cleared = true
	return target.CameraOnBoot, f.err
}

func testImage(t *testing.T, w, h int, encode func(*bytes.Buffer, image.Image) error) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodePNG(b *bytes.Buffer, img image.Image) error { return pngcodec.Encode(b, img) }
func encodeJPEG(b *bytes.Buffer, img image.Image) error {
	return jpeg.Encode(b, img, nil)
}

func TestCameraPassesAPNGThrough(t *testing.T) {
	body := testImage(t, 40, 30, encodePNG)
	f := &cameraTarget{}
	w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", bytes.NewReader(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s; want 200", w.Code, w.Body)
	}
	if !bytes.Equal(f.got, body) {
		t.Errorf("target got %d bytes; want the PNG unchanged", len(f.got))
	}
	if s := w.Body.String(); !strings.Contains(s, `"applies":"cameraOpen"`) || !strings.Contains(s, `"width":40`) {
		t.Errorf("answer %s; want applies and the size", s)
	}
}

// The emulator reads the staged file's type from its extension, which is
// always .png, so a JPEG has to arrive as a PNG.
func TestCameraReencodesAJPEGAsPNG(t *testing.T) {
	f := &cameraTarget{}
	w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", bytes.NewReader(testImage(t, 64, 48, encodeJPEG)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s; want 200", w.Code, w.Body)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(f.got))
	if err != nil || format != "png" || cfg.Width != 64 || cfg.Height != 48 {
		t.Errorf("target got %s %dx%d (%v); want a 64x48 PNG", format, cfg.Width, cfg.Height, err)
	}
}

func TestCameraRefusesWhatIsNotAnImage(t *testing.T) {
	f := &cameraTarget{}
	for _, body := range []string{apk, "\x89PNG\r\n\x1a\n truncated", ""} {
		w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", strings.NewReader(body))
		if w.Code != http.StatusBadRequest || f.got != nil {
			t.Errorf("%.12q: status %d, target given %d bytes; want 400 and nothing", body, w.Code, len(f.got))
		}
	}
}

func TestCameraRefusesAnImageTooLargeToDecode(t *testing.T) {
	f := &cameraTarget{}
	// A header claiming 9000x10, with no pixels behind it: refused from the
	// header, before a decode would allocate for it.
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := []byte{0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 0, 0x23, 0x28, 0, 0, 0, 10, 8, 6, 0, 0, 0, 0, 0, 0, 0}
	b.Write(ihdr)
	w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", &b)
	if w.Code != http.StatusBadRequest || f.got != nil {
		t.Errorf("status %d %s; want 400 and nothing staged", w.Code, w.Body)
	}
}

func TestCameraSizeLimit(t *testing.T) {
	defer func(n int64) { maxCameraBytes = n }(maxCameraBytes)
	maxCameraBytes = 64
	f := &cameraTarget{}
	w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", bytes.NewReader(testImage(t, 200, 200, encodeJPEG)))
	if w.Code != http.StatusRequestEntityTooLarge || f.got != nil {
		t.Errorf("status %d; want 413 and nothing staged", w.Code)
	}
}

func TestCameraDeleteClears(t *testing.T) {
	f := &cameraTarget{}
	w := serveAgent(t, f, http.MethodDelete, "/api/camera?target=stub", nil)
	if w.Code != http.StatusOK || !f.cleared || !strings.Contains(w.Body.String(), `"applies":"boot"`) {
		t.Errorf("status %d %s, cleared %v; want 200, cleared, applies boot", w.Code, w.Body, f.cleared)
	}
}

func TestCameraIs501WithoutTheCapability(t *testing.T) {
	w := serveAgent(t, &stubTarget{}, http.MethodPost, "/api/camera?target=stub", bytes.NewReader(testImage(t, 4, 4, encodePNG)))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status %d %s; want 501", w.Code, w.Body)
	}
}

func TestCameraDeviceFailureIs502(t *testing.T) {
	f := &cameraTarget{err: errors.New("xcrun: no SDK")}
	w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", bytes.NewReader(testImage(t, 4, 4, encodePNG)))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "no SDK") {
		t.Errorf("status %d %s; want 502 naming the cause", w.Code, w.Body)
	}
}

// hostCameraTarget is a HostCameraFeeder.
type hostCameraTarget struct {
	cameraTarget
	cams []target.HostCamera
	name string
	live bool
}

func (f *hostCameraTarget) SetHostCamera(ctx context.Context, name string) (target.CameraApplies, error) {
	for _, c := range f.cams {
		if c.Name == name {
			f.name = name
			if f.live {
				return target.CameraLive, nil
			}
			return target.CameraOnBoot, nil
		}
	}
	return "", fmt.Errorf("%w: %q", target.ErrNoHostCamera, name)
}

func (f *hostCameraTarget) HostCameras(ctx context.Context) ([]target.HostCamera, error) {
	return f.cams, f.err
}

func serveJSON(t *testing.T, tg target.Target, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(tg)
	s := &Server{mgr: m, token: "secret"}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestCameraJSONStagesAHostCamera(t *testing.T) {
	f := &hostCameraTarget{cams: []target.HostCamera{{Name: "Insta360 Link 2", Label: "webcam0"}}}
	w := serveJSON(t, f, http.MethodPost, "/api/camera?target=stub", `{"hostCamera":"Insta360 Link 2"}`)
	if w.Code != http.StatusOK || f.name != "Insta360 Link 2" || f.got != nil {
		t.Fatalf("status %d %s, staged %q, image %d bytes; want 200 and the camera only", w.Code, w.Body, f.name, len(f.got))
	}
	if s := w.Body.String(); !strings.Contains(s, `"applies":"boot"`) || !strings.Contains(s, `"hostCamera":"Insta360 Link 2"`) {
		t.Errorf("answer %s; want applies boot and the camera", s)
	}
	f.live = true
	w = serveJSON(t, f, http.MethodPost, "/api/camera?target=stub", `{"hostCamera":"Insta360 Link 2"}`)
	if !strings.Contains(w.Body.String(), `"applies":"live"`) {
		t.Errorf("answer %s; want applies live", w.Body)
	}
}

func TestCameraJSONErrors(t *testing.T) {
	f := &hostCameraTarget{cams: []target.HostCamera{{Name: "A", Label: "webcam0"}}}
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"unknown camera", `{"hostCamera":"B"}`, http.StatusNotFound},
		{"no name", `{}`, http.StatusBadRequest},
		{"not JSON", `nope`, http.StatusBadRequest},
	} {
		if w := serveJSON(t, f, http.MethodPost, "/api/camera?target=stub", tc.body); w.Code != tc.want {
			t.Errorf("%s: status %d %s; want %d", tc.name, w.Code, w.Body, tc.want)
		}
	}
}

// A simulator or iPhone takes images only.
func TestCameraJSONIs501ForAnImageOnlyTarget(t *testing.T) {
	f := &cameraTarget{}
	w := serveJSON(t, f, http.MethodPost, "/api/camera?target=stub", `{"hostCamera":"A"}`)
	if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "AVDs only") {
		t.Errorf("status %d %s; want 501 naming AVDs", w.Code, w.Body)
	}
	if w := serveJSON(t, &stubTarget{}, http.MethodPost, "/api/camera?target=stub", `{"hostCamera":"A"}`); w.Code != http.StatusNotImplemented {
		t.Errorf("no camera at all: status %d; want 501", w.Code)
	}
}

func TestCameraRawImageStillWorksOnAHostCameraTarget(t *testing.T) {
	f := &hostCameraTarget{}
	body := testImage(t, 40, 30, encodePNG)
	w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", bytes.NewReader(body))
	if w.Code != http.StatusOK || !bytes.Equal(f.got, body) || f.name != "" {
		t.Errorf("status %d %s; want the image staged and no camera", w.Code, w.Body)
	}
}

func TestCameraSources(t *testing.T) {
	f := &hostCameraTarget{cams: []target.HostCamera{{Name: "Insta360 Link 2", Label: "webcam0"}, {Name: "FaceTime HD Camera", Label: "webcam1"}}}
	w := serveAgent(t, f, http.MethodGet, "/api/camera/sources?target=stub", nil)
	want := `{"target":"stub","cameras":[{"name":"Insta360 Link 2","label":"webcam0"},{"name":"FaceTime HD Camera","label":"webcam1"}]}`
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
		t.Errorf("status %d %s; want %s", w.Code, w.Body, want)
	}
	if w := serveAgent(t, &hostCameraTarget{}, http.MethodGet, "/api/camera/sources?target=stub", nil); !strings.Contains(w.Body.String(), `"cameras":[]`) {
		t.Errorf("no cameras: %s; want an empty list, not null", w.Body)
	}
	if w := serveAgent(t, &cameraTarget{}, http.MethodGet, "/api/camera/sources?target=stub", nil); w.Code != http.StatusNotImplemented {
		t.Errorf("image-only target: status %d; want 501", w.Code)
	}
	if w := serveAgent(t, &hostCameraTarget{cams: nil, cameraTarget: cameraTarget{err: errors.New("no emulator")}}, http.MethodGet, "/api/camera/sources?target=stub", nil); w.Code != http.StatusBadGateway {
		t.Errorf("lister fails: status %d; want 502", w.Code)
	}
}

// liveCameraTarget takes frames live, as an AVD on Handset Camera does.
type liveCameraTarget struct {
	cameraTarget
	handled bool
	liveErr error
	frames  [][]byte
	stops   int
}

func (f *liveCameraTarget) StopCameraFeed() bool {
	if !f.handled {
		return false
	}
	f.stops++
	return true
}

func (f *liveCameraTarget) FeedCameraFrame(ctx context.Context, frame []byte) (target.CameraApplies, bool, error) {
	if !f.handled {
		return "", false, nil
	}
	f.frames = append(f.frames, frame)
	return target.CameraLive, true, f.liveErr
}

// A live camera takes the JPEG as sent, with no PNG round trip, and the
// staged image is left alone.
func TestCameraFeedsALiveCameraTheFrameAsSent(t *testing.T) {
	body := testImage(t, 64, 48, encodeJPEG)
	f := &liveCameraTarget{handled: true}
	w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", bytes.NewReader(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s; want 200", w.Code, w.Body)
	}
	if len(f.frames) != 1 || !bytes.Equal(f.frames[0], body) || f.got != nil {
		t.Errorf("live got %d frames, staged %d bytes; want the JPEG live and nothing staged", len(f.frames), len(f.got))
	}
	if s := w.Body.String(); !strings.Contains(s, `"applies":"live"`) || !strings.Contains(s, `"hostCamera":"Handset Camera"`) {
		t.Errorf("answer %s; want live on Handset Camera", s)
	}
}

func TestCameraStagesWhenALiveCameraDeclines(t *testing.T) {
	f := &liveCameraTarget{}
	w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", bytes.NewReader(testImage(t, 40, 30, encodePNG)))
	if w.Code != http.StatusOK || f.got == nil {
		t.Errorf("status %d, staged %d bytes; want 200 and the image staged", w.Code, len(f.got))
	}
}

func TestCameraLiveFeedErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{camfeed.ErrNoCamera, http.StatusServiceUnavailable},
		{camfeed.ErrNoHelper, http.StatusServiceUnavailable},
		{errors.New("helper crashed"), http.StatusBadGateway},
	} {
		f := &liveCameraTarget{handled: true, liveErr: tc.err}
		w := serveAgent(t, f, http.MethodPost, "/api/camera?target=stub", bytes.NewReader(testImage(t, 40, 30, encodePNG)))
		if w.Code != tc.want {
			t.Errorf("%v: status %d; want %d", tc.err, w.Code, tc.want)
		}
	}
}

// DELETE /api/camera/feed blanks the live camera and leaves it staged.
func TestCameraFeedStop(t *testing.T) {
	f := &liveCameraTarget{handled: true}
	w := serveAgent(t, f, http.MethodDelete, "/api/camera/feed?target=stub", nil)
	if w.Code != http.StatusOK || f.stops != 1 || f.cleared {
		t.Errorf("status %d %s, %d stops, cleared %v; want 200, one stop, still staged", w.Code, w.Body, f.stops, f.cleared)
	}
	if w := serveAgent(t, &liveCameraTarget{}, http.MethodDelete, "/api/camera/feed?target=stub", nil); w.Code != http.StatusConflict {
		t.Errorf("not on Handset Camera: status %d; want 409", w.Code)
	}
	if w := serveAgent(t, &cameraTarget{}, http.MethodDelete, "/api/camera/feed?target=stub", nil); w.Code != http.StatusNotImplemented {
		t.Errorf("no live camera: status %d; want 501", w.Code)
	}
}
