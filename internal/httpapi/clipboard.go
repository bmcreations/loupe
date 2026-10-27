package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/bmcreations/loupe/internal/target"
)

// maxClipboardBytes bounds a clipboard set. scrcpy's own limit is about
// 256KB; this is text a person copied on a phone, so far less is plenty.
const maxClipboardBytes = 64 << 10

// ClipboardResponse is what GET /api/clipboard returns.
type ClipboardResponse struct {
	Target string `json:"target"`
	Text   string `json:"text"`
}

// ClipboardRequest is POST /api/clipboard's body.
type ClipboardRequest struct {
	Text string `json:"text"`
	// Paste also puts the text into whatever field has focus. On a
	// simulator that is typed, since iOS asks before an app reads a
	// pasteboard another process set.
	Paste bool `json:"paste,omitempty"`
}

// handleClipboard reads the target's clipboard on GET and sets it on POST.
// Neither the text nor its contents are traced, only its length.
func (s *Server) handleClipboard(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	cb, ok := t.(target.Clipboarder)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no clipboard", t.ID()),
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
	defer cancel()

	fail := func(action string, err error) {
		s.traceAgent(r, t.ID(), action, err)
		status := http.StatusBadGateway
		if errors.Is(err, target.ErrClipboardNoSession) {
			status = http.StatusConflict
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
	}

	if r.Method == http.MethodGet {
		text, err := cb.ReadClipboard(ctx)
		if err != nil {
			fail("clipboard-read", err)
			return
		}
		s.traceAgent(r, t.ID(), "clipboard-read", nil)
		writeJSON(w, http.StatusOK, ClipboardResponse{Target: t.ID(), Text: text})
		return
	}

	var req ClipboardRequest
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
	action := "clipboard-set"
	if req.Paste {
		action = "clipboard-paste"
	}
	if err := cb.WriteClipboard(ctx, req.Text, req.Paste); err != nil {
		fail(action, err)
		return
	}
	s.traceAgent(r, t.ID(), action, nil)
	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}
