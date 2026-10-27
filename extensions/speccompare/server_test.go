package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func pngBytes(t *testing.T, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

const tree = `{"width":320,"height":640,"root":{"class":"FrameLayout","bounds":[0,0,320,640],"children":[
 {"class":"LinearLayout","id":"app:id/content","bounds":[0,0,320,640],"children":[
  {"class":"Button","id":"app:id/send","text":"Send","bounds":[40,380,280,480]},
  {"class":"TextView","bounds":[40,100,280,140]}]}]}}`

func TestNodeForPicksTheDeepestNamedNode(t *testing.T) {
	roots, err := parseTree([]byte(tree))
	if err != nil {
		t.Fatal(err)
	}
	if n := nodeFor(roots, Rect{40, 400, 280, 448}); n == nil || n.ID != "app:id/send" || n.label() != "Send" {
		t.Fatalf("got %+v, want the Send button", n)
	}
	// The unnamed TextView gives way to its named parent.
	if n := nodeFor(roots, Rect{50, 110, 90, 130}); n == nil || n.label() != "content" {
		t.Fatalf("got %+v, want app:id/content", n)
	}
	if n := nodeFor(roots, Rect{0, 600, 400, 700}); n != nil {
		t.Fatalf("got %+v for a region off the screen", n)
	}
}

// fakeLoupe serves /api/screenshot and /api/ui and records overlay posts.
type fakeLoupe struct {
	shot     []byte
	mu       sync.Mutex
	overlays []overlay
	deleted  []string
	headers  http.Header
}

func (f *fakeLoupe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch r.URL.Path {
	case "/api/screenshot":
		w.Write(f.shot)
	case "/api/ui":
		w.Write([]byte(tree))
	case "/api/overlays":
		f.mu.Lock()
		defer f.mu.Unlock()
		f.headers = r.Header.Clone()
		if r.Method == "DELETE" {
			f.deleted = append(f.deleted, r.URL.Query().Get("source"))
			w.Write([]byte(`{"removed":1}`))
			return
		}
		var o overlay
		json.NewDecoder(r.Body).Decode(&o)
		f.overlays = append(f.overlays, o)
		w.Write([]byte(`{"overlays":[]}`))
	default:
		http.NotFound(w, r)
	}
}

func call(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("X-Loupe-Agent", "claude")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestPostAReferenceThenCompareShowsTheOverlay(t *testing.T) {
	ref := solid(160, 320, white) // the frame, in points
	fill(ref, Rect{20, 190, 140, 214}, blue)
	shot := solid(320, 640, white) // the device, in pixels
	fill(shot, Rect{40, 400, 280, 448}, blue)
	fill(shot, Rect{0, 0, 320, 32}, blue) // a live status bar, masked below

	fl := &fakeLoupe{shot: pngBytes(t, shot)}
	ls := httptest.NewServer(fl)
	defer ls.Close()
	s, err := NewServer(Config{LoupeURL: ls.URL, Token: "tok", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()

	code, out := call(t, h, "POST", "/references?target=avd:Pixel_7a", AddRequest{
		Image: base64.StdEncoding.EncodeToString(pngBytes(t, ref)),
		Label: "Send", Figma: &Figma{File: "abc", Node: "9:2"},
		Masks: []Rect{{0, 0, 320, 32}},
	})
	if code != 200 {
		t.Fatalf("add: %d %v", code, out)
	}
	id := out["id"].(string)
	if code, out := call(t, h, "GET", "/references?target=avd:Pixel_7a", nil); code != 200 || len(out["references"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}

	// No reference id: the newest one is used.
	code, out = call(t, h, "POST", "/compare?target=avd:Pixel_7a", nil)
	if code != 200 {
		t.Fatalf("compare: %d %v", code, out)
	}
	regions := out["regions"].([]any)
	if len(regions) == 0 {
		t.Fatalf("no regions for a button 16 px off; score %v", out["score"])
	}
	node := regions[0].(map[string]any)["node"].(map[string]any)
	if node["id"] != "app:id/send" {
		t.Fatalf("region names %v, want the Send button", node)
	}
	if out["reference"].(map[string]any)["id"] != id || out["overlayError"] != nil {
		t.Fatalf("compare body %v", out)
	}

	fl.mu.Lock()
	if len(fl.overlays) != 1 {
		t.Fatalf("%d overlay posts, want 1", len(fl.overlays))
	}
	o := fl.overlays[0]
	if o.ID != overlayID || o.Source != source || o.Image == "" || len(o.Boxes) == 0 || o.Boxes[0].Label != "Send" {
		t.Fatalf("overlay %+v", o)
	}
	if !strings.Contains(o.Label, "Send") {
		t.Fatalf("overlay label %q", o.Label)
	}
	if fl.headers.Get("X-Loupe-Agent") != "claude" {
		t.Fatal("the caller's agent header didn't reach loupe")
	}
	fl.mu.Unlock()

	diffPath := out["diff"].(string)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", diffPath, nil))
	if rec.Code != 200 || !bytes.HasPrefix(rec.Body.Bytes(), pngSig) {
		t.Fatalf("diff %s: %d", diffPath, rec.Code)
	}

	if code, _ := call(t, h, "DELETE", "/overlay?target=avd:Pixel_7a", nil); code != 200 || fl.deleted[0] != source {
		t.Fatalf("clear: %d %v", code, fl.deleted)
	}
	if code, _ := call(t, h, "DELETE", "/references/"+id+"?target=avd:Pixel_7a", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := call(t, h, "POST", "/compare?target=avd:Pixel_7a", nil); code != 404 {
		t.Fatalf("compare with no reference: %d, want 404", code)
	}
}

func TestCompareCanSkipTheOverlay(t *testing.T) {
	shot := solid(320, 640, white)
	fl := &fakeLoupe{shot: pngBytes(t, shot)}
	ls := httptest.NewServer(fl)
	defer ls.Close()
	s, _ := NewServer(Config{LoupeURL: ls.URL, Token: "tok", DataDir: t.TempDir()})
	h := s.Handler()
	call(t, h, "POST", "/references?target=t1", AddRequest{Image: base64.StdEncoding.EncodeToString(pngBytes(t, shot))})
	no := false
	code, out := call(t, h, "POST", "/compare?target=t1", CompareRequest{Overlay: &no})
	if code != 200 || out["score"].(float64) != 0 {
		t.Fatalf("compare: %d %v", code, out)
	}
	if len(fl.overlays) != 0 {
		t.Fatal("posted an overlay with overlay:false")
	}
}

func TestBadReferenceIDsAreRefused(t *testing.T) {
	s, _ := NewServer(Config{DataDir: t.TempDir()})
	h := s.Handler()
	if code, _ := call(t, h, "POST", "/compare?target=t1&reference=../../etc", nil); code != 404 {
		t.Fatalf("traversal id: %d", code)
	}
	if code, _ := call(t, h, "GET", "/diffs/..%2Fx.png", nil); code != 404 {
		t.Fatalf("traversal diff: %d", code)
	}
}
