package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
)

// tokenCookie holds the shared token once a request has presented it in the
// query string, so a phone can open one bookmarked link and keep working.
const tokenCookie = "loupe_token"

// NewToken returns a fresh shared secret. 32 bytes because this guards a
// device someone can drive, not a page they can read.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("loupe: no entropy for a token: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// SetToken enables authentication. An empty token disables it, which is what
// -no-auth does; the warning belongs at startup, not here.
func (s *Server) SetToken(t string) { s.token = t }

// authed wraps a handler with the token check.
//
// Three ways in, because three kinds of caller cannot all use the same one:
// an agent sends Authorization, a browser that has already been here sends the
// cookie, and a freshly opened link carries the token in the query string. The
// query form is how a phone bookmark works at all, so it is not optional -- it
// is also the leakiest, which is why a browser navigation is redirected to
// strip it once the cookie is set.
//
// The cookie is the one credential a browser attaches on its own, so it is the
// one that needs an Origin check. SameSite=Strict stops another site, but a
// site is scheme and registrable domain: a page on another port of this host
// is same-site and its requests carry the cookie. Bearer and ?token= need no
// check, because a page that can present the token was not forged by someone
// who lacks it.
func (s *Server) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every response, because the token rides in the first URL a phone
		// opens and in the /wt URL, and a Referer would carry it to whatever
		// the page links to.
		w.Header().Set("Referrer-Policy", "no-referrer")

		if s.token == "" || r.URL.Path == "/healthz" || isHomeScreenAsset(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		if s.tokenMatches(bearer(r)) {
			next.ServeHTTP(w, r)
			return
		}

		q := r.URL.Query().Get("token")
		if s.tokenMatches(cookieToken(r)) && !s.tokenMatches(q) {
			if !sameOrigin(r) {
				writeJSON(w, http.StatusForbidden, ErrorResponse{
					Error: "loupe: the cookie was sent from another origin (" +
						r.Header.Get("Origin") + "). Open this server's own page, " +
						"or send Authorization: Bearer <token>.",
				})
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if s.tokenMatches(q) {
			http.SetCookie(w, &http.Cookie{
				Name:  tokenCookie,
				Value: s.token,
				Path:  "/",
				// Strict rather than Lax: /api/input drives a real device, and
				// a cookie is attached by the browser whoever asked for the
				// request. Strict is what stops another page on the tailnet
				// from issuing one.
				SameSite: http.SameSiteStrictMode,
				// Secure only over TLS. Setting it unconditionally would make
				// the cookie unstorable on a plain-http run, and since the
				// redirect below strips the token from the URL, the next
				// request would arrive with neither and 401 -- turning the
				// flag into a lockout rather than a hardening.
				Secure: r.TLS != nil,
				// Readable by script on purpose. The client has to put the
				// token on the /wt URL itself, because WebTransport sends no
				// cookies, and the token is in the address bar of the link
				// that got here anyway.
				HttpOnly: false,
				MaxAge:   int((30 * 24 * 60 * 60)),
			})
			// Only a page load is worth redirecting. Rewriting a transport or
			// API URL would turn one round trip into two and, for /wt, would
			// drop the token the handler still needs.
			if r.Method == http.MethodGet && isNavigation(r.URL.Path) {
				stripped := *r.URL
				vals := stripped.Query()
				vals.Del("token")
				stripped.RawQuery = vals.Encode()
				http.Redirect(w, r, stripped.RequestURI(), http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="loupe"`)
		http.Error(w, "loupe: a token is required. Open the link the server "+
			"printed at startup, or send Authorization: Bearer <token>.",
			http.StatusUnauthorized)
	})
}

// sameOrigin reports whether a request's Origin, if it sent one, is this
// server as the request reached it: the scheme the connection used, and the
// Host it asked for. A missing Origin passes, because the native Mac app's
// URLSession sends none and a browser always does on the requests that
// matter here -- a POST, a WebSocket upgrade, a WebTransport CONNECT.
// "null", from a sandboxed frame or a file:// page, is an origin and fails.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return strings.EqualFold(u.Scheme, scheme) &&
		strings.EqualFold(withPort(u.Host, scheme), withPort(r.Host, scheme))
}

// withPort spells out the scheme's default port, so https://host and
// https://host:443 compare equal.
func withPort(host, scheme string) string {
	if u := (&url.URL{Host: host}); u.Port() != "" {
		return host
	}
	if scheme == "https" {
		return host + ":443"
	}
	return host + ":80"
}

// isHomeScreenAsset reports whether a path is one of the files iOS reads to
// add the page to the Home Screen. They are open because nothing says Safari
// sends the cookie when it fetches them, and a 401 there leaves a screenshot
// of the page where the icon should be. None of them is secret: the manifest
// carries the token only when the request that asked for it already held
// one (see handleManifest).
func isHomeScreenAsset(path string) bool {
	switch path {
	case manifestPath, "/icon.svg", "/icon-180.png", "/icon-192.png", "/icon-512.png":
		return true
	}
	return false
}

// isNavigation reports whether a path is a page a browser is displaying, as
// opposed to a transport endpoint or an API call.
func isNavigation(path string) bool {
	switch path {
	case "/ws", "/wt":
		return false
	}
	return !strings.HasPrefix(path, "/api/")
}

// tokenMatches compares in constant time. The lengths are equal in every real
// case, and ConstantTimeCompare returns 0 rather than leaking when they are not.
func (s *Server) tokenMatches(got string) bool {
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func cookieToken(r *http.Request) string {
	c, err := r.Cookie(tokenCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// redactQuery returns a raw query string with every token value replaced by
// REDACTED, for writing a URL anywhere that outlives the request: the trace
// and the logs are files on disk, and the token in them would be as good as
// the one in the token file. The rest of the query is kept byte for byte.
func redactQuery(raw string) string {
	if raw == "" {
		return raw
	}
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		k, _, _ := strings.Cut(p, "=")
		if uk, err := url.QueryUnescape(k); err == nil {
			k = uk
		}
		if k == "token" {
			parts[i] = "token=REDACTED"
		}
	}
	return strings.Join(parts, "&")
}

// redactURL is redactQuery for a whole URL string, as a client sends one in a
// record: only the query between ? and # is touched.
func redactURL(u string) string {
	base, rest, ok := strings.Cut(u, "?")
	if !ok {
		return u
	}
	q, frag, hasFrag := strings.Cut(rest, "#")
	out := base + "?" + redactQuery(q)
	if hasFrag {
		out += "#" + frag
	}
	return out
}
