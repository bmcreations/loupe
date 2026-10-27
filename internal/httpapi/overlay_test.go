package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	pngenc "image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

func pngBase64(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := pngenc.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func overlayCall(s *Server, method, query, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/api/overlays?"+query, strings.NewReader(body)))
	return rec
}

// nextOverlay drains out until a MsgOverlay and decodes it.
func nextOverlay(t *testing.T, out <-chan session.OutMsg) OverlaysResponse {
	t.Helper()
	for {
		select {
		case msg := <-out:
			if msg.Data[0] != session.MsgOverlay {
				continue
			}
			var got OverlaysResponse
			if err := json.Unmarshal(msg.Data[1:], &got); err != nil {
				t.Fatal(err)
			}
			return got
		default:
			t.Fatal("no MsgOverlay queued")
		}
	}
}

func TestAnOverlayReachesViewersInScreenFractions(t *testing.T) {
	s, sess, _ := heldSession(t)
	_, out := sess.AddViewer()
	for len(out) > 0 {
		<-out
	}
	body := `{"id":"spec","source":"spec-compare","image":"` + pngBase64(t, 1080, 2400) +
		`","boxes":[{"bounds":[0,600,540,1200],"label":"row","kind":"diff"}]}`
	rec := overlayCall(s, http.MethodPost, "target=dev1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := nextOverlay(t, out)
	if len(got.Overlays) != 1 {
		t.Fatalf("overlays = %+v; want one", got.Overlays)
	}
	o := got.Overlays[0]
	if o.Rect != [4]float64{0, 0, 1, 1} || o.Opacity != 0.5 || o.Width != 1080 || o.Height != 2400 {
		t.Errorf("overlay = %+v; want the whole screen at 0.5 with the image's size", o)
	}
	if len(o.Boxes) != 1 || o.Boxes[0].Rect != [4]float64{0, 0.25, 0.5, 0.5} || o.Boxes[0].Kind != "diff" {
		t.Errorf("boxes = %+v; want one diff at 0, .25, .5, .5", o.Boxes)
	}

	img := httptest.NewRecorder()
	s.Handler().ServeHTTP(img, httptest.NewRequest(http.MethodGet, o.Image, nil))
	if img.Code != http.StatusOK || img.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("image: status %d, type %q", img.Code, img.Header().Get("Content-Type"))
	}

	// A viewer that joins later gets the set straight after init.
	_, late := sess.AddViewer()
	if got := nextOverlay(t, late); len(got.Overlays) != 1 {
		t.Errorf("joiner got %+v; want the overlay", got.Overlays)
	}

	rec = overlayCall(s, http.MethodDelete, "target=dev1&source=spec-compare", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":1`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if got := nextOverlay(t, out); len(got.Overlays) != 0 {
		t.Errorf("after delete viewers got %+v; want an empty set", got.Overlays)
	}
}

func TestAnOverlayPostedBeforeAnyoneWatchesReachesTheFirstViewer(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400})
	s := &Server{mgr: m}
	rec := overlayCall(s, http.MethodPost, "target=dev1",
		`{"id":"a","source":"x","boxes":[{"bounds":[0,0,108,240]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	sess, err := m.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	_, out := sess.AddViewer()
	got := nextOverlay(t, out)
	if len(got.Overlays) != 1 || got.Overlays[0].Boxes[0].Rect != [4]float64{0, 0, 0.1, 0.1} {
		t.Fatalf("first viewer got %+v", got.Overlays)
	}
}

func TestReplacingAnOverlayKeepsItsPlace(t *testing.T) {
	s, _, _ := heldSession(t)
	for _, id := range []string{"a", "b", "a"} {
		rec := overlayCall(s, http.MethodPost, "target=dev1",
			`{"id":"`+id+`","source":"x","boxes":[{"bounds":[0,0,10,10]}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", id, rec.Code, rec.Body)
		}
	}
	var got OverlaysResponse
	_ = json.Unmarshal(overlayCall(s, http.MethodGet, "target=dev1", "").Body.Bytes(), &got)
	if len(got.Overlays) != 2 || got.Overlays[0].ID != "a" || got.Overlays[1].ID != "b" {
		t.Fatalf("overlays = %+v; want a then b", got.Overlays)
	}
}

func TestBadOverlaysAreRefused(t *testing.T) {
	s, _, _ := heldSession(t)
	for name, body := range map[string]string{
		"bad id":          `{"id":"a/b","source":"x","boxes":[{"bounds":[0,0,1,1]}]}`,
		"no source":       `{"id":"a","boxes":[{"bounds":[0,0,1,1]}]}`,
		"empty":           `{"id":"a","source":"x"}`,
		"not an image":    `{"id":"a","source":"x","image":"aGVsbG8="}`,
		"inverted box":    `{"id":"a","source":"x","boxes":[{"bounds":[5,5,1,1]}]}`,
		"opacity over 1":  `{"id":"a","source":"x","opacity":2,"boxes":[{"bounds":[0,0,1,1]}]}`,
		"not json at all": `nope`,
	} {
		if rec := overlayCall(s, http.MethodPost, "target=dev1", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, rec.Code, rec.Body)
		}
	}
}

func TestOverlaysPerTargetAreCapped(t *testing.T) {
	s, _, _ := heldSession(t)
	for i := 0; i <= maxOverlays; i++ {
		rec := overlayCall(s, http.MethodPost, "target=dev1",
			`{"id":"o`+string(rune('a'+i))+`","source":"x","boxes":[{"bounds":[0,0,1,1]}]}`)
		want := http.StatusOK
		if i == maxOverlays {
			want = http.StatusConflict
		}
		if rec.Code != want {
			t.Fatalf("post %d: status %d, want %d", i, rec.Code, want)
		}
	}
}

func TestAnOverlayOnATargetWithNoScreenSizeUsesTheImage(t *testing.T) {
	so, err := buildOverlay(OverlayRequest{ID: "a", Source: "x", Image: pngBase64(t, 200, 400),
		Bounds: &[4]int{0, 0, 100, 400}}, "synthetic", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if so.Rect != [4]float64{0, 0, 0.5, 1} {
		t.Fatalf("rect = %v; want the left half", so.Rect)
	}
	if _, err := buildOverlay(OverlayRequest{ID: "a", Source: "x",
		Boxes: []OverlayBoxRequest{{Bounds: [4]int{0, 0, 1, 1}}}}, "synthetic", 0, 0); err == nil {
		t.Fatal("boxes with no screen size and no image: want an error naming screen")
	}
}
