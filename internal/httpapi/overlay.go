package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // DecodeConfig reads the size of a posted JPEG
	_ "image/png"  // and of a PNG
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"sync"

	"github.com/bmcreations/loupe/internal/session"
)

// Overlays are what an extension puts in front of the people watching a
// target: an image over the video (a design to compare against), boxes with
// labels (where it differs), or both. loupe neither makes nor reads them. It
// keeps the latest set per target, sends it to every viewer as MsgOverlay,
// and serves the images, so a client draws an extension's output while only
// ever talking to loupe.
//
// Coordinates come in as device pixels, the space /api/screenshot and
// /api/ui use, and go out to viewers as fractions of the screen, the way
// MsgCursor's points do: a client already maps those onto its video.

const (
	// maxOverlayBytes bounds a post. A full-resolution phone screenshot as a
	// PNG is 1 to 3 MB, and base64 adds a third.
	maxOverlayBytes = 12 << 20
	// maxOverlays is per target. Each is drawn on every frame, and more than
	// a handful on one phone screen is unreadable anyway.
	maxOverlays = 8
	// maxOverlayBoxes is per overlay.
	maxOverlayBoxes = 200
)

var overlayIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// OverlayBoxRequest is one labelled rectangle, in the request's pixel space.
type OverlayBoxRequest struct {
	// Bounds is [x1, y1, x2, y2].
	Bounds [4]int `json:"bounds"`
	Label  string `json:"label,omitempty"`
	// Kind is a hint for how to draw it: "diff" (something that differs) or
	// "note". Clients draw any other value as a note.
	Kind string `json:"kind,omitempty"`
}

// OverlayRequest is POST /api/overlays's body.
type OverlayRequest struct {
	// ID names the overlay. Posting an existing ID replaces it.
	ID string `json:"id"`
	// Source names the extension that posted it, for a client's label and
	// for DELETE ?source=.
	Source string `json:"source"`
	Label  string `json:"label,omitempty"`
	// Image is a base64 PNG or JPEG, or empty for boxes only.
	Image string `json:"image,omitempty"`
	// Screen is the [width, height] in pixels that Bounds and every box are
	// in. It defaults to the target's screen size, or failing that the
	// image's own size, which is right for an image the size of a screenshot.
	Screen *[2]int `json:"screen,omitempty"`
	// Bounds is where the image goes, [x1, y1, x2, y2]; the whole screen if
	// omitted.
	Bounds *[4]int `json:"bounds,omitempty"`
	// Opacity is the image's starting opacity, 0 to 1; 0.5 if omitted. A
	// client may let the person change it.
	Opacity *float64            `json:"opacity,omitempty"`
	Boxes   []OverlayBoxRequest `json:"boxes,omitempty"`
}

// OverlayBox is a box as viewers get it.
type OverlayBox struct {
	// Rect is [x1, y1, x2, y2] as fractions of the screen.
	Rect  [4]float64 `json:"rect"`
	Label string     `json:"label,omitempty"`
	Kind  string     `json:"kind,omitempty"`
}

// Overlay is one overlay as viewers and GET /api/overlays get it.
type Overlay struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Label  string `json:"label,omitempty"`
	// Image is the path to GET the image from, with the token left for the
	// client to add, or empty when the overlay has none. It changes with
	// every post, so a client may cache by it.
	Image string `json:"image,omitempty"`
	// Width and Height are the image's size in pixels, 0 without one.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Rect is where the image goes, as fractions of the screen.
	Rect    [4]float64   `json:"rect"`
	Opacity float64      `json:"opacity"`
	Boxes   []OverlayBox `json:"boxes,omitempty"`
}

// OverlaysResponse is GET /api/overlays's body, and MsgOverlay's.
type OverlaysResponse struct {
	Overlays []Overlay `json:"overlays"`
}

type storedOverlay struct {
	Overlay
	data []byte
	mime string
	// seq orders overlays by first post, so a replaced one keeps its place.
	seq int
}

