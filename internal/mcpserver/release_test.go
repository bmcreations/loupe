package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// blockFlagsWait makes fake hold every flags call that waits until unblock
// is closed, as the server does while no flag comes.
func blockFlagsWait(fake *fakeLoupe, unblock <-chan struct{}) {
	fake.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/flags" && r.URL.Query().Get("waitMs") != "" {
			select {
			case <-unblock:
			case <-r.Context().Done():
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"flags":[],"rev":0}`))
			return true
		}
		return false
	}
}

func seen(fake *fakeLoupe, prefix string) bool {
	for _, r := range fake.requests() {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A signal ends ctx while a flags call waits. The claim has to go back then,
// not when the wait ends: whoever sent the signal may kill the process first,
// and the claim would sit out its lease.
func TestEndingRunReleasesClaimsWithoutWaitingForAFlagsWait(t *testing.T) {
	fake := &fakeLoupe{}
	unblock := make(chan struct{})
	blockFlagsWait(fake, unblock)
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	c, err := NewClient(ts.URL, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	c.noPush = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	// Cleanups run last first: the held call is let go before cs.Close and
	// ts.Close, which both wait for it.
	t.Cleanup(func() { close(unblock) })

	call(t, cs, "tap", map[string]any{"target": "emulator-5554", "x": 1, "y": 2})
	go cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "flags",
		Arguments: map[string]any{"target": "emulator-5554", "wait_seconds": 60}})
	waitFor(t, "the flags wait", func() bool { return seen(fake, "GET /api/flags?") })

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run waited for the flags call before releasing")
	}
	if !seen(fake, "DELETE /api/claims?target=emulator-5554") {
		t.Errorf("claim not released; requests: %v", fake.requests())
	}
}

func TestNoClaimIsTakenAfterReleaseAll(t *testing.T) {
	fake := &fakeLoupe{}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	c, err := NewClient(ts.URL, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	c.ReleaseAll(context.Background())
	ran := false
	_, err = c.drive(context.Background(), "emulator-5554", func() (json.RawMessage, error) { ran = true; return nil, nil })
	if !errors.Is(err, errClosing) {
		t.Errorf("drive after ReleaseAll = %v, want errClosing", err)
	}
	if ran || seen(fake, "POST /api/claims") {
		t.Errorf("drive after ReleaseAll claimed or drove; requests: %v", fake.requests())
	}
}

// ReleaseAll can run while a claim is on its way to the server, before the
// client has noted it. That claim goes back as soon as it lands.
func TestAClaimInFlightDuringReleaseAllIsGivenBack(t *testing.T) {
	fake := &fakeLoupe{}
	var c *Client
	fake.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/api/claims" {
			c.ReleaseAll(context.Background())
		}
		return false
	}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	var err error
	c, err = NewClient(ts.URL, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	_, err = c.drive(context.Background(), "emulator-5554", func() (json.RawMessage, error) { ran = true; return nil, nil })
	if !errors.Is(err, errClosing) {
		t.Errorf("drive = %v, want errClosing", err)
	}
	if ran {
		t.Error("drove the target after ReleaseAll")
	}
	got := fake.requests()
	if len(got) != 2 || !strings.HasPrefix(got[0], "POST /api/claims?target=emulator-5554") ||
		!strings.HasPrefix(got[1], "DELETE /api/claims?target=emulator-5554") {
		t.Errorf("requests = %v, want the claim then its release", got)
	}
}
