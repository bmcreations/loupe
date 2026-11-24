package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	imgpng "image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// flagTarget can be screenshotted, inspected and sized, as a device can.
type flagTarget struct {
	screenshotTarget
	ui *target.UI
}

func (f *flagTarget) Inspect(ctx context.Context) (*target.UI, error)  { return f.ui, nil }
func (f *flagTarget) ScreenSize(ctx context.Context) (int, int, error) { return 100, 200, nil }

func whitePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	var b bytes.Buffer
	if err := imgpng.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func newFlagServer(t *testing.T) (*Server, *flagTarget) {
	t.Helper()
	f := &flagTarget{
		screenshotTarget: screenshotTarget{id: "dev1", png: whitePNG(t, 100, 200)},
		ui: &target.UI{Width: 100, Height: 200, Root: &target.Node{
			Text: "Send", Clickable: true, Bounds: [4]int{10, 20, 90, 60},
		}},
	}
	m := NewManager(target.Options{})
	m.Register(f)
	return &Server{mgr: m, token: "secret", flags: newFlagStore()}, f
}

func decodeFlag(t *testing.T, body []byte) Flag {
	t.Helper()
	var f Flag
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return f
}

func TestFlagKeepsTheScreenAndTheRegionInDevicePixels(t *testing.T) {
	s, _ := newFlagServer(t)
	w := serveOn(s, http.MethodPost, "/api/flags?target=dev1",
		`{"note":"the badge is clipped","region":[0.1,0.25,0.5,0.5]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	f := decodeFlag(t, w.Body.Bytes())
	if f.ID != 1 || f.Target != "dev1" || f.Note != "the badge is clipped" || f.Watching {
		t.Errorf("flag = %+v", f)
	}
	if f.Region == nil || *f.Region != (FlagRect{X: 10, Y: 50, Width: 40, Height: 50}) {
		t.Errorf("region = %+v, want 10,50 40x50", f.Region)
	}
	if len(f.Start.UI) != 1 || f.Start.UI[0].Text != "Send" {
		t.Errorf("ui = %+v", f.Start.UI)
	}
	if f.Start.Screenshot != "/api/flags/shot?id=1&which=start" {
		t.Fatalf("screenshot = %q", f.Start.Screenshot)
	}

	shot := serveOn(s, http.MethodGet, f.Start.Screenshot, "", nil)
	if shot.Code != http.StatusOK || shot.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("shot status = %d, type %q", shot.Code, shot.Header().Get("Content-Type"))
	}
	img, err := imgpng.Decode(bytes.NewReader(shot.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	// The region's top-left corner is outlined; the middle of the screen is not.
	if r, g, _, _ := img.At(10, 50).RGBA(); r>>8 != 0xff || g>>8 == 0xff {
		t.Errorf("corner of the region is %v, want red", img.At(10, 50))
	}
	if c := color.RGBAModel.Convert(img.At(30, 70)).(color.RGBA); c.G != 0xff {
		t.Errorf("inside the region is %v, want white", c)
	}
}

func TestFlagRefusesABadRegion(t *testing.T) {
	s, _ := newFlagServer(t)
	for _, body := range []string{
		`{"region":[0.5,0.5,0.1,0.9]}`,
		`{"region":[0,0,1]}`,
		`{"region":[0,0,1.5,1]}`,
	} {
		if w := serveOn(s, http.MethodPost, "/api/flags?target=dev1", body, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, w.Code)
		}
	}
}

func TestFlagListLeavesOutTheTreeAndFiltersByRevision(t *testing.T) {
	s, _ := newFlagServer(t)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"note":"one"}`, nil)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"note":"two"}`, nil)

	var list FlagsResponse
	w := serveOn(s, http.MethodGet, "/api/flags?after=1", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Rev != 2 || len(list.Flags) != 1 || list.Flags[0].Note != "two" || list.Flags[0].Start.UI != nil {
		t.Errorf("list = %+v", list)
	}

	one := decodeFlag(t, serveOn(s, http.MethodGet, "/api/flags?id=1", "", nil).Body.Bytes())
	if one.Note != "one" || len(one.Start.UI) != 1 {
		t.Errorf("flag 1 = %+v", one)
	}
}

func TestFlagWaitWakesOnANewFlag(t *testing.T) {
	s, _ := newFlagServer(t)
	done := make(chan FlagsResponse, 1)
	go func() {
		var list FlagsResponse
		w := serveOn(s, http.MethodGet, "/api/flags?after=0&waitMs=5000", "", nil)
		json.Unmarshal(w.Body.Bytes(), &list)
		done <- list
	}()
	time.Sleep(50 * time.Millisecond)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"note":"now"}`, nil)
	select {
	case list := <-done:
		if len(list.Flags) != 1 || list.Flags[0].Note != "now" {
			t.Errorf("woke with %+v", list)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the wait did not wake")
	}
}

