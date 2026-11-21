package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const mcpHelperURL = "LOUPE_TEST_MCP_URL"

// TestMCPHelperProcess is `loupe mcp` for TestMCPReleasesItsClaimOnHangup,
// which runs this test binary again with mcpHelperURL set.
func TestMCPHelperProcess(t *testing.T) {
	u := os.Getenv(mcpHelperURL)
	if u == "" {
		t.Skip("run by TestMCPReleasesItsClaimOnHangup")
	}
	os.Exit(runMCP([]string{"-url", u, "-token", "tok", "-agent", "hup"}))
}

// An agent in a tmux pane that is respawned with -k, or in a terminal that
// closes, takes loupe mcp down with a SIGHUP, often while it waits on flags.
// Its claim has to go back then, not ten minutes later.
func TestMCPReleasesItsClaimOnHangup(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	waiting := make(chan struct{}, 1)
	unblock := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/flags" && r.URL.Query().Get("waitMs") == "60000" {
			select {
			case waiting <- struct{}{}:
			default:
			}
			select {
			case <-unblock:
			case <-r.Context().Done():
			}
		}
		if r.URL.Path == "/api/flags" {
			io.WriteString(w, `{"flags":[],"rev":1}`)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(unblock) })

	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPHelperProcess$")
	cmd.Env = append(os.Environ(), mcpHelperURL+"="+ts.URL)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	send := func(m map[string]any) {
		b, _ := json.Marshal(m)
		stdin.Write(append(b, '\n'))
	}
	answer := func(id int) {
		for lines.Scan() {
			var m struct {
				ID int `json:"id"`
			}
			if json.Unmarshal(lines.Bytes(), &m) == nil && m.ID == id {
				return
			}
		}
		t.Fatalf("no answer to request %d; stderr: %s", id, stderr.String())
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "0"}}})
	answer(1)
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		"name": "tap", "arguments": map[string]any{"target": "emulator-5554", "x": 1, "y": 2}}})
	answer(2)
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
		"name": "flags", "arguments": map[string]any{"target": "emulator-5554", "wait_seconds": 60}}})
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the flags call never reached the server")
	}

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("loupe mcp after SIGHUP: %v; stderr: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loupe mcp still running 5s after SIGHUP")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range seen {
		if strings.HasPrefix(r, "DELETE /api/claims?target=emulator-5554") {
			return
		}
	}
	t.Errorf("claim not released on SIGHUP; requests: %v", seen)
}
