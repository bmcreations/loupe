package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
)

// manifestPath is the web app manifest index.html links to.
const manifestPath = "/manifest.webmanifest"

// handleManifest serves the manifest that makes Add to Home Screen open
// loupe as its own app, full screen and with its own icon.
//
// start_url carries the token when the request presented one. A Home Screen
// app keeps its own cookies, apart from Safari's, so without it the first
// launch has no cookie and answers 401. The token then arrives as a query,
// and the auth middleware sets the cookie and redirects it out of the URL,
// the same as for a freshly opened link. A request without the token gets
// a plain "/", which is what an unauthenticated caller could reach anyway.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	start := "/"
	if s.token != "" && (s.tokenMatches(bearer(r)) || s.tokenMatches(cookieToken(r))) {
		start = "/?token=" + url.QueryEscape(s.token)
	}
	// The page names its resolved appearance, so a Home Screen app added
	// from a light page opens light. iOS reads this once, at the add, so a
	// later change of appearance does not reach an installed app.
	color := "#0b0d10"
	if r.URL.Query().Get("theme") == "light" {
		color = "#f2f3f5"
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":             "Loupe",
		"short_name":       "Loupe",
		"id":               "/",
		"start_url":        start,
		"scope":            "/",
		"display":          "standalone",
		"background_color": color,
		"theme_color":      color,
		"icons": []map[string]string{
			{"src": "/icon-192.png", "sizes": "192x192", "type": "image/png"},
			{"src": "/icon-512.png", "sizes": "512x512", "type": "image/png"},
			{"src": "/icon.svg", "sizes": "any", "type": "image/svg+xml"},
		},
	})
}
