package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

const (
	defaultCrashLimit = 20
	maxCrashLimit     = 200
)

// CrashList is GET /api/crashes's answer.
type CrashList struct {
	Target  string               `json:"target"`
	Crashes []target.CrashReport `json:"crashes"`
}

// crashQuery reads GET /api/crashes's parameters. since is parsed as
// /api/logcat's is.
func crashQuery(r *http.Request, now time.Time) (target.CrashQuery, error) {
	q := target.CrashQuery{Limit: defaultCrashLimit}
	v := r.URL.Query()
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxCrashLimit {
			return q, fmt.Errorf("limit %q: want 1 to %d", s, maxCrashLimit)
		}
		q.Limit = n
	}
	if s := v.Get("since"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			q.Since = now.Add(-d)
		} else if at, err := time.Parse(time.RFC3339Nano, s); err == nil {
			q.Since = at
		} else {
			return q, fmt.Errorf("since %q: want a duration such as 5m or an RFC 3339 time", s)
		}
	}
	return q, nil
}

// crasher is the target's Crasher, or a 501 written to w.
func (s *Server) crasher(w http.ResponseWriter, r *http.Request) (target.Target, target.Crasher, bool) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return nil, nil, false
	}
	c, ok := t.(target.Crasher)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no crash reports", t.ID()),
		})
		return nil, nil, false
	}
	return t, c, true
}

// handleCrashes lists the target's crash reports, newest first: the last
// ?limit= of them (20 by default), or those since ?since=. A row is a
// summary; /api/crashes/{id} is the report itself. Reads are not
// arbitrated, and nothing of a report reaches the trace.
func (s *Server) handleCrashes(w http.ResponseWriter, r *http.Request) {
	t, c, ok := s.crasher(w, r)
	if !ok {
		return
	}
	q, err := crashQuery(r, time.Now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	start := time.Now()
	list, err := c.Crashes(r.Context(), q)
	s.traceInputWith(r, t.ID(), "crashes", inputCaller{}, err, map[string]any{
		"count": len(list), "tookMs": time.Since(start).Milliseconds(),
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	if list == nil {
		list = []target.CrashReport{}
	}
	writeJSON(w, http.StatusOK, CrashList{Target: t.ID(), Crashes: list})
}

// handleCrash serves /api/crashes/{id}: the report as it was written. An
// .ips is two JSON documents, a header line and the body, so it goes out
// as text. A report of another target's process is 404 like a missing
// one, since the reports of every simulator and of the Mac share one
// directory.
func (s *Server) handleCrash(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/crashes/")
	if !target.ValidCrashID(id) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("%q is not a crash id", id)})
		return
	}
	t, c, ok := s.crasher(w, r)
	if !ok {
		return
	}
	raw, err := c.Crash(r.Context(), id)
	s.traceAgent(r, t.ID(), "crash", err)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, target.ErrCrashNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(raw)
}
