package httpapi

import (
	"net/http"
	"strconv"

	"github.com/bmcreations/loupe/internal/session"
)

// handleEvents serves the events a client missed: control messages go out
// with a non-blocking send (PublishEvent), so a busy viewer can silently miss
// one, and a client that joins late has seen none of what happened before it.
// Either case is a gap in seq, and this is how it is filled.
//
// The response also carries the session's own identity and the range the
// ring currently holds (session.EventPage), because seq gaps are not the
// only thing a poller needs to detect. A session restart resets seq to 1, so
// a caller holding a stale `after` against a new session would otherwise read
// an empty list as "caught up" rather than "reading the wrong session
// entirely." See EventPage's doc comment.
//
// No session for the target is not an error. A target with no viewer and
// nothing driving it through /api/input has published nothing, which is
// exactly what an empty list already says -- there is no third state to
// invent a 404 for.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}

	afterParam := r.URL.Query().Get("after")
	var after uint64
	if afterParam != "" {
		v, err := strconv.ParseUint(afterParam, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: "after must be an unsigned 64-bit integer",
			})
			return
		}
		after = v
	}

	page := session.EventPage{Events: []session.Event{}}
	if sess, ok := s.mgr.Session(t.ID()); ok {
		page = sess.EventsSince(after)
	}
	writeJSON(w, http.StatusOK, page)
}
