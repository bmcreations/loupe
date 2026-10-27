package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

// urlTarget is a launchTarget that can also open a URL, with a canned
// answer.
type urlTarget struct {
	launchTarget
	openErr error
	opened  []string
}

func (f *urlTarget) OpenURL(ctx context.Context, u string) (*target.LaunchResult, error) {
	f.opened = append(f.opened, u)
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &target.LaunchResult{Activity: "com.android.chrome/.Main"}, nil
}

func (f *urlTarget) CheckURL(u string) error { return target.CheckURL(u) }

func newURLServer(t *testing.T, f *urlTarget) *Server {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(f)
	return &Server{mgr: m, token: "secret"}
}

func TestOpenURLActionOpensTheURL(t *testing.T) {
	f := &urlTarget{launchTarget: launchTarget{androidTarget: androidTarget{id: "dev1"}}}
	w := serveOn(newURLServer(t, f), http.MethodPost, "/api/input?target=dev1",
		`{"action":"openURL","url":"myapp://settings/profile"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(f.opened) != 1 || f.opened[0] != "myapp://settings/profile" {
		t.Errorf("opened %v", f.opened)
	}
}

func TestOpenURLActionNeedsAURL(t *testing.T) {
	f := &urlTarget{launchTarget: launchTarget{androidTarget: androidTarget{id: "dev1"}}}
	w := serveOn(newURLServer(t, f), http.MethodPost, "/api/input?target=dev1", `{"action":"openURL"}`, nil)
	if w.Code != http.StatusBadRequest || len(f.opened) != 0 {
		t.Fatalf("status = %d, opened %v; want 400 and nothing opened", w.Code, f.opened)
	}
}

func TestOpenURLActionErrorCodes(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: loupenope:…", target.ErrNoURLHandler), http.StatusUnprocessableEntity, ErrCodeNoURLHandler},
		{target.ErrDeviceUnreachable, http.StatusBadGateway, ""},
	} {
		f := &urlTarget{launchTarget: launchTarget{androidTarget: androidTarget{id: "dev1"}}, openErr: c.err}
		w := serveOn(newURLServer(t, f), http.MethodPost, "/api/input?target=dev1",
			`{"action":"openURL","url":"loupenope://x"}`, nil)
		var body ErrorResponse
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != c.status || body.Code != c.code {
			t.Errorf("%v: status %d code %q; want %d %q", c.err, w.Code, body.Code, c.status, c.code)
		}
	}
}

// A physical iPhone has no way to open a URL without naming the app, so a
// target without the capability says so rather than guessing one.
func TestOpenURLActionIs501WithoutTheCapability(t *testing.T) {
	f := &launchTarget{androidTarget: androidTarget{id: "dev1"}}
	w := serveOn(newLaunchServer(t, f), http.MethodPost, "/api/input?target=dev1",
		`{"action":"openURL","url":"https://example.com/"}`, nil)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501: %s", w.Code, w.Body)
	}
}

func TestOpenURLActionIsHeldWhileAPersonDrives(t *testing.T) {
	f := &urlTarget{launchTarget: launchTarget{androidTarget: androidTarget{id: "dev1"}}}
	s := newURLServer(t, f)
	sess, err := s.mgr.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	id, _ := sess.AddViewer()
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 0, PointerID: 1})
	_ = sess.ViewerTouch(id, session.TouchEvent{Action: 1, PointerID: 1})

	agent := map[string]string{HeaderAgent: "claude"}
	w := serveOn(s, http.MethodPost, "/api/input?target=dev1", `{"action":"openURL","url":"https://example.com/"}`, agent)
	if w.Code != http.StatusConflict || len(f.opened) != 0 {
		t.Fatalf("agent: status %d opened %v; want 409 and nothing opened", w.Code, f.opened)
	}
	// A URL that can never open is a 400 under the hold too, so the agent
	// does not retry it.
	w = serveOn(s, http.MethodPost, "/api/input?target=dev1", `{"action":"openURL","url":"x'; reboot"}`, agent)
	if w.Code != http.StatusBadRequest || len(f.opened) != 0 {
		t.Fatalf("bad url: status %d opened %v; want 400: %s", w.Code, f.opened, w.Body)
	}
}

// The trace names the scheme and host and never the path or query, which
// is where a deep link carries its token; so does the error.
func TestOpenURLTraceLeavesOutThePathAndQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, openErr := range []error{nil, fmt.Errorf("%w: https://example.com/…", target.ErrNoURLHandler)} {
		f := &urlTarget{launchTarget: launchTarget{androidTarget: androidTarget{id: "dev1"}}, openErr: openErr}
		s := newURLServer(t, f)
		s.trace = tw
		w := serveOn(s, http.MethodPost, "/api/input?target=dev1",
			`{"action":"openURL","url":"https://Example.com/reset?token=SECRET"}`, map[string]string{HeaderAgent: "claude"})
		if strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("response carries the query: %s", w.Body)
		}
	}
	tw.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "/reset") {
		t.Errorf("trace carries the path or query:\n%s", raw)
	}
	var found int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["action"] != "openURL" {
			continue
		}
		found++
		if rec["scheme"] != "https" || !strings.EqualFold(fmt.Sprint(rec["host"]), "example.com") || rec["urlLen"] == nil {
			t.Errorf("record = %v", rec)
		}
	}
	if found != 2 {
		t.Errorf("found %d openURL records; want 2:\n%s", found, raw)
	}
}
