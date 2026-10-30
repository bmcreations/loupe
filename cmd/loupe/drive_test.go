package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// driveServer records the one request a drive command sends.
func driveServer(t *testing.T) (*httptest.Server, *http.Request, *string) {
	t.Helper()
	var got http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, body = *r.Clone(r.Context()), string(b)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &body
}

func TestLoupeTapSendsAMatchOrAPoint(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-text", "Sign in"}, `{"action":"tap","match":{"text":"Sign in"}}`},
		{[]string{"-desc", "Back", "-index", "1"}, `{"action":"tap","match":{"desc":"Back","index":1}}`},
		{[]string{"0", "12"}, `{"action":"tap","x":0,"y":12}`},
	} {
		srv, req, body := driveServer(t)
		args := append([]string{"-url", srv.URL, "-token", "t", "-target", "emulator-5554", "-session", "s1"}, tc.args...)
		if code := runTap(args); code != 0 {
			t.Fatalf("runTap(%v) = %d", tc.args, code)
		}
		if *body != tc.want || req.URL.Path != "/api/input" || req.URL.Query().Get("target") != "emulator-5554" || req.Header.Get("X-Loupe-Session") != "s1" {
			t.Errorf("runTap(%v) sent %s %s session %q body %s; want %s", tc.args, req.URL.Path, req.URL.RawQuery, req.Header.Get("X-Loupe-Session"), *body, tc.want)
		}
	}
}

func TestLoupeTapNeedsAPointOrANode(t *testing.T) {
	for _, args := range [][]string{{}, {"1"}, {"-text", "a", "1", "2"}, {"x", "y"}} {
		if code := runTap(append([]string{"-token", "t", "-target", "d"}, args...)); code != 2 {
			t.Errorf("runTap(%v) = %d; want 2", args, code)
		}
	}
}

func TestLoupeSequenceSendsTheStepsAsWritten(t *testing.T) {
	srv, req, body := driveServer(t)
	f := filepath.Join(t.TempDir(), "steps.json")
	os.WriteFile(f, []byte(`[{"action":"tap","x":0,"y":0},{"action":"wait","ms":100}]`), 0o600)
	if code := runSequence([]string{"-url", srv.URL, "-token", "t", "-target", "d", f}); code != 0 {
		t.Fatalf("runSequence = %d", code)
	}
	want := `{"steps":[{"action":"tap","x":0,"y":0},{"action":"wait","ms":100}]}`
	if req.URL.Path != "/api/sequence" || *body != want {
		t.Fatalf("sent %s %s; want /api/sequence %s", req.URL.Path, *body, want)
	}
}
