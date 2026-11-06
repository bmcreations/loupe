package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

func presetsServer(t *testing.T, tgt target.Target) *Server {
	t.Helper()
	s := settingsServer(t, tgt)
	s.presets = &presetStore{}
	return s
}

func putPreset(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handlePresets(w, httptest.NewRequest(http.MethodPut, "/api/presets", strings.NewReader(body)))
	return w
}

func decodePresets(t *testing.T, w *httptest.ResponseRecorder) []Preset {
	t.Helper()
	var got PresetsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return got.Presets
}

func TestPresetsSaveListReplaceDelete(t *testing.T) {
	s := presetsServer(t, &settingsTarget{})

	if w := putPreset(t, s, `{"name":"Large text","settings":{"fontScale":1.3}}`); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	putPreset(t, s, `{"name":"Dark","settings":{"darkMode":true}}`)
	w := putPreset(t, s, `{"name":"Large text","settings":{"fontScale":2}}`)
	got := decodePresets(t, w)
	if len(got) != 2 || got[0].Name != "Dark" || got[1].Name != "Large text" || got[1].Settings["fontScale"] != 2.0 {
		t.Fatalf("after replace: %+v", got)
	}

	w = httptest.NewRecorder()
	s.handlePresets(w, httptest.NewRequest(http.MethodDelete, "/api/presets?name=Dark", nil))
	if got := decodePresets(t, w); w.Code != http.StatusOK || len(got) != 1 {
		t.Fatalf("delete: %d %+v", w.Code, got)
	}
	w = httptest.NewRecorder()
	s.handlePresets(w, httptest.NewRequest(http.MethodDelete, "/api/presets?name=Dark", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", w.Code)
	}
}

func TestPresetsRejectsBadShapes(t *testing.T) {
	s := presetsServer(t, &settingsTarget{})
	for _, body := range []string{
		`{"name":"","settings":{"darkMode":true}}`,
		`{"name":" padded","settings":{"darkMode":true}}`,
		`{"name":"empty","settings":{}}`,
		`{"name":"` + strings.Repeat("x", maxPresetName+1) + `","settings":{"darkMode":true}}`,
		`not json`,
	} {
		if w := putPreset(t, s, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, w.Code)
		}
	}
}

func TestPresetsPersistAcrossLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.json")
	s := presetsServer(t, &settingsTarget{})
	if err := s.SetPresetsPath(path); err != nil {
		t.Fatal(err)
	}
	putPreset(t, s, `{"name":"Dark","settings":{"darkMode":true}}`)

	again := presetsServer(t, &settingsTarget{})
	if err := again.SetPresetsPath(path); err != nil {
		t.Fatal(err)
	}
	if pr, ok := again.presets.get("Dark"); !ok || pr.Settings["darkMode"] != true {
		t.Fatalf("reloaded: %+v %v", pr, ok)
	}
}

func TestPresetApplySkipsWhatTheDeviceLacks(t *testing.T) {
	settings := append(sampleSettings(), target.Setting{ID: "talkBack", Type: target.SettingBool, Value: false, Supported: false})
	tgt := &settingsTarget{settings: settings}
	s := presetsServer(t, tgt)
	putPreset(t, s, `{"name":"Fold dark","settings":{"darkMode":true,"posture":"half-open","talkBack":true}}`)

	w := httptest.NewRecorder()
	s.handlePresetApply(w, httptest.NewRequest(http.MethodPost, "/api/presets/apply?name=Fold+dark&target=settings-target", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(tgt.gotChanges, map[string]any{"darkMode": true}) {
		t.Errorf("applied %v, want only darkMode", tgt.gotChanges)
	}
	var got PresetApplyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Skipped, []string{"posture", "talkBack"}) || len(got.Settings) != 3 {
		t.Errorf("got %+v", got)
	}
}

func TestPresetApplyWithNothingApplicableIs409(t *testing.T) {
	tgt := &settingsTarget{settings: sampleSettings()}
	s := presetsServer(t, tgt)
	putPreset(t, s, `{"name":"Fold","settings":{"posture":"closed"}}`)

	w := httptest.NewRecorder()
	s.handlePresetApply(w, httptest.NewRequest(http.MethodPost, "/api/presets/apply?name=Fold&target=settings-target", nil))
	if w.Code != http.StatusConflict || tgt.gotChanges != nil {
		t.Errorf("status %d, applied %v; want 409 and nothing applied", w.Code, tgt.gotChanges)
	}
}

func TestPresetApplyErrors(t *testing.T) {
	tgt := &settingsTarget{settings: sampleSettings(), applyErr: target.ErrInvalidSetting}
	s := presetsServer(t, tgt)
	putPreset(t, s, `{"name":"Bad","settings":{"fontScale":9}}`)

	for _, tc := range []struct {
		url  string
		want int
	}{
		{"/api/presets/apply?name=Missing&target=settings-target", http.StatusNotFound},
		{"/api/presets/apply?name=Bad&target=nope", http.StatusNotFound},
		{"/api/presets/apply?name=Bad&target=settings-target", http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		s.handlePresetApply(w, httptest.NewRequest(http.MethodPost, tc.url, nil))
		if w.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.url, w.Code, tc.want, w.Body.String())
		}
	}

	tgt.applyErr = errors.New("adb went away")
	tgt.failed = []string{"fontScale"}
	w := httptest.NewRecorder()
	s.handlePresetApply(w, httptest.NewRequest(http.MethodPost, "/api/presets/apply?name=Bad&target=settings-target", nil))
	if w.Code != http.StatusConflict {
		t.Errorf("partial failure: %d, want 409", w.Code)
	}
}

func TestPresetsLoadDropsInvalidEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.json")
	if err := os.WriteFile(path, []byte(`{"presets":[{"name":"","settings":{"a":1}},{"name":"ok","settings":{"darkMode":true}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &presetStore{}
	if err := p.load(path); err != nil {
		t.Fatal(err)
	}
	if got := p.list(); len(got) != 1 || got[0].Name != "ok" {
		t.Errorf("loaded %+v", got)
	}
}