func TestFlagWatchEndsWithASecondScreen(t *testing.T) {
	s, _ := newFlagServer(t)
	f := decodeFlag(t, serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"watch":true}`, nil).Body.Bytes())
	if !f.Watching || f.End != nil {
		t.Fatalf("flag = %+v, want watching", f)
	}
	w := serveOn(s, http.MethodPost, "/api/flags/end?id=1", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("end status = %d: %s", w.Code, w.Body)
	}
	f = decodeFlag(t, w.Body.Bytes())
	if f.Watching || f.End == nil || f.End.Screenshot != "/api/flags/shot?id=1&which=end" || f.Rev != 2 {
		t.Errorf("ended flag = %+v", f)
	}
	// Ending again changes nothing.
	again := decodeFlag(t, serveOn(s, http.MethodPost, "/api/flags/end?id=1", "", nil).Body.Bytes())
	if again.Rev != 2 {
		t.Errorf("second end moved rev to %d", again.Rev)
	}
}

func TestFlagStoreKeepsTheNewest(t *testing.T) {
	st := newFlagStore()
	for range maxFlags + 3 {
		st.add(&storedFlag{})
	}
	list := st.list(0, "")
	if len(list.Flags) != maxFlags || list.Flags[0].ID != 4 {
		t.Errorf("kept %d flags, oldest %d", len(list.Flags), list.Flags[0].ID)
	}
}

func deleteFlags(t *testing.T, s *Server, query string, wantStatus int) []byte {
	t.Helper()
	w := serveOn(s, http.MethodDelete, "/api/flags"+query, "", nil)
	if w.Code != wantStatus {
		t.Fatalf("DELETE /api/flags%s = %d, want %d: %s", query, w.Code, wantStatus, w.Body)
	}
	return w.Body.Bytes()
}

func flagIDs(t *testing.T, s *Server) []uint64 {
	t.Helper()
	var list FlagsResponse
	json.Unmarshal(serveOn(s, http.MethodGet, "/api/flags", "", nil).Body.Bytes(), &list)
	ids := []uint64{}
	for _, f := range list.Flags {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestDeleteFlagByIDRemovesOnlyThatFlag(t *testing.T) {
	s, _ := newFlagServer(t)
	for range 3 {
		serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{}`, nil)
	}
	var got DeleteFlagsResponse
	json.Unmarshal(deleteFlags(t, s, "?id=2", http.StatusOK), &got)
	if got.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", got.Deleted)
	}
	if ids := flagIDs(t, s); len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Errorf("flags left = %v, want [1 3]", ids)
	}
	if w := serveOn(s, http.MethodGet, "/api/flags?id=2", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("GET of the deleted flag = %d, want 404", w.Code)
	}
	// Deleting it again is a 404, as reading it is.
	body := deleteFlags(t, s, "?id=2", http.StatusNotFound)
	var e ErrorResponse
	json.Unmarshal(body, &e)
	if e.Error != fmt.Sprintf("no flag 2; the server keeps the last %d", maxFlags) {
		t.Errorf("error = %q", e.Error)
	}
	deleteFlags(t, s, "?id=99", http.StatusNotFound)
}

func TestDeleteFlagsByTargetLeavesOtherTargets(t *testing.T) {
	s, _ := newFlagServer(t)
	other := &flagTarget{screenshotTarget: screenshotTarget{id: "dev2", png: whitePNG(t, 100, 200)}, ui: &target.UI{Width: 100, Height: 200, Root: &target.Node{}}}
	s.mgr.Register(other)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{}`, nil)
	serveOn(s, http.MethodPost, "/api/flags?target=dev2", `{}`, nil)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{}`, nil)

	var got DeleteFlagsResponse
	json.Unmarshal(deleteFlags(t, s, "?target=dev1", http.StatusOK), &got)
	if got.Deleted != 2 {
		t.Errorf("deleted = %d, want 2", got.Deleted)
	}
	if ids := flagIDs(t, s); len(ids) != 1 || ids[0] != 2 {
		t.Errorf("flags left = %v, want [2]", ids)
	}
	// A target with no flags is not an error: nothing to clear.
	json.Unmarshal(deleteFlags(t, s, "?target=dev1", http.StatusOK), &got)
	if got.Deleted != 0 {
		t.Errorf("second delete = %d, want 0", got.Deleted)
	}
	json.Unmarshal(deleteFlags(t, s, "?target=nope", http.StatusOK), &got)
	if got.Deleted != 0 {
		t.Errorf("unknown target = %d, want 0", got.Deleted)
	}
}

