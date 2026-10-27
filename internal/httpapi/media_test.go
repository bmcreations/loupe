package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// mediaTarget is a MediaAdder that keeps what it was handed.
type mediaTarget struct {
	stubTarget
	err     error
	gotBody []byte
	gotExt  string
	gotName string
	gotKind target.MediaKind
}

func (f *mediaTarget) AddMedia(ctx context.Context, path, name string, kind target.MediaKind) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f.gotBody, f.gotExt, f.gotName, f.gotKind = b, filepath.Ext(path), name, kind
	return f.err
}

const png = "\x89PNG\r\n\x1a\n rest of the picture"

func TestMediaAddsAPictureUnderTheServersName(t *testing.T) {
	f := &mediaTarget{}
	w := serveAgent(t, f, http.MethodPost, "/api/media?target=stub", strings.NewReader(png))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s; want 200", w.Code, w.Body)
	}
	if string(f.gotBody) != png {
		t.Errorf("staged %q; want the body unchanged", f.gotBody)
	}
	// simctl reads the type from the staged file's extension.
	if f.gotExt != ".png" || !strings.HasSuffix(f.gotName, ".png") || !strings.HasPrefix(f.gotName, "loupe-") {
		t.Errorf("staged ext %q, device name %q; want .png and loupe-*.png", f.gotExt, f.gotName)
	}
}

func TestMediaRefusesWhatIsNotAPhotoOrVideo(t *testing.T) {
	f := &mediaTarget{}
	w := serveAgent(t, f, http.MethodPost, "/api/media?target=stub", strings.NewReader(apk))
	if w.Code != http.StatusBadRequest || f.gotBody != nil {
		t.Errorf("status %d, target given %d bytes; want 400 and nothing", w.Code, len(f.gotBody))
	}
}

func TestMediaIs501WithoutTheCapability(t *testing.T) {
	w := serveAgent(t, &stubTarget{}, http.MethodPost, "/api/media?target=stub", strings.NewReader(png))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status %d %s; want 501", w.Code, w.Body)
	}
}

func TestMediaDeviceFailureIs502(t *testing.T) {
	f := &mediaTarget{err: errors.New("adb push: no space left")}
	w := serveAgent(t, f, http.MethodPost, "/api/media?target=stub", strings.NewReader(png))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "no space left") {
		t.Errorf("status %d %s; want 502 naming the cause", w.Code, w.Body)
	}
}

func TestMediaSizeLimit(t *testing.T) {
	defer func(n int64) { maxMediaBytes = n }(maxMediaBytes)
	maxMediaBytes = 64
	big := png + strings.Repeat("x", 100)

	f := &mediaTarget{}
	w := serveAgent(t, f, http.MethodPost, "/api/media?target=stub", strings.NewReader(big))
	if w.Code != http.StatusRequestEntityTooLarge || f.gotBody != nil {
		t.Errorf("declared length: status %d; want 413 and nothing staged", w.Code)
	}
	f = &mediaTarget{}
	w = serveAgent(t, f, http.MethodPost, "/api/media?target=stub", io.MultiReader(strings.NewReader(big)))
	if w.Code != http.StatusRequestEntityTooLarge || f.gotBody != nil {
		t.Errorf("chunked: status %d %s; want 413 and nothing staged", w.Code, w.Body)
	}
}
