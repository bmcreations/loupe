package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// listTarget is a screen with one vertical list of rows. A swipe whose
// finger moves up scrolls it down a row, until it has shown maxPos rows
// past the first; a finger moving down scrolls it back toward row 0. It
// records each swipe so a test can see where the strokes ran.
type listTarget struct {
	id      string
	kind    target.Kind
	pos     int
	maxPos  int
	swipes  [][4]int
	visible int
	// bounce is how many dumps after a stroke that moved nothing show the
	// rows a few pixels off, as iOS does while a list springs back from
	// its edge.
	bounce   int
	bouncing int
	lastMs   int
}

func (f *listTarget) ID() string { return f.id }
func (f *listTarget) Kind() target.Kind {
	if f.kind == "" {
		return target.KindDevice
	}
	return f.kind
}
func (f *listTarget) Label() string { return f.id }
func (f *listTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	return &stubStream{frames: make(chan target.Frame)}, nil
}
func (f *listTarget) Tap(ctx context.Context, x, y int) error          { return nil }
func (f *listTarget) TypeText(ctx context.Context, s string) error     { return nil }
func (f *listTarget) PressKey(ctx context.Context, keycode int) error  { return nil }
func (f *listTarget) ScreenSize(ctx context.Context) (int, int, error) { return 1000, 2000, nil }
func (f *listTarget) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error {
	f.swipes = append(f.swipes, [4]int{x1, y1, x2, y2})
	f.lastMs = ms
	was := f.pos
	switch {
	case y2 < y1:
		f.pos = min(f.pos+1, f.maxPos)
	case y2 > y1:
		f.pos = max(f.pos-1, 0)
	}
	if f.pos == was {
		f.bouncing = f.bounce
	}
	return nil
}

func (f *listTarget) Inspect(ctx context.Context) (*target.UI, error) {
	n := f.visible
	if n == 0 {
		n = 5
	}
	list := &target.Node{Class: "RecyclerView", Scrollable: true, Bounds: [4]int{0, 200, 1000, 1800}, Center: [2]int{500, 1000}}
	off := 0
	if f.bouncing > 0 {
		off = 4 * f.bouncing
		f.bouncing--
	}
	for i := 0; i < n; i++ {
		y := 200 + i*300 + off
		list.Children = append(list.Children, &target.Node{
			Class: "TextView", Text: fmt.Sprintf("row %d", f.pos+i),
			Bounds: [4]int{0, y, 1000, y + 300}, Center: [2]int{500, y + 150},
		})
	}
	clock := &target.Node{Class: "TextView", ID: "com.android.systemui:id/clock", Text: "9:41",
		Bounds: [4]int{20, 0, 120, 60}, Center: [2]int{70, 30}}
	root := &target.Node{Class: "FrameLayout", Bounds: [4]int{0, 0, 1000, 2000}, Center: [2]int{500, 1000},
		Children: []*target.Node{clock, list}}
	return &target.UI{Width: 1000, Height: 2000, Root: root}, nil
}

func scrollServer(t *testing.T, ft *listTarget) *Server {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(ft)
	return &Server{mgr: m}
}

func decodeInput(t *testing.T, body []byte) InputResponse {
	t.Helper()
	var resp InputResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return resp
}

