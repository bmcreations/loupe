package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
			if _, ok := props[strings.TrimSuffix(a, "...")]; !ok {
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
		{"install", []string{"avd:P", "a.apk", "b.apk", "-launch"}, `map[launch:true paths:[a.apk b.apk] target:avd:P]`},
		{"type", []string{"avd:P", "--", "-hello"}, `map[target:avd:P text:-hello]`},
		{"tap", []string{"-server", "https://x", "avd:P", "-5", "7"}, `map[target:avd:P x:-5 y:7]`},
		{"flags", nil, `map[]`},
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
