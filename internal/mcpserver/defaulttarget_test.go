package mcpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// targetsFake answers /api/targets with rows and every other route with ok.
func targetsFake(rows string) *fakeLoupe {
	return &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/targets" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, rows)
		return true
	}}
}

// uiTarget is the ?target= the ui tool sent, or "" if it sent no /api/ui.
func uiTarget(f *fakeLoupe) string {
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "GET /api/ui?") {
			i := strings.Index(r, "target=")
			if i < 0 {
				return ""
			}
			v := r[i+len("target="):]
			if j := strings.IndexByte(v, '&'); j >= 0 {
				v = v[:j]
			}
			if u, err := url.QueryUnescape(v); err == nil {
				v = u
			}
			return v
		}
	}
	return ""
}

func TestMissingTargetDrivesTheOneRunningDevice(t *testing.T) {
	f := targetsFake(`[{"id":"SM02","state":"device","available":true},
		{"id":"avd:Pixel_9","available":true},
		{"id":"ios:0000","state":"missing","available":false}]`)
	cs, stop := connect(t, f)
	defer stop()
	res := call(t, cs, "ui", map[string]any{"interactive": true})
	if res.IsError {
		t.Fatalf("ui: %s", text(res))
	}
	if got := uiTarget(f); got != "SM02" {
		t.Fatalf("ui drove %q, want SM02; requests %v", got, f.requests())
	}
}

func TestMissingTargetWithSeveralRunningSaysSo(t *testing.T) {
	f := targetsFake(`[{"id":"SM02","state":"device","available":true},
		{"id":"sim:A","state":"device","available":true}]`)
	cs, stop := connect(t, f)
	defer stop()
	res := call(t, cs, "ui", nil)
	if !res.IsError || !strings.Contains(text(res), "2 devices are running") {
		t.Fatalf("want an error naming 2 running devices, got error=%v %q", res.IsError, text(res))
	}
	if got := uiTarget(f); got != "" {
		t.Fatalf("ui reached the server with target %q", got)
	}
}

func TestGivenTargetIsLeftAlone(t *testing.T) {
	f := targetsFake(`[]`)
	cs, stop := connect(t, f)
	defer stop()
	call(t, cs, "ui", map[string]any{"target": "sim:B"})
	if got := uiTarget(f); got != "sim:B" {
		t.Fatalf("ui drove %q, want sim:B", got)
	}
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "GET /api/targets") {
			t.Fatalf("listed targets for a call that named one: %v", f.requests())
		}
	}
}

func TestFlagTargetWinsWithoutListing(t *testing.T) {
	f := targetsFake(`[{"id":"SM02","state":"device","available":true},
		{"id":"sim:A","state":"device","available":true}]`)
	ts := httptest.NewServer(f)
	defer ts.Close()
	c, err := NewClient(ts.URL, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	c.noPush = true
	c.SetDefaultTarget("sim:A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	call(t, cs, "ui", nil)
	cs.Close()
	cancel()
	<-done
	if got := uiTarget(f); got != "sim:A" {
		t.Fatalf("ui drove %q, want sim:A", got)
	}
}

func TestTargetsToolGetsNoDefault(t *testing.T) {
	f := targetsFake(`[{"id":"SM02","state":"device","available":true}]`)
	cs, stop := connect(t, f)
	defer stop()
	if res := call(t, cs, "flags", nil); res.IsError {
		t.Fatalf("flags: %s", text(res))
	}
	for _, r := range f.requests() {
		if strings.Contains(r, "target=SM02") {
			t.Fatalf("flags got a default target: %v", f.requests())
		}
	}
}