func TestScrollUntilEdgeStopsWhenTheListStopsMoving(t *testing.T) {
	ft := &listTarget{id: "dev1", maxPos: 3}
	s := scrollServer(t, ft)
	rec := postInput(s, "dev1", `{"action":"scroll","direction":"down","until":"edge"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeInput(t, rec.Body.Bytes())
	// Three strokes that moved, and the fourth that did not.
	if resp.Swipes != 4 || !resp.AtEdge {
		t.Fatalf("swipes = %d, atEdge = %v; want 4, true", resp.Swipes, resp.AtEdge)
	}
	if resp.Region == nil || *resp.Region != [4]int{0, 200, 1000, 1800} {
		t.Fatalf("region = %v; want the list's bounds", resp.Region)
	}
	for _, sw := range ft.swipes {
		if sw[3] >= sw[1] {
			t.Fatalf("swipe %v: scrolling down moves the finger up", sw)
		}
		if sw[1] < 200 || sw[1] > 1800 || sw[3] < 200 || sw[3] > 1800 {
			t.Fatalf("swipe %v runs outside the list", sw)
		}
	}
}

func TestScrollUpAtTheTopAnswersAtEdgeAfterOneStroke(t *testing.T) {
	ft := &listTarget{id: "dev1", maxPos: 10}
	s := scrollServer(t, ft)
	rec := postInput(s, "dev1", `{"action":"scroll","direction":"up","until":"edge"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeInput(t, rec.Body.Bytes())
	if resp.Swipes != 1 || !resp.AtEdge {
		t.Fatalf("swipes = %d, atEdge = %v; want 1, true", resp.Swipes, resp.AtEdge)
	}
}

func TestScrollWithNoUntilMakesOneStroke(t *testing.T) {
	ft := &listTarget{id: "dev1", maxPos: 10}
	s := scrollServer(t, ft)
	rec := postInput(s, "dev1", `{"action":"scroll","direction":"down"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeInput(t, rec.Body.Bytes())
	if resp.Swipes != 1 || resp.AtEdge {
		t.Fatalf("swipes = %d, atEdge = %v; want 1, false", resp.Swipes, resp.AtEdge)
	}
}

func TestScrollFindStopsAtTheNode(t *testing.T) {
	ft := &listTarget{id: "dev1", maxPos: 20}
	s := scrollServer(t, ft)
	rec := postInput(s, "dev1", `{"action":"scroll","direction":"down","find":{"text":"row 7"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeInput(t, rec.Body.Bytes())
	// Rows 0-4 are on screen; each stroke shows one more.
	if resp.Swipes != 3 || resp.Matched == nil || resp.Matched.Text != "row 7" {
		t.Fatalf("swipes = %d, matched = %+v; want 3, row 7", resp.Swipes, resp.Matched)
	}
}

func TestScrollFindAlreadyOnScreenDoesNotSwipe(t *testing.T) {
	ft := &listTarget{id: "dev1", maxPos: 20}
	s := scrollServer(t, ft)
	rec := postInput(s, "dev1", `{"action":"scroll","direction":"down","find":{"text":"row 2"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ft.swipes) != 0 {
		t.Fatalf("swipes = %v; want none", ft.swipes)
	}
}

func TestScrollFindPastTheEdgeIsNoMatch(t *testing.T) {
	ft := &listTarget{id: "dev1", maxPos: 2}
	s := scrollServer(t, ft)
	rec := postInput(s, "dev1", `{"action":"scroll","direction":"down","find":{"text":"row 99"}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var e ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != ErrCodeNoMatch {
		t.Fatalf("code = %q; want noMatch", e.Code)
	}
	if len(ft.swipes) != 3 {
		t.Fatalf("swipes = %d; want 3, two that moved and one at the edge", len(ft.swipes))
	}
}

func TestScrollOnASimulatorSettlesBeforeComparing(t *testing.T) {
	ft := &listTarget{id: "sim1", kind: target.KindSimulator, maxPos: 2}
	s := scrollServer(t, ft)
	rec := postInput(s, "sim1", `{"action":"scroll","direction":"down","until":"edge"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeInput(t, rec.Body.Bytes())
	if resp.Swipes != 3 || !resp.AtEdge {
		t.Fatalf("swipes = %d, atEdge = %v; want 3, true", resp.Swipes, resp.AtEdge)
	}
}

// A list at its edge on iOS springs back for about a second after the
// stroke. A settle shorter than that read each bounce as movement and
// scrolled to max without ever answering atEdge.
func TestScrollOnASimulatorWaitsOutTheBounceAtTheEdge(t *testing.T) {
	ft := &listTarget{id: "sim1", kind: target.KindSimulator, maxPos: 10, bounce: 8}
	s := scrollServer(t, ft)
	rec := postInput(s, "sim1", `{"action":"scroll","direction":"up","until":"edge"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeInput(t, rec.Body.Bytes())
	if resp.Swipes != 1 || !resp.AtEdge {
		t.Fatalf("swipes = %d, atEdge = %v; want 1, true", resp.Swipes, resp.AtEdge)
	}
	if ft.lastMs != scrollFlingMs {
		t.Fatalf("stroke ms = %d; want a %dms fling on a simulator", ft.lastMs, scrollFlingMs)
	}
}

func TestScrollToTheEdgeOnAndroidDoesNotFling(t *testing.T) {
	ft := &listTarget{id: "dev1", maxPos: 1}
	s := scrollServer(t, ft)
	if rec := postInput(s, "dev1", `{"action":"scroll","direction":"down","until":"edge"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ft.lastMs != defaultSwipeMs {
		t.Fatalf("stroke ms = %d; want %d on Android", ft.lastMs, defaultSwipeMs)
	}
}

func TestScrollStopsWhenAPersonHoldsTheDevice(t *testing.T) {
	ft := &listTarget{id: "dev1", maxPos: 10}
	s := scrollServer(t, ft)
	sess, err := s.mgr.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()
	id, _ := sess.AddViewer()
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 0, PointerID: 1})
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 1, PointerID: 1})
	rec := postInput(s, "dev1", `{"action":"scroll","direction":"down","until":"edge"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s; want 409 while a person holds it", rec.Code, rec.Body.String())
	}
	if len(ft.swipes) != 0 {
		t.Fatalf("swipes = %v; want none", ft.swipes)
	}
}

func TestScrollRejectsABadDirection(t *testing.T) {
	ft := &listTarget{id: "dev1"}
	s := scrollServer(t, ft)
	for _, body := range []string{
		`{"action":"scroll"}`,
		`{"action":"scroll","direction":"sideways"}`,
		`{"action":"scroll","direction":"down","until":"forever"}`,
		`{"action":"scroll","direction":"down","max":31}`,
		`{"action":"scroll","direction":"down","find":{}}`,
	} {
		if rec := postInput(s, "dev1", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d; want 400", body, rec.Code)
		}
	}
}

func TestScrollStrokeKeepsClearOfTheScreenEdges(t *testing.T) {
	x1, y1, x2, y2 := scrollStrokeFor([4]int{0, 0, 1000, 2000}, 1000, 2000, "up", 0, 0, false)
	if y1 < 160 || y2 > 1840 || x1 != x2 {
		t.Fatalf("stroke %d,%d -> %d,%d; want it inside the 8%% band", x1, y1, x2, y2)
	}
}
