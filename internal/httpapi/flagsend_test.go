package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/config"
	"github.com/bmcreations/loupe/internal/t3"
)

// t3Server is a flag server whose T3 CLI is a script that records each call
// in dir/calls and answers a thread list with one open thread and one
// settled one.
func t3Server(t *testing.T) (*Server, string) {
	t.Helper()
	s, _ := newFlagServer(t)
	dir := t.TempDir()
	script := `#!/bin/sh
echo "$@" >> ` + dir + `/calls
case "$2" in
list) echo '[{"threadId":"469bab44-1f43-4536-b0f6-3349d0c7092f","title":"Fix login","status":"idle","providerInstanceId":"codex","updatedAt":"2026-10-29T01:00:00Z"},{"threadId":"d5b40970-48a1-4086-9552-eb534e09d3de","title":"Old","status":"completed","settled":true,"updatedAt":"2026-10-28T01:00:00Z"}]' ;;
send) cat > ` + dir + `/sent ;;
start) cat > ` + dir + `/sent; echo '{"threadId":"9cfd27bc-1c9e-46f4-be88-3144e57d7707"}' ;;
esac
`
	p := filepath.Join(dir, "t3")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s.findT3 = func() (t3.CLI, error) { return t3.CLI{Path: p}, nil }
	s.flagDir = filepath.Join(dir, "flags")
	cfg := config.Default()
	cfg.T3Projects = "/work/app, /work/web"
	s.cfg = NewConfigStore(filepath.Join(dir, "config.json"), cfg, nil, cfg, func(config.Config) {})
	return s, dir
}

// delivered waits for flag id's delivery to settle.
func delivered(t *testing.T, s *Server, id uint64) FlagDelivery {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f := s.flagView(id); f != nil && f.Delivery != nil && f.Delivery.State != "sending" {
			return *f.Delivery
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delivery did not settle")
	return FlagDelivery{}
}

func TestDestinationsListEachProjectsOpenThreads(t *testing.T) {
	s, _ := t3Server(t)
	w := serveOn(s, http.MethodGet, "/api/flags/destinations", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{`"path":"/work/app"`, `"name":"web"`, `"title":"Fix login"`, `"provider":"codex"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
	// A settled thread is listed after the newer open one.
	if i, j := strings.Index(body, `"Fix login"`), strings.Index(body, `"Old"`); j < 0 || j < i {
		t.Errorf("the settled thread should follow the open one: %s", body)
	}
}

func TestDestinationsAreEmptyWithNoProjects(t *testing.T) {
	s, _ := newFlagServer(t)
	w := serveOn(s, http.MethodGet, "/api/flags/destinations", "", nil)
	if strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("body = %s, want {}", w.Body)
	}
}

func TestAFlagSentToAT3ThreadGoesAsAMessageThatStandsAlone(t *testing.T) {
	s, dir := t3Server(t)
	w := serveOn(s, http.MethodPost, "/api/flags?target=dev1",
		`{"note":"badge clipped","region":[0.1,0.25,0.5,0.5],"to":{"t3Thread":"469bab44-1f43-4536-b0f6-3349d0c7092f"}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	d := delivered(t, s, 1)
	if d.State != "sent" || d.Thread != "469bab44-1f43-4536-b0f6-3349d0c7092f" {
		t.Fatalf("delivery = %+v", d)
	}
	msg, _ := os.ReadFile(filepath.Join(dir, "sent"))
	if !strings.HasPrefix(string(msg), "> badge clipped") {
		t.Errorf("the note should lead the message:\n%s", msg)
	}
	shot := filepath.Join(d.Dir, "start.png")
	for _, want := range []string{"> badge clipped\n", "10,50, 40x50 device pixels", "![Flag 1](<" + shot + ">)", "[Screen when flagged](<" + shot + ">)", "[UI tree](<" + filepath.Join(d.Dir, "ui.json") + ">)"} {
		if !strings.Contains(string(msg), want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if _, err := os.Stat(shot); err != nil {
		t.Errorf("screenshot not written: %v", err)
	}
}

func TestAFlagSentToAT3ProjectStartsAThread(t *testing.T) {
	s, dir := t3Server(t)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"note":"crash on send","to":{"t3Project":"/work/app"}}`, nil)
	d := delivered(t, s, 1)
	if d.State != "sent" || d.Thread != "9cfd27bc-1c9e-46f4-be88-3144e57d7707" {
		t.Fatalf("delivery = %+v", d)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "thread start --json --title Loupe: crash on send /work/app -") {
		t.Errorf("calls = %s", calls)
	}
}

func TestAT3ProjectMustBeConfigured(t *testing.T) {
	s, _ := t3Server(t)
	w := serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"to":{"t3Project":"/etc"}}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	w = serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"to":{"t3Thread":"--wait"}}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("thread --wait: status = %d, want 400", w.Code)
	}
}

func TestAWatchGoesToT3OnlyWhenItEnds(t *testing.T) {
	s, dir := t3Server(t)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"watch":true,"to":{"t3Thread":"469bab44-1f43-4536-b0f6-3349d0c7092f"}}`, nil)
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "sent")); err == nil {
		t.Fatal("sent before the watch ended")
	}
	serveOn(s, http.MethodPost, "/api/flags/end?id=1", "", nil)
	if d := delivered(t, s, 1); d.State != "sent" {
		t.Fatalf("delivery = %+v", d)
	}
	msg, _ := os.ReadFile(filepath.Join(dir, "sent"))
	if !strings.Contains(string(msg), "end.png") {
		t.Errorf("message lacks the end screen:\n%s", msg)
	}
}

func TestAnInterruptSentToT3StillStopsTheHolder(t *testing.T) {
	s, _ := t3Server(t)
	req := httptest.NewRequest(http.MethodPost, "/api/input?target=dev1", nil)
	serveOn(s, http.MethodPost, "/api/flags?target=dev1", `{"kind":"interrupt"}`, nil)
	serveOn(s, http.MethodPost, "/api/flags/end?id=1", `{"to":{"t3Thread":"469bab44-1f43-4536-b0f6-3349d0c7092f"}}`, nil)
	if r := s.mayAct(req, "dev1", "tap", inputCaller{agent: "claude", session: "a"}); r == nil || r.Code != ErrCodeInterrupted {
		t.Errorf("tap = %+v; want interrupted: whoever drove must still re-check the screen", r)
	}
	if d := delivered(t, s, 1); d.State != "sent" {
		t.Errorf("delivery = %+v", d)
	}
}

func TestPruneKeepsTheNewestFlagDirs(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"300-3", "100-1", "200-2"} {
		os.Mkdir(filepath.Join(root, n), 0o700)
	}
	pruneFlagDirs(root, 2)
	if _, err := os.Stat(filepath.Join(root, "100-1")); err == nil {
		t.Error("oldest kept")
	}
	if _, err := os.Stat(filepath.Join(root, "300-3")); err != nil {
		t.Error("newest removed")
	}
}
