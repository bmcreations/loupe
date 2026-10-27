package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeLoupe is a device with named screens. An input moves it to the screen
// its rule names; screenshots and trees describe whichever screen it is on.
type fakeLoupe struct {
	screen string
	trees  map[string]string
	shades map[string]uint8
	// moves maps "screen|action x,y" (or "screen|button back") to the next screen.
	moves  map[string]string
	inputs []map[string]any
	refuse error
}

func (f *fakeLoupe) screenshot(string, http.Header) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, 400, 800))
	for i := range img.Pix {
		img.Pix[i] = f.shades[f.screen]
	}
	img.Set(0, 0, color.Gray{1}) // not a flat image, so the thumbnail really scales
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes(), nil
}

func (f *fakeLoupe) ui(string, bool, http.Header) ([]byte, error) {
	return []byte(f.trees[f.screen]), nil
}

func (f *fakeLoupe) input(_ string, body map[string]any, _ http.Header) error {
	if f.refuse != nil {
		return f.refuse
	}
	f.inputs = append(f.inputs, body)
	key := f.screen + "|" + body["action"].(string)
	switch body["action"] {
	case "tap":
		key += fmt.Sprintf(" %v,%v", body["x"], body["y"])
	case "button":
		key += " " + body["button"].(string)
	}
	if next, ok := f.moves[key]; ok {
		f.screen = next
	}
	return nil
}

func node(class, id, text string, clickable bool, bounds [4]int) string {
	return fmt.Sprintf(`{"class":%q,"id":%q,"text":%q,"clickable":%v,"bounds":[%d,%d,%d,%d],"center":[%d,%d]}`,
		class, id, text, clickable, bounds[0], bounds[1], bounds[2], bounds[3], (bounds[0]+bounds[2])/2, (bounds[1]+bounds[3])/2)
}

func tree(children ...string) string {
	return `{"root":{"class":"FrameLayout","children":[` + strings.Join(children, ",") + `]}}`
}

// chats is a chat list; its rows change text and count from visit to visit.
func chats(rows ...string) string {
	kids := []string{node("TextView", "title", "Chats", false, [4]int{0, 0, 400, 50}), node("Button", "send", "Send", true, [4]int{300, 700, 400, 800})}
	for i, r := range rows {
		kids = append(kids, node("LinearLayout", "chat_row", r, true, [4]int{0, 60 + 60*i, 400, 110 + 60*i}))
	}
	return tree(kids...)
}

var sendScreen = tree(
	node("TextView", "title", "Send money", false, [4]int{0, 0, 400, 50}),
	node("EditText", "amount", "", true, [4]int{0, 100, 400, 150}),
)

