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

func TestExtensionPageMustBeAPlainPathUnderTheExtension(t *testing.T) {
	s := extServer()
	for _, page := range []string{
		"builds",
		"/../api/targets",
		"/a/..",
		"/%2e%2e/api/targets",
		"/%2E%2E/api/targets",
		"/x?y=1",
		"/x%3Fy=1",
		"/x#top",
		`/a\b`,
		"/a%5cb",
		"/a\nb",
		"/a%0ab",
		"/%zz",
		"/" + strings.Repeat("a", 200),
	} {
		body, _ := json.Marshal(map[string]string{"name": "slipway", "url": "http://127.0.0.1:1", "page": page})
		if rec := extDo(s, http.MethodPost, "/api/extensions", string(body)); rec.Code != http.StatusBadRequest {
			t.Errorf("page %q: status %d, want 400", page, rec.Code)
		}
	}
	for _, page := range []string{"/", "/index.html", "/builds/latest", "/" + strings.Repeat("a", 199)} {
		body, _ := json.Marshal(map[string]string{"name": "slipway", "url": "http://127.0.0.1:1", "page": page})
		if rec := extDo(s, http.MethodPost, "/api/extensions", string(body)); rec.Code != http.StatusOK {
			t.Errorf("page %q: status %d: %s", page, rec.Code, rec.Body)
		}
	}
}

func TestExtensionPageIsListedAsALoupePath(t *testing.T) {
	s := extServer()
	rec := extDo(s, http.MethodPost, "/api/extensions", `{"name":"slipway","url":"http://127.0.0.1:1/base","label":"Slipway","page":"/"}`)
	var got Extension
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Page != "/api/ext/slipway/" {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	extDo(s, http.MethodPost, "/api/extensions", `{"name":"appmap","url":"http://127.0.0.1:2"}`)

	raw := extDo(s, http.MethodGet, "/api/extensions", "").Body.String()
	var list ExtensionsResponse
	json.Unmarshal([]byte(raw), &list)
	if len(list.Extensions) != 2 || list.Extensions[1].Page != "/api/ext/slipway/" {
		t.Fatalf("list = %s", raw)
	}
	// An extension with no page has no page key at all, not an empty one.
	if strings.Count(raw, `"page"`) != 1 {
		t.Errorf("list = %s, want page only on slipway", raw)
	}
}

// An extension written for a newer loupe sends fields this one does not
// know. It has to register anyway, so the decoder must not refuse them.
func TestExtensionRegistrationIgnoresUnknownFields(t *testing.T) {
	s := extServer()
	if rec := extDo(s, http.MethodPost, "/api/extensions", `{"name":"slipway","url":"http://127.0.0.1:1","icon":"ship"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

// What an extension sees of a browser's request, and what loupe adds to the
// response, as docs/extensions.md states them. The request is the one a page
// in the web client's frame makes: the cookie, and loupe's own origin.
func TestExtensionProxyHeaders(t *testing.T) {
	var got *http.Request
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Set-Cookie", "ext=1; Path=/")
		w.Write([]byte("<html>"))
	}))
	defer ext.Close()
	s := extServer()
	extDo(s, http.MethodPost, "/api/extensions", `{"name":"slipway","url":"`+ext.URL+`","page":"/"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/ext/slipway/builds", strings.NewReader("{}"))
	req.Header.Set("Cookie", tokenCookie+"=secret")
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("Forwarded", "for=203.0.113.9")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || got == nil {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if want := strings.TrimPrefix(ext.URL, "http://"); got.Host != want {
		t.Errorf("Host = %q, want the extension's own %q", got.Host, want)
	}
	if o := got.Header.Get("Origin"); o != "http://example.com" {
		t.Errorf("Origin = %q, want loupe's origin passed through", o)
	}
	for _, h := range []string{"Cookie", "Authorization", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if v := got.Header.Get(h); v != "" {
			t.Errorf("%s = %q reached the extension", h, v)
		}
	}

	if v := rec.Header().Get("Referrer-Policy"); v != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want loupe's no-referrer", v)
	}
	if v := rec.Header().Get("Set-Cookie"); v != "ext=1; Path=/" {
		t.Errorf("Set-Cookie = %q, want the extension's own", v)
	}
	for _, h := range []string{"X-Frame-Options", "Content-Security-Policy"} {
		if v := rec.Header().Get(h); v != "" {
			t.Errorf("%s = %q: loupe must not stop its own page framing an extension", h, v)
		}
	}
}

func TestExtensionPageIsServedFromUnderTheExtensionsURL(t *testing.T) {
	var path string
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path = r.URL.Path }))
	defer ext.Close()
	s := extServer()
	extDo(s, http.MethodPost, "/api/extensions", `{"name":"slipway","url":"`+ext.URL+`/base","page":"/"}`)
	if rec := extDo(s, http.MethodGet, "/api/ext/slipway/", ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if path != "/base/" {
		t.Errorf("extension saw %q, want /base/", path)
	}
}
