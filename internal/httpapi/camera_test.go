package httpapi

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	pngcodec "image/png"
	"net/http"
	"strings"
	"testing"

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