func TestDeleteFlagsRefusesBadQueries(t *testing.T) {
	s, _ := newFlagServer(t)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{}`, nil)
	deleteFlags(t, s, "", http.StatusBadRequest)
	deleteFlags(t, s, "?id=abc", http.StatusBadRequest)
	deleteFlags(t, s, "?id=-1", http.StatusBadRequest)
	// Both given is ambiguous, so it is refused and nothing is deleted.
	deleteFlags(t, s, "?id=1&target=dev1", http.StatusBadRequest)
	if ids := flagIDs(t, s); len(ids) != 1 {
		t.Errorf("flags left = %v, want the one flag untouched", ids)
	}
}

// An agent holding the token must not clear a person's stop without reading
// it, so deleting an unread interrupt is refused and nothing goes, whether it
// is still open, ended, or read only by an agent it does not stop. Presence
// is checked with the list and the gate with mayAct: a GET by id would be a
// read.
func TestDeletingAnUnreadInterruptIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(s *Server)
	}{
		{"open", func(s *Server) {}},
		{"ended", func(s *Server) {
			serveOn(s, http.MethodPost, "/api/flags/end?id=2", "", nil)
		}},
		{"read by another session", func(s *Server) {
			serveOn(s, http.MethodPost, "/api/flags/end?id=2", "", nil)
			serveOn(s, http.MethodGet, "/api/flags?id=2", "", map[string]string{HeaderAgent: "claude", HeaderSession: "s2"})
		}},
	} {
		for _, q := range []string{"?id=2", "?target=dev1"} {
			s, _ := newFlagServer(t)
			s.claims = newClaimBook(nil)
			if _, refused := s.claims.take("dev1", "claude", "s1", ""); refused != nil {
				t.Fatalf("claim refused: %+v", refused)
			}
			holder := inputCaller{agent: "claude", session: "s1"}
			req := httptest.NewRequest(http.MethodPost, "/api/input?target=dev1", nil)
			serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{}`, nil)
			serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"kind":"interrupt"}`, nil)
			tc.setup(s)

			var e ErrorResponse
			json.Unmarshal(deleteFlags(t, s, q, http.StatusConflict), &e)
			if e.Code != "unreadInterrupt" || e.ID != "2" {
				t.Errorf("%s %s: refusal = %+v, want code unreadInterrupt naming flag 2", tc.name, q, e)
			}
			if !strings.Contains(e.Error, "GET /api/flags?id=2") {
				t.Errorf("%s %s: error %q does not say to read flag 2 first", tc.name, q, e.Error)
			}
			if ids := flagIDs(t, s); len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
				t.Errorf("%s %s: flags left = %v, want [1 2]: a refused delete removes nothing", tc.name, q, ids)
			}
			if r := s.mayAct(req, "dev1", "tap", holder); r == nil || r.Code != ErrCodeInterrupted {
				t.Errorf("%s %s: tap after the refused delete = %+v, want still interrupted", tc.name, q, r)
			}
		}
	}
}

func TestDeletingAReadInterruptSucceeds(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want int
	}{{"?id=2", 1}, {"?target=dev1", 2}} {
		s, _ := newFlagServer(t)
		req := httptest.NewRequest(http.MethodPost, "/api/input?target=dev1", nil)
		caller := inputCaller{agent: "claude", session: "s1"}
		serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{}`, nil)
		serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"kind":"interrupt"}`, nil)
		serveOn(s, http.MethodPost, "/api/flags/end?id=2", "", nil)
		serveOn(s, http.MethodGet, "/api/flags?id=2", "", map[string]string{HeaderAgent: "claude", HeaderSession: "s1"})
		if r := s.mayAct(req, "dev1", "tap", caller); r != nil {
			t.Fatalf("%s: tap after reading the interrupt = %+v, want allowed", tc.q, r)
		}

		var got DeleteFlagsResponse
		json.Unmarshal(deleteFlags(t, s, tc.q, http.StatusOK), &got)
		if got.Deleted != tc.want {
			t.Errorf("%s: deleted = %d, want %d", tc.q, got.Deleted, tc.want)
		}
		if r := s.mayAct(req, "dev1", "tap", caller); r != nil {
			t.Errorf("%s: tap after the delete = %+v, want allowed", tc.q, r)
		}
	}
}

func TestDeletingAFlagLeavesAnotherTargetsInterrupt(t *testing.T) {
	s, _ := newFlagServer(t)
	other := &flagTarget{screenshotTarget: screenshotTarget{id: "dev2", png: whitePNG(t, 100, 200)}, ui: &target.UI{Width: 100, Height: 200, Root: &target.Node{}}}
	s.mgr.Register(other)
	serveOn(s, http.MethodPost, "/api/flags?target=dev2", `{"kind":"interrupt"}`, nil)
	deleteFlags(t, s, "?target=dev1", http.StatusOK)
	if _, held := s.flags.interrupted("dev2", ""); !held {
		t.Error("deleting dev1's flags lifted dev2's interrupt")
	}
}

func TestDeleteFlagDoesNotWakeOrFeedAWaiter(t *testing.T) {
	s, _ := newFlagServer(t)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"note":"old"}`, nil)
	var list FlagsResponse
	json.Unmarshal(serveOn(s, http.MethodGet, "/api/flags", "", nil).Body.Bytes(), &list)

	done := make(chan FlagsResponse, 1)
	go func() {
		var got FlagsResponse
		w := serveOn(s, http.MethodGet, fmt.Sprintf("/api/flags?after=%d&waitMs=5000", list.Rev), "", nil)
		json.Unmarshal(w.Body.Bytes(), &got)
		done <- got
	}()
	time.Sleep(50 * time.Millisecond)
	deleteFlags(t, s, "?id=1", http.StatusOK)
	select {
	case got := <-done:
		t.Fatalf("a delete answered the waiter with %+v", got)
	case <-time.After(150 * time.Millisecond):
	}
	// The waiter is still live, and hears only the next new flag.
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"note":"new"}`, nil)
	select {
	case got := <-done:
		if len(got.Flags) != 1 || got.Flags[0].Note != "new" {
			t.Errorf("waiter got %+v, want only the new flag", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiter did not wake on a new flag after a delete")
	}
}

func TestDeletingAWatchingFlagStopsItsTimerAndEnd(t *testing.T) {
	s, _ := newFlagServer(t)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"watch":true}`, nil)
	deleteFlags(t, s, "?id=1", http.StatusOK)
	if w := serveOn(s, http.MethodPost, "/api/flags/end?id=1", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("end of a deleted flag = %d, want 404", w.Code)
	}
	if ids := flagIDs(t, s); len(ids) != 0 {
		t.Errorf("flags = %v, want none", ids)
	}
}

