package target

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testRevylKey is lower case so that it also passes for an error code: a
// server that echoes it as one must still not get it repeated.
const testRevylKey = "rk_test_9f8e7d6c5b4a3210"

const testRunID = "wfr-test"

// fakeProxy stands in for Revyl's device proxy. Requests are recorded in
// arrival order. reply picks the answer per action; nil answers success.
type fakeProxy struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	reqs  []proxyReq
	reply func(action string) (status int, body string)
	// hold, when set, is called before answering and may block.
	hold func(action string, r *http.Request)

	inflight, maxInflight atomic.Int32
	conns                 atomic.Int32
	got                   chan string // each request's action, once recorded
}

type proxyReq struct {
	method, path string
	header       http.Header
	raw          string
}

func (r proxyReq) action() string { return r.path[strings.LastIndex(r.path, "/")+1:] }

func (r proxyReq) body(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.raw), &m); err != nil {
		t.Fatalf("%s body %q: %v", r.path, r.raw, err)
	}
	return m
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	p := &fakeProxy{t: t, got: make(chan string, 256)}
	p.srv = httptest.NewUnstartedServer(http.HandlerFunc(p.serve))
	p.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			p.conns.Add(1)
		}
	}
	p.srv.Start()
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProxy) serve(w http.ResponseWriter, r *http.Request) {
	n := p.inflight.Add(1)
	defer p.inflight.Add(-1)
	for {
		m := p.maxInflight.Load()
		if n <= m || p.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	b, _ := io.ReadAll(r.Body)
	req := proxyReq{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), raw: string(b)}
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	reply, hold := p.reply, p.hold
	p.mu.Unlock()
	p.got <- req.action()
	if hold != nil {
		hold(req.action(), r)
	}
	status, body := http.StatusOK, `{"success":true,"action":"`+req.action()+`","latency_ms":6,"error":null}`
	if reply != nil {
		if s, b := reply(req.action()); s != 0 {
			status, body = s, b
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (p *fakeProxy) requests() []proxyReq {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proxyReq(nil), p.reqs...)
}

func (p *fakeProxy) set(reply func(string) (int, string), hold func(string, *http.Request)) {
	p.mu.Lock()
	p.reply, p.hold = reply, hold
	p.mu.Unlock()
}

// waitFor waits for the nth request, counting from 1.
func (p *fakeProxy) waitFor(n int) []proxyReq {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := p.requests()
		if len(r) >= n {
			return r
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("got %d proxy requests, want %d", len(r), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// traceLog collects the input's trace records.
type traceLog struct {
	mu   sync.Mutex
	recs []map[string]any
}

func (l *traceLog) record(rec map[string]any) {
	l.mu.Lock()
	l.recs = append(l.recs, rec)
	l.mu.Unlock()
}

// wait waits for n records and returns them all.
func (l *traceLog) wait(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l.mu.Lock()
		got := append([]map[string]any(nil), l.recs...)
		l.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d trace records, want %d: %v", len(got), n, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type restRig struct {
	in   *revylInput
	clk  *fixedClock
	rest *RevylREST
	log  *traceLog
	key  atomic.Value // string
}

func restGestures(t *testing.T, f *fakeRevyl, baseURL, platform string, screenW, screenH int) *restRig {
	t.Helper()
	g := &restRig{log: &traceLog{}}
	g.key.Store(testRevylKey)
	g.rest = &RevylREST{BaseURL: baseURL, UserAgent: "loupe/test", Timeout: 5 * time.Second,
		Key: func() string { return g.key.Load().(string) }}
	g.in = newRevylInput(f.cli, g.rest, RevylSession{SessionID: testSession, WorkflowRunID: testRunID,
		Platform: platform, ScreenWidth: screenW, ScreenHeight: screenH}, g.log.record)
	t.Cleanup(g.in.close)
	g.clk = &fixedClock{t: time.Unix(1_700_000_000, 0)}
	g.in.now = g.clk.now
	return g
}

func (g *restRig) tap(t *testing.T, m Meta, x, y int32) {
	t.Helper()
	press(t, g.in, m, g.clk, 10*time.Millisecond, [2]int32{x, y})
}

func checkHeaders(t *testing.T, r proxyReq) {
	t.Helper()
	if r.method != http.MethodPost {
		t.Fatalf("%s method %s, want POST", r.path, r.method)
	}
	if got := r.header.Get("Authorization"); got != "Bearer "+testRevylKey {
		t.Fatalf("%s Authorization %q", r.path, got)
	}
	if got := r.header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("%s Content-Type %q", r.path, got)
	}
	// Claiming to be the CLI without its version got a 426 on a live
	// account; loupe says what it is.
	if _, ok := r.header["X-Revyl-Client"]; ok {
		t.Fatalf("%s sent X-Revyl-Client", r.path)
	}
	if ua := r.header.Get("User-Agent"); !strings.HasPrefix(ua, "loupe/") {
		t.Fatalf("%s User-Agent %q, want loupe/...", r.path, ua)
	}
}

func checkBodies(t *testing.T, reqs []proxyReq, want []any) {
	t.Helper()
	if len(reqs) != len(want) {
		t.Fatalf("got %d requests, want %d", len(reqs), len(want))
	}
	for i, r := range reqs {
		checkHeaders(t, r)
		switch w := want[i].(type) {
		case string: // an action with no body
			if r.action() != w || r.raw != "" {
				t.Fatalf("request %d: %s with body %q, want %s with none", i, r.path, r.raw, w)
			}
		case [2]any:
			if r.action() != w[0] {
				t.Fatalf("request %d: action %s, want %s", i, r.action(), w[0])
			}
			if got := r.body(t); !reflect.DeepEqual(got, w[1]) {
				t.Fatalf("request %d (%s): body %v, want %v", i, r.action(), got, w[1])
			}
		}
		if want := "/api/v1/execution/device-proxy/" + testRunID + "/" + r.action(); r.path != want {
			t.Fatalf("path %q, want %q", r.path, want)
		}
	}
}

func TestRevylRESTGesturesInAndroidPixels(t *testing.T) {
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	m := Meta{Width: 540, Height: 1200}

	press(t, g.in, m, g.clk, 80*time.Millisecond, [2]int32{270, 600}, [2]int32{272, 601})
	press(t, g.in, m, g.clk, 300*time.Millisecond, [2]int32{100, 1000}, [2]int32{100, 700}, [2]int32{100, 200})
	press(t, g.in, m, g.clk, 900*time.Millisecond, [2]int32{50, 60})
	for _, k := range []int32{KeycodeHome, KeycodeBack, keycodeEnter, keycodeDel} {
		if err := g.in.key(Key{Action: KeyDown, Keycode: k}); err != nil {
			t.Fatalf("key %d: %v", k, err)
		}
		_ = g.in.key(Key{Action: KeyUp, Keycode: k})
	}
	if err := g.in.text("hello wörld"); err != nil {
		t.Fatal(err)
	}

	reqs := p.waitFor(8)
	n := func(v float64) any { return v }
	checkBodies(t, reqs, []any{
		[2]any{"tap", map[string]any{"x": n(540), "y": n(1200)}},
		[2]any{"drag", map[string]any{"start_x": n(200), "start_y": n(2000), "end_x": n(200), "end_y": n(400)}},
		[2]any{"longpress", map[string]any{"x": n(100), "y": n(120), "duration_ms": n(900)}},
		"go_home",
		"back",
		[2]any{"key", map[string]any{"key": "ENTER"}},
		[2]any{"key", map[string]any{"key": "BACKSPACE"}},
		[2]any{"input", map[string]any{"x": n(100), "y": n(120), "text": "hello wörld", "clear_first": false}},
	})
	// clear_first is sent, not left to the worker's default of true.
	if !strings.Contains(reqs[7].raw, `"clear_first":false`) {
		t.Fatalf("input body %q lacks clear_first:false", reqs[7].raw)
	}
	if c := f.calls(); len(c) != 0 {
		t.Fatalf("the CLI ran: %q", c)
	}
	for _, rec := range g.log.wait(t, 8) {
		if rec["via"] != "rest" || rec["action"] != "input" {
			t.Fatalf("trace %v, want via rest", rec)
		}
		if _, ok := rec["ms"].(int64); !ok {
			t.Fatalf("trace %v has no round trip", rec)
		}
		if b, _ := json.Marshal(rec); strings.Contains(string(b), "hello") {
			t.Fatalf("typed text in the trace: %s", b)
		}
	}
	if err := g.in.err(); err != nil {
		t.Fatal(err)
	}
}

func TestRevylRESTGesturesInIOSPoints(t *testing.T) {
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	// iPhone 16 Pro: 393x852 points, streamed at its 1179x2556 pixels (3x).
	g := restGestures(t, f, p.srv.URL, "ios", 393, 852)
	m := Meta{Width: 1179, Height: 2556}

	press(t, g.in, m, g.clk, 50*time.Millisecond, [2]int32{589, 1278})
	press(t, g.in, m, g.clk, 200*time.Millisecond, [2]int32{589, 2400}, [2]int32{589, 1200})
	press(t, g.in, m, g.clk, 700*time.Millisecond, [2]int32{30, 30})
	if err := g.in.key(Key{Action: KeyDown, Keycode: KeycodeBack}); !errors.Is(err, ErrNoControl) {
		t.Fatalf("iOS back = %v, want ErrNoControl", err)
	}
	for _, k := range []int32{KeycodeHome, keycodeEnter} {
		if err := g.in.key(Key{Action: KeyDown, Keycode: k}); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.in.text("ok"); err != nil {
		t.Fatal(err)
	}
	// The stream turned to landscape: the point space turns with it.
	g.tap(t, Meta{Width: 2556, Height: 1179}, 2556-1, 1179-1)

	n := func(v float64) any { return v }
	checkBodies(t, p.waitFor(7), []any{
		[2]any{"tap", map[string]any{"x": n(196), "y": n(426)}},
		[2]any{"drag", map[string]any{"start_x": n(196), "start_y": n(800), "end_x": n(196), "end_y": n(400)}},
		[2]any{"longpress", map[string]any{"x": n(10), "y": n(10), "duration_ms": n(700)}},
		"go_home",
		[2]any{"key", map[string]any{"key": "ENTER"}},
		[2]any{"input", map[string]any{"x": n(10), "y": n(10), "text": "ok", "clear_first": false}},
		[2]any{"tap", map[string]any{"x": n(851), "y": n(392)}},
	})
	if c := f.calls(); len(c) != 0 {
		t.Fatalf("the CLI ran: %q", c)
	}
}

func TestRevylRESTKeepsOrderUnderABurst(t *testing.T) {
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	p.set(nil, func(string, *http.Request) { time.Sleep(15 * time.Millisecond) })
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	m := Meta{Width: 1080, Height: 2400}

	start := time.Now()
	const n = 20
	for i := range n {
		g.tap(t, m, int32(10+i*40), 100)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("%d taps took %v to queue; input must not wait for the network", n, d)
	}
	reqs := p.waitFor(n)
	for i, r := range reqs {
		if x := r.body(t)["x"]; x != float64(10+i*40) {
			t.Fatalf("request %d has x %v: out of order", i, x)
		}
	}
	if got := p.maxInflight.Load(); got != 1 {
		t.Fatalf("%d requests in flight at once, want 1", got)
	}
	if got := p.conns.Load(); got != 1 {
		t.Fatalf("%d connections for %d taps, want one kept alive", got, n)
	}
}

func TestRevylTextJoinsAndIsNeverDropped(t *testing.T) {
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	release := make(chan struct{})
	p.set(nil, func(action string, _ *http.Request) {
		if action == "tap" {
			<-release
		}
	})
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	m := Meta{Width: 1080, Height: 2400}

	// The first tap is in flight and held, so what follows waits.
	g.tap(t, m, 5, 5)
	if a := <-p.got; a != "tap" {
		t.Fatalf("first request %s", a)
	}
	for _, s := range []string{"he", "l", "lo"} {
		if err := g.in.text(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.in.key(Key{Action: KeyDown, Keycode: keycodeEnter}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"wo", "rld"} {
		if err := g.in.text(s); err != nil {
			t.Fatal(err)
		}
	}
	// Fill the queue with taps until it refuses one.
	queued := 3
	var full error
	for full == nil {
		full = g.in.touch(Touch{Action: TouchDown, PointerID: 1, X: 7, Y: 7}, m)
		if full == nil {
			full = g.in.touch(Touch{Action: TouchUp, PointerID: 1, X: 7, Y: 7}, m)
		}
		if full == nil {
			queued++
		}
		if queued > 2*revylQueue {
			t.Fatal("the queue never filled")
		}
	}
	if queued != revylQueue || !strings.Contains(full.Error(), "queue full") {
		t.Fatalf("queued %d before %v, want %d then queue full", queued, full, revylQueue)
	}
	// Text still goes in, and joins.
	for _, s := range []string{"x", "y"} {
		if err := g.in.text(s); err != nil {
			t.Fatalf("text with the queue full: %v", err)
		}
	}
	close(release)

	reqs := p.waitFor(1 + revylQueue + 1)
	got := make([]string, len(reqs))
	for i, r := range reqs {
		got[i] = r.action()
		if r.action() == "input" {
			got[i] += ":" + r.body(t)["text"].(string)
		}
	}
	want := []string{"tap", "input:hello", "key", "input:world"}
	for range revylQueue - 3 {
		want = append(want, "tap")
	}
	want = append(want, "input:xy")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests\n got %q\nwant %q", got, want)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(p.requests()); n != len(want) {
		t.Fatalf("%d requests, want %d", n, len(want))
	}
}

// closedURL is a base URL nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String()
	l.Close()
	return u
}

func TestRevylRESTFallsBackToTheCLI(t *testing.T) {
	m := Meta{Width: 1080, Height: 2400}

	t.Run("426 turns REST off for the stream", func(t *testing.T) {
		f := newFakeRevyl(t)
		p := newFakeProxy(t)
		p.set(func(string) (int, string) {
			return http.StatusUpgradeRequired, `{"detail":{"code":"cli_upgrade_required","message":"Upgrade the CLI"}}`
		}, nil)
		g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
		g.tap(t, m, 10, 10)
		g.tap(t, m, 20, 20)
		calls := f.waitCalls(2)
		if !reflect.DeepEqual(calls[0], argv("device", "tap", "--x", "10", "--y", "10")) ||
			!reflect.DeepEqual(calls[1], argv("device", "tap", "--x", "20", "--y", "20")) {
			t.Fatalf("CLI argv %q", calls)
		}
		if n := len(p.requests()); n != 1 {
			t.Fatalf("%d proxy requests, want 1: a 426 is not asked again", n)
		}
		recs := g.log.wait(t, 2)
		if recs[0]["via"] != "cli" || !strings.Contains(recs[0]["restError"].(string), "426 cli_upgrade_required") {
			t.Fatalf("trace %v", recs[0])
		}
		if recs[1]["via"] != "cli" || recs[1]["rest"] != "refused earlier" {
			t.Fatalf("trace %v", recs[1])
		}
		if err := g.in.err(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("dial error, for a gesture and for text", func(t *testing.T) {
		f := newFakeRevyl(t)
		g := restGestures(t, f, closedURL(t), "android", 1080, 2400)
		g.tap(t, m, 10, 10)
		if err := g.in.text("abc"); err != nil {
			t.Fatal(err)
		}
		calls := f.waitCalls(2)
		if !reflect.DeepEqual(calls[0], argv("device", "tap", "--x", "10", "--y", "10")) ||
			!reflect.DeepEqual(calls[1], argv("device", "type", "--text-env", "LOUPE_REVYL_TEXT", "--clear-first=false", "--x", "10", "--y", "10")) {
			t.Fatalf("CLI argv %q", calls)
		}
		if f.file("text.log") != "abc\n" {
			t.Fatalf("typed %q", f.file("text.log"))
		}
		recs := g.log.wait(t, 2)
		for _, rec := range recs {
			if rec["via"] != "cli" || rec["restError"] == nil {
				t.Fatalf("trace %v", rec)
			}
		}
	})

	t.Run("401 holds while the key is the same", func(t *testing.T) {
		f := newFakeRevyl(t)
		p := newFakeProxy(t)
		p.set(func(string) (int, string) { return http.StatusUnauthorized, `{"detail":"Invalid API key"}` }, nil)
		g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
		g.tap(t, m, 10, 10)
		g.tap(t, m, 20, 20)
		f.waitCalls(2)
		if n := len(p.requests()); n != 1 {
			t.Fatalf("%d proxy requests, want 1", n)
		}
		// A new sign-in is tried.
		g.key.Store("rk_test_new")
		p.set(nil, nil)
		g.tap(t, m, 30, 30)
		if r := p.waitFor(2)[1]; r.header.Get("Authorization") != "Bearer rk_test_new" {
			t.Fatalf("second key not sent: %q", r.header.Get("Authorization"))
		}
		if n := len(f.calls()); n != 2 {
			t.Fatalf("%d CLI calls, want 2", n)
		}
	})

	t.Run("5xx falls back for a tap but not for text", func(t *testing.T) {
		f := newFakeRevyl(t)
		p := newFakeProxy(t)
		p.set(func(string) (int, string) { return http.StatusBadGateway, `bad gateway` }, nil)
		g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
		g.tap(t, m, 10, 10)
		if err := g.in.text("hello"); err != nil {
			t.Fatal(err)
		}
		recs := g.log.wait(t, 2)
		if calls := f.calls(); len(calls) != 1 || calls[0][1] != "tap" {
			t.Fatalf("CLI calls %q, want the tap alone", calls)
		}
		if n := len(p.requests()); n != 2 {
			t.Fatalf("%d proxy requests, want 2", n)
		}
		if recs[1]["via"] != "rest" || !strings.Contains(recs[1]["error"].(string), "status 502") {
			t.Fatalf("input trace %v", recs[1])
		}
		if err := g.in.err(); err == nil || !strings.Contains(err.Error(), "502") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("no workflow run or no key", func(t *testing.T) {
		f := newFakeRevyl(t)
		p := newFakeProxy(t)
		g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
		g.key.Store("")
		g.tap(t, m, 10, 10)
		f.waitCalls(1)
		if rec := g.log.wait(t, 1)[0]; rec["via"] != "cli" || rec["rest"] != "no key" {
			t.Fatalf("trace %v", rec)
		}
		in := newRevylInput(f.cli, g.rest, RevylSession{SessionID: testSession, Platform: "android", ScreenWidth: 1080, ScreenHeight: 2400}, nil)
		t.Cleanup(in.close)
		g.key.Store(testRevylKey)
		press(t, in, m, g.clk, 10*time.Millisecond, [2]int32{20, 20})
		f.waitCalls(2)
		if n := len(p.requests()); n != 0 {
			t.Fatalf("%d proxy requests, want 0", n)
		}
	})
}

func TestRevylRESTDoesNotResend(t *testing.T) {
	m := Meta{Width: 1080, Height: 2400}
	for _, tc := range []struct {
		name  string
		reply func(string) (int, string)
		hold  bool
		want  string
	}{
		{name: "timeout after the send", hold: true, want: "timed out"},
		{name: "device refused", reply: func(string) (int, string) {
			return http.StatusOK, `{"success":false,"action":"tap","error":"no element at point"}`
		}, want: "the device refused it: no element at point"},
		{name: "unreadable answer", reply: func(string) (int, string) { return http.StatusOK, `<html>` }, want: "no success field"},
		{name: "409", reply: func(string) (int, string) { return http.StatusConflict, `{"code":"session_busy"}` }, want: "status 409 session_busy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRevyl(t)
			p := newFakeProxy(t)
			var hold func(string, *http.Request)
			if tc.hold {
				hold = func(_ string, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
					}
				}
			}
			p.set(tc.reply, hold)
			g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
			g.rest.Timeout = 200 * time.Millisecond
			g.tap(t, m, 10, 10)
			if err := g.in.text("typed"); err != nil {
				t.Fatal(err)
			}
			recs := g.log.wait(t, 2)
			time.Sleep(50 * time.Millisecond)
			if n := len(p.requests()); n != 2 {
				t.Fatalf("%d proxy requests, want one each", n)
			}
			if c := f.calls(); len(c) != 0 {
				t.Fatalf("the CLI resent: %q", c)
			}
			for _, rec := range recs {
				if rec["via"] != "rest" || !strings.Contains(rec["error"].(string), tc.want) {
					t.Fatalf("trace %v, want an error with %q", rec, tc.want)
				}
			}
		})
	}
}

func TestRevylRESTFallbackRules(t *testing.T) {
	for _, tc := range []struct {
		action string
		status int
		wrote  bool
		want   bool
	}{
		{"tap", 0, false, true}, // never left: dial, DNS or TLS
		{"tap", 0, true, false}, // sent, then a timeout or a dropped connection
		{"input", 0, false, true},
		{"input", 0, true, false},
		{"tap", 401, true, true},
		{"tap", 403, true, true},
		{"tap", 404, true, true},
		{"tap", 426, true, true},
		{"input", 426, true, true},
		{"tap", 500, true, true},
		{"key", 503, true, true},
		{"input", 502, true, false},
		{"input", 504, true, false},
		{"tap", 400, true, false},
		{"tap", 409, true, false},
		{"tap", 429, true, false},
		{"tap", 200, true, false}, // success false
	} {
		e := &revylRESTError{action: tc.action, status: tc.status, wrote: tc.wrote}
		if got := e.fallback(); got != tc.want {
			t.Errorf("%s status %d wrote %v: fallback %v, want %v", tc.action, tc.status, tc.wrote, got, tc.want)
		}
	}
}

func TestRevylRESTKeepsTheKeyOut(t *testing.T) {
	m := Meta{Width: 1080, Height: 2400}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	var step atomic.Int32
	echo := `"` + testRevylKey + `"`
	replies := []func() (int, string){
		// A server that echoes the key back, as a code, a detail and a
		// device error.
		func() (int, string) { return http.StatusBadRequest, `{"code":` + echo + `}` },
		func() (int, string) {
			return http.StatusUnauthorized, `{"detail":{"code":` + echo + `},"message":"Bearer ` + testRevylKey + `"}`
		},
		func() (int, string) {
			return http.StatusOK, `{"success":false,"error":"token ` + testRevylKey + ` rejected at https://backend.revyl.invalid/x?k=1"}`
		},
	}
	p.set(func(string) (int, string) { return replies[min(int(step.Load()), len(replies)-1)]() }, nil)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)

	var seen []string
	for i := range replies {
		step.Store(int32(i))
		g.tap(t, m, int32(10+i), 10)
		g.log.wait(t, i+1)
		if err := g.in.err(); err != nil {
			seen = append(seen, err.Error())
		}
	}
	// The 401 latched this key, so give a new one for the last two.
	g.key.Store(testRevylKey + "x")
	// Not sent at all.
	g.rest.BaseURL = closedURL(t)
	g.tap(t, m, 40, 10)
	// The worker reads the config; change it only between gestures.
	g.log.wait(t, len(replies)+1)
	// Sent, then timed out.
	g.rest.BaseURL = p.srv.URL
	g.rest.Timeout = 100 * time.Millisecond
	p.set(nil, func(_ string, r *http.Request) { <-r.Context().Done() })
	g.tap(t, m, 50, 10)
	recs := g.log.wait(t, len(replies)+2)
	if err := g.in.err(); err != nil {
		seen = append(seen, err.Error())
	}
	for _, rec := range recs {
		b, _ := json.Marshal(rec)
		seen = append(seen, string(b))
	}
	if len(seen) < len(recs)+2 {
		t.Fatalf("only %d errors seen: %q", len(seen), seen)
	}
	for _, s := range seen {
		if strings.Contains(s, testRevylKey) {
			t.Fatalf("key in %q", s)
		}
		if strings.Contains(s, "device-proxy") || strings.Contains(s, testRunID) || strings.Contains(s, "?k=") {
			t.Fatalf("URL in %q", s)
		}
	}
}

func TestRevylAPIKeyLookup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("REVYL_API_KEY", "")
	if got := revylAPIKey(path); got != "" {
		t.Fatalf("no file: %q", got)
	}
	write(`{"api_key":"rk_file","api_key_id":"id","auth_method":"browser","email":"dev@example.invalid","org_id":"o","user_id":"u"}`)
	if got := revylAPIKey(path); got != "rk_file" {
		t.Fatalf("file: %q", got)
	}
	// Read per call: a new sign-in counts without a restart.
	write(`{"api_key":"rk_file2"}`)
	if got := revylAPIKey(path); got != "rk_file2" {
		t.Fatalf("rewritten file: %q", got)
	}
	t.Setenv("REVYL_API_KEY", "rk_env")
	if got := revylAPIKey(path); got != "rk_env" {
		t.Fatalf("env: %q, want it ahead of the file", got)
	}
	t.Setenv("REVYL_API_KEY", "")
	write(`{"access_token":"only a browser token"}`)
	if got := revylAPIKey(path); got != "" {
		t.Fatalf("no api_key: %q", got)
	}
	write(`not json`)
	if got := revylAPIKey(path); got != "" {
		t.Fatalf("bad file: %q", got)
	}
}

func TestRevylTargetSendsInputOverREST(t *testing.T) {
	w := newFakeWHEP(t)
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	f.respond("device-info", "device-info", w.srv.URL+"/whep/endpoint")
	log := &traceLog{}
	tgt := &RevylTarget{
		CLI: f.cli, Session: RevylSession{SessionID: testSession, Platform: "android"},
		REST:     &RevylREST{BaseURL: p.srv.URL, UserAgent: "loupe/test", Key: func() string { return testRevylKey }},
		Trace:    log.record,
		settings: loopbackSettings(), poll: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := tgt.Open(ctx, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.Touch(Touch{Action: TouchDown, PointerID: 1, X: 180, Y: 101, Pressure: 1})
	_ = st.Touch(Touch{Action: TouchUp, PointerID: 1, X: 180, Y: 101})
	// The run ID comes from `device info`, which Open read.
	r := p.waitFor(1)[0]
	if r.path != "/api/v1/execution/device-proxy/"+testRunID+"/tap" {
		t.Fatalf("path %q", r.path)
	}
	if rec := log.wait(t, 1)[0]; rec["target"] != "revyl:"+testSession || rec["via"] != "rest" {
		t.Fatalf("trace %v", rec)
	}
}