// overlayStore keeps every target's overlays. Its mutex is held across a
// change and the push to the session, and across apply, so two posts or a
// post and a session opening cannot deliver an older set after a newer one.
// It is taken before Manager.mu, never inside it.
type overlayStore struct {
	mu   sync.Mutex
	byT  map[string]map[string]*storedOverlay
	gen  int
	next int
}

func newOverlayStore() *overlayStore {
	return &overlayStore{byT: make(map[string]map[string]*storedOverlay)}
}

// listLocked returns targetID's overlays in the order they were first posted.
func (o *overlayStore) listLocked(targetID string) []Overlay {
	set := o.byT[targetID]
	all := make([]*storedOverlay, 0, len(set))
	for _, so := range set {
		all = append(all, so)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	out := make([]Overlay, 0, len(all))
	for _, so := range all {
		out = append(out, so.Overlay)
	}
	return out
}

func (o *overlayStore) bodyLocked(targetID string) []byte {
	body, _ := json.Marshal(OverlaysResponse{Overlays: o.listLocked(targetID)})
	return body
}

// apply hands s the overlays already posted for targetID. Called once a new
// session is published. A target with none sends nothing, so a client that
// never sees MsgOverlay has nothing to clear.
func (o *overlayStore) apply(targetID string, s *session.Session) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.byT[targetID]) == 0 {
		return
	}
	s.SetOverlays(o.bodyLocked(targetID))
}

// change runs fn on targetID's set under the store's lock, then sends the
// result to the target's session, if it has one.
func (o *overlayStore) change(m *Manager, targetID string, fn func(set map[string]*storedOverlay) error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	set := o.byT[targetID]
	if set == nil {
		set = make(map[string]*storedOverlay)
	}
	if err := fn(set); err != nil {
		return err
	}
	if len(set) == 0 {
		delete(o.byT, targetID)
	} else {
		o.byT[targetID] = set
	}
	if sess, ok := m.Session(targetID); ok {
		sess.SetOverlays(o.bodyLocked(targetID))
	}
	return nil
}

func (o *overlayStore) image(targetID, id string) (*storedOverlay, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	so, ok := o.byT[targetID][id]
	if !ok || so.data == nil {
		return nil, false
	}
	return so, true
}

// handleOverlays lists (GET), posts (POST) and removes (DELETE) a target's
// overlays.
func (s *Server) handleOverlays(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	store := s.mgr.overlays
	switch r.Method {
	case http.MethodGet:
		store.mu.Lock()
		list := store.listLocked(t.ID())
		store.mu.Unlock()
		writeJSON(w, http.StatusOK, OverlaysResponse{Overlays: list})

	case http.MethodPost:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOverlayBytes))
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{
				Error: fmt.Sprintf("overlay body over %d bytes", maxOverlayBytes),
			})
			return
		}
		var req OverlayRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad overlay JSON: " + err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
		sw, sh := agentScreenSize(ctx, t)
		cancel()
		so, err := buildOverlay(req, t.ID(), sw, sh)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
		err = store.change(s.mgr, t.ID(), func(set map[string]*storedOverlay) error {
			old, replacing := set[so.ID]
			if !replacing && len(set) >= maxOverlays {
				return fmt.Errorf("target has %d overlays already; DELETE one first", maxOverlays)
			}
			store.gen++
			if so.data != nil {
				so.Image = overlayImagePath(t.ID(), so.ID, store.gen)
			}
			if replacing {
				so.seq = old.seq
			} else {
				store.next++
				so.seq = store.next
			}
			set[so.ID] = so
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error()})
			return
		}
		s.traceAgent(r, t.ID(), "overlay", nil)
		writeJSON(w, http.StatusOK, so.Overlay)

	case http.MethodDelete:
		q := r.URL.Query()
		id, source := q.Get("id"), q.Get("source")
		removed := 0
		_ = store.change(s.mgr, t.ID(), func(set map[string]*storedOverlay) error {
			for k, so := range set {
				if (id == "" || k == id) && (source == "" || so.Source == source) {
					delete(set, k)
					removed++
				}
			}
			return nil
		})
		writeJSON(w, http.StatusOK, OverlayDeleteResponse{Removed: removed})
	}
}