func TestFlagListenersAreTheSessionsWaiting(t *testing.T) {
	s, _ := newFlagServer(t)
	s.claims = newClaimBook(func(map[string]any) {})
	old := listenerGrace
	listenerGrace = 0
	defer func() { listenerGrace = old }()

	serveOn(s, http.MethodPost, "/api/claims?target=dev1", "", map[string]string{HeaderAgent: "claude", HeaderSession: "a"})
	done := make(chan struct{})
	for _, sess := range []string{"a", "b"} {
		go func() {
			serveOn(s, http.MethodGet, "/api/flags?after=0&waitMs=5000", "", map[string]string{HeaderAgent: "claude", HeaderSession: sess})
			done <- struct{}{}
		}()
	}
	// A plain list is not listening.
	serveOn(s, http.MethodGet, "/api/flags", "", map[string]string{HeaderAgent: "claude", HeaderSession: "c"})
	time.Sleep(50 * time.Millisecond)

	var got FlagListenersResponse
	json.Unmarshal(serveOn(s, http.MethodGet, "/api/flags/listeners?target=dev1", "", nil).Body.Bytes(), &got)
	want := []FlagListener{{Agent: "claude", Session: "a", Holds: true}, {Agent: "claude", Session: "b"}}
	if got.ClaimedBy != "claude" || len(got.Listeners) != 2 || got.Listeners[0] != want[0] || got.Listeners[1] != want[1] {
		t.Errorf("listeners = %+v, want %+v claimed by claude", got, want)
	}
	if got.Holder == nil || *got.Holder != (FlagRecipient{Agent: "claude", Session: "a"}) {
		t.Errorf("holder = %+v, want claude session a", got.Holder)
	}

	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{}`, nil)
	<-done
	<-done
	got = FlagListenersResponse{}
	json.Unmarshal(serveOn(s, http.MethodGet, "/api/flags/listeners", "", nil).Body.Bytes(), &got)
	if len(got.Listeners) != 0 {
		t.Errorf("after the polls answered, listeners = %+v", got.Listeners)
	}
}

func TestAnInterruptStopsTheHolderUntilItReadsTheEnd(t *testing.T) {
	s, _ := newFlagServer(t)
	s.claims = newClaimBook(nil)
	if _, refused := s.claims.take("dev1", "claude", "s1", ""); refused != nil {
		t.Fatalf("claim refused: %+v", refused)
	}
	holder := inputCaller{agent: "claude", session: "s1"}
	req := httptest.NewRequest(http.MethodPost, "/api/input?target=dev1", nil)

	w := serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"kind":"interrupt"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	f := decodeFlag(t, w.Body.Bytes())
	if f.Kind != FlagInterrupt || !f.Watching {
		t.Fatalf("flag = %+v, want an open interrupt", f)
	}
	refused := s.mayAct(req, "dev1", "tap", holder)
	if refused == nil || refused.Code != ErrCodeInterrupted || refused.ID != "1" {
		t.Fatalf("holder's tap = %+v, want interrupted by flag 1", refused)
	}
	if r := s.mayAct(req, "dev1", "move", holder); r != nil {
		t.Errorf("pointing was refused: %+v", r)
	}

	// Read mid-interrupt: the end screen is not in yet, so it still stops.
	hdr := map[string]string{HeaderAgent: "claude", HeaderSession: "s1"}
	serveOn(s, http.MethodGet, "/api/flags?id=1", "", hdr)
	if r := s.mayAct(req, "dev1", "tap", holder); r == nil || r.Code != ErrCodeInterrupted {
		t.Fatalf("after a read mid-interrupt, tap = %+v; want still interrupted", r)
	}

	w = serveOn(s, http.MethodPost, "/api/flags/end?id=1", `{"note":"wrong chat opened","region":[0,0,0.5,0.5]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("end status = %d: %s", w.Code, w.Body)
	}
	f = decodeFlag(t, w.Body.Bytes())
	if f.Watching || f.End == nil || f.Note != "wrong chat opened" || f.Region == nil || f.Region.Width != 50 {
		t.Fatalf("ended flag = %+v, want the note and region from the end", f)
	}

	// Another session's read lifts nothing.
	serveOn(s, http.MethodGet, "/api/flags?id=1", "", map[string]string{HeaderAgent: "claude", HeaderSession: "s2"})
	if r := s.mayAct(req, "dev1", "tap", holder); r == nil || r.Code != ErrCodeInterrupted {
		t.Fatalf("after another session read it, tap = %+v; want still interrupted", r)
	}
	serveOn(s, http.MethodGet, "/api/flags?id=1", "", hdr)
	if r := s.mayAct(req, "dev1", "tap", holder); r != nil {
		t.Fatalf("after the holder read it, tap = %+v; want allowed", r)
	}
}

