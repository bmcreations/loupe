package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

// matchTarget is an Inspector and Actuator that records every tap. Each
// Inspect returns the next of uis, then the last one again.
type matchTarget struct {
	fakeAgentTarget
	mu       sync.Mutex
	uis      []*target.UI
	inspects int
	taps     [][2]int
}

func (f *matchTarget) Inspect(ctx context.Context) (*target.UI, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ui := f.uis[min(f.inspects, len(f.uis)-1)]
	f.inspects++
	return ui, nil
}

func (f *matchTarget) Tap(ctx context.Context, x, y int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taps = append(f.taps, [2]int{x, y})
	return nil
}

func signInScreen() *target.UI {
	return &target.UI{Width: 1080, Height: 2400, Root: &target.Node{
		Bounds: [4]int{0, 0, 1080, 2400}, Center: [2]int{540, 1200},
		Children: []*target.Node{
			{Text: "Sign in", Bounds: [4]int{100, 500, 980, 700}, Center: [2]int{540, 600}, Clickable: true},
			{Text: "Help", Bounds: [4]int{0, 2200, 200, 2300}, Center: [2]int{100, 2250}},
			{Text: "Help", Bounds: [4]int{880, 2200, 1080, 2300}, Center: [2]int{980, 2250}},
		},
	}}
}

func emptyScreen() *target.UI {
	return &target.UI{Width: 1080, Height: 2400, Root: &target.Node{Bounds: [4]int{0, 0, 1080, 2400}}}
}

func matchServer(t *testing.T, uis ...*target.UI) (*Server, *matchTarget) {
	t.Helper()
	m := NewManager(target.Options{})
	ft := &matchTarget{fakeAgentTarget: fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}, uis: uis}
	m.Register(ft)
	return &Server{mgr: m}, ft
}

func TestATapByTextTapsTheNodesCenterAndNamesIt(t *testing.T) {
	s, ft := matchServer(t, signInScreen())
	rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ft.taps) != 1 || ft.taps[0] != [2]int{540, 600} {
		t.Fatalf("taps = %v; want one at 540,600", ft.taps)
	}
	var resp InputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Matched == nil || resp.Matched.Text != "Sign in" || resp.Matched.Center != [2]int{540, 600} {
		t.Fatalf("response = %s; want ok and the Sign in node", rec.Body.String())
	}
}

func TestAnAmbiguousMatchTouchesNothingAndListsTheCandidates(t *testing.T) {
	s, ft := matchServer(t, signInScreen())
	rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Help"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != ErrCodeAmbiguous || len(body.Candidates) != 2 {
		t.Fatalf("body = %+v; want ambiguous with two candidates", body)
	}
	if len(ft.taps) != 0 {
		t.Fatalf("taps = %v; an ambiguous match must touch nothing", ft.taps)
	}

	rec = postInput(s, "dev1", `{"action":"tap","match":{"text":"Help","index":1}}`)
	if rec.Code != http.StatusOK || len(ft.taps) != 1 || ft.taps[0] != [2]int{980, 2250} {
		t.Fatalf("index 1: status %d taps %v", rec.Code, ft.taps)
	}
}

func TestANoMatchIs404AndTouchesNothing(t *testing.T) {
	s, ft := matchServer(t, signInScreen())
	rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign out"}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != ErrCodeNoMatch {
		t.Fatalf("code = %q; want noMatch", body.Code)
	}
	if len(ft.taps) != 0 || ft.inspects != 1 {
		t.Fatalf("taps %v inspects %d; want none and one look", ft.taps, ft.inspects)
	}
}

func TestAMatchWaitsForItsNode(t *testing.T) {
	s, ft := matchServer(t, emptyScreen(), emptyScreen(), signInScreen())
	rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in","waitMs":3000}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ft.inspects < 3 || len(ft.taps) != 1 {
		t.Fatalf("inspects %d taps %v; want three looks or more, then one tap", ft.inspects, ft.taps)
	}
}

