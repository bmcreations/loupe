package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bmcreations/loupe/internal/mcpserver"
)

// listTools returns every tool `loupe mcp` serves, by name.
func listTools(t *testing.T) map[string]*mcp.Tool {
	t.Helper()
	c, err := mcpserver.NewClient("http://127.0.0.1:1", "tok", "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := mcpserver.NewServer(c).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		out[tl.Name] = tl
	}
	return out
}

func TestEveryToolHasACommand(t *testing.T) {
	tools := listTools(t)
	byTool := map[string]cliCommand{}
	for _, c := range cliCommands {
		tl, ok := tools[c.tool]
		if !ok {
			t.Errorf("command %s names no tool %q", c.name, c.tool)
			continue
		}
		byTool[c.tool] = c
		props := schemaProps(tl)
		for _, a := range c.args {
			if _, ok := props[strings.TrimSuffix(strings.TrimSuffix(a, "..."), "?")]; !ok {
				t.Errorf("command %s: positional %q is not a field of %s", c.name, a, c.tool)
			}
		}
	}
	for name := range tools {
		if _, ok := byTool[name]; !ok {
			t.Errorf("tool %s has no CLI command", name)
		}
	}
}

func TestCLIGlobalsAreNotFields(t *testing.T) {
	var g cliGlobals
	for name, tl := range listTools(t) {
		for field := range schemaProps(tl) {
			if g.ptr(field) != nil || field == "h" || field == "help" {
				t.Errorf("tool %s field %q collides with a global flag", name, field)
			}
		}
	}
}

func TestParseCLIArgs(t *testing.T) {
	tools := listTools(t)
	cases := []struct {
		cmd  string
		argv []string
		want string
	}{
		{"tap", []string{"avd:P", "540", "1200"}, `map[target:avd:P x:540 y:1200]`},
		{"swipe", []string{"avd:P", "1", "2", "3", "4", "-ms", "300"}, `map[ms:300 target:avd:P x:1 x2:3 y:2 y2:4]`},
		{"ui", []string{"avd:P", "--interactive"}, `map[interactive:true target:avd:P]`},
		{"ui", []string{"avd:P", "--compose"}, `map[compose:true target:avd:P]`},
		{"install", []string{"avd:P", "a.apk", "b.apk", "-launch"}, `map[launch:true paths:[a.apk b.apk] target:avd:P]`},
		{"install", []string{"avd:P", "a.apk", "-downgrade"}, `map[downgrade:true paths:[a.apk] target:avd:P]`},
		{"uninstall", []string{"avd:P", "com.example.app"}, `map[package:com.example.app target:avd:P]`},
		{"type", []string{"avd:P", "--", "-hello"}, `map[target:avd:P text:-hello]`},
		{"tap", []string{"-server", "https://x", "avd:P", "-5", "7"}, `map[target:avd:P x:-5 y:7]`},
		{"flags", nil, `map[]`},
		{"tap", []string{"avd:P", "--text", "Sign in"}, `map[target:avd:P text:Sign in]`},
		{"tap", []string{"avd:P", "-desc", "Back", "-index", "1"}, `map[desc:Back index:1 target:avd:P]`},
		{"boot", []string{"avd:P", "--wait", "10m"}, `map[target:avd:P wait:10m]`},
		{"boot", []string{"avd:P", "-wait", "0"}, `map[target:avd:P wait:0]`},
		{"boot-status", []string{"avd:P"}, `map[target:avd:P]`},
		{"ext list", nil, `map[]`},
		{"ext call", []string{"slipway"}, `map[name:slipway]`},
		{"ext call", []string{"slipway", "builds", "-method", "POST", "-body", `{"a":1}`}, `map[body:{"a":1} method:POST name:slipway path:builds]`},
	}
	for _, tc := range cases {
		cmd, _ := findCLICommand(tc.cmd)
		var g cliGlobals
		in, err := parseCLIArgs(cmd, tools[cmd.tool], tc.argv, &g)
		if err != nil {
			t.Errorf("%s %v: %v", tc.cmd, tc.argv, err)
			continue
		}
		if got := fmtMap(in); got != tc.want {
			t.Errorf("%s %v = %s, want %s", tc.cmd, tc.argv, got, tc.want)
		}
	}
	seq, _ := findCLICommand("sequence")
	in, err := parseCLIArgs(seq, tools["sequence"], []string{"avd:P", `[{"action":"tap","x":0,"y":0}]`}, &cliGlobals{})
	if b, _ := json.Marshal(in); err != nil || string(b) != `{"steps":[{"action":"tap","x":0,"y":0}],"target":"avd:P"}` {
		t.Errorf("sequence = %s, %v; want the steps decoded as JSON", b, err)
	}
	for _, bad := range [][]string{{"avd:P"}, {"avd:P", "[{"}, {"avd:P", "@/nonexistent/steps.json"}} {
		var g cliGlobals
		if _, err := parseCLIArgs(seq, tools["sequence"], bad, &g); err == nil {
			t.Errorf("sequence %v: no error", bad)
		}
	}
	for _, bad := range [][]string{{"avd:P", "1"}, {"avd:P", "x", "2"}, {"avd:P", "1", "2", "3"}, {"avd:P", "1", "2", "-nope", "1"}} {
		cmd, _ := findCLICommand("tap")
		var g cliGlobals
		if _, err := parseCLIArgs(cmd, tools["tap"], bad, &g); err == nil {
			t.Errorf("tap %v: no error", bad)
		}
	}
}

