package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bmcreations/loupe/internal/target"
)

// handleApp reports the foreground activity: what is on screen right now,
// without touching the video path.
func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}

	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	ai, ok := t.(target.AppInfo)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no foreground app", t.ID()),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
	defer cancel()

	app, err := ai.ForegroundApp(ctx)
	if err != nil {
		s.traceAgent(r, t.ID(), "app", err)
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}

	s.traceAgent(r, t.ID(), "app", nil)
	writeJSON(w, http.StatusOK, app)
}
