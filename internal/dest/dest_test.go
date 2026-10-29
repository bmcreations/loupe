package dest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, mode os.FileMode, f any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "destinations.json")
	b, _ := json.Marshal(f)
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(f.Commands)+len(f.Webhooks) != 0 {
		t.Fatalf("got %+v, %v", f, err)
	}
}

func TestLoadRefusesAFileOthersMayWrite(t *testing.T) {
	p := writeFile(t, 0o620, File{})
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		f    File
		want string
	}{
		{"relative program", File{Commands: []Command{{Name: "a", Program: "claude"}}}, "absolute"},
		{"bad name", File{Commands: []Command{{Name: "A b", Program: "/bin/cat"}}}, "name"},
		{"duplicate", File{Commands: []Command{{Name: "a", Program: "/bin/cat"}, {Name: "a", Program: "/bin/cat"}}}, "twice"},
		{"note placeholder", File{Commands: []Command{{Name: "a", Program: "/bin/cat", Args: []string{"{note}"}}}}, "unknown placeholder"},
		{"loupe env", File{Commands: []Command{{Name: "a", Program: "/bin/cat", Env: map[string]string{"LOUPE_TOKEN": "x"}}}}, "LOUPE_"},
		{"plain http", File{Webhooks: []Webhook{{Name: "a", URL: "http://example.com/x"}}}, "https"},
		{"ftp", File{Webhooks: []Webhook{{Name: "a", URL: "ftp://example.com/x"}}}, "https"},
	}
	for _, c := range cases {
		err := c.f.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	ok := File{
		Commands: []Command{{Name: "claude", Program: "/bin/cat", Args: []string{"-p", "--id={id}", "{start}"}}},
		Webhooks: []Webhook{{Name: "local", URL: "http://127.0.0.1:9/x"}, {Name: "hook", URL: "https://example.com/x"}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid file refused: %v", err)
	}
}

func TestRunFillsPlaceholdersAndKeepsTheTokenOut(t *testing.T) {
	t.Setenv("LOUPE_TOKEN", "secret")
	dir := t.TempDir()
	script := filepath.Join(dir, "fake")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"args:$*\"\necho \"token:${LOUPE_TOKEN}\"\necho \"extra:${EXTRA}\"\necho \"pwd:$(pwd -P)\"\ncat\n"), 0o755)
	c := Command{Name: "x", Program: script, Args: []string{"--flag={id}", "{target}"}, Env: map[string]string{"EXTRA": "yes"}}
	run, _ := filepath.EvalSymlinks(dir)
	out, err := c.Run(context.Background(), map[string]string{"id": "7", "target": "avd:p"}, dir, "the message")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"args:--flag=7 avd:p", "token:\n", "extra:yes", "pwd:" + run, "the message"} {
		if !strings.Contains(out+"\n", want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestRunReportsAFailure(t *testing.T) {
	c := Command{Name: "x", Program: "/bin/sh", Args: []string{"-c", "echo boom >&2; exit 3"}}
	out, err := c.Run(context.Background(), nil, t.TempDir(), "")
	if err == nil || out != "boom" {
		t.Fatalf("out %q, err %v", out, err)
	}
}

func TestPostSendsHeadersAndDoesNotFollowRedirects(t *testing.T) {
	var got http.Header
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		got = r.Header
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	w := Webhook{Name: "h", URL: srv.URL + "/in", Headers: map[string]string{"Authorization": "Bearer k"}}
	code, _, err := w.Post(context.Background(), []byte(`{"a":1}`))
	if err != nil || code != 200 {
		t.Fatalf("code %d, err %v", code, err)
	}
	if got.Get("Authorization") != "Bearer k" || got.Get("Content-Type") != "application/json" || body != `{"a":1}` {
		t.Fatalf("headers %v body %q", got, body)
	}
	w.URL = srv.URL + "/redirect"
	got = nil
	if code, _, err := w.Post(context.Background(), []byte(`{}`)); err == nil || code != http.StatusFound || got != nil {
		t.Fatalf("redirect: code %d, err %v, followed %v", code, err, got != nil)
	}
}

func TestTerminalRunOpensAScriptThatPassesTheTextIntact(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "open.log")
	fake := filepath.Join(dir, "open")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+log+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := opener
	opener = fake
	t.Cleanup(func() { opener = old })

	c := Command{Name: "watch", Program: "/usr/bin/printf", Args: []string{"%s|", "{text}", "{id}"}, Terminal: "Terminal"}
	text := "> it's -n wrong; $(echo no)\nsecond line"
	got, err := c.Run(context.Background(), map[string]string{"text": text, "id": "7"}, dir, "")
	if err != nil || got != "Opened in Terminal" {
		t.Fatalf("Run = %q, %v", got, err)
	}
	logged, _ := os.ReadFile(log)
	script := filepath.Join(dir, "run.command")
	if want := "-a\nTerminal\n" + script + "\n"; string(logged) != want {
		t.Fatalf("open got %q, want %q", logged, want)
	}
	cmd := exec.Command("/bin/sh", script)
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := text + "|7|"; string(b) != want {
		t.Fatalf("script printed %q, want %q", b, want)
	}
}