// OverlayDeleteResponse is DELETE /api/overlays's body.
type OverlayDeleteResponse struct {
	Removed int `json:"removed"`
}

// handleOverlayImage serves one overlay's image.
func (s *Server) handleOverlayImage(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	so, ok := s.mgr.overlays.image(t.ID(), r.URL.Query().Get("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no such overlay image"})
		return
	}
	w.Header().Set("Content-Type", so.mime)
	// The path carries ?v=, which changes on every post, so an old URL never
	// names new bytes.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(so.data)
}

func overlayImagePath(targetID, id string, gen int) string {
	q := url.Values{"target": {targetID}, "id": {id}, "v": {strconv.Itoa(gen)}}
	return "/api/overlays/image?" + q.Encode()
}

// buildOverlay checks req and converts it to what viewers get. screenW and
// screenH are the target's screen size, or 0 when it has none.
func buildOverlay(req OverlayRequest, targetID string, screenW, screenH int) (*storedOverlay, error) {
	if !overlayIDPattern.MatchString(req.ID) {
		return nil, fmt.Errorf("id %q: want 1 to 64 of A-Z a-z 0-9 . _ -", req.ID)
	}
	if req.Source == "" || len(req.Source) > 64 {
		return nil, fmt.Errorf("source: want 1 to 64 characters")
	}
	if len(req.Boxes) > maxOverlayBoxes {
		return nil, fmt.Errorf("%d boxes; at most %d", len(req.Boxes), maxOverlayBoxes)
	}
	so := &storedOverlay{Overlay: Overlay{ID: req.ID, Source: req.Source, Label: req.Label, Opacity: 0.5}}
	if req.Opacity != nil {
		if *req.Opacity < 0 || *req.Opacity > 1 {
			return nil, fmt.Errorf("opacity %v: want 0 to 1", *req.Opacity)
		}
		so.Opacity = *req.Opacity
	}

	if req.Image != "" {
		data, err := base64.StdEncoding.DecodeString(req.Image)
		if err != nil {
			return nil, fmt.Errorf("image: not base64: %v", err)
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || (format != "png" && format != "jpeg") {
			return nil, fmt.Errorf("image: want a PNG or JPEG")
		}
		so.data, so.mime = data, "image/"+format
		so.Width, so.Height = cfg.Width, cfg.Height
	} else if len(req.Boxes) == 0 {
		return nil, fmt.Errorf("an overlay needs an image, boxes, or both")
	}

	w, h := screenW, screenH
	if req.Screen != nil {
		w, h = req.Screen[0], req.Screen[1]
	} else if w <= 0 || h <= 0 {
		w, h = so.Width, so.Height
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("target %q reports no screen size; pass screen: [width, height]", targetID)
	}
	frac := func(b [4]int) ([4]float64, error) {
		if b[2] <= b[0] || b[3] <= b[1] {
			return [4]float64{}, fmt.Errorf("bounds %v: want x1 < x2 and y1 < y2", b)
		}
		return [4]float64{
			float64(b[0]) / float64(w), float64(b[1]) / float64(h),
			float64(b[2]) / float64(w), float64(b[3]) / float64(h),
		}, nil
	}
	so.Rect = [4]float64{0, 0, 1, 1}
	if req.Bounds != nil {
		r, err := frac(*req.Bounds)
		if err != nil {
			return nil, err
		}
		so.Rect = r
	}
	for _, b := range req.Boxes {
		r, err := frac(b.Bounds)
		if err != nil {
			return nil, fmt.Errorf("box: %w", err)
		}
		so.Boxes = append(so.Boxes, OverlayBox{Rect: r, Label: b.Label, Kind: b.Kind})
	}
	return so, nil
}
