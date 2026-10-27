package httpapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// maxMediaBytes bounds a photo or video upload. A minute of 4K from a phone
// is a few hundred megabytes. A var so a test can shrink it.
var maxMediaBytes int64 = 1 << 30

// mediaTimeout covers pushing a large video and the scan after it.
const mediaTimeout = 2 * time.Minute

// MediaResponse is what POST /api/media answers on success.
type MediaResponse struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Bytes  int64  `json:"bytes"`
	TookMs int64  `json:"tookMs"`
}

// handleMedia adds the request body to the device's photo library: Pictures
// or Movies plus a media scan on Android, `simctl addmedia` on a simulator.
// The body is one file, raw, which is what a browser's drop sends with
// fetch(url, {body: file}). Its type comes from its signature, not from a
// header or a filename, and the name on the device is the server's.
//
// Like /api/install it is not arbitrated: it changes no screen.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	ma, ok := t.(target.MediaAdder)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q cannot add photos or videos", t.ID()),
		})
		return
	}
	if r.ContentLength > maxMediaBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{
			Error: fmt.Sprintf("file is over %d bytes", maxMediaBytes),
		})
		return
	}
	s.awake.Touch(agentAwake)

	body := &countingReader{r: http.MaxBytesReader(w, r.Body, maxMediaBytes)}
	rec := map[string]any{"kind": "agent", "target": t.ID(), "action": "media", "ua": r.UserAgent()}
	finish := func(outcome string, err error) {
		rec["outcome"], rec["bytes"], rec["tookMs"] = outcome, body.n, time.Since(began).Milliseconds()
		if err != nil {
			rec["err"] = err.Error()
		}
		s.trace.Record(rec)
	}

	br := bufio.NewReader(body)
	head, _ := br.Peek(target.SniffLen)
	kind, known := target.SniffMedia(head)
	if !known {
		var tooBig *http.MaxBytesError
		if _, err := br.Peek(1); errors.As(err, &tooBig) {
			finish("tooLarge", nil)
			writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{Error: fmt.Sprintf("file is over %d bytes", maxMediaBytes)})
			return
		}
		finish("refused", target.ErrNotMedia)
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: target.ErrNotMedia.Error()})
		return
	}
	rec["ext"] = kind.Ext

	dir, err := os.MkdirTemp("", "loupe-media-")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	defer os.RemoveAll(dir)
	// simctl and adb take a path, and simctl reads the type from its
	// extension, so the staged file carries the sniffed one.
	path := filepath.Join(dir, "0."+kind.Ext)
	if err := writeFile(path, br); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			finish("tooLarge", nil)
			writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{Error: fmt.Sprintf("file is over %d bytes", maxMediaBytes)})
			return
		}
		finish("error", err)
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}

	name := fmt.Sprintf("loupe-%s.%s", began.Format("20060102-150405.000"), kind.Ext)
	ctx, cancel := context.WithTimeout(r.Context(), mediaTimeout)
	defer cancel()
	if err := ma.AddMedia(ctx, path, name, kind); err != nil {
		finish("error", err)
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	finish("ok", nil)
	writeJSON(w, http.StatusOK, MediaResponse{
		Target: t.ID(), OK: true, Name: name, Kind: kind.Ext,
		Bytes: body.n, TookMs: time.Since(began).Milliseconds(),
	})
}

func writeFile(path string, src io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
