package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/mp4"
	"github.com/bmcreations/loupe/internal/record"
)

// RecordStartResponse is POST /api/record's 201.
type RecordStartResponse struct {
	ID string `json:"id"`
}

// RecordMeta is one recording's metadata, in progress or finished.
type RecordMeta = record.Meta

// RecordListResponse is GET /api/record: every recording, newest first.
type RecordListResponse []RecordMeta

// sheetTimeout bounds one contact sheet's ffmpeg run.
const sheetTimeout = 30 * time.Second

// maxSheetFPS is the highest ?fps= a contact sheet accepts.
const maxSheetFPS = 240

// SetRecordings gives /api/record its store. Without one both record
// routes answer 501.
func (s *Server) SetRecordings(st *record.Store) { s.recordings = st }

func (s *Server) noRecordings(w http.ResponseWriter) bool {
	if s.recordings != nil {
		return false
	}
	writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: "recording is not available on this server"})
	return true
}

// handleRecord: GET lists recordings, POST starts one on ?target=.
func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request) {
	if s.noRecordings(w) {
		return
	}
	began := time.Now()
	if r.Method != http.MethodPost {
		list := RecordListResponse(s.recordings.List())
		if list == nil {
			list = RecordListResponse{}
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	// The target's own id, not the one asked for: a running AVD's serial
	// names the same session, and a recording is keyed on the session.
	id := t.ID()
	if m, busy := s.recordings.ActiveFor(id); busy {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf("target %q is already recording as %s", id, m.ID),
			Code:  ErrCodeRecording,
			ID:    m.ID,
		})
		return
	}
	// A recording is a read: it is not arbitrated, so a person's hold does
	// not refuse it. A shut-down AVD boots here, as it does for /wt.
	sess, err := s.mgr.Acquire(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	// The hold keeps the session open with no viewer attached; the recorder
	// takes none of its own. onEnd gives both back on every way it ends.
	release := sess.Hold()
	meta, err := s.recordings.Start(sess, id, func(m record.Meta) {
		release()
		s.mgr.Release(id)
		s.traceRecordEnd(m)
	})
	if err != nil {
		// Start calls onEnd only for a recording it began.
		release()
		s.mgr.Release(id)
		if errors.Is(err, record.ErrBusy) {
			// The other recording may have ended since Start refused.
			resp := ErrorResponse{
				Error: fmt.Sprintf("target %q is already recording", id),
				Code:  ErrCodeRecording,
			}
			if m, ok := s.recordings.ActiveFor(id); ok {
				resp.Error = fmt.Sprintf("target %q is already recording as %s", id, m.ID)
				resp.ID = m.ID
			}
			writeJSON(w, http.StatusConflict, resp)
			return
		}
		status := http.StatusInternalServerError
		if errors.Is(err, record.ErrClosed) {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	rec := map[string]any{"kind": "record", "action": "start", "target": id, "id": meta.ID,
		"tookMs": time.Since(began).Milliseconds()}
	s.inputCaller(r, id).annotate(rec)
	s.trace.Record(rec)
	writeJSON(w, http.StatusCreated, RecordStartResponse{ID: meta.ID})
}

// traceRecordEnd records how a recording ended, from the store's onEnd.
func (s *Server) traceRecordEnd(m record.Meta) {
	rec := map[string]any{
		"kind":           "record",
		"action":         "end",
		"target":         m.Target,
		"id":             m.ID,
		"reason":         m.Reason,
		"frames":         m.Frames,
		"bytes":          m.Bytes,
		"durationMs":     m.DurationMs,
		"bitrateChanges": len(m.BitrateChanges),
		"firstKeyMs":     m.FirstKeyMs,
	}
	if m.Error != "" {
		rec["error"] = m.Error
	}
	s.trace.Record(rec)
}

// handleRecordItem serves the id-addressed subtree: {id}.mp4 GET,
// {id}/stop POST, {id}/frames GET, and {id} GET or DELETE. Every id goes
// through the store, which validates it before touching the filesystem.
func (s *Server) handleRecordItem(w http.ResponseWriter, r *http.Request) {
	if s.noRecordings(w) {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/record/")
	var id, op string
	switch {
	case strings.HasSuffix(tail, ".mp4") && !strings.Contains(tail, "/"):
		id, op = strings.TrimSuffix(tail, ".mp4"), "mp4"
	case strings.Count(tail, "/") == 1:
		id, op, _ = strings.Cut(tail, "/")
		if op != "stop" && op != "frames" {
			notFound(w)
			return
		}
	case !strings.Contains(tail, "/") && tail != "":
		id, op = tail, "meta"
	default:
		notFound(w)
		return
	}
	allowed := map[string][]string{
		"mp4":    {http.MethodGet},
		"stop":   {http.MethodPost},
		"frames": {http.MethodGet},
		"meta":   {http.MethodGet, http.MethodDelete},
	}[op]
	if !methodAllowed(allowed, r.Method) {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	switch op {
	case "mp4":
		s.serveRecording(w, r, id)
	case "stop":
		m, err := s.recordings.Stop(id)
		if err != nil {
			recordError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	case "frames":
		s.serveSheet(w, r, id)
	case "meta":
		if r.Method == http.MethodDelete {
			if err := s.recordings.Delete(id); err != nil {
				recordError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		m, err := s.recordings.Get(id)
		if err != nil {
			recordError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no such recording"})
}

// recordError maps the store's errors to statuses.
func recordError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, record.ErrNotFound):
		notFound(w)
	case errors.Is(err, record.ErrInProgress):
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: "recording in progress; stop it first",
			Code:  ErrCodeRecording,
		})
	default:
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
}

// serveRecording sends the file through ServeContent, so a Range request
// works and Safari can seek.
func (s *Server) serveRecording(w http.ResponseWriter, r *http.Request, id string) {
	path, err := s.recordings.Path(id)
	if err != nil {
		recordError(w, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		recordError(w, record.ErrNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, id+".mp4", fi.ModTime(), f)
}

// parseSheetTime reads a sheet bound: a Go duration ("1.5s", "250ms") or a
// bare number of seconds. Negative and non-finite values are refused.
func parseSheetTime(v string) (time.Duration, error) {
	if v == "" {
		return 0, errors.New("missing")
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return 0, errors.New("negative")
		}
		return d, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1e6 {
		return 0, fmt.Errorf("%q is not a duration or a number of seconds", v)
	}
	return time.Duration(f * float64(time.Second)), nil
}

// serveSheet answers a JPEG contact sheet of the frames between ?from= and
// ?to=, with X-Loupe-Frames listing each tile's PTS in milliseconds.
func (s *Server) serveSheet(w http.ResponseWriter, r *http.Request, id string) {
	path, err := s.recordings.Path(id)
	if err != nil {
		recordError(w, err)
		return
	}
	q := r.URL.Query()
	from, err := parseSheetTime(q.Get("from"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "from: " + err.Error()})
		return
	}
	to, err := parseSheetTime(q.Get("to"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "to: " + err.Error()})
		return
	}
	var fps float64
	if v := q.Get("fps"); v != "" {
		fps, err = strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(fps) || fps <= 0 || fps > maxSheetFPS {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("fps: %q is not a number above 0 and at most %d", v, maxSheetFPS)})
			return
		}
	}
	samples, err := mp4.ReadSamples(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	seek, picks, pts, err := record.SheetPlan(samples, from, to, fps)
	switch {
	case errors.Is(err, record.ErrBadRange):
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "to is before from"})
		return
	case errors.Is(err, record.ErrRangeTooLong):
		writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{Error: "a sheet covers at most 10s"})
		return
	case errors.Is(err, record.ErrNoFrames):
		writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{Error: "no frames in that range"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	find := s.ffmpeg
	if find == nil {
		find = record.FFmpeg
	}
	bin, err := find()
	if err != nil {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: "ffmpeg not found on PATH, in /opt/homebrew/bin or in /usr/local/bin",
			Code:  ErrCodeNoFFmpeg,
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sheetTimeout)
	defer cancel()
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, record.SheetArgs(path, seek, picks)...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: "ffmpeg: " + msg})
		return
	}
	ms := make([]string, len(pts))
	for i, p := range pts {
		ms[i] = strconv.FormatInt(p.Milliseconds(), 10)
	}
	w.Header().Set("X-Loupe-Frames", strings.Join(ms, ","))
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(out.Bytes())
}
