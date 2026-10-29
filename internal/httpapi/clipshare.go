package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/bmcreations/loupe/internal/clipshare"
)

// How long GET /api/clipboard/shared holds a request with nothing newer.
const (
	sharedWaitDefault = 25 * time.Second
	sharedWaitMax     = 30 * time.Second
)

// SharedClipboardRequest is POST /api/clipboard/shared's body. ID and At are
// optional and filled in by the server; a peer bridge sends both so the value
// keeps the identity it had where it was copied.
type SharedClipboardRequest struct {
	Text string `json:"text"`
	ID   string `json:"id,omitempty"`
	At   int64  `json:"at,omitempty"`
}

// SetClipShare serves c on /api/clipboard/shared and bridges it to every
// peer. Call it before SetPeers.
func (s *Server) SetClipShare(c *clipshare.Service) {
	s.clipShare = c
	s.syncClipBridges()
}

// handleSharedClipboard long-polls the shared clipboard on GET and publishes
// to it on POST. Only text lengths are traced.
func (s *Server) handleSharedClipboard(w http.ResponseWriter, r *http.Request) {
	share := s.clipShare
	if share == nil {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "this server has no shared clipboard"})
		return
	}
	off := func() {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: "sharedClipboard is off on this server",
			Code:  ErrCodeSharedClipboardOff,
		})
	}

	if r.Method == http.MethodGet {
		q := r.URL.Query()
		var after uint64
		if v := q.Get("after"); v != "" {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "after must be a sequence number"})
				return
			}
			after = n
		}
		wait := sharedWaitDefault
		if v := q.Get("waitMs"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "waitMs must be a number of milliseconds"})
				return
			}
			wait = min(time.Duration(n)*time.Millisecond, sharedWaitMax)
		}
		ctx, cancel := context.WithTimeout(r.Context(), wait)
		defer cancel()
		v, err := share.Wait(ctx, after)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, v)
		case errors.Is(err, clipshare.ErrDisabled):
			off()
		case errors.Is(err, context.DeadlineExceeded):
			w.WriteHeader(http.StatusNoContent)
		}
		// Canceled: the caller went away, and there is no one to answer.
		return
	}

	var req SharedClipboardRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxClipboardBytes+1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad request: " + err.Error()})
		return
	}
	if len(req.Text) > maxClipboardBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{
			Error: fmt.Sprintf("clipboard text is %d bytes; at most %d", len(req.Text), maxClipboardBytes),
		})
		return
	}
	if !utf8.ValidString(req.Text) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "clipboard text is not UTF-8"})
		return
	}
	if req.Text == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "clipboard text is empty"})
		return
	}
	if !share.Enabled() {
		off()
		return
	}
	// Every POST is a participant's, whoever sends it: a peer bridge is
	// told apart by the id it keeps, not by anything a token holder could
	// claim.
	v, ok := share.Publish(clipshare.Value{Text: req.Text, ID: req.ID, At: req.At, Origin: "participant"})
	if ok {
		s.trace.Record(map[string]any{"kind": "clipboard", "from": "participant", "textLen": utf8.RuneCountInString(v.Text), "seq": v.Seq})
	}
	w.WriteHeader(http.StatusNoContent)
}
