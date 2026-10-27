package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var fakePNG = append([]byte("\x89PNG\r\n\x1a\n"), 1, 2, 3)

type fakeLoupe struct {
	mu      sync.Mutex
	headers []http.Header
	paths   []string
	shots   [][]byte      // served in order, the last one repeated
	next    func() []byte // when set, makes every screenshot
	noTree  bool          // /api/ui answers 501, as for a physical iPhone
}

func (f *fakeLoupe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.paths = append(f.paths, r.URL.String())
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "no", 401)
		return
	}
	switch r.URL.Path {
	case "/api/screenshot":
		f.mu.Lock()
		if f.next != nil {
			b := f.next()
			f.mu.Unlock()
			w.Write(b)
			return
		}
		shots := f.shots
		if len(shots) > 1 {
			f.shots = shots[1:]
		}
		f.mu.Unlock()
		if len(shots) > 0 {
			w.Write(shots[0])
			return
		}
		w.Write(fakePNG)
	case "/api/ui":
		if f.noTree {
			http.Error(w, `{"error":"target has no view hierarchy"}`, 501)
			return
		}
		w.Write([]byte(`{"nodes":[{"text":"hi"}]}`))
	default:
		http.NotFound(w, r)
	}
}

func writeBuilder(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "build.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

type rig struct {
	s     *Server
	ts    *httptest.Server
	loupe *fakeLoupe
}

func newRig(t *testing.T, build string) *rig {
	t.Helper()
	fl := &fakeLoupe{}
	lts := httptest.NewServer(fl)
	t.Cleanup(lts.Close)
	s, err := NewServer(Config{LoupeURL: lts.URL, Token: "tok", BuildCmd: build, BuildDir: t.TempDir(), DataDir: t.TempDir(),
		BuildTimeout: 5 * time.Second, ReportTimeout: 600 * time.Millisecond, PollWindow: time.Second, SettleTimeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &rig{s: s, ts: ts, loupe: fl}
}

// app plays the debug app: it polls /next and answers each patch with reply(seq).
func (g *rig) app(t *testing.T, reply func(seq int64) string) (stop func()) {
	done := make(chan struct{})
	cctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var after int64
		for {
			select {
			case <-done:
				return
			default:
			}
			req, _ := http.NewRequestWithContext(cctx, "GET", fmt.Sprintf("%s/next?after=%d", g.ts.URL, after), nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				continue
			}
			var n map[string]any
			json.Unmarshal(b, &n)
			after = int64(n["seq"].(float64))
			if r := reply(after); r != "" {
				http.Post(g.ts.URL+"/report", "application/json", strings.NewReader(r))
			}
		}
	}()
	return func() { close(done); cancel(); wg.Wait() }
}

func post(t *testing.T, g *rig, target string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", g.ts.URL+"/apply?target="+target, nil)
	req.Header.Set("X-Loupe-Agent", "claude")
	req.Header.Set("X-Loupe-Session", "s1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("body %q: %v", b, err)
	}
	return resp.StatusCode, out
}

const okBuild = `echo "compiling"; echo "{\"seq\":99,\"classes\":[{\"name\":\"A\",\"file\":\"$HOTSWAP_SEQ-0.dex\"}],\"compileMs\":5}"`

func waitPolling(t *testing.T, g *rig) {
	t.Helper()
	for i := 0; i < 100 && !g.s.polling(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !g.s.polling() {
		t.Fatal("app never polled")
	}
}

func TestApplyOK(t *testing.T) {
	g := newRig(t, writeBuilder(t, okBuild))
	stop := g.app(t, func(seq int64) string {
		return fmt.Sprintf(`{"event":"patch","seq":%d,"ok":true,"downloadMs":12,"redefineMs":3,"recomposeMs":40}`, seq)
	})
	defer stop()
	waitPolling(t, g)
	code, out := post(t, g, "avd:x")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	seq := int64(out["seq"].(float64))
	if seq != g.s.currentSeq() || seq == 99 {
		t.Fatalf("seq %d (builder's own seq must be replaced)", seq)
	}
	if tm := out["timings"].(map[string]any); tm["recomposeMs"].(float64) != 40 || tm["downloadMs"].(float64) != 12 {
		t.Fatalf("timings %v", tm)
	}
	if _, ok := out["ui"].(map[string]any); !ok {
		t.Fatalf("ui %v", out["ui"])
	}
	shot := out["screenshot"].(map[string]any)
	b, err := os.ReadFile(shot["path"].(string))
	if err != nil || !bytes.Equal(b, fakePNG) || !filepath.IsAbs(shot["path"].(string)) {
		t.Fatalf("screenshot file: %v", err)
	}
	if !strings.Contains(shot["url"].(string), "/shots/") {
		t.Fatalf("url %v", shot["url"])
	}
	r, _ := http.Get(shot["url"].(string))
	if r.StatusCode != 200 {
		t.Fatalf("served shot %d", r.StatusCode)
	}
	if _, ok := out["buildMs"]; !ok {
		t.Fatal("no buildMs")
	}
	if _, ok := out["changed"]; ok {
		t.Fatal("changed reported for screenshots that do not decode")
	}
	// headers and token reach loupe
	g.loupe.mu.Lock()
	defer g.loupe.mu.Unlock()
	// the screen before, the screen after, the tree
	if len(g.loupe.headers) != 3 {
		t.Fatalf("loupe calls %v", g.loupe.paths)
	}
	for i, h := range g.loupe.headers {
		if h.Get("X-Loupe-Agent") != "claude" || h.Get("X-Loupe-Session") != "s1" {
			t.Fatalf("call %d headers %v", i, h)
		}
	}
	if !strings.Contains(strings.Join(g.loupe.paths, " "), "interactive=1") {
		t.Fatalf("paths %v", g.loupe.paths)
	}
}

func TestTargetWithoutATreeIsStillOK(t *testing.T) {
	g := newRig(t, writeBuilder(t, okBuild))
	g.loupe.noTree = true
	stop := g.app(t, func(seq int64) string {
		return fmt.Sprintf(`{"event":"patch","seq":%d,"ok":true}`, seq)
	})
	defer stop()
	waitPolling(t, g)
	code, out := post(t, g, "ios:x")
	if code != 200 || out["ok"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if _, ok := out["ui"]; ok {
		t.Fatalf("ui %v", out["ui"])
	}
	if _, ok := out["screenshot"]; !ok {
		t.Fatal("no screenshot")
	}
}

func TestAppNotPollingSkipsBuild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	g := newRig(t, writeBuilder(t, "touch "+marker+"; "+okBuild))
	code, out := post(t, g, "avd:x")
	if code != 409 || out["code"] != "appNotPolling" {
		t.Fatalf("%d %v", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("build ran")
	}
}

func TestBuildFailed(t *testing.T) {
	g := newRig(t, writeBuilder(t, `echo "e: Foo.kt:3 unresolved" >&2; exit 1`))
	stop := g.app(t, func(int64) string { t.Error("published"); return "" })
	defer stop()
	waitPolling(t, g)
	code, out := post(t, g, "avd:x")
	if code != 422 || out["code"] != "buildFailed" || !strings.Contains(out["output"].(string), "unresolved") {
		t.Fatalf("%d %v", code, out)
	}
	if g.s.latest["seq"].(float64) != 0 {
		t.Fatal("something was published")
	}
}

func TestBuildWithoutNotice(t *testing.T) {
	g := newRig(t, writeBuilder(t, `echo nope`))
	stop := g.app(t, func(int64) string { return "" })
	defer stop()
	waitPolling(t, g)
	if code, out := post(t, g, "avd:x"); code != 422 {
		t.Fatalf("%d %v", code, out)
	}
}

func TestNeedsRebuild(t *testing.T) {
	g := newRig(t, writeBuilder(t, okBuild))
	stop := g.app(t, func(seq int64) string {
		return fmt.Sprintf(`{"event":"patch","seq":%d,"ok":false,"error":"UNSUPPORTED_REDEFINITION_SCHEMA_CHANGED"}`, seq)
	})
	defer stop()
	waitPolling(t, g)
	code, out := post(t, g, "avd:x")
	if code != 409 || out["code"] != "needsRebuild" || !strings.Contains(out["error"].(string), "SCHEMA") {
		t.Fatalf("%d %v", code, out)
	}
	g.loupe.mu.Lock()
	defer g.loupe.mu.Unlock()
	if strings.Contains(strings.Join(g.loupe.paths, " "), "/api/ui") {
		t.Fatalf("read back after a failed patch: %v", g.loupe.paths)
	}
}

// The screen is taken before the build as well, so an apply that loaded and redrew with the old
// body still showing says so.
func TestApplyReportsWhetherTheScreenChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before color.Gray
		want   bool
	}{{"changed", color.Gray{0}, true}, {"unchanged", color.Gray{200}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, writeBuilder(t, okBuild))
			drawn := solidPNG(t, color.Gray{200})
			g.loupe.shots = [][]byte{solidPNG(t, tc.before), drawn, drawn}
			stop := g.app(t, func(seq int64) string {
				return fmt.Sprintf(`{"event":"patch","seq":%d,"ok":true}`, seq)
			})
			defer stop()
			waitPolling(t, g)
			code, out := post(t, g, "avd:x")
			if code != 200 || out["changed"] != tc.want {
				t.Fatalf("%d changed=%v; want %v", code, out["changed"], tc.want)
			}
		})
	}
}

