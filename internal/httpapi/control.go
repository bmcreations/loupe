package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
)

// handleControlRelease is a person handing a target back to the agents
// before the human hold runs out: POST /api/control/release?target=, sent
// with X-Loupe-Viewer. It frees the target at once, a pinned hold included,
// when that viewer holds it, and answers the control state after. A viewer
// that does not hold it gets the state unchanged, so a second Hand Back is
// not an error.
func (s *Server) handleControlRelease(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("target")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing ?target=; GET /api/targets lists them"})
		return
	}
	if _, ok := s.mgr.Target(id); !ok {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: fmt.Sprintf("no target %q", id)})
		return
	}
	viewer, err := strconv.ParseUint(r.Header.Get(HeaderViewer), 10, 64)
	if err != nil || viewer == 0 {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing " + HeaderViewer + ": only a viewer holds a target"})
		return
	}
	sess, ok := s.mgr.Session(id)
	if !ok {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: fmt.Sprintf("target %q is not streaming", id)})
		return
	}
	st, err := sess.ReleaseHold(viewer)
	if err != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
