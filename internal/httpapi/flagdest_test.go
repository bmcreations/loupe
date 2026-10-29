package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/dest"
)

// destServer is a flag server with a destinations.json holding one command,
// a script that records its arguments and stdin in dir, and one webhook.
func destServer(t *testing.T, hookURL string) (*Server, string) {
	t.Helper()
	s, _ := newFlagServer(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "agent")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+dir+"/args\ncat > "+dir+"/stdin\necho started\n"), 0o755)
	f := dest.File{
		Commands: []dest.Command{{Name: "fix", Label: "Fix it", Program: script, Args: []string{"-p", "{start}", "--id={id}"}}},
		Webhooks: []dest.Webhook{{Name: "hook", URL: hookURL, Headers: map[string]string{"Authorization": "Bearer k"}}},
	}
	b, _ := json.Marshal(f)
	s.destPath = filepath.Join(dir, "destinations.json")
	if err := os.WriteFile(s.destPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.flagDir = filepath.Join(dir, "flags")
	return s, dir
}

func TestDestinationsListCommandsAndWebhooksByNameOnly(t *testing.T) {
	s, dir := destServer(t, "https://example.com/secret-path")
	body := serveOn(s, http.MethodGet, "/api/flags/destinations", "", nil).Body.String()
	for _, want := range []string{`"name":"fix","label":"Fix it"`, `"name":"hook","label":"hook"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
	for _, leak := range []string{dir, "secret-path", "Bearer", "{start}"} {
		if strings.Contains(body, leak) {
			t.Errorf("listing leaks %q: %s", leak, body)
		}
	}
}

func TestAFlagSentToACommandRunsItWithTheMessageOnStdin(t *testing.T) {
	s, dir := destServer(t, "https://example.com/x")
	w := serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"note":"badge clipped","to":{"command":"fix"}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	d := delivered(t, s, 1)
	if d.State != "sent" || d.Output != "started" {
		t.Fatalf("delivery = %+v", d)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if want := "-p " + filepath.Join(d.Dir, "start.png") + " --id=1\n"; string(args) != want {
		t.Errorf("args = %q, want %q", args, want)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !strings.HasPrefix(string(stdin), "> badge clipped") || strings.Contains(string(stdin), "Start your reply") {
		t.Errorf("stdin:\n%s", stdin)
	}
	if b, _ := os.ReadFile(filepath.Join(d.Dir, "message.md")); string(b) != string(stdin) {
		t.Errorf("message.md differs from stdin")
	}
}

func TestAFlagSentToAWebhookCarriesTheScreenshot(t *testing.T) {
	var auth string
	var got WebhookFlag
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
	}))
	defer hook.Close()
	s, _ := destServer(t, hook.URL)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"note":"off","to":{"webhook":"hook"}}`, nil)
	if d := delivered(t, s, 1); d.State != "sent" {
		t.Fatalf("delivery = %+v", d)
	}
	if auth != "Bearer k" || got.Flag.ID != 1 || len(got.Start) == 0 || !strings.Contains(got.Message, "> off") {
		t.Fatalf("auth %q, body %+v", auth, got.Flag)
	}
}

func TestAnUnknownDestinationIsRefused(t *testing.T) {
	s, _ := destServer(t, "https://example.com/x")
	for _, to := range []string{`{"command":"nope"}`, `{"webhook":"fix"}`, `{"command":"fix","webhook":"hook"}`, `{"command":"fix","agent":"claude"}`} {
		if w := serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"to":`+to+`}`, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", to, w.Code)
		}
	}
}
