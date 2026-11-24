package mcpserver

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListExtensionsPassesThroughTheServersList(t *testing.T) {
	const list = `{"extensions":[{"name":"slipway","label":"Slipway","path":"/api/ext/slipway/","page":"/api/ext/slipway/"}]}`
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		io.WriteString(w, list)
		return true
	}}
	cs, stop := connect(t, fake)
	res := call(t, cs, "list_extensions", nil)
	stop()
	if res.IsError || text(res) != list {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(fake.requests(), "\n"); got != "GET /api/extensions" {
		t.Errorf("requests %q: a list takes no claim", got)
	}
}

func TestCallExtensionSendsMethodPathBodyAndToken(t *testing.T) {
	var body, ct string
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		b, _ := io.ReadAll(r.Body)
		body, ct = string(b), r.Header.Get("Content-Type")
		io.WriteString(w, `{"queued":true}`)
		return true
	}}
	cs, stop := connect(t, fake)
	res := call(t, cs, "call_extension", map[string]any{
		"name": "slipway", "method": "post", "path": "builds/run?limit=5", "body": `{"ref":"main"}`,
	})
	stop()
	if res.IsError {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(fake.requests(), "\n"); got != "POST /api/ext/slipway/builds/run?limit=5" {
		t.Errorf("requests %q: want one call, no claim", got)
	}
	if body != `{"ref":"main"}` || ct != "application/json" {
		t.Errorf("body %q content-type %q", body, ct)
	}
	if got := fake.heads[0].Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization %q", got)
	}
	if !strings.HasPrefix(text(res), "status: 200 OK\ncontent-type: ") || !strings.HasSuffix(text(res), `{"queued":true}`) {
		t.Errorf("text %q", text(res))
	}
}

func TestCallExtensionDefaultsToGetAtTheRoot(t *testing.T) {
	fake := &fakeLoupe{}
	cs, stop := connect(t, fake)
	call(t, cs, "call_extension", map[string]any{"name": "slipway"})
	stop()
	if got := strings.Join(fake.requests(), "\n"); got != "GET /api/ext/slipway/" {
		t.Errorf("requests %q", got)
	}
}

// docs/mcp.md says the path is decoded once and then checked, so one layer of
// encoding is a plain character, not a refusal.
func TestCallExtensionDecodesThePathOnce(t *testing.T) {
	fake := &fakeLoupe{}
	cs, stop := connect(t, fake)
	for _, p := range []string{"a%2Fb", "x%2Etxt"} {
		if res := call(t, cs, "call_extension", map[string]any{"name": "slipway", "path": p}); res.IsError {
			t.Errorf("%s: %s", p, text(res))
		}
	}
	stop()
	if got := strings.Join(fake.requests(), "\n"); got != "GET /api/ext/slipway/a/b\nGET /api/ext/slipway/x.txt" {
		t.Errorf("requests %q", got)
	}
}

func TestCallExtensionRefusesAnEscapeWithoutARequest(t *testing.T) {
	bad := []map[string]any{
		{"name": "slipway", "path": "../claims"},
		{"name": "slipway", "path": "a/../../claims"},
		{"name": "slipway", "path": "%2e%2e/claims"},
		{"name": "slipway", "path": "%2E%2E%2Fclaims"},
		{"name": "slipway", "path": "%252e%252e/claims"},
		{"name": "slipway", "path": "./x"},
		{"name": "slipway", "path": "//evil.example/x"},
		{"name": "slipway", "path": "https://evil.example/x"},
		{"name": "slipway", "path": `..\claims`},
		{"name": "../targets", "path": "x"},
		{"name": "Slipway", "path": "x"},
		{"name": "", "path": "x"},
		{"name": "slipway", "path": "x", "method": "PATCH"},
		{"name": "slipway", "path": "x", "method": "CONNECT"},
	}
	fake := &fakeLoupe{}
	cs, stop := connect(t, fake)
	for _, args := range bad {
		if res := call(t, cs, "call_extension", args); !res.IsError {
			t.Errorf("%v: want a tool error, got %q", args, text(res))
		}
	}
	stop()
	if got := fake.requests(); len(got) != 0 {
		t.Errorf("requests %v, want none", got)
	}
}

func TestCallExtensionReturnsANon2xxNonJSONBody(t *testing.T) {
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, "short and stout")
		return true
	}}
	cs, stop := connect(t, fake)
	res := call(t, cs, "call_extension", map[string]any{"name": "slipway", "path": "brew"})
	stop()
	want := "status: 418 I'm a teapot\ncontent-type: text/plain\n\nshort and stout"
	if res.IsError || text(res) != want {
		t.Errorf("isError %v text %q, want %q", res.IsError, text(res), want)
	}
}

