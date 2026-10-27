package httpapi

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// authTestServer builds a Server with the middleware under test wrapped around
// a handler that reports whether it ran.
func authTestServer(token string) (*Server, http.Handler, *bool) {
	s := &Server{token: token}
	reached := new(bool)
	h := s.authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}))
	return s, h, reached
}

func TestAuthRejectsARequestWithNoToken(t *testing.T) {
	_, h, reached := authTestServer("secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if *reached {
		t.Error("the handler ran for an unauthenticated request")
	}
}

func TestAuthAcceptsBearerCookieAndQuery(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*http.Request)
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") }},
		{"cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: tokenCookie, Value: "secret"}) }},
		{"query", func(r *http.Request) { r.URL.RawQuery = "token=secret" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, h, reached := authTestServer("secret")
			// An API path, so the query case is not redirected away.
			r := httptest.NewRequest(http.MethodGet, "/api/targets", nil)
			c.mut(r)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if !*reached {
				t.Errorf("a valid %s token did not reach the handler (status %d)", c.name, rec.Code)
			}
		})
	}
}

func TestAuthRejectsAWrongToken(t *testing.T) {
	for _, bad := range []string{"", "wrong", "secre", "secrett", "SECRET"} {
		_, h, reached := authTestServer("secret")
		r := httptest.NewRequest(http.MethodGet, "/api/targets", nil)
		r.Header.Set("Authorization", "Bearer "+bad)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if *reached {
			t.Errorf("token %q was accepted", bad)
		}
	}
}

// TestAuthStripsTheTokenFromABrowserURL covers the leak: a token left in the
// address bar goes into history, and into the Referer of anything the page
// later fetches.
func TestAuthStripsTheTokenFromABrowserURL(t *testing.T) {
	_, h, _ := authTestServer("secret")
	r := httptest.NewRequest(http.MethodGet, "/?token=secret&transport=ws", nil)
	r.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if loc.Query().Has("token") {
		t.Errorf("Location %q still carries the token", loc)
	}
	// Everything else on the URL has to survive, or ?transport=ws stops being
	// a way to compare the transports.
	if loc.Query().Get("transport") != "ws" {
		t.Errorf("Location %q dropped the other query parameters", loc)
	}

	cookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, tokenCookie+"=secret") {
		t.Errorf("Set-Cookie = %q, want the token", cookie)
	}
	if !strings.Contains(cookie, "SameSite=Strict") {
		t.Errorf("Set-Cookie = %q, want SameSite=Strict: /api/input drives a "+
			"real device, so a cross-site request must not carry the cookie", cookie)
	}
	if !strings.Contains(cookie, "Secure") {
		t.Errorf("Set-Cookie = %q, want Secure over TLS", cookie)
	}
}

// TestAuthDoesNotRedirectTheTransportEndpoints is the one that would fail
// silently in a browser. WebTransport sends no cookies, so /wt must be allowed
// to authenticate by query string on every connect; redirecting it would strip
// the only credential it has, and a 303 is not something a WebTransport client
// follows -- it would surface as trap 11's symptom, a transport that fails
// with nothing in the log.
func TestAuthDoesNotRedirectTheTransportEndpoints(t *testing.T) {
	for _, path := range []string{"/wt", "/ws", "/api/ui"} {
		_, h, reached := authTestServer("secret")
		r := httptest.NewRequest(http.MethodGet, path+"?target=x&token=secret", nil)
		r.TLS = &tls.ConnectionState{}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if rec.Code == http.StatusSeeOther {
			t.Errorf("%s was redirected; the token it needs would be stripped", path)
		}
		if !*reached {
			t.Errorf("%s did not reach the handler (status %d)", path, rec.Code)
		}
	}
}

// TestAuthCookieIsNotSecureWithoutTLS guards a lockout rather than a leak: a
// Secure cookie is discarded on a plain-http origin, and since the redirect
// removes the token from the URL, the next request would carry neither.
func TestAuthCookieIsNotSecureWithoutTLS(t *testing.T) {
	_, h, _ := authTestServer("secret")
	r := httptest.NewRequest(http.MethodGet, "/?token=secret", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if strings.Contains(rec.Header().Get("Set-Cookie"), "Secure") {
		t.Errorf("Set-Cookie = %q, want no Secure over plain http", rec.Header().Get("Set-Cookie"))
	}
}

func TestAuthLeavesHealthzOpen(t *testing.T) {
	// The supervisor polls it to decide whether the process came up, and it
	// has no way to learn the token.
	_, h, reached := authTestServer("secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !*reached {
		t.Errorf("/healthz was gated (status %d)", rec.Code)
	}
}

func TestNoTokenDisablesTheCheck(t *testing.T) {
	_, h, reached := authTestServer("")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/input?target=x", nil))
	if !*reached {
		t.Errorf("-no-auth did not pass the request through (status %d)", rec.Code)
	}
}

func TestNewTokenIsUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok := NewToken()
		if len(tok) != 64 {
			t.Fatalf("token %q is %d chars, want 64 hex", tok, len(tok))
		}
		if seen[tok] {
			t.Fatal("NewToken repeated a value")
		}
		seen[tok] = true
	}
}
