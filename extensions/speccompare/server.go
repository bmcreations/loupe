package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // a reference may come as a JPEG
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is the extension's settings.
type Config struct {
	Addr     string
	LoupeURL string
	Token    string
	DataDir  string
}

// source is how this extension signs its overlays, and what it clears.
const source = "speccompare"

// overlayID is the one overlay a target gets: a new comparison replaces it.
const overlayID = "spec-compare"

// Reference is a stored design image and what it came from.
type Reference struct {
	ID     string `json:"id"`
	Target string `json:"target"`
	Label  string `json:"label,omitempty"`
	Figma  *Figma `json:"figma,omitempty"`
	// Masks are regions, in screenshot pixels, never compared: a clock, an
	// avatar, a balance.
	Masks []Rect `json:"masks,omitempty"`
	// Top is how far down the screen, in pixels, the reference starts. 0
	// suits a Figma frame drawn with its own status bar.
	Top     int       `json:"top,omitempty"`
	Width   int       `json:"width"`
	Height  int       `json:"height"`
	Created time.Time `json:"created"`
}

// Figma names the frame a reference was exported from.
type Figma struct {
	File string `json:"file"`
	Node string `json:"node"`
}

// Server stores references and runs comparisons.
type Server struct {
	cfg      Config
	refsDir  string
	diffsDir string
	loupe    *loupeClient
	mu       sync.Mutex // guards the references on disk
}

func NewServer(cfg Config) (*Server, error) {
	s := &Server{
		cfg:      cfg,
		refsDir:  filepath.Join(cfg.DataDir, "references"),
		diffsDir: filepath.Join(cfg.DataDir, "diffs"),
		loupe:    newLoupeClient(cfg.LoupeURL, cfg.Token),
	}
	for _, d := range []string{s.refsDir, s.diffsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /references", s.handleList)
	mux.HandleFunc("POST /references", s.handleAdd)
	mux.HandleFunc("DELETE /references/{id}", s.handleDelete)
	mux.HandleFunc("GET /references/{id}/image", s.handleRefImage)
	mux.HandleFunc("POST /compare", s.handleCompare)
	mux.HandleFunc("DELETE /overlay", s.handleClear)
	mux.HandleFunc("GET /diffs/{name}", s.handleDiff)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	return mux
}

var (
	idPattern   = regexp.MustCompile(`^[a-f0-9]{12}$`)
	diffPattern = regexp.MustCompile(`^[a-f0-9]{12}-[a-f0-9]{8}\.png$`)
	unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

// targetDir is where a target's references live. Target ids carry colons
// (sim:UDID, avd:Pixel_7a), so they're flattened for the filesystem.
func (s *Server) targetDir(target string) string {
	return filepath.Join(s.refsDir, unsafeChars.ReplaceAllString(target, "_"))
}

func (s *Server) load(target, id string) (*Reference, image.Image, error) {
	if !idPattern.MatchString(id) {
		return nil, nil, fmt.Errorf("bad reference id")
	}
	dir := s.targetDir(target)
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, nil, err
	}
	var ref Reference
	if err := json.Unmarshal(b, &ref); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(filepath.Join(dir, id+".png"))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	return &ref, img, err
}

func (s *Server) list(target string) ([]Reference, error) {
	matches, _ := filepath.Glob(filepath.Join(s.targetDir(target), "*.json"))
	refs := []Reference{}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		var ref Reference
		if json.Unmarshal(b, &ref) == nil {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Created.After(refs[j].Created) })
	return refs, nil
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		writeErr(w, 400, "target is required")
		return
	}
	refs, err := s.list(target)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"references": refs})
}

