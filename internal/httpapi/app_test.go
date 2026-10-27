package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// appTarget is a stubTarget that also reports a foreground app, mirroring
// framedTarget's shape in frame_test.go: the interface-having and
// interface-lacking cases are the same underlying stub, present or absent.
type appTarget struct {
	stubTarget
	app *target.App
	err error
}

func (a *appTarget) ID() string    { return "app-target" }
func (a *appTarget) Label() string { return "app-target" }

func (a *appTarget) ForegroundApp(context.Context) (*target.App, error) {
	if a.err != nil {
		return nil, a.err
	}
	return a.app, nil
}

func appServer(t *testing.T, tgt target.Target) *Server {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(tgt)
	return &Server{mgr: m}
}

func TestHandleAppReturnsTheForegroundApp(t *testing.T) {
	s := appServer(t, &appTarget{app: &target.App{
		Package: "com.android.settings", Activity: "com.android.settings.homepage.SettingsHomepageActivity",
		VersionName: "17", VersionCode: 37, PID: 28446,
		APKPath: "/system_ext/priv-app/SettingsGoogle/SettingsGoogle.apk",
	}})

	w := httptest.NewRecorder()
	s.handleApp(w, httptest.NewRequest(http.MethodGet, "/api/app?target=app-target", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got target.App
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Package != "com.android.settings" || got.VersionCode != 37 {
		t.Errorf("got %+v", got)
	}
}

// A target without AppInfo -- synthetic, in production -- answers 501 rather
// than a broken response, the same as Inspector and Screenshotter.
func TestHandleAppReturns501WithoutTheInterface(t *testing.T) {
	s := appServer(t, &stubTarget{})

	w := httptest.NewRecorder()
	s.handleApp(w, httptest.NewRequest(http.MethodGet, "/api/app?target=stub", nil))

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleAppReturns502WhenTheCallFails(t *testing.T) {
	s := appServer(t, &appTarget{err: errors.New("dumpsys: device offline")})

	w := httptest.NewRecorder()
	s.handleApp(w, httptest.NewRequest(http.MethodGet, "/api/app?target=app-target", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleAppReturns400WithNoTarget(t *testing.T) {
	s := appServer(t, &stubTarget{})

	w := httptest.NewRecorder()
	s.handleApp(w, httptest.NewRequest(http.MethodGet, "/api/app", nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleAppReturns404ForAnUnknownTarget(t *testing.T) {
	s := appServer(t, &stubTarget{})

	w := httptest.NewRecorder()
	s.handleApp(w, httptest.NewRequest(http.MethodGet, "/api/app?target=missing", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

// item 12: /api/app accepts GET only.
func TestHandleAppRejectsOtherMethods(t *testing.T) {
	s := appServer(t, &appTarget{app: &target.App{Package: "com.android.settings"}})

	w := httptest.NewRecorder()
	s.handleApp(w, httptest.NewRequest(http.MethodPost, "/api/app?target=app-target", nil))

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body: %s", w.Code, w.Body.String())
	}
}
