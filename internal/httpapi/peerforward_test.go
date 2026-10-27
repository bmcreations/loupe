package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/peer"
	"github.com/bmcreations/loupe/internal/target"
)

// Owner-side capabilities for recTarget, so a hub's forwarded calls have
// something to land on.
var (
	fwdMu    sync.Mutex
	fwdTaps  = map[*recTarget][][2]int{}
	fwdTexts = map[*recTarget][]string{}
)

func (r *recTarget) taps() [][2]int {
	fwdMu.Lock()
	defer fwdMu.Unlock()
	return append([][2]int(nil), fwdTaps[r]...)
}

func (r *recTarget) Tap(_ context.Context, x, y int) error {
	fwdMu.Lock()
	fwdTaps[r] = append(fwdTaps[r], [2]int{x, y})
	fwdMu.Unlock()
	return nil
}
func (r *recTarget) Swipe(context.Context, int, int, int, int, int) error { return nil }
func (r *recTarget) TypeText(_ context.Context, s string) error {
	fwdMu.Lock()
	fwdTexts[r] = append(fwdTexts[r], s)
	fwdMu.Unlock()
	return nil
}
func (r *recTarget) PressKey(context.Context, int) error { return nil }
func (r *recTarget) Inspect(context.Context) (*target.UI, error) {
	return &target.UI{Width: 360, Height: 780, Rotation: 1, Root: &target.Node{
		Class: "OwnerRoot", Bounds: [4]int{0, 0, 360, 780},
		Children: []*target.Node{{Class: "Button", Text: "Go", Clickable: true}},
	}}, nil
}
func (r *recTarget) Screenshot(context.Context) ([]byte, error) {
	return append([]byte("\x89PNG\r\n\x1a\n"), "owner pixels"...), nil
}
func (r *recTarget) DeviceFrame(context.Context) (*target.DeviceFrame, error) {
	return &target.DeviceFrame{Source: "derived", Width: 400, Height: 820, CornerRadius: 22}, nil
}
func (r *recTarget) FrameAsset(context.Context, string) ([]byte, string, error) {
	return nil, "", nil
}

// pair is an owner and a hub peered with it as "mini", with the hub's session
// on mini/rec open so the owner has a relay viewer.
type pair struct {
	o *quicOwner
	h *hub
}

func newPair(t *testing.T, openRelay bool) *pair {
	t.Helper()
	restore := peer.SetTimings(50*time.Millisecond, 10*time.Second, time.Minute)
	t.Cleanup(restore)
	o := startQUICOwner(t, serveReal)
	h := quicHub(t)
	if w := h.add(t, "mini", o.url, "tok"); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	eventually(t, "mini/rec listed", func() bool { return find(h.targets(t, ""), "mini/rec") != nil })
	if openRelay {
		if _, err := h.mgr.Acquire(context.Background(), "mini/rec"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.mgr.Release("mini/rec") })
		eventually(t, "owner session", func() bool { _, ok := o.mgr.Session("rec"); return ok })
	}
	return &pair{o: o, h: h}
}

func (p *pair) hub(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	p.h.h.ServeHTTP(w, req)
	return w
}

func (p *pair) owner(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	p.o.Handler().ServeHTTP(w, req)
	return w
}

func errBody(t *testing.T, w *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var e ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return e
}

// sameRefusal compares two error bodies, leaving out claim.idleMs: the
// owner computes it per request, so two requests a millisecond apart differ.
func sameRefusal(t *testing.T, a, b *httptest.ResponseRecorder) bool {
	t.Helper()
	decode := func(w *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode %q: %v", w.Body, err)
		}
		if c, ok := m["claim"].(map[string]any); ok {
			delete(c, "idleMs")
		}
		return m
	}
	return reflect.DeepEqual(decode(a), decode(b))
}

const tapBody = `{"action":"tap","x":12,"y":34}`

func TestPeerAgentTapReachesTheOwnerAsThatAgent(t *testing.T) {
	p := newPair(t, false)
	w := p.hub(http.MethodPost, "/api/input?target=mini/rec", tapBody, HeaderAgent, "claude", HeaderSession, "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("tap = %d %s", w.Code, w.Body)
	}
	if got := p.o.rec.taps(); len(got) != 1 || got[0] != [2]int{12, 34} {
		t.Fatalf("owner taps = %v", got)
	}
	// The owner saw the agent's name: its row shows who drove.
	var rows []TargetInfo
	_ = json.Unmarshal(p.owner(http.MethodGet, "/api/targets?local=1", "").Body.Bytes(), &rows)
	if r := find(rows, "rec"); r == nil || r.DrivenBy != "claude" {
		t.Fatalf("owner row = %+v", r)
	}
}