func TestNoReport(t *testing.T) {
	g := newRig(t, writeBuilder(t, okBuild))
	stop := g.app(t, func(int64) string { return "" })
	defer stop()
	waitPolling(t, g)
	code, out := post(t, g, "avd:x")
	if code != 504 || out["code"] != "noReport" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestReportForAnotherSeqIsIgnored(t *testing.T) {
	g := newRig(t, writeBuilder(t, okBuild))
	stop := g.app(t, func(seq int64) string { return fmt.Sprintf(`{"event":"patch","seq":%d,"ok":true}`, seq-1) })
	defer stop()
	waitPolling(t, g)
	if code, _ := post(t, g, "avd:x"); code != 504 {
		t.Fatalf("%d", code)
	}
}

func TestSeqSeededFromTimeAndNextLongPoll(t *testing.T) {
	g := newRig(t, "true")
	resp, _ := http.Get(g.ts.URL + "/seq")
	var s map[string]int64
	json.NewDecoder(resp.Body).Decode(&s)
	if d := time.Now().Unix() - s["seq"]; d < 0 || d > 5 {
		t.Fatalf("seq %d", s["seq"])
	}
	http.Post(g.ts.URL+"/publish", "application/json", strings.NewReader(fmt.Sprintf(`{"seq":%d,"name":"x"}`, s["seq"]+1)))
	resp, _ = http.Get(fmt.Sprintf("%s/next?after=%d", g.ts.URL, s["seq"]))
	var n map[string]any
	json.NewDecoder(resp.Body).Decode(&n)
	if resp.StatusCode != 200 || n["name"] != "x" || n["publishedAt"] == nil {
		t.Fatalf("%d %v", resp.StatusCode, n)
	}
}

func TestLoupeFailureKeepsPatchInfo(t *testing.T) {
	g := newRig(t, writeBuilder(t, okBuild))
	g.s.loupe.token = "wrong"
	stop := g.app(t, func(seq int64) string { return fmt.Sprintf(`{"event":"patch","seq":%d,"ok":true}`, seq) })
	defer stop()
	waitPolling(t, g)
	code, out := post(t, g, "avd:x")
	if code != 502 || out["code"] != "loupeError" || out["report"] == nil {
		t.Fatalf("%d %v", code, out)
	}
}

func solidPNG(t *testing.T, c color.Gray) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = c.Y
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSettleShootsUntilTwoMatch(t *testing.T) {
	g := newRig(t, "true")
	g.s.cfg.SettleTimeout = 2 * time.Second
	blank, drawn := solidPNG(t, color.Gray{0}), solidPNG(t, color.Gray{200})
	g.loupe.shots = [][]byte{blank, drawn, drawn}
	sh := g.s.settle("t", http.Header{})
	if sh.err != nil || !sh.stable || sh.shots != 3 || !bytes.Equal(sh.b, drawn) {
		t.Fatalf("settle = stable %v shots %d err %v; want the drawn frame after 3 shots", sh.stable, sh.shots, sh.err)
	}
}

func TestSettleGivesUpAtTheTimeout(t *testing.T) {
	g := newRig(t, "true")
	g.s.cfg.SettleTimeout = 50 * time.Millisecond
	n := 0
	g.loupe.next = func() []byte { n++; return solidPNG(t, color.Gray{uint8(n)}) }
	sh := g.s.settle("t", http.Header{})
	if sh.stable || sh.shots < 2 || sh.err != nil {
		t.Fatalf("settle = stable %v shots %d err %v; want unstable", sh.stable, sh.shots, sh.err)
	}
}