func newRig(t *testing.T) (*Server, *fakeLoupe, *httptest.Server) {
	t.Helper()
	f := &fakeLoupe{
		screen: "chats",
		trees:  map[string]string{"chats": chats("Jeff", "Ana"), "send": sendScreen, "settings": tree(node("Switch", "wifi", "Wi-Fi", true, [4]int{0, 0, 400, 50}))},
		shades: map[string]uint8{"chats": 200, "send": 100, "settings": 50},
		moves:  map[string]string{"chats|tap 350,750": "send", "send|button back": "chats"},
	}
	s, err := NewServer(Config{DataDir: t.TempDir(), SettleTimeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	s.loupe = f
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, f, srv
}

func call(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestFingerprintIgnoresWhatTheScreenSays(t *testing.T) {
	a, _ := parseTree([]byte(chats("Jeff", "Ana")))
	b, _ := parseTree([]byte(chats("Ana", "Bo", "Cy")))
	c, _ := parseTree([]byte(sendScreen))
	fa, fb, fc := fingerprint(a), fingerprint(b), fingerprint(c)
	if fa.Key != fb.Key {
		t.Errorf("the chat list with other rows got another key: %s vs %s", fa.Key, fb.Key)
	}
	if fa.Key == fc.Key {
		t.Errorf("the chat list and the send screen share key %s", fa.Key)
	}
	if fa.Basis != "ids" {
		t.Errorf("basis = %q, want ids", fa.Basis)
	}
}

func TestFingerprintSplitsSameComponentsByScreenName(t *testing.T) {
	row := `{"class":"View","clickable":true}`
	a, _ := parseTree([]byte(`{"root":{"class":"View","composable":"InboxScreen","children":[` + row + `]}}`))
	b, _ := parseTree([]byte(`{"root":{"class":"View","composable":"ArchiveScreen","children":[` + row + `]}}`))
	fa, fb := fingerprint(a), fingerprint(b)
	if fa.Key == fb.Key || fa.Screen != "InboxScreen" {
		t.Fatalf("got %+v and %+v; want two keys, the first named InboxScreen", fa, fb)
	}
	if fa.Basis != "classes" {
		t.Errorf("basis = %q, want classes with no ids or labels", fa.Basis)
	}
}

func TestFingerprintFallsBackToLabelsWithoutIDs(t *testing.T) {
	a, _ := parseTree([]byte(`{"nodes":[{"class":"Button","desc":"Send","clickable":true}]}`))
	b, _ := parseTree([]byte(`{"nodes":[{"class":"Button","desc":"Cancel","clickable":true}]}`))
	fa, fb := fingerprint(a), fingerprint(b)
	if fa.Basis != "labels" || fa.Key == fb.Key {
		t.Fatalf("got %+v and %+v; want label keys that differ", fa, fb)
	}
}

// An iOS tree puts a button's label in text and has no desc, so a dialog of
// Leave Chat and Cancel and a sheet of request rows were one screen on classes.
func TestFingerprintReadsAnIOSLabelFromText(t *testing.T) {
	a, _ := parseTree([]byte(`{"nodes":[{"class":"Button","text":"Leave Chat","clickable":true},{"class":"Button","text":"Cancel","clickable":true}]}`))
	b, _ := parseTree([]byte(`{"nodes":[{"class":"Button","text":"Brandon M, Hasn't paid yet, $2","clickable":true}]}`))
	fa, fb := fingerprint(a), fingerprint(b)
	if fa.Basis != "labels" || fa.Key == fb.Key {
		t.Fatalf("got %+v and %+v; want label keys that differ", fa, fb)
	}
}

// The root of an iOS tree is the Application node, which says the app's name.
func TestSuggestNamePrefersTheHeadingOverTheApp(t *testing.T) {
	roots, _ := parseTree([]byte(`{"nodes":[{"class":"Application","text":"Acmechat","children":[{"class":"Button","text":"Back"},{"class":"Heading","text":"Members"}]}]}`))
	if got := suggestName(roots, fingerprint(roots)); got != "Members" {
		t.Errorf("name = %q, want Members", got)
	}
	roots, _ = parseTree([]byte(`{"nodes":[{"class":"Application","text":"Acmechat","children":[{"class":"StaticText","text":"Leave XCTest Bed?"}]}]}`))
	if got := suggestName(roots, fingerprint(roots)); got != "Leave XCTest Bed?" {
		t.Errorf("name = %q, want the first text under the app", got)
	}
}

func TestAStepPassesALongPressLength(t *testing.T) {
	x, ms := 300, 800
	b, err := (&Step{Action: "swipe", X: &x, Y: &x, X2: &x, Y2: &x, Ms: &ms}).input(nil)
	if err != nil || b["ms"] != 800 {
		t.Fatalf("input = %v, %v; want ms 800", b, err)
	}
}

func TestAStepNamesAScreenTheTreeCannotTellApart(t *testing.T) {
	_, _, srv := newRig(t)
	_, created := call(t, srv, "POST", "/maps?target=emu", nil)
	id := created["id"].(string)
	x := 10
	// A menu with no ids leaves the tree's key as it was.
	press := map[string]any{"action": "swipe", "x": x, "y": x, "x2": x, "y2": x, "ms": 1, "screen": "Message menu"}
	code, st := call(t, srv, "POST", "/maps/"+id+"/steps", press)
	sc, _ := st["screen"].(map[string]any)
	if code != 200 || st["new"] != true || sc["name"] != "Message menu" || st["from"] != "s1" {
		t.Fatalf("press = %d %v; want a new screen named Message menu", code, st)
	}
	menu := sc["id"]
	_, st = call(t, srv, "POST", "/maps/"+id+"/steps", map[string]any{"action": "tap", "x": x, "y": x})
	if st["from"] != menu || st["screen"].(map[string]any)["id"] != "s1" {
		t.Fatalf("plain tap = %v; want from the menu to s1", st)
	}
	_, st = call(t, srv, "POST", "/maps/"+id+"/steps", press)
	if st["new"] != false || st["screen"].(map[string]any)["id"] != menu {
		t.Fatalf("second press = %v; want %v again", st, menu)
	}
}

func TestDeletingAnEdgeKeepsItsScreens(t *testing.T) {
	_, _, srv := newRig(t)
	_, created := call(t, srv, "POST", "/maps?target=emu", nil)
	id := created["id"].(string)
	x := 10
	call(t, srv, "POST", "/maps/"+id+"/steps", map[string]any{"action": "tap", "x": x, "y": x, "screen": "Sheet"})
	code, m := call(t, srv, "DELETE", "/maps/"+id+"/edges?from=s1&to=s2", nil)
	if code != 200 || len(m["edges"].([]any)) != 0 || len(m["screens"].([]any)) != 2 {
		t.Fatalf("delete = %d %v; want no edges, two screens", code, m)
	}
	if code, _ = call(t, srv, "DELETE", "/maps/"+id+"/edges?from=s1&to=s2", nil); code != 404 {
		t.Errorf("second delete = %d, want 404", code)
	}
}

// A step named after a screen the map already has lands on it, even when the
// tree there differs from the one recorded, so naming the way back from a
// sheet does not copy the screen under it.
func TestAStepNamedAfterAKnownScreenLandsOnIt(t *testing.T) {
	_, _, srv := newRig(t)
	_, created := call(t, srv, "POST", "/maps?target=emu", nil)
	id := created["id"].(string)
	call(t, srv, "POST", "/maps/"+id+"/screens/s1", map[string]any{"name": "Home"})
	x := 10
	call(t, srv, "POST", "/maps/"+id+"/steps", map[string]any{"action": "tap", "x": x, "y": x, "screen": "Sheet"})
	_, st := call(t, srv, "POST", "/maps/"+id+"/steps", map[string]any{"action": "tap", "x": x, "y": x, "screen": "Home"})
	if st["new"] != false || st["screen"].(map[string]any)["id"] != "s1" {
		t.Fatalf("step named Home = %v; want s1, not a new screen", st)
	}
}

func TestAWalkRecordsScreensOnceAndCountsEdges(t *testing.T) {
	_, f, srv := newRig(t)
	code, created := call(t, srv, "POST", "/maps?target=emu&name=wallet", nil)
	if code != 200 {
		t.Fatalf("create = %d %v", code, created)
	}
	id := created["id"].(string)

	tapSend := map[string]any{"action": "tap", "node": map[string]string{"id": "send"}, "note": "open send"}
	code, st := call(t, srv, "POST", "/maps/"+id+"/steps", tapSend)
	if code != 200 || st["new"] != true || st["from"] != "s1" {
		t.Fatalf("tap send = %d %v; want a new screen from s1", code, st)
	}
	if got := f.inputs[0]; got["x"] != float64(350) && got["x"] != 350 {
		t.Errorf("tap went to %v; want the send button's centre", got)
	}
	sendID := st["screen"].(map[string]any)["id"]

	// The rows change between visits; it is still the same screen.
	f.trees["chats"] = chats("Ana", "Bo", "Cy")
	code, st = call(t, srv, "POST", "/maps/"+id+"/steps", map[string]any{"action": "button", "button": "back"})
	if code != 200 || st["new"] != false || st["screen"].(map[string]any)["id"] != "s1" {
		t.Fatalf("back = %d %v; want s1 again", code, st)
	}
	call(t, srv, "POST", "/maps/"+id+"/steps", tapSend)

	_, m := call(t, srv, "GET", "/maps/"+id, nil)
	if n := len(m["screens"].([]any)); n != 2 {
		t.Fatalf("map has %d screens, want 2: %v", n, m)
	}
	edges := m["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("map has %d edges, want 2: %v", len(edges), edges)
	}
	first := edges[0].(map[string]any)
	if first["from"] != "s1" || first["to"] != sendID || first["count"] != float64(2) || first["note"] != "open send" {
		t.Errorf("first edge = %v; want s1 -> %v twice, noted", first, sendID)
	}
	if m["current"] != sendID {
		t.Errorf("current = %v, want %v", m["current"], sendID)
	}
	if s1 := m["screens"].([]any)[0].(map[string]any); s1["name"] != "Chats" {
		t.Errorf("s1 is named %q, want its title", s1["name"])
	}
}

func TestAStepFromOffTheMapRecordsWhereItStarted(t *testing.T) {
	_, f, srv := newRig(t)
	_, created := call(t, srv, "POST", "/maps?target=emu", nil)
	id := created["id"].(string)
	f.screen = "settings" // a person moved the device
	f.moves["settings|button back"] = "chats"
	code, st := call(t, srv, "POST", "/maps/"+id+"/steps", map[string]any{"action": "button", "button": "back"})
	if code != 200 || st["from"] != "s2" || st["screen"].(map[string]any)["id"] != "s1" {
		t.Fatalf("step = %d %v; want from the new s2 back to s1", code, st)
	}
}

func TestATapOnANodeThatIsNotThereSendsNothing(t *testing.T) {
	_, f, srv := newRig(t)
	_, created := call(t, srv, "POST", "/maps?target=emu", nil)
	code, out := call(t, srv, "POST", "/maps/"+created["id"].(string)+"/steps", map[string]any{"action": "tap", "node": map[string]string{"id": "nope"}})
	if code != 422 || out["code"] != "noSuchNode" || len(f.inputs) != 0 {
		t.Fatalf("= %d %v, %d inputs; want 422 noSuchNode and no input", code, out, len(f.inputs))
	}
}

func TestLoupesRefusalReachesTheAgent(t *testing.T) {
	_, f, srv := newRig(t)
	_, created := call(t, srv, "POST", "/maps?target=emu", nil)
	f.refuse = &statusError{"/api/input", 409, `{"code":"humanHeld","retryAfterMs":4000}`}
	code, out := call(t, srv, "POST", "/maps/"+created["id"].(string)+"/steps", map[string]any{"action": "button", "button": "back"})
	if code != 409 || out["code"] != "humanHeld" {
		t.Fatalf("= %d %v; want loupe's 409 humanHeld", code, out)
	}
}

func TestMergeFoldsOneScreenIntoAnother(t *testing.T) {
	_, _, srv := newRig(t)
	_, created := call(t, srv, "POST", "/maps?target=emu", nil)
	id := created["id"].(string)
	call(t, srv, "POST", "/maps/"+id+"/steps", map[string]any{"action": "tap", "x": 350, "y": 750})
	call(t, srv, "POST", "/maps/"+id+"/steps", map[string]any{"action": "button", "button": "back"})
	code, m := call(t, srv, "POST", "/maps/"+id+"/screens/s2", map[string]any{"mergeInto": "s1", "name": "Home"})
	if code != 200 {
		t.Fatalf("merge = %d %v", code, m)
	}
	if n := len(m["screens"].([]any)); n != 1 {
		t.Errorf("%d screens after merge, want 1", n)
	}
	if n := len(m["edges"].([]any)); n != 0 {
		t.Errorf("%d edges after merge, want 0: both ran between the merged screens", n)
	}
	if s1 := m["screens"].([]any)[0].(map[string]any); s1["name"] != "Chats" || s1["visits"] != float64(3) {
		t.Errorf("s1 = %v; want its own name kept and 3 visits", s1)
	}
}

func TestMapsSurviveARestart(t *testing.T) {
	s, _, srv := newRig(t)
	_, created := call(t, srv, "POST", "/maps?target=emu", nil)
	id := created["id"].(string)
	call(t, srv, "POST", "/maps/"+id+"/steps", map[string]any{"action": "tap", "x": 350, "y": 750})

	again, err := NewServer(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := again.maps[id]
	if m == nil || len(m.Screens) != 2 || len(m.Edges) != 1 {
		t.Fatalf("reloaded map = %+v; want 2 screens and 1 edge", m)
	}
	rec := httptest.NewRecorder()
	again.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/maps/"+id+"/screens/s2/thumb.png", nil))
	cfg, err := png.DecodeConfig(rec.Body)
	if rec.Code != 200 || err != nil || cfg.Width != 240 {
		t.Fatalf("thumb = %d %v width %d; want a 240px PNG", rec.Code, err, cfg.Width)
	}
}

func TestRegisterTellsLoupeWhereItListens(t *testing.T) {
	var got map[string]string
	var auth string
	l := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/extensions" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer l.Close()
	if err := newLoupeClient(l.URL, "tok").register("127.0.0.1:18494"); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "appmap" || got["url"] != "http://127.0.0.1:18494" || auth != "Bearer tok" {
		t.Fatalf("got %v, auth %q", got, auth)
	}
}