func TestAMatchIsRejectedWhereItDoesNotBelong(t *testing.T) {
	s, ft := matchServer(t, signInScreen())
	for _, body := range []string{
		`{"action":"tap","x":1,"y":2,"match":{"text":"Sign in"}}`,
		`{"action":"swipe","x":1,"y":2,"x2":3,"y2":4,"match":{"text":"Sign in"}}`,
		`{"action":"tap","match":{}}`,
		`{"action":"tap","match":{"text":"Sign in","waitMs":999999}}`,
	} {
		if rec := postInput(s, "dev1", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d; want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
	if len(ft.taps) != 0 {
		t.Fatalf("taps = %v", ft.taps)
	}
}

// A match is resolved before arbitration, so a node that is not there is a
// 404 even while a person holds the target, not a 409 no retry would fix.
func TestANoMatchIsAnsweredBeforeTheHold(t *testing.T) {
	s, ft := matchServer(t, signInScreen())
	sess, err := s.mgr.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	id, _ := sess.AddViewer()
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 0, PointerID: 1})
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 1, PointerID: 1})

	rec := postInputAs(s, "dev1", `{"action":"tap","match":{"text":"Sign out"}}`, map[string]string{HeaderAgent: "claude"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404: %s", rec.Code, rec.Body.String())
	}
	rec = postInputAs(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`, map[string]string{HeaderAgent: "claude"})
	if rec.Code != http.StatusConflict || len(ft.taps) != 0 {
		t.Fatalf("status %d taps %v; want the hold's 409 and no tap", rec.Code, ft.taps)
	}
}

// The caller named a node, not a point, so it could not point first: the
// server sends the cursor to the node before the tap's own event.
func TestAMatchTapSendsTheCursorToTheNode(t *testing.T) {
	s, _ := matchServer(t, signInScreen())
	sess, err := s.mgr.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_, out := sess.AddViewer()
	for len(out) > 0 {
		<-out
	}
	rec := postInputAs(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`, map[string]string{HeaderAgent: "claude"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for len(out) > 0 {
		msg := <-out
		if msg.Data[0] != session.MsgCursor {
			continue
		}
		var e session.Event
		if err := json.Unmarshal(msg.Data[1:], &e); err != nil {
			t.Fatal(err)
		}
		if e.Agent != "claude" || e.From == nil || e.From.X != 0.5 || e.From.Y != 0.25 {
			t.Fatalf("cursor = %+v at %+v; want claude at 0.5, 0.25", e, e.From)
		}
		return
	}
	t.Fatal("no cursor reached the viewer")
}

// The trace names what a match tap looked for and what it hit, so a tap
// that landed on the wrong node can be read back from loupe-trace.jsonl.
func TestAMatchTapIsTracedWithItsMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := matchServer(t, signInScreen())
	s.trace = tw
	if rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	tw.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec map[string]any
		_ = json.Unmarshal([]byte(line), &rec)
		if rec["action"] == "tap" {
			if rec["match"] != `text="Sign in"` || rec["matchedText"] != "Sign in" {
				t.Fatalf("trace record %v; want the match and the node it hit", rec)
			}
			return
		}
	}
	t.Fatalf("no tap in the trace: %s", b)
}