// AddRequest is POST /references's body.
type AddRequest struct {
	// Image is a base64 PNG or JPEG: a Figma frame export, at any scale.
	Image string `json:"image"`
	Label string `json:"label,omitempty"`
	Figma *Figma `json:"figma,omitempty"`
	Masks []Rect `json:"masks,omitempty"`
	Top   int    `json:"top,omitempty"`
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		writeErr(w, 400, "target is required")
		return
	}
	var req AddRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 24<<20)).Decode(&req); err != nil {
		writeErr(w, 400, "body: "+err.Error())
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Image)
	if err != nil {
		writeErr(w, 400, "image: not base64")
		return
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		writeErr(w, 400, "image: "+err.Error())
		return
	}
	for _, m := range req.Masks {
		if m.empty() {
			writeErr(w, 400, fmt.Sprintf("mask %v is empty; masks are [x1,y1,x2,y2]", m))
			return
		}
	}
	ref := Reference{
		ID: randHex(6), Target: target, Label: req.Label, Figma: req.Figma,
		Masks: req.Masks, Top: req.Top,
		Width: img.Bounds().Dx(), Height: img.Bounds().Dy(), Created: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.targetDir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Re-encode as PNG so every stored reference reads back the same way.
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	meta, _ := json.MarshalIndent(ref, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, ref.ID+".png"), buf.Bytes(), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, ref.ID+".json"), meta, 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ref)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	target, id := r.URL.Query().Get("target"), r.PathValue("id")
	if target == "" || !idPattern.MatchString(id) {
		writeErr(w, 400, "target and a reference id are required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.targetDir(target)
	err := os.Remove(filepath.Join(dir, id+".json"))
	os.Remove(filepath.Join(dir, id+".png"))
	if os.IsNotExist(err) {
		writeErr(w, 404, "no such reference")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) handleRefImage(w http.ResponseWriter, r *http.Request) {
	target, id := r.URL.Query().Get("target"), r.PathValue("id")
	if target == "" || !idPattern.MatchString(id) {
		writeErr(w, 400, "target and a reference id are required")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	http.ServeFile(w, r, filepath.Join(s.targetDir(target), id+".png"))
}

// CompareRequest is POST /compare's optional body.
type CompareRequest struct {
	// Masks add to the reference's own, for this comparison only.
	Masks []Rect `json:"masks,omitempty"`
	// Overlay false skips showing the result to viewers.
	Overlay *bool    `json:"overlay,omitempty"`
	Options *Options `json:"options,omitempty"`
}

// CompareResponse is POST /compare's body.
type CompareResponse struct {
	Result
	Reference Reference `json:"reference"`
	// Diff is the path, on this extension, of the diff image.
	Diff string `json:"diff"`
	// OverlayError is the error from showing the result to viewers, if any. The
	// comparison stands either way.
	OverlayError string `json:"overlayError,omitempty"`
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	target, id := q.Get("target"), q.Get("reference")
	if target == "" {
		writeErr(w, 400, "target is required")
		return
	}
	var req CompareRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && err != io.EOF {
			writeErr(w, 400, "body: "+err.Error())
			return
		}
	}
	if id == "" {
		// The newest reference for the target, so an agent that posted one
		// can just call /compare after each change.
		refs, _ := s.list(target)
		if len(refs) == 0 {
			writeErr(w, 404, "no reference for this target; POST /references first")
			return
		}
		id = refs[0].ID
	}
	ref, refImg, err := s.load(target, id)
	if err != nil {
		writeErr(w, 404, "reference: "+err.Error())
		return
	}
	shotPNG, err := s.loupe.screenshot(target, r.Header)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	shot, err := png.Decode(bytes.NewReader(shotPNG))
	if err != nil {
		writeErr(w, 502, "screenshot: "+err.Error())
		return
	}
	// The tree only names regions; a target without one still compares.
	var roots []*uiNode
	if b, err := s.loupe.ui(target, r.Header); err == nil {
		roots, _ = parseTree(b)
	}

	o := defaultOptions
	if req.Options != nil {
		o = *req.Options
		if o.Block <= 0 || o.PixelDelta <= 0 || o.BlockShare <= 0 {
			writeErr(w, 400, "options: block, pixelDelta and blockShare must all be positive")
			return
		}
	}
	masks := append(append([]Rect{}, ref.Masks...), req.Masks...)
	res, diff := Compare(refImg, shot, ref.Top, masks, o)
	for i := range res.Regions {
		res.Regions[i].Node = nodeFor(roots, res.Regions[i].Bounds)
	}

	name := ref.ID + "-" + randHex(4) + ".png"
	f, err := os.Create(filepath.Join(s.diffsDir, name))
	if err == nil {
		err = png.Encode(f, diff)
		f.Close()
	}
	if err != nil {
		writeErr(w, 500, "diff: "+err.Error())
		return
	}
	out := CompareResponse{Result: res, Reference: *ref, Diff: "/diffs/" + name}
	if req.Overlay == nil || *req.Overlay {
		if err := s.showOverlay(target, ref, refImg, shot.Bounds(), res, r.Header); err != nil {
			out.OverlayError = err.Error()
		}
	}
	writeJSON(w, 200, out)
}

// maxBoxes is how many regions get a box on the overlay, biggest first.
// Past a handful the boxes hide the screen they point at.
const maxBoxes = 12

// showOverlay puts the reference over the live video where the comparison
// placed it, with a box on each differing region, so a person sees what the
// agent was told. Only the part of the reference that lands on the screen is
// sent.
func (s *Server) showOverlay(target string, ref *Reference, refImg image.Image, screen image.Rectangle, res Result, from http.Header) error {
	W, H := screen.Dx(), screen.Dy()
	cover := res.Placed.intersect(Rect{0, 0, W, H})
	if cover.empty() {
		return fmt.Errorf("the reference lands off the screen")
	}
	ph := res.Placed[3] - res.Placed[1]
	rb := refImg.Bounds()
	y0 := rb.Min.Y + (cover[1]-res.Placed[1])*rb.Dy()/ph
	y1 := rb.Min.Y + (cover[3]-res.Placed[1])*rb.Dy()/ph
	crop := toRGBA(refImg).SubImage(image.Rect(rb.Min.X, y0, rb.Max.X, y1))
	var buf bytes.Buffer
	if err := png.Encode(&buf, crop); err != nil {
		return err
	}
	label := ref.Label
	if label == "" && ref.Figma != nil {
		label = "node " + ref.Figma.Node
	}
	label = strings.TrimSpace(fmt.Sprintf("Spec %s · %.1f%% differs", label, res.Score*100))
	opacity := 0.5
	o := overlay{
		ID: overlayID, Source: source, Label: label,
		Image:   base64.StdEncoding.EncodeToString(buf.Bytes()),
		Screen:  &[2]int{W, H},
		Bounds:  &cover,
		Opacity: &opacity,
	}
	for i, reg := range res.Regions {
		if i == maxBoxes {
			break
		}
		o.Boxes = append(o.Boxes, overlayBox{Bounds: reg.Bounds, Label: reg.Node.label(), Kind: "diff"})
	}
	return s.loupe.postOverlay(target, o, from)
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		writeErr(w, 400, "target is required")
		return
	}
	if err := s.loupe.clearOverlays(target, source, r.Header); err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"cleared": true})
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !diffPattern.MatchString(name) {
		writeErr(w, 404, "no such diff")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	http.ServeFile(w, r, filepath.Join(s.diffsDir, name))
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