func TestAnUnclaimedInterruptStopsEveryAgent(t *testing.T) {
	s, _ := newFlagServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/input?target=dev1", nil)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"kind":"interrupt"}`, nil)
	serveOn(s, http.MethodPost, "/api/flags/end?id=1", "", nil)
	for _, c := range []inputCaller{{agent: "claude", session: "a"}, {agent: "codex"}} {
		if r := s.mayAct(req, "dev1", "tap", c); r == nil || r.Code != ErrCodeInterrupted {
			t.Errorf("%+v: tap = %+v, want interrupted", c, r)
		}
	}
	serveOn(s, http.MethodGet, "/api/flags?id=1", "", map[string]string{HeaderAgent: "codex"})
	if r := s.mayAct(req, "dev1", "tap", inputCaller{agent: "claude", session: "a"}); r != nil {
		t.Errorf("once one agent read it, tap = %+v; want allowed", r)
	}
}

func TestAViewersReadLeavesAnInterruptInPlace(t *testing.T) {
	s, _ := newFlagServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/input?target=dev1", nil)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"kind":"interrupt"}`, nil)
	serveOn(s, http.MethodPost, "/api/flags/end?id=1", "", nil)
	serveOn(s, http.MethodGet, "/api/flags?id=1", "", map[string]string{HeaderViewer: "7"})
	if r := s.mayAct(req, "dev1", "tap", inputCaller{agent: "codex"}); r == nil || r.Code != ErrCodeInterrupted {
		t.Errorf("after the person's app read it, tap = %+v; want interrupted", r)
	}
}

