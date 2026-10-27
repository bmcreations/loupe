package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
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
	Addr          string
	LoupeURL      string
	Token         string
	DataDir       string
	SettleTimeout time.Duration
}

// Server keeps the maps and records steps into them.
type Server struct {
	cfg   Config
	loupe loupeAPI
	mu    sync.Mutex // guards maps and every Map's exported fields
	maps  map[string]*Map
}

// loupeAPI is what the server needs from loupe; tests stand in a fake.
type loupeAPI interface {
	screenshot(target string, from http.Header) ([]byte, error)
	ui(target string, compose bool, from http.Header) ([]byte, error)
	input(target string, body map[string]any, from http.Header) error
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

func NewServer(cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, loupe: newLoupeClient(cfg.LoupeURL, cfg.Token), maps: map[string]*Map{}}
	return s, s.loadAll()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /maps", s.handleCreate)
	mux.HandleFunc("GET /maps", s.handleList)
	mux.HandleFunc("GET /maps/{id}", s.handleGet)
	mux.HandleFunc("DELETE /maps/{id}", s.handleDelete)
	mux.HandleFunc("POST /maps/{id}/steps", s.handleStep)
	mux.HandleFunc("POST /maps/{id}/screens/{sid}", s.handleScreen)
	mux.HandleFunc("DELETE /maps/{id}/edges", s.handleEdgeDelete)
	mux.HandleFunc("GET /maps/{id}/screens/{sid}/{file}", s.handleScreenFile)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// loupeFail passes loupe's own status on: a 409 humanHeld from /api/input
// should reach the agent as one, so it knows to wait rather than give up.
func loupeFail(w http.ResponseWriter, err error) {
	var se *statusError
	if errors.As(err, &se) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(se.status)
		w.Write([]byte(se.body))
		return
	}
	fail(w, http.StatusBadGateway, "loupe", err.Error())
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// capture is one look at the device: the settled screenshot and the tree read
// right after it.
type capture struct {
	png    []byte
	tree   []byte
	roots  []*uiNode
	fp     Fingerprint
	stable bool
	w, h   int
}

func (s *Server) look(target string, compose bool, from http.Header) (*capture, error) {
	png, stable, err := s.settle(target, from)
	if err != nil {
		return nil, err
	}
	tree, err := s.loupe.ui(target, compose, from)
	if err != nil {
		return nil, err
	}
	roots, err := parseTree(tree)
	if err != nil {
		return nil, fmt.Errorf("/api/ui: %w", err)
	}
	c := &capture{png: png, tree: tree, roots: roots, fp: fingerprint(roots), stable: stable}
	if cfg, err := pngConfig(png); err == nil {
		c.w, c.h = cfg.Width, cfg.Height
	}
	return c, nil
}

// settle screenshots until two in a row have the same pixels, or the timeout
// passes, and returns the last. Hotswap's rule: pixels, not PNG bytes.
func (s *Server) settle(target string, from http.Header) ([]byte, bool, error) {
	start := time.Now()
	var prev image.Image
	for {
		b, err := s.loupe.screenshot(target, from)
		if err != nil {
			return nil, false, err
		}
		if s.cfg.SettleTimeout <= 0 {
			return b, false, nil
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			return b, false, nil
		}
		if prev != nil && samePixels(prev, img) {
			return b, true, nil
		}
		prev = img
		if time.Since(start) >= s.cfg.SettleTimeout {
			return b, false, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func samePixels(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	r := a.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if a.At(x, y) != b.At(x, y) {
				return false
			}
		}
	}
	return true
}

func pngConfig(b []byte) (image.Config, error) { return png.DecodeConfig(bytes.NewReader(b)) }

// place finds the screen a capture shows, or adds it, and writes its files.
// Callers hold s.mu.
// landing places a step's capture. A step named after a screen the map
// already has lands on that screen, whatever the tree shows: the agent saw
// where it is, and the tree under a closed sheet often differs from the one
// first recorded. The named key becomes an alias so the next visit matches.
func (s *Server) landing(m *Map, c *capture) (*Screen, bool, error) {
	if name := c.fp.Named; name != "" && m.find(c.fp.Key) == nil {
		for _, sc := range m.Screens {
			if sc.Name == name {
				sc.Aliases = append(sc.Aliases, c.fp.Key)
				sc.Visits++
				return sc, false, nil
			}
		}
	}
	return s.place(m, c)
}

func (s *Server) place(m *Map, c *capture) (*Screen, bool, error) {
	if sc := m.find(c.fp.Key); sc != nil {
		sc.Visits++
		return sc, false, nil
	}
	sc := &Screen{
		ID:          m.nextScreenID(),
		Name:        suggestName(c.roots, c.fp),
		Fingerprint: c.fp,
		Visits:      1,
		First:       time.Now().UTC(),
		Width:       c.w,
		Height:      c.h,
	}
	dir := filepath.Join(s.mapDir(m.ID), sc.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, err
	}
	files := map[string][]byte{"screenshot.png": c.png, "tree.json": c.tree}
	if t, err := thumbnail(c.png, 240); err == nil {
		files["thumb.png"] = t
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return nil, false, err
		}
	}
	m.Screens = append(m.Screens, sc)
	return sc, true, nil
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		fail(w, http.StatusBadRequest, "badRequest", "target is required")
		return
	}
	q := r.URL.Query()
	m := &Map{ID: newID(), Target: target, Name: q.Get("name"), Compose: q.Get("compose") == "1", Created: time.Now().UTC()}
	if err := os.MkdirAll(s.mapDir(m.ID), 0o755); err != nil {
		fail(w, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	c, err := s.look(target, m.Compose, r.Header)
	if err != nil {
		os.RemoveAll(s.mapDir(m.ID))
		loupeFail(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, _, err := s.place(m, c)
	if err != nil {
		fail(w, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	m.Root, m.Current = sc.ID, sc.ID
	s.maps[m.ID] = m
	if err := s.save(m); err != nil {
		fail(w, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": m.ID, "screen": s.screenView(m, sc)})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := r.URL.Query().Get("target")
	out := []map[string]any{}
	for _, m := range s.maps {
		if target != "" && m.Target != target {
			continue
		}
		out = append(out, map[string]any{"id": m.ID, "target": m.Target, "name": m.Name, "created": m.Created, "screens": len(m.Screens), "edges": len(m.Edges)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["created"].(time.Time).After(out[j]["created"].(time.Time)) })
	writeJSON(w, http.StatusOK, map[string]any{"maps": out})
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) *Map {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		fail(w, http.StatusNotFound, "noSuchMap", "no map "+id)
		return nil
	}
	s.mu.Lock()
	m := s.maps[id]
	s.mu.Unlock()
	if m == nil {
		fail(w, http.StatusNotFound, "noSuchMap", "no map "+id)
	}
	return m
}

// screenView is a screen as the API shows it: file URLs, not file bodies. A
// map of 40 screens is tens of MB of PNG (docs/app-map.md).
func (s *Server) screenView(m *Map, sc *Screen) map[string]any {
	base := "/maps/" + m.ID + "/screens/" + sc.ID + "/"
	return map[string]any{
		"id": sc.ID, "name": sc.Name, "fingerprint": sc.Fingerprint, "visits": sc.Visits,
		"first": sc.First, "width": sc.Width, "height": sc.Height,
		"screenshot": base + "screenshot.png", "thumb": base + "thumb.png", "tree": base + "tree.json",
	}
}

func (s *Server) mapView(m *Map) map[string]any {
	screens := make([]map[string]any, 0, len(m.Screens))
	for _, sc := range m.Screens {
		screens = append(screens, s.screenView(m, sc))
	}
	edges := m.Edges
	if edges == nil {
		edges = []*Edge{}
	}
	return map[string]any{
		"id": m.ID, "target": m.Target, "name": m.Name, "created": m.Created,
		"root": m.Root, "current": m.Current, "screens": screens, "edges": edges,
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	m := s.lookup(w, r)
	if m == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.mapView(m))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	m := s.lookup(w, r)
	if m == nil {
		return
	}
	m.steps.Lock()
	defer m.steps.Unlock()
	s.mu.Lock()
	delete(s.maps, m.ID)
	s.mu.Unlock()
	os.RemoveAll(s.mapDir(m.ID))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": m.ID})
}

// Step is one action the agent reports. Node picks what a tap aims at from the
// tree as it is right before the step; X and Y aim it by hand.
type Step struct {
	Action  string   `json:"action"`
	Node    *NodeSel `json:"node,omitempty"`
	X       *int     `json:"x,omitempty"`
	Y       *int     `json:"y,omitempty"`
	X2      *int     `json:"x2,omitempty"`
	Y2      *int     `json:"y2,omitempty"`
	Ms      *int     `json:"ms,omitempty"` // a swipe's length; one in place is a long press
	Text    string   `json:"text,omitempty"`
	Keycode int      `json:"keycode,omitempty"`
	Button  string   `json:"button,omitempty"`
	Package string   `json:"package,omitempty"`
	URL     string   `json:"url,omitempty"`
	Note    string   `json:"note,omitempty"`
	// Screen names where the step lands when the tree cannot tell it apart
	// from the screen under it: a menu whose buttons carry no ids, or a sheet
	// the tree leaves out. It keeps that screen apart from the one under it.
	Screen string `json:"screen,omitempty"`
}

// stepActions are the /api/input actions a map records. Pinch, rotate and the
// rest change the view, not the screen, so they are no edge.
var stepActions = map[string]bool{"tap": true, "swipe": true, "text": true, "key": true, "button": true, "launch": true, "openURL": true}

// input builds the /api/input body, aiming a node tap at its centre.
func (st *Step) input(roots []*uiNode) (map[string]any, error) {
	b := map[string]any{"action": st.Action}
	if st.Node != nil && !st.Node.empty() && st.X == nil {
		n := st.Node.find(roots)
		if n == nil {
			return nil, errNoNode
		}
		x, y := n.Center[0], n.Center[1]
		if x == 0 && y == 0 {
			x, y = (n.Bounds[0]+n.Bounds[2])/2, (n.Bounds[1]+n.Bounds[3])/2
		}
		st.X, st.Y = &x, &y
	}
	set := func(k string, v *int) {
		if v != nil {
			b[k] = *v
		}
	}
	set("x", st.X)
	set("y", st.Y)
	set("x2", st.X2)
	set("y2", st.Y2)
	set("ms", st.Ms)
	if st.Text != "" {
		b["text"] = st.Text
	}
	if st.Keycode != 0 {
		b["keycode"] = st.Keycode
	}
	if st.Button != "" {
		b["button"] = st.Button
	}
	if st.Package != "" {
		b["package"] = st.Package
	}
	if st.URL != "" {
		b["url"] = st.URL
	}
	return b, nil
}

var errNoNode = errors.New("no node on the screen matches")

// label is how an edge names its action: "tap", "button back", "key 66".
func (st *Step) label() string {
	switch {
	case st.Action == "button" && st.Button != "":
		return "button " + st.Button
	case st.Action == "key":
		return fmt.Sprintf("key %d", st.Keycode)
	case st.Action == "launch" && st.Package != "":
		return "launch " + st.Package
	}
	return st.Action
}

func (s *Server) handleStep(w http.ResponseWriter, r *http.Request) {
	m := s.lookup(w, r)
	if m == nil {
		return
	}
	var st Step
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		fail(w, http.StatusBadRequest, "badRequest", err.Error())
		return
	}
	if !stepActions[st.Action] {
		fail(w, http.StatusBadRequest, "badRequest", "action must be one of tap, swipe, text, key, button, launch, openURL")
		return
	}
	m.steps.Lock()
	defer m.steps.Unlock()

	// Where the step starts: the tree as it is now, not where the last step
	// left it. A person may have moved the device in between, and a node
	// tap must aim at what is on screen.
	before, err := s.loupe.ui(m.Target, m.Compose, r.Header)
	if err != nil {
		loupeFail(w, err)
		return
	}
	roots, err := parseTree(before)
	if err != nil {
		fail(w, http.StatusBadGateway, "loupe", "/api/ui: "+err.Error())
		return
	}
	from := fingerprint(roots)
	body, err := st.input(roots)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "noSuchNode", err.Error())
		return
	}

	s.mu.Lock()
	fromScreen := m.find(from.Key)
	// A named screen looks like the one under it, so while the tree is
	// unchanged the step starts where the last one landed.
	if cur := m.screen(m.Current); cur != nil && cur.Fingerprint.Named != "" && cur.Fingerprint.Base == from.Key {
		fromScreen = cur
	}
	s.mu.Unlock()
	if fromScreen == nil {
		// Moved off the map since the last step: record where it is first,
		// so the edge has a real start.
		c, err := s.look(m.Target, m.Compose, r.Header)
		if err != nil {
			loupeFail(w, err)
			return
		}
		s.mu.Lock()
		fromScreen, _, err = s.place(m, c)
		s.mu.Unlock()
		if err != nil {
			fail(w, http.StatusInternalServerError, "storage", err.Error())
			return
		}
	}

	if err := s.loupe.input(m.Target, body, r.Header); err != nil {
		loupeFail(w, err)
		return
	}
	// The input call can return while a long press is still held, and a held
	// screen does not change, so settling would end before the menu shows.
	if st.Ms != nil {
		time.Sleep(time.Duration(*st.Ms) * time.Millisecond)
	}
	c, err := s.look(m.Target, m.Compose, r.Header)
	if err != nil {
		loupeFail(w, err)
		return
	}
	if name := strings.TrimSpace(st.Screen); name != "" {
		c.fp = c.fp.named(name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	to, isNew, err := s.landing(m, c)
	if err != nil {
		fail(w, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	var edge *Edge
	if to.ID != fromScreen.ID {
		edge = m.addEdge(&Edge{From: fromScreen.ID, To: to.ID, Action: st.label(), Node: st.Node, Note: st.Note, Count: 1})
	}
	m.Current = to.ID
	if err := s.save(m); err != nil {
		fail(w, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": fromScreen.ID, "screen": s.screenView(m, to), "new": isNew,
		"edge": edge, "settled": c.stable,
	})
}

// handleScreen renames a screen ({"name"}) or folds it into another
// ({"mergeInto"}), when the fingerprint split one screen in two.
func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	m := s.lookup(w, r)
	if m == nil {
		return
	}
	var body struct {
		Name      *string `json:"name"`
		MergeInto string  `json:"mergeInto"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "badRequest", err.Error())
		return
	}
	m.steps.Lock()
	defer m.steps.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	sc := m.screen(r.PathValue("sid"))
	if sc == nil {
		fail(w, http.StatusNotFound, "noSuchScreen", "no screen "+r.PathValue("sid"))
		return
	}
	if body.Name != nil {
		sc.Name = strings.TrimSpace(*body.Name)
	}
	if body.MergeInto != "" {
		into := m.screen(body.MergeInto)
		if into == nil || into == sc {
			fail(w, http.StatusBadRequest, "badRequest", "mergeInto must name another screen")
			return
		}
		m.merge(sc, into)
		// The merged screen's files stay on disk until the map is deleted:
		// they are what the person looked at when they chose to merge.
		sc = into
	}
	if err := s.save(m); err != nil {
		fail(w, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.mapView(m))
}

// handleEdgeDelete drops the edges from one screen to another, for a step the
// fingerprint filed under the wrong screen. The screens stay.
func (s *Server) handleEdgeDelete(w http.ResponseWriter, r *http.Request) {
	m := s.lookup(w, r)
	if m == nil {
		return
	}
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" || to == "" {
		fail(w, http.StatusBadRequest, "badRequest", "from and to are required")
		return
	}
	m.steps.Lock()
	defer m.steps.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := m.Edges[:0]
	for _, e := range m.Edges {
		if e.From != from || e.To != to {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(m.Edges) {
		fail(w, http.StatusNotFound, "noSuchEdge", "no edge "+from+" -> "+to)
		return
	}
	m.Edges = kept
	if err := s.save(m); err != nil {
		fail(w, http.StatusInternalServerError, "storage", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.mapView(m))
}

var screenFiles = map[string]string{"screenshot.png": "image/png", "thumb.png": "image/png", "tree.json": "application/json"}

func (s *Server) handleScreenFile(w http.ResponseWriter, r *http.Request) {
	m := s.lookup(w, r)
	if m == nil {
		return
	}
	ct, ok := screenFiles[r.PathValue("file")]
	sid := r.PathValue("sid")
	if !ok || !regexp.MustCompile(`^s[0-9]+$`).MatchString(sid) {
		fail(w, http.StatusNotFound, "noSuchFile", "no such file")
		return
	}
	b, err := os.ReadFile(filepath.Join(s.mapDir(m.ID), sid, r.PathValue("file")))
	if err != nil {
		fail(w, http.StatusNotFound, "noSuchFile", "no such file")
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Write(b)
}