func TestPeerTextAndKeyAreForwarded(t *testing.T) {
	p := newPair(t, false)
	for _, body := range []string{`{"action":"text","text":"hi"}`, `{"action":"key","keycode":4}`, `{"action":"swipe","x":1,"y":2,"x2":3,"y2":4}`} {
		if w := p.hub(http.MethodPost, "/api/input?target=mini/rec", body, HeaderAgent, "claude"); w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", body, w.Code, w.Body)
		}
	}
	fwdMu.Lock()
	defer fwdMu.Unlock()
	if got := fwdTexts[p.o.rec]; len(got) != 1 || got[0] != "hi" {
		t.Fatalf("owner texts = %v", got)
	}
}

func TestPeerOwnerRefusalsComeBackUnchanged(t *testing.T) {
	p := newPair(t, true)

	// Another agent's claim on the owner.
	if w := p.owner(http.MethodPost, "/api/claims?target=rec", "", HeaderAgent, "other", HeaderSession, "o1"); w.Code != http.StatusOK {
		t.Fatalf("owner claim = %d %s", w.Code, w.Body)
	}
	direct := p.owner(http.MethodPost, "/api/input?target=rec", tapBody, HeaderAgent, "claude", HeaderSession, "s1")
	via := p.hub(http.MethodPost, "/api/input?target=mini/rec", tapBody, HeaderAgent, "claude", HeaderSession, "s1")
	if via.Code != http.StatusConflict || via.Code != direct.Code || !sameRefusal(t, via, direct) {
		t.Fatalf("claimedBy via hub = %d %s, direct = %d %s", via.Code, via.Body, direct.Code, direct.Body)
	}
	if e := errBody(t, via); e.Code != ErrCodeClaimed || e.Claim == nil || e.Claim.Agent != "other" {
		t.Fatalf("claimedBy body = %+v", e)
	}
	if got := p.o.rec.taps(); len(got) != 0 {
		t.Fatalf("a refused tap reached the device: %v", got)
	}
	if w := p.owner(http.MethodDelete, "/api/claims?target=rec", "", HeaderAgent, "other", HeaderSession, "o1"); w.Code != http.StatusNoContent {
		t.Fatalf("release = %d", w.Code)
	}

	// A person at the owner's own viewer.
	osess, _ := p.o.mgr.Session("rec")
	vid, out := osess.AddViewer()
	go func() {
		for range out {
		}
	}()
	osess.HumanInput(vid)
	via = p.hub(http.MethodPost, "/api/input?target=mini/rec", tapBody, HeaderAgent, "claude", HeaderSession, "s1")
	if via.Code != http.StatusConflict {
		t.Fatalf("humanHeld via hub = %d %s", via.Code, via.Body)
	}
	if e := errBody(t, via); e.Code != ErrCodeHumanHeld || e.RetryAfterMs <= 0 {
		t.Fatalf("humanHeld body = %+v", e)
	}
	if via.Header().Get("Retry-After") == "" {
		t.Error("the owner's Retry-After was dropped")
	}
	if got := p.o.rec.taps(); len(got) != 0 {
		t.Fatalf("a held tap reached the device: %v", got)
	}
}

func TestPeerHubViewerInputReachesTheOwnerAsAViewer(t *testing.T) {
	p := newPair(t, true)
	osess, _ := p.o.mgr.Session("rec")
	vid, out := osess.AddViewer()
	go func() {
		for range out {
		}
	}()
	osess.HumanInput(vid)
	// An agent is held out while that person has the target...
	if w := p.hub(http.MethodPost, "/api/input?target=mini/rec", tapBody, HeaderAgent, "claude"); w.Code != http.StatusConflict {
		t.Fatalf("agent = %d %s", w.Code, w.Body)
	}
	// ...and a phone on the hub is a person too, so it is not.
	hsess, ok := p.h.mgr.Session("mini/rec")
	if !ok {
		t.Fatal("no hub session")
	}
	hv, hout := hsess.AddViewer()
	go func() {
		for range hout {
		}
	}()
	// A claim by another agent on the owner does not stop a person either.
	p.owner(http.MethodPost, "/api/claims?target=rec", "", HeaderAgent, "other")
	w := p.hub(http.MethodPost, "/api/input?target=mini/rec", tapBody, HeaderViewer, strconv.FormatUint(hv, 10))
	if w.Code != http.StatusOK {
		t.Fatalf("viewer tap = %d %s", w.Code, w.Body)
	}
	if got := p.o.rec.taps(); len(got) != 1 {
		t.Fatalf("owner taps = %v", got)
	}
}

