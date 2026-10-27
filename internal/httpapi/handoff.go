package httpapi

import (
	"net/http"
	"net/url"
)

// maxHandoffTargets matches the web client's pane grid.
const maxHandoffTargets = 4

// HandoffResponse is GET /api/handoff's body.
type HandoffResponse struct {
	URL string `json:"url"`
}

// SetHandoffURL sets the link another device can open: the startup banner's
// phone link, whose host the TLS certificate covers. Empty turns
// /api/handoff off.
func (s *Server) SetHandoffURL(u string) { s.handoffURL = u }

// handleHandoff answers with the web client's link for the given targets,
// so a native viewer can pass what it shows to a browser elsewhere: the
// first target is the focused pane, as in the web's own URL (panes.js).
// The link carries the token, which is fine: only a caller that already
// holds it gets this far.
func (s *Server) handleHandoff(w http.ResponseWriter, r *http.Request) {
	if s.handoffURL == "" {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "this server has no address another device can open"})
		return
	}
	u, err := url.Parse(s.handoffURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "bad handoff URL: " + err.Error()})
		return
	}
	q := u.Query()
	q.Del("target")
	seen := map[string]bool{}
	for _, t := range r.URL.Query()["target"] {
		if t == "" || seen[t] {
			continue
		}
		if len(seen) == maxHandoffTargets {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "at most 4 targets"})
			return
		}
		seen[t] = true
		q.Add("target", t)
	}
	u.RawQuery = q.Encode()
	writeJSON(w, http.StatusOK, HandoffResponse{URL: u.String()})
}