func TestFlagRefusesAnUnknownKind(t *testing.T) {
	s, _ := newFlagServer(t)
	if w := serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"kind":"panic"}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAnInterruptSentToASessionStopsOnlyThatSession(t *testing.T) {
	s, _ := newFlagServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/input?target=dev1", nil)
	w := serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"kind":"interrupt","to":{"agent":"codex","session":"s2"}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if f := decodeFlag(t, w.Body.Bytes()); f.To == nil || f.To.Session != "s2" {
		t.Fatalf("flag.to = %+v, want session s2", f.To)
	}
	if r := s.mayAct(req, "dev1", "tap", inputCaller{agent: "codex", session: "s2"}); r == nil || r.Code != ErrCodeInterrupted {
		t.Errorf("recipient's tap = %+v, want interrupted", r)
	}
	if r := s.mayAct(req, "dev1", "tap", inputCaller{agent: "claude", session: "s1"}); r != nil && r.Code == ErrCodeInterrupted {
		t.Errorf("other session's tap = %+v; the interrupt was sent to s2", r)
	}
}

func TestFlagRefusesAnEmptyRecipient(t *testing.T) {
	s, _ := newFlagServer(t)
	if w := serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"to":{}}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAnInterruptsEndCanPickTheSessionItStops(t *testing.T) {
	s, _ := newFlagServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/input?target=dev1", nil)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"kind":"interrupt"}`, nil)
	a, b := inputCaller{agent: "claude", session: "a"}, inputCaller{agent: "claude", session: "b"}
	if r := s.mayAct(req, "dev1", "tap", b); r == nil || r.Code != ErrCodeInterrupted {
		t.Fatalf("before the pick, b's tap = %+v; want interrupted, as every agent is", r)
	}
	w := serveOn(s, http.MethodPost, "/api/flags/end?id=1", `{"note":"you, a","to":{"agent":"claude","session":"a"}}`, nil)
	if f := decodeFlag(t, w.Body.Bytes()); f.To == nil || f.To.Session != "a" {
		t.Fatalf("ended flag.to = %+v, want session a", f.To)
	}
	if r := s.mayAct(req, "dev1", "tap", b); r != nil && r.Code == ErrCodeInterrupted {
		t.Errorf("after picking a, b's tap = %+v; want b let go", r)
	}
	if r := s.mayAct(req, "dev1", "tap", a); r == nil || r.Code != ErrCodeInterrupted {
		t.Errorf("a's tap = %+v, want interrupted until it reads", r)
	}
}

func TestFlagListenersOfferSessionsThatDriveWithoutWaiting(t *testing.T) {
	s, _ := newFlagServer(t)
	s.claims = newClaimBook(func(map[string]any) {})
	serveOn(s, http.MethodPost, "/api/claims?target=dev1", "", map[string]string{HeaderAgent: "claude", HeaderSession: "a"})
	serveOn(s, http.MethodGet, "/api/flags/shot?id=0", "", nil) // not an agent call: no headers
	s.flags.saw("claude", "b", "dev1")

	var got FlagListenersResponse
	json.Unmarshal(serveOn(s, http.MethodGet, "/api/flags/listeners?target=dev1", "", nil).Body.Bytes(), &got)
	if len(got.Listeners) != 0 {
		t.Errorf("listeners = %+v, want none: nobody waits on flags", got.Listeners)
	}
	byKey := map[string]FlagListener{}
	for _, a := range got.Active {
		byKey[a.Session] = a
	}
	if a, ok := byKey["a"]; !ok || !a.Holds || a.Target != "dev1" {
		t.Errorf("active a = %+v, want the holder, seen on dev1 when it claimed", a)
	}
	if b, ok := byKey["b"]; !ok || b.Holds {
		t.Errorf("active b = %+v, want listed, not holding", b)
	}
}