func TestPeerUIScreenshotAndFrameAreTheOwners(t *testing.T) {
	p := newPair(t, false)

	w := p.hub(http.MethodGet, "/api/ui?target=mini/rec", "")
	var ui UIResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &ui) != nil {
		t.Fatalf("ui = %d %s", w.Code, w.Body)
	}
	if ui.Target != "mini/rec" || ui.Width != 360 || ui.Height != 780 || ui.Rotation != 1 ||
		len(ui.Nodes) != 1 || ui.Nodes[0].Class != "OwnerRoot" || len(ui.Nodes[0].Children) != 1 {
		t.Fatalf("ui = %+v", ui)
	}
	w = p.hub(http.MethodGet, "/api/ui?target=mini/rec&interactive=1", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"Button"`) {
		t.Fatalf("interactive ui = %d %s", w.Code, w.Body)
	}

	w = p.hub(http.MethodGet, "/api/screenshot?target=mini/rec", "")
	want, _ := p.o.rec.Screenshot(context.Background())
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("screenshot = %d, %d bytes, %s", w.Code, w.Body.Len(), w.Header().Get("Content-Type"))
	}

	w = p.hub(http.MethodGet, "/api/frame?target=mini/rec", "")
	var f target.DeviceFrame
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &f) != nil || f.Width != 400 || f.CornerRadius != 22 {
		t.Fatalf("frame = %d %s", w.Code, w.Body)
	}
}

func TestPeerClaimsLandOnTheOwner(t *testing.T) {
	p := newPair(t, false)
	w := p.hub(http.MethodPost, "/api/claims?target=mini/rec", "", HeaderAgent, "claude", HeaderSession, "s1")
	var got ClaimInfo
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Target != "mini/rec" || got.Agent != "claude" {
		t.Fatalf("claim = %d %s", w.Code, w.Body)
	}
	// The owner holds it; the hub's own book does not.
	var rows []TargetInfo
	_ = json.Unmarshal(p.owner(http.MethodGet, "/api/targets?local=1", "").Body.Bytes(), &rows)
	if r := find(rows, "rec"); r == nil || r.Claim == nil || r.Claim.Agent != "claude" {
		t.Fatalf("owner row = %+v", r)
	}
	if n := len(p.h.claims.list()); n != 0 {
		t.Fatalf("hub book holds %d claims", n)
	}
	// The hub's row shows it after a poll.
	eventually(t, "hub row shows the claim", func() bool {
		r := find(p.h.targets(t, ""), "mini/rec")
		return r != nil && r.Claim != nil && r.Claim.Agent == "claude"
	})

	// Another agent is refused by the owner, and the id in the body is the hub's.
	w = p.hub(http.MethodPost, "/api/claims?target=mini/rec", "", HeaderAgent, "other", HeaderSession, "s2")
	if w.Code != http.StatusConflict {
		t.Fatalf("second claim = %d %s", w.Code, w.Body)
	}
	if e := errBody(t, w); e.Code != ErrCodeClaimed || e.Claim == nil || e.Claim.Target != "mini/rec" || e.Claim.Agent != "claude" {
		t.Fatalf("second claim body = %+v", e)
	}
	// The holder's own call on the hub renews it on the owner and is allowed.
	if w := p.hub(http.MethodPost, "/api/input?target=mini/rec", tapBody, HeaderAgent, "claude", HeaderSession, "s1"); w.Code != http.StatusOK {
		t.Fatalf("holder tap = %d %s", w.Code, w.Body)
	}
	if w := p.hub(http.MethodPost, "/api/input?target=mini/rec", tapBody, HeaderAgent, "other", HeaderSession, "s2"); w.Code != http.StatusConflict {
		t.Fatalf("other tap = %d %s", w.Code, w.Body)
	}
	if w := p.hub(http.MethodDelete, "/api/claims?target=mini/rec", "", HeaderAgent, "claude", HeaderSession, "s1"); w.Code != http.StatusNoContent {
		t.Fatalf("release = %d %s", w.Code, w.Body)
	}
	if r := find(func() []TargetInfo {
		var r []TargetInfo
		_ = json.Unmarshal(p.owner(http.MethodGet, "/api/targets?local=1", "").Body.Bytes(), &r)
		return r
	}(), "rec"); r.Claim != nil {
		t.Fatalf("owner still holds %+v", r.Claim)
	}
}

func TestPeerOwnerDownIs502ServerDown(t *testing.T) {
	p := newPair(t, false)
	p.o.ts.CloseClientConnections()
	p.o.ts.Close()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/input?target=mini/rec", tapBody},
		{http.MethodGet, "/api/ui?target=mini/rec", ""},
		{http.MethodGet, "/api/screenshot?target=mini/rec", ""},
		{http.MethodPost, "/api/claims?target=mini/rec", ""},
	} {
		w := p.hub(c.method, c.path, c.body, HeaderAgent, "claude")
		if w.Code != http.StatusBadGateway || errBody(t, w).Code != ErrCodeServerDown {
			t.Errorf("%s %s = %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
}
