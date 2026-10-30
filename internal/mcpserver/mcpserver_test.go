package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bmcreations/loupe/internal/httpapi"
)

// fakeLoupe records each request and answers from handle.
type fakeLoupe struct {
	mu     sync.Mutex
	seen   []string // "METHOD /path?query"
	heads  []http.Header
	handle func(w http.ResponseWriter, r *http.Request) bool
}

func (f *fakeLoupe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.RequestURI())
	f.heads = append(f.heads, r.Header.Clone())
	f.mu.Unlock()
	if f.handle != nil && f.handle(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"ok":true}`)
}

func (f *fakeLoupe) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// connect runs the MCP server over in-memory transports against fake and
// returns the client session, and a func that ends the server and waits for
// Run to return.
func connect(t *testing.T, fake *fakeLoupe) (*mcp.ClientSession, func()) {
	t.Helper()
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	c, err := NewClient(ts.URL, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	// Tests compare the requests a tool sends; flagpush_test.go covers pushing.
	c.noPush = true
	ctx, cancel := context.WithCancel(context.Background())
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := func() {
		cs.Close()
		cancel()
		<-done
	}
	t.Cleanup(func() { cancel() })
	return cs, stop
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestEveryToolNamesARoute(t *testing.T) {
	have := map[string]bool{}
	for _, r := range httpapi.Routes() {
		have[r.Method+" "+r.Path] = true
	}
	for _, tr := range toolRoutes {
		if !have[tr.Method+" "+tr.Path] {
			t.Errorf("tool %s calls %s %s, which the server does not route", tr.Tool, tr.Method, tr.Path)
		}
	}
}

func TestEveryRegisteredToolIsInTheRouteTable(t *testing.T) {
	cs, stop := connect(t, &fakeLoupe{})
	defer stop()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got, want []string
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
	}
	for _, tr := range toolRoutes {
		want = append(want, tr.Tool)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("registered %v, toolRoutes lists %v", got, want)
	}
}

func TestTheFirstInputClaimsTheTargetThenPointsAndTaps(t *testing.T) {
	fake := &fakeLoupe{}
	cs, stop := connect(t, fake)
	call(t, cs, "tap", map[string]any{"target": "avd:Pixel_10", "x": 10, "y": 20})
	call(t, cs, "tap", map[string]any{"target": "avd:Pixel_10", "x": 30, "y": 40})
	stop()

	want := []string{
		"POST /api/claims?target=avd%3APixel_10",
		"POST /api/input?target=avd%3APixel_10",
		"POST /api/input?target=avd%3APixel_10",
		"POST /api/input?target=avd%3APixel_10",
		"POST /api/input?target=avd%3APixel_10",
		"DELETE /api/claims?target=avd%3APixel_10",
	}
	if got := fake.requests(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i, h := range fake.heads {
		if h.Get("Authorization") != "Bearer tok" || h.Get(httpapi.HeaderAgent) != "claude" || h.Get(httpapi.HeaderSession) == "" {
			t.Errorf("request %d headers: %v", i, h)
		}
		// A viewer id would make the request the person's and bypass the hold.
		if h.Get("X-Loupe-Viewer") != "" {
			t.Errorf("request %d sent X-Loupe-Viewer", i)
		}
	}
}

func TestTheTapCarriesItsActionAndPoint(t *testing.T) {
	var body httpapi.InputRequest
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/input" {
			json.NewDecoder(r.Body).Decode(&body)
		}
		return false
	}}
	cs, stop := connect(t, fake)
	defer stop()
	call(t, cs, "tap", map[string]any{"target": "emulator-5554", "x": 540, "y": 1200})
	if body.Action != "tap" || body.X != 540 || body.Y != 1200 {
		t.Errorf("input body %+v", body)
	}
}

// inputBodies runs one tool call and returns every /api/input body it sent,
// in order.
func inputBodies(t *testing.T, tool string, args map[string]any) []httpapi.InputRequest {
	t.Helper()
	var mu sync.Mutex
	var bodies []httpapi.InputRequest
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/input" {
			var b httpapi.InputRequest
			json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			bodies = append(bodies, b)
			mu.Unlock()
		}
		return false
	}}
	cs, stop := connect(t, fake)
	if res := call(t, cs, tool, args); res.IsError {
		t.Fatalf("%s: %s", tool, text(res))
	}
	stop()
	mu.Lock()
	defer mu.Unlock()
	return bodies
}

func TestATapPointsAtItsSpotBeforeTouching(t *testing.T) {
	got := inputBodies(t, "tap", map[string]any{"target": "e", "x": 540, "y": 1200})
	if len(got) != 2 || got[0].Action != "move" || got[0].X != 540 || got[0].Y != 1200 || got[1].Action != "tap" {
		t.Errorf("bodies %+v, want a move to 540,1200 then the tap", got)
	}
}

// The tool has no point to send a move to; the server points once it has
// found the node.
func TestATapByTextSendsTheMatchAndDoesNotPoint(t *testing.T) {
	got := inputBodies(t, "tap", map[string]any{"target": "e", "text": "Sign in", "index": 0, "waitMs": 2000})
	if len(got) != 1 || got[0].Action != "tap" || got[0].Match == nil {
		t.Fatalf("bodies %+v, want one tap carrying a match", got)
	}
	m := got[0].Match
	if m.Text != "Sign in" || m.Index == nil || *m.Index != 0 || m.WaitMs != 2000 {
		t.Errorf("match %+v, want text, index 0 and waitMs carried over", m)
	}
}

func TestASwipePointsAtItsStartBeforeTouching(t *testing.T) {
	got := inputBodies(t, "swipe", map[string]any{"target": "e", "x": 100, "y": 900, "x2": 100, "y2": 200})
	if len(got) != 2 || got[0].Action != "move" || got[0].X != 100 || got[0].Y != 900 || got[1].Action != "swipe" {
		t.Errorf("bodies %+v, want a move to the start then the swipe", got)
	}
}

func TestPointOnlyMoves(t *testing.T) {
	got := inputBodies(t, "point", map[string]any{"target": "e", "x": 7, "y": 9})
	if len(got) != 1 || got[0].Action != "move" || got[0].X != 7 || got[0].Y != 9 {
		t.Errorf("bodies %+v, want one move", got)
	}
}

func TestTypingDoesNotPoint(t *testing.T) {
	got := inputBodies(t, "type_text", map[string]any{"target": "e", "text": "hi"})
	if len(got) != 1 || got[0].Action != "text" {
		t.Errorf("bodies %+v, want only the text", got)
	}
}

func TestAFailedMoveDoesNotStopTheTap(t *testing.T) {
	var mu sync.Mutex
	var actions []string
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/input" {
			return false
		}
		var b httpapi.InputRequest
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		actions = append(actions, b.Action)
		mu.Unlock()
		if b.Action != "move" {
			return false
		}
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"unknown action \"move\""}`)
		return true
	}}
	cs, stop := connect(t, fake)
	defer stop()
	if res := call(t, cs, "tap", map[string]any{"target": "e", "x": 1, "y": 1}); res.IsError {
		t.Fatalf("tap failed on its move: %s", text(res))
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(actions, ",") != "move,tap" {
		t.Errorf("actions %v", actions)
	}
}

