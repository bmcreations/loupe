package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// fakeComposeTarget is a fakeInspectorTarget that also answers ComposeDump,
// with the acmechat fixture reply unless dumpErr is set.
type fakeComposeTarget struct {
	fakeInspectorTarget
	dumpErr error
	dumps   int
}

func (f *fakeComposeTarget) ComposeDump(ctx context.Context) (*target.ComposeDump, error) {
	f.dumps++
	if f.dumpErr != nil {
		return nil, f.dumpErr
	}
	reply, err := os.ReadFile("../composetree/testdata/acmechat-messages.pb")
	if err != nil {
		return nil, err
	}
	return &target.ComposeDump{Package: "com.example", Fetch: func(context.Context) ([][]byte, error) {
		return [][]byte{reply}, nil
	}}, nil
}

func messageRowUI() *target.UI {
	full := [4]int{0, 0, 1080, 2400}
	row := &target.Node{Class: "View", Clickable: true, Bounds: [4]int{53, 1674, 1027, 2125}}
	cv := &target.Node{Class: "ComposeView", Bounds: full, Children: []*target.Node{row}}
	return &target.UI{Width: 1080, Height: 2400, Root: &target.Node{
		Class: "FrameLayout", Bounds: full, Children: []*target.Node{cv},
	}}
}

func getUI(t *testing.T, ft target.Target, query string) (*httptest.ResponseRecorder, UIResponse) {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(ft)
	s := &Server{mgr: m}
	w := httptest.NewRecorder()
	s.handleUI(w, httptest.NewRequest(http.MethodGet, "/api/ui?target=dev1"+query, nil))
	var resp UIResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
	}
	return w, resp
}

func TestUIComposeAnnotates(t *testing.T) {
	ft := &fakeComposeTarget{fakeInspectorTarget: fakeInspectorTarget{id: "dev1", ui: messageRowUI()}}
	w, resp := getUI(t, ft, "&compose=1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"composable":"MessageRow"`) {
		t.Fatalf("body has no MessageRow: %s", w.Body.String())
	}
	if len(resp.Composables) == 0 {
		t.Fatal("composables is empty")
	}
	if resp.ComposeError != "" {
		t.Fatalf("composeError = %q", resp.ComposeError)
	}
}

// interactive=1 prunes the tree, so the names must be set before pruning or
// the pruned copy carries none.
func TestUIComposeAnnotatesThePrunedTree(t *testing.T) {
	ft := &fakeComposeTarget{fakeInspectorTarget: fakeInspectorTarget{id: "dev1", ui: messageRowUI()}}
	w, _ := getUI(t, ft, "&compose=1&interactive=1")
	if !strings.Contains(w.Body.String(), `"composable":"MessageRow"`) {
		t.Fatalf("pruned body has no MessageRow: %s", w.Body.String())
	}
}

func TestUIComposeErrorKeepsTheTree(t *testing.T) {
	ft := &fakeComposeTarget{
		fakeInspectorTarget: fakeInspectorTarget{id: "dev1", ui: messageRowUI()},
		dumpErr:             fmt.Errorf("com.example: %w", target.ErrNotDebuggable),
	}
	w, resp := getUI(t, ft, "&compose=1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(resp.Nodes) == 0 {
		t.Fatal("nodes missing")
	}
	if !strings.Contains(resp.ComposeError, "not debuggable") {
		t.Fatalf("composeError = %q", resp.ComposeError)
	}
}

func TestUIComposeOnATargetWithoutAnInspector(t *testing.T) {
	ft := &fakeInspectorTarget{id: "dev1", ui: messageRowUI()}
	w, resp := getUI(t, ft, "&compose=1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if resp.ComposeError != "target has no compose inspector" {
		t.Fatalf("composeError = %q", resp.ComposeError)
	}
}

func TestUIWithoutComposeNeverAttaches(t *testing.T) {
	ft := &fakeComposeTarget{fakeInspectorTarget: fakeInspectorTarget{id: "dev1", ui: messageRowUI()}}
	w, _ := getUI(t, ft, "")
	if ft.dumps != 0 {
		t.Fatalf("ComposeDump called %d times without compose=1", ft.dumps)
	}
	if strings.Contains(w.Body.String(), "compos") {
		t.Fatalf("body mentions compose without compose=1: %s", w.Body.String())
	}
}
