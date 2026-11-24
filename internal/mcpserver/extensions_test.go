package mcpserver

import (
	"io"
	"net/http"
	"strings"
	"testing"
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

func TestFormatExtensionAnswerCutsALargeBodyAndHidesBinary(t *testing.T) {
	out := formatExtensionAnswer(200, "text/plain", []byte(strings.Repeat("a", maxExtensionBody+10)))
	if !strings.Contains(out, "[truncated: showing 65536 of 65546 bytes]") {
		t.Errorf("no truncation note: %q", out[len(out)-60:])
	}
	if out := formatExtensionAnswer(200, "image/png", []byte{0xff, 0xfe, 0x00}); !strings.Contains(out, "3 bytes of non-text content") {
		t.Errorf("binary: %q", out)
	}
}
