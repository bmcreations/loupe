package httpapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// maxDropBytes bounds an image dropped into a field. A var so a test can
// shrink it.
var maxDropBytes int64 = 50 << 20

// dropTimeout covers setting a large image on the pasteboard, two taps, a
// long press and the polls for Paste after each.
const dropTimeout = time.Minute

// DropResponse is what POST /api/drop answers on success.
type DropResponse struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	TookMs int64  `json:"tookMs"`
}

// handleDrop pastes the request body, an image, into the text field at
// ?x=&y= in device pixels: what dropping a picture from the Mac onto a field
// does. The body is raw, its type read from its signature as /api/media
// reads it. It is input, so it is claimed and held like a tap, and an
// X-Loupe-Viewer header makes it a person's.
//
// The trace names the point and the outcome. The image itself is never
// traced or logged.
func (s *Server) handleDrop(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	// Each refusal that no retry can change comes before arbitration, as
	// /api/input's do, so a hold never hides it.
	dr, ok := t.(target.ImageDropper)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q cannot paste an image; only an iOS simulator can", t.ID()),
			Code:  ErrCodeUnsupported,
		})
		return
	}
	x, y, err := dropPoint(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if err := dr.CanDropImage(r.Context()); err != nil {
		if errors.Is(err, target.ErrSimcapOutdated) {
			writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: err.Error(), Code: ErrCodeSimcapOutdated})
			return
		}
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	tooLarge := func() {
		writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{Error: fmt.Sprintf("image is over %d bytes", maxDropBytes)})
	}
	if r.ContentLength > maxDropBytes {
		tooLarge()
		return
	}
	s.awake.Touch(agentAwake)

	body := &countingReader{r: http.MaxBytesReader(w, r.Body, maxDropBytes)}
	br := bufio.NewReader(body)
	head, _ := br.Peek(target.SniffLen)
	kind, known := target.SniffMedia(head)
	if !known || kind.Video {
		var big *http.MaxBytesError
		if _, err := br.Peek(1); errors.As(err, &big) {
			tooLarge()
			return
		}
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "not an image this can paste (png, jpg, gif, webp, heic)"})
		return
	}

	c := s.inputCaller(r, t.ID())
	if !s.arbitrate(w, r, t.ID(), "drop", c) {
		return
	}
	acted := false
	defer func() {
		if !acted {
			s.agentFailed(t.ID(), "drop", c)
		}
	}()
	extra := map[string]any{"x": x, "y": y, "ext": kind.Ext}
	trace := func(outcome string, err error) {
		extra["outcome"], extra["bytes"], extra["tookMs"] = outcome, body.n, time.Since(began).Milliseconds()
		s.traceInputWith(r, t.ID(), "drop", c, err, extra)
	}

	dir, err := os.MkdirTemp("", "loupe-drop-")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	defer os.RemoveAll(dir)
	// NSImage reads the type from the bytes, but the extension keeps the
	// staged file honest for anyone who finds it.
	path := filepath.Join(dir, "0."+kind.Ext)
	if err := writeFile(path, br); err != nil {
		var big *http.MaxBytesError
		if errors.As(err, &big) {
			trace("tooLarge", nil)
			tooLarge()
			return
		}
		trace("error", err)
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dropTimeout)
	defer cancel()
	// Viewers see the drop land as they see an agent's tap: its cursor
	// goes to the point first, and a tap there goes to the event log after,
	// with the error when it failed. A person's own drop draws no agent
	// cursor. No peer is an ImageDropper, so this server always publishes.
	if c.viewer == 0 {
		s.publishCursor(ctx, t, InputRequest{Action: "move", X: x, Y: y}, c)
	}
	res, err := dr.DropImage(ctx, path, x, y)
	s.publishAgentEvent(ctx, t, InputRequest{Action: "tap", X: x, Y: y}, c, err, nil)
	_, extra["published"] = s.mgr.Session(t.ID())
	extra["longPressed"], extra["verified"], extra["retried"] = res.LongPressed, res.Verified, res.Retried
	if res.PasteFrom != "" {
		extra["pasteFrom"] = res.PasteFrom
	}
	if res.KeyPasteErr != "" {
		extra["keyPasteErr"] = res.KeyPasteErr
	}
	if res.ClearErr != "" {
		extra["clearErr"] = res.ClearErr
	}
	switch {
	case errors.Is(err, target.ErrNoPaste):
		trace("noPaste", nil)
		writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{Error: err.Error(), Code: ErrCodeNoPaste})
		return
	case errors.Is(err, target.ErrPasteFailed):
		trace("pasteFailed", nil)
		writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{Error: err.Error(), Code: ErrCodePasteFailed})
		return
	case errors.Is(err, target.ErrSimcapOutdated):
		trace("simcapOutdated", nil)
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: err.Error(), Code: ErrCodeSimcapOutdated})
		return
	case errors.Is(err, target.ErrHubHoldsInput):
		trace("error", err)
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: ErrCodeHubHeld})
		return
	case err != nil:
		trace("error", err)
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	trace("ok", nil)
	acted = true
	s.agentActed(t.ID(), "drop", c)
	writeJSON(w, http.StatusOK, DropResponse{Target: t.ID(), OK: true, TookMs: time.Since(began).Milliseconds()})
}

// dropPoint reads ?x= and ?y=, both required.
func dropPoint(r *http.Request) (int, int, error) {
	q := r.URL.Query()
	var p [2]int
	for i, k := range []string{"x", "y"} {
		v := q.Get(k)
		if v == "" {
			return 0, 0, fmt.Errorf("missing ?%s=, in device pixels as /api/ui reports them", k)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, 0, fmt.Errorf("?%s=%q is not a pixel coordinate", k, v)
		}
		p[i] = n
	}
	return p[0], p[1], nil
}
