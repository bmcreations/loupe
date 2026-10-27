package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func manifestStart(t *testing.T, s *Server, r *http.Request) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleManifest(rec, r)
	if ct := rec.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var m struct {
		StartURL string `json:"start_url"`
		Display  string `json:"display"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	if m.Display != "standalone" {
		t.Errorf("display = %q, want standalone", m.Display)
	}
	return m.StartURL
}

func TestManifestCarriesTheTokenOnlyToAHolder(t *testing.T) {
	// A Home Screen app keeps its own cookies, so its first launch has only
	// start_url to authenticate with.
	s := &Server{token: "secret"}
	r := httptest.NewRequest(http.MethodGet, manifestPath, nil)
	r.AddCookie(&http.Cookie{Name: tokenCookie, Value: "secret"})
	if got := manifestStart(t, s, r); got != "/?token=secret" {
		t.Errorf("start_url with the cookie = %q, want /?token=secret", got)
	}

	for name, r := range map[string]*http.Request{
		"none":  httptest.NewRequest(http.MethodGet, manifestPath, nil),
		"wrong": httptest.NewRequest(http.MethodGet, manifestPath+"?token=nope", nil),
	} {
		if got := manifestStart(t, s, r); got != "/" {
			t.Errorf("start_url with %s token = %q, want /", name, got)
		}
	}
}

func TestManifestWithAuthOffHasNoToken(t *testing.T) {
	if got := manifestStart(t, &Server{}, httptest.NewRequest(http.MethodGet, manifestPath, nil)); got != "/" {
		t.Errorf("start_url = %q, want /", got)
	}
}

func TestManifestFollowsThePageTheme(t *testing.T) {
	for query, want := range map[string]string{"": "#0b0d10", "?theme=dark": "#0b0d10", "?theme=light": "#f2f3f5", "?theme=x": "#0b0d10"} {
		rec := httptest.NewRecorder()
		(&Server{}).handleManifest(rec, httptest.NewRequest(http.MethodGet, manifestPath+query, nil))
		var m struct {
			Theme string `json:"theme_color"`
			Back  string `json:"background_color"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if m.Theme != want || m.Back != want {
			t.Errorf("%q: theme %q background %q, want %q", query, m.Theme, m.Back, want)
		}
	}
}

func TestAuthLeavesHomeScreenAssetsOpen(t *testing.T) {
	for _, p := range []string{manifestPath, "/icon-180.png", "/icon-192.png", "/icon-512.png", "/icon.svg"} {
		_, h, reached := authTestServer("secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if !*reached {
			t.Errorf("%s was gated (status %d)", p, rec.Code)
		}
	}
	// Only those: a lookalike path stays behind the token.
	_, h, reached := authTestServer("secret")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/icon-180.png.bak", nil))
	if *reached {
		t.Error("/icon-180.png.bak reached the handler without a token")
	}
}