func TestAWaitForANodeAnswersWithItAndTouchesNothing(t *testing.T) {
	s, ft := matchServer(t, emptyScreen(), signInScreen())
	rec := postInput(s, "dev1", `{"action":"wait","match":{"text":"Sign in"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp InputResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Matched == nil || resp.Matched.Text != "Sign in" || len(ft.taps) != 0 || ft.inspects != 2 {
		t.Fatalf("response %s, taps %v, inspects %d; want the node after a second look and no tap", rec.Body.String(), ft.taps, ft.inspects)
	}
}

// sendIconsScreen is the screen on its way out: icon buttons whose only
// labels are content-descs, and no node with text.
func sendIconsScreen(descs ...string) *target.UI {
	root := &target.Node{Bounds: [4]int{0, 0, 1080, 2400}, Center: [2]int{540, 1200}}
	for i, d := range descs {
		x := 900 - i*120
		root.Children = append(root.Children, &target.Node{
			Class: "ImageButton", Desc: d, Clickable: true,
			Bounds: [4]int{x, 100, x + 100, 200}, Center: [2]int{x + 50, 150},
		})
	}
	return &target.UI{Width: 1080, Height: 2400, Root: root}
}

// sendButtonScreen is the screen coming in, with a Send button labelled in
// its text.
func sendButtonScreen() *target.UI {
	return &target.UI{Width: 1080, Height: 2400, Root: &target.Node{
		Bounds: [4]int{0, 0, 1080, 2400}, Center: [2]int{540, 1200},
		Children: []*target.Node{
			{Class: "Button", Text: "Send", Clickable: true, Bounds: [4]int{100, 2000, 980, 2200}, Center: [2]int{540, 2100}},
		},
	}}
}

// While a match has time left, nodes found only by their content-desc that
// are more than one keep it looking, as no match would: a later dump may hold
// the node with that text. Once the time is up it answers as a single look.
func TestAPollingMatchLooksPastSeveralDescOnlyNodes(t *testing.T) {
	outgoing := sendIconsScreen("Send now", "Send later")
	for _, tc := range []struct {
		name, body string
		uis        []*target.UI
		status     int
		tap        bool
		minLooks   int // 0 means exactly one
	}{
		{"tap finds the button", `{"action":"tap","match":{"text":"send","contains":true,"waitMs":2000}}`,
			[]*target.UI{outgoing, outgoing, sendButtonScreen()}, http.StatusOK, true, 3},
		{"wait finds the button", `{"action":"wait","match":{"text":"send","contains":true,"waitMs":2000}}`,
			[]*target.UI{outgoing, outgoing, sendButtonScreen()}, http.StatusOK, false, 3},
		{"no button by the deadline", `{"action":"tap","match":{"text":"send","contains":true,"waitMs":300}}`,
			[]*target.UI{outgoing}, http.StatusConflict, false, 2},
		{"one look answers at once", `{"action":"tap","match":{"text":"send","contains":true}}`,
			[]*target.UI{outgoing, sendButtonScreen()}, http.StatusConflict, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ft := matchServer(t, tc.uis...)
			rec := postInput(s, "dev1", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d after %d looks; want %d: %s", rec.Code, ft.inspects, tc.status, rec.Body.String())
			}
			if tc.minLooks == 0 && ft.inspects != 1 || ft.inspects < tc.minLooks {
				t.Fatalf("inspects = %d; want %d", ft.inspects, max(tc.minLooks, 1))
			}
			switch {
			case tc.status == http.StatusConflict:
				var body ErrorResponse
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				if body.Code != ErrCodeAmbiguous || len(body.Candidates) != 2 || len(ft.taps) != 0 {
					t.Fatalf("body %+v taps %v; want ambiguous with both icons and no tap", body, ft.taps)
				}
			case tc.tap:
				if len(ft.taps) != 1 || ft.taps[0] != [2]int{540, 2100} {
					t.Fatalf("taps = %v; want one on the Send button at 540,2100", ft.taps)
				}
			default:
				var resp InputResponse
				_ = json.Unmarshal(rec.Body.Bytes(), &resp)
				if resp.Matched == nil || resp.Matched.Text != "Send" || len(ft.taps) != 0 {
					t.Fatalf("response %s taps %v; want the Send button and no tap", rec.Body.String(), ft.taps)
				}
			}
		})
	}
}

// One node found by its content-desc is the answer on the dump it is found
// in, polling or not: on Flutter and React Native it is the only node that
// will ever carry the label, so looking again would only add a dump.
func TestAOneDescOnlyNodeAnswersOnTheFirstLook(t *testing.T) {
	outgoing := sendIconsScreen("Send")
	for _, body := range []string{
		`{"action":"wait","match":{"text":"Send","waitMs":2000}}`,
		`{"action":"tap","match":{"text":"Send"}}`,
	} {
		s, ft := matchServer(t, outgoing, outgoing, sendButtonScreen())
		rec := postInput(s, "dev1", body)
		var resp InputResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if rec.Code != http.StatusOK || ft.inspects != 1 || resp.Matched == nil || resp.Matched.Desc != "Send" {
			t.Fatalf("%s: status %d after %d looks, matched %+v; want the icon on the first look", body, rec.Code, ft.inspects, resp.Matched)
		}
	}
}

func TestAWaitOfMsIsBounded(t *testing.T) {
	s, _ := matchServer(t, signInScreen())
	if rec := postInput(s, "dev1", `{"action":"wait","ms":999999}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	if rec := postInput(s, "dev1", `{"action":"wait","ms":5}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}