func TestARefusalIsAToolErrorWithItsCodeAndRetry(t *testing.T) {
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/input" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":"a person is using this device","code":"humanHeld","retryAfterMs":4200}`)
		return true
	}}
	cs, stop := connect(t, fake)
	defer stop()
	res := call(t, cs, "tap", map[string]any{"target": "emulator-5554", "x": 1, "y": 1})
	if !res.IsError {
		t.Fatalf("not an error: %s", text(res))
	}
	for _, s := range []string{"409", "humanHeld", "4200"} {
		if !strings.Contains(text(res), s) {
			t.Errorf("%q lacks %s", text(res), s)
		}
	}
}

func TestAClaimByAnotherAgentIsRetriedOnTheNextCall(t *testing.T) {
	var mu sync.Mutex
	refuse := true
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/api/claims" || r.Method != http.MethodPost || !refuse {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":"claimed","code":"claimedBy","claim":{"agent":"other"}}`)
		return true
	}}
	cs, stop := connect(t, fake)
	defer stop()
	if res := call(t, cs, "tap", map[string]any{"target": "e", "x": 1, "y": 1}); !res.IsError || !strings.Contains(text(res), "claimedBy") {
		t.Fatalf("first tap: %s", text(res))
	}
	mu.Lock()
	refuse = false
	mu.Unlock()
	if res := call(t, cs, "tap", map[string]any{"target": "e", "x": 1, "y": 1}); res.IsError {
		t.Fatalf("second tap: %s", text(res))
	}
	var claims int
	for _, r := range fake.requests() {
		if strings.HasPrefix(r, "POST /api/claims") {
			claims++
		}
	}
	if claims != 2 {
		t.Errorf("claimed %d times, want 2: %v", claims, fake.requests())
	}
}

func TestLogcatPassesItsFormatThrough(t *testing.T) {
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/logcat" {
			return false
		}
		io.WriteString(w, `{"eventMessage":"hi"}`+"\n")
		return true
	}}
	cs, stop := connect(t, fake)
	defer stop()
	res := call(t, cs, "logcat", map[string]any{"target": "sim:abc", "format": "ndjson", "package": "com.apple.Preferences"})
	if res.IsError || text(res) != `{"eventMessage":"hi"}`+"\n" {
		t.Fatalf("result %+v", res)
	}
	got := fake.requests()
	if len(got) != 1 || !strings.Contains(got[0], "format=ndjson") || !strings.Contains(got[0], "package=com.apple.Preferences") {
		t.Errorf("requests %v", got)
	}
}

func TestAScreenshotComesBackAsAnImage(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/screenshot" {
			return false
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
		return true
	}}
	cs, stop := connect(t, fake)
	defer stop()
	res := call(t, cs, "screenshot", map[string]any{"target": "emulator-5554"})
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("result %+v", res)
	}
	img, ok := res.Content[0].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/png" || !bytes.Equal(img.Data, png) {
		t.Errorf("content %#v", res.Content[0])
	}
	// A read never claims.
	for _, r := range fake.requests() {
		if strings.Contains(r, "/api/claims") {
			t.Errorf("screenshot sent %s", r)
		}
	}
}

func TestReleaseIsNeverForced(t *testing.T) {
	fake := &fakeLoupe{}
	cs, stop := connect(t, fake)
	call(t, cs, "tap", map[string]any{"target": "e", "x": 1, "y": 1})
	call(t, cs, "release", map[string]any{"target": "e"})
	stop()
	var dels int
	for _, r := range fake.requests() {
		if strings.Contains(r, "force") {
			t.Errorf("forced: %s", r)
		}
		if strings.HasPrefix(r, "DELETE /api/claims") {
			dels++
		}
	}
	// Released by the tool, so exit has nothing left to release.
	if dels != 1 {
		t.Errorf("%d releases: %v", dels, fake.requests())
	}
}
