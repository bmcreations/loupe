package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func extServer() *Server {
	return &Server{token: "secret", exts: newExtensionRegistry()}
}

func extDo(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestExtensionRegistrationAcceptsOnlyLoopback(t *testing.T) {
	s := extServer()
	for _, u := range []string{
		"http://10.0.0.5:8080",
		"http://example.com",
		"http://100.101.102.103:18494",
		"file:///etc/passwd",
		"http://user:pw@127.0.0.1:1",
		"http://127.0.0.1:1/?x=1",
	} {
		rec := extDo(s, http.MethodPost, "/api/extensions", `{"name":"appmap","url":"`+u+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", u, rec.Code)
		}
	}
	for _, u := range []string{"http://127.0.0.1:18494", "http://[::1]:18494", "http://localhost:18494/base"} {
		if rec := extDo(s, http.MethodPost, "/api/extensions", `{"name":"appmap","url":"`+u+`"}`); rec.Code != http.StatusOK {
			t.Errorf("%s: status %d: %s", u, rec.Code, rec.Body)
		}
	}
	if rec := extDo(s, http.MethodPost, "/api/extensions", `{"name":"App Map","url":"http://127.0.0.1:1"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: status %d, want 400", rec.Code)
	}
}

func TestExtensionProxyPassesThroughWithoutLoupesCredentials(t *testing.T) {
	var got *http.Request
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("png"))
	}))
	defer ext.Close()
	s := extServer()
	if rec := extDo(s, http.MethodPost, "/api/extensions", `{"name":"appmap","url":"`+ext.URL+`","label":"App map"}`); rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}

	rec := extDo(s, http.MethodGet, "/api/ext/appmap/maps/m1/screens/s1/thumb.png?target=avd:P&token=secret", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "png" || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("proxy: %d %q %q", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}
	if got.URL.Path != "/maps/m1/screens/s1/thumb.png" {
		t.Errorf("path = %q", got.URL.Path)
	}
	if got.URL.Query().Get("target") != "avd:P" || got.URL.Query().Has("token") {
		t.Errorf("query = %q, want target kept and token gone", got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "" {
		t.Errorf("Authorization reached the extension")
	}

	var list ExtensionsResponse
	json.Unmarshal(extDo(s, http.MethodGet, "/api/extensions", "").Body.Bytes(), &list)
	if len(list.Extensions) != 1 || list.Extensions[0].Path != "/api/ext/appmap/" || list.Extensions[0].Label != "App map" {
		t.Fatalf("list = %+v", list)
	}
}

func TestExtensionProxyNeedsLoupesToken(t *testing.T) {
	s := extServer()
	extDo(s, http.MethodPost, "/api/extensions", `{"name":"appmap","url":"http://127.0.0.1:1"}`)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ext/appmap/maps", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestAnExtensionThatStopsAnsweringDropsOut(t *testing.T) {
	ext := httptest.NewServer(http.NotFoundHandler())
	url := ext.URL
	ext.Close()
	s := extServer()
	extDo(s, http.MethodPost, "/api/extensions", `{"name":"appmap","url":"`+url+`"}`)
	if rec := extDo(s, http.MethodGet, "/api/ext/appmap/maps", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	if rec := extDo(s, http.MethodGet, "/api/ext/appmap/maps", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("after a failure: status %d, want 404", rec.Code)
	}
}

func TestAnExtensionRegistrationExpires(t *testing.T) {
	s := extServer()
	now := time.Now()
	s.exts.now = func() time.Time { return now }
	extDo(s, http.MethodPost, "/api/extensions", `{"name":"appmap","url":"http://127.0.0.1:1"}`)
	now = now.Add(extensionTTL + time.Second)
	if got := s.exts.list(); len(got) != 0 {
		t.Fatalf("list = %+v, want it expired", got)
	}
}
