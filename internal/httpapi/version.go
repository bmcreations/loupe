package httpapi

import "net/http"

// VersionResponse is GET /api/version's body.
type VersionResponse struct {
	// Version is what `loupe version` prints: the release tag set with
	// -ldflags "-X main.version=...", or "dev" for a plain go build.
	Version string `json:"version"`
}

// SetVersion sets what /api/version reports. A supervisor that ships loupe
// beside itself, such as Handset's login item, compares it with the binary
// it bundles to tell whether the running server is an old one.
func (s *Server) SetVersion(v string) { s.version = v }

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, VersionResponse{Version: s.version})
}