func fmtMap(m map[string]any) string {
	var b strings.Builder
	b.WriteString("map[")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(k + ":")
		switch v := m[k].(type) {
		case []any:
			b.WriteString("[")
			for j, x := range v {
				if j > 0 {
					b.WriteString(" ")
				}
				b.WriteString(x.(string))
			}
			b.WriteString("]")
		default:
			b.WriteString(fmt.Sprint(v))
		}
	}
	b.WriteString("]")
	return b.String()
}

// TestCLIKeepsItsClaim runs two taps as two commands. Each claims and acts,
// both under the same agent and no session, and neither releases: the claim
// has to outlive the process for the second command to hold it.
func TestCLIKeepsItsClaim(t *testing.T) {
	var mu sync.Mutex
	var log []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		log = append(log, r.Method+" "+r.URL.Path+" agent="+r.Header.Get("X-Loupe-Agent")+" session="+r.Header.Get("X-Loupe-Session")+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	t.Setenv("LOUPE_URL", srv.URL)
	t.Setenv("LOUPE_TOKEN", "tok")
	t.Setenv("LOUPE_AGENT", "")
	t.Setenv("LOUPE_SESSION", "")

	cmd, _ := findCLICommand("tap")
	for i := 0; i < 2; i++ {
		if code := runCLI(cmd, []string{"avd:P", "10", "20"}); code != 0 {
			t.Fatalf("tap %d exited %d", i, code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	var claims int
	for _, l := range log {
		if strings.HasPrefix(l, "DELETE ") {
			t.Errorf("released a claim: %s", l)
		}
		if strings.HasPrefix(l, "POST /api/claims") {
			claims++
		}
		if !strings.Contains(l, "agent=cli session= auth=Bearer tok") {
			t.Errorf("headers: %s", l)
		}
	}
	if claims != 2 {
		t.Errorf("claims posted = %d, want one per command:\n%s", claims, strings.Join(log, "\n"))
	}
}

func TestResolveCLICommand(t *testing.T) {
	cases := []struct {
		args     []string
		wantName string
		wantRest string
		wantOK   bool
	}{
		{[]string{"ext", "list"}, "ext list", "", true},
		{[]string{"ext", "call", "slipway", "builds"}, "ext call", "slipway builds", true},
		{[]string{"tap", "avd:P", "1", "2"}, "tap", "avd:P 1 2", true},
		{[]string{"ext"}, "", "", false},
		{[]string{"ext", "nope"}, "", "", false},
		{[]string{"list"}, "", "", false},
	}
	for _, tc := range cases {
		cmd, rest, ok := resolveCLICommand(tc.args)
		if ok != tc.wantOK || cmd.name != tc.wantName || strings.Join(rest, " ") != tc.wantRest {
			t.Errorf("%v: got %q %q %v, want %q %q %v", tc.args, cmd.name, strings.Join(rest, " "), ok, tc.wantName, tc.wantRest, tc.wantOK)
		}
	}
}

// TestExtCallExitsOneOnAnErrorStatus: call_extension returns a 4xx or 5xx as
// an answer, not a tool error, so a script checks `ext call`'s exit status.
// It is 1 from 400 up, loupe's own 404 and 502 included, and the answer stays
// on stdout.
func TestExtCallExitsOneOnAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ext/fake/ok":
			io.WriteString(w, `{"ok":true}`)
		case "/api/ext/fake/moved":
			w.Header().Set("Location", "/api/ext/fake/ok")
			w.WriteHeader(http.StatusFound)
		case "/api/ext/fake/missing":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"no such build"}`)
		case "/api/ext/nosuch/x":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"no extension nosuch"}`)
		case "/api/ext/down/x":
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"error":"extension down is not answering"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("LOUPE_URL", srv.URL)
	t.Setenv("LOUPE_TOKEN", "tok")
	t.Setenv("LOUPE_AGENT", "")
	t.Setenv("LOUPE_SESSION", "")

	cmd, _ := findCLICommand("ext call")
	for _, tc := range []struct {
		args     []string
		wantCode int
		wantOut  string
	}{
		{[]string{"fake", "ok"}, 0, `{"ok":true}`},
		{[]string{"fake", "moved"}, 0, "location: /api/ext/fake/ok"},
		{[]string{"fake", "missing"}, 1, `{"error":"no such build"}`},
		{[]string{"nosuch", "x"}, 1, `{"error":"no extension nosuch"}`},
		{[]string{"down", "x"}, 1, "status: 502 Bad Gateway"},
	} {
		code, out := captureStdout(t, func() int { return runCLI(cmd, tc.args) })
		if code != tc.wantCode || !strings.Contains(out, tc.wantOut) {
			t.Errorf("ext call %v: exit %d stdout %q, want exit %d with %q", tc.args, code, out, tc.wantCode, tc.wantOut)
		}
	}
}

func captureStdout(t *testing.T, f func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := f()
	os.Stdout = old
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	return code, string(b)
}

func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = old
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b)
}

// A note about the answer goes to stderr, so a piped log, ndjson above
// all, stays as the server sent it.
func TestWriteResultSendsNotesToStderr(t *testing.T) {
	log := `{"eventMessage":"a"}` + "\n"
	res := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: log},
		&mcp.TextContent{Text: "loupe cut this log", Meta: mcp.Meta{mcpserver.MetaNote: true}},
	}}
	var code int
	var stdout string
	stderr := captureStderr(t, func() {
		code, stdout = captureStdout(t, func() int { return writeResult(res, "") })
	})
	if code != 0 || stdout != log {
		t.Errorf("exit %d stdout %q; want 0 and only the log", code, stdout)
	}
	if stderr != "loupe cut this log\n" {
		t.Errorf("stderr = %q; want the note", stderr)
	}
}
