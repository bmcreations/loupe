package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// handleFrame describes the bezel to draw around a target's stream.
//
// This is a property of the device rather than of a session: it needs no
// stream open, and it does not change while one is. That is why it is its own
// endpoint and not a field on the stream metadata the client already receives,
// which describes what the encoder produced and exists only while something is
// streaming.
func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	framer, ok := t.(target.Framer)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no device frame", t.ID()),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
	defer cancel()
	ctx = s.peerReadCtx(ctx, r, t)

	frame, err := framer.DeviceFrame(ctx)
	if err != nil {
		s.traceAgent(r, t.ID(), "frame", err)
		if writeOwnerError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}

	s.traceAgent(r, t.ID(), "frame", nil)
	writeJSON(w, http.StatusOK, frame)
}

// frameAssetMaxAge is how long a browser may keep a piece of skin art.
//
// Long, because the art is a file the SDK installed and does not change under a
// running emulator, and the alternative is re-fetching a couple of hundred
// kilobytes of webp over the phone's link on every reload. Trap 7's no-store
// rule is about the client's own code, where a stale module produces behaviour
// that makes no sense; a stale bezel is a stale bezel.
const frameAssetMaxAge = time.Hour

// handleFrameAsset serves one image the frame named.
//
// The filename is caller-supplied and reaches the filesystem, so it is worth
// being explicit about why there is no path sanitising here: FrameAsset accepts
// only a name equal to one the parsed skin layout declared, which is a string
// the skin itself produced. A caller cannot name anything else, so there is no
// traversal to normalise and no encoding of ".." or a separator to get wrong.
func (s *Server) handleFrameAsset(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	framer, ok := t.(target.Framer)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no device frame", t.ID()),
		})
		return
	}

	name := r.URL.Query().Get("file")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "missing ?file=; GET /api/frame names the assets it has",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
	defer cancel()
	ctx = s.peerReadCtx(ctx, r, t)

	raw, mime, err := framer.FrameAsset(ctx, name)
	if err != nil {
		s.traceAgent(r, t.ID(), "frame-asset", err)
		if writeOwnerError(w, err) {
			return
		}
		// A name the skin does not declare is the caller asking for something
		// that is not there, not the device failing, so it is a 404 rather
		// than the 502 a broken read would give. They are not distinguished
		// further: telling the two apart would report whether a given filename
		// exists on disk, which is the one thing the guard exists to withhold.
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
		return
	}

	// Content-addressed, so a skin swapped underneath a cached copy is still
	// picked up on the next conditional request rather than at the max-age.
	sum := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control",
		fmt.Sprintf("private, max-age=%d", int(frameAssetMaxAge.Seconds())))
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	s.traceAgent(r, t.ID(), "frame-asset", nil)
	w.Header().Set("Content-Type", mime)
	w.Write(raw)
}