// The proxy passes an extension's redirect through unchanged, and a Location
// that starts with / names a path on loupe, outside /api/ext/{name}/. Following
// it would send the session's token there and report loupe's answer as the
// extension's.
func TestCallExtensionReturnsARedirectWithoutFollowingIt(t *testing.T) {
	for _, tc := range []struct {
		method string
		status int
	}{
		{"GET", http.StatusFound},
		{"POST", http.StatusTemporaryRedirect},
	} {
		fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasPrefix(r.URL.Path, "/api/ext/") {
				return false
			}
			w.Header().Set("Location", "/api/claims?target=avd:P")
			w.WriteHeader(tc.status)
			return true
		}}
		cs, stop := connect(t, fake)
		res := call(t, cs, "call_extension", map[string]any{"name": "slipway", "method": tc.method, "path": "old", "body": `{"a":1}`})
		stop()
		if got := strings.Join(fake.requests(), "\n"); got != tc.method+" /api/ext/slipway/old" {
			t.Errorf("%s: requests %q, want only the extension call", tc.method, got)
		}
		want := fmt.Sprintf("status: %d %s\n", tc.status, http.StatusText(tc.status))
		if res.IsError || !strings.HasPrefix(text(res), want) || !strings.Contains(text(res), "location: /api/claims?target=avd:P\n") {
			t.Errorf("%s: isError %v text %q, want %q and the location", tc.method, res.IsError, text(res), want)
		}
	}
}

// call_extension reads one byte past the cut and no more, so a large download
// or a stream that never ends comes back at once instead of being read whole.
func TestCallExtensionStopsReadingPastTheCut(t *testing.T) {
	gone := make(chan bool, 1)
	fake := &fakeLoupe{handle: func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.Repeat("a", 2*maxExtensionBody))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			gone <- true
		case <-time.After(10 * time.Second):
			gone <- false
		}
		return true
	}}
	cs, stop := connect(t, fake)
	res := call(t, cs, "call_extension", map[string]any{"name": "slipway", "path": "events"})
	stop()
	if !<-gone {
		t.Error("the stream stayed open until the server gave up")
	}
	if res.IsError || !strings.HasSuffix(text(res), "[truncated: showing the first 65536 bytes; the rest was not read]") {
		t.Errorf("isError %v, tail %q", res.IsError, text(res)[max(0, len(text(res))-80):])
	}
}

// The CLI's exit status reads the status back off the answer.
func TestExtensionAnswerStatusReadsTheFirstLine(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   int
		ok     bool
	}{
		{formatExtensionAnswer(404, http.Header{"Content-Type": {"application/json"}}, []byte(`{"error":"no extension x"}`)), 404, true},
		{formatExtensionAnswer(302, http.Header{"Location": {"/api/claims"}}, nil), 302, true},
		{formatExtensionAnswer(200, http.Header{}, []byte("status: 500")), 200, true},
		{`bad extension name "X": lowercase letters, digits and dashes, 1-32 characters`, 0, false},
	} {
		if got, ok := ExtensionAnswerStatus(tc.answer); got != tc.want || ok != tc.ok {
			t.Errorf("%q: got %d %v, want %d %v", tc.answer, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFormatExtensionAnswerCutsALargeBodyAndHidesBinary(t *testing.T) {
	big := []byte(strings.Repeat("a", maxExtensionBody+1))
	out := formatExtensionAnswer(200, http.Header{"Content-Type": {"text/plain"}, "Content-Length": {"65546"}}, big)
	if !strings.HasSuffix(out, "\n[truncated: showing 65536 of 65546 bytes]") {
		t.Errorf("no truncation note: %q", out[len(out)-60:])
	}
	// A two-byte rune split by the cut is still text.
	split := []byte(strings.Repeat("a", maxExtensionBody-1) + "é")
	if out := formatExtensionAnswer(200, http.Header{"Content-Type": {"text/plain"}}, split); !strings.HasSuffix(out, "\n[truncated: showing the first 65535 bytes; the rest was not read]") {
		t.Errorf("split rune: %q", out[len(out)-80:])
	}
	if out := formatExtensionAnswer(200, http.Header{"Content-Type": {"image/png"}}, []byte{0xff, 0xfe, 0x00}); !strings.Contains(out, "3 bytes of non-text content") {
		t.Errorf("binary: %q", out)
	}
}
