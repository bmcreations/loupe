package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bmcreations/loupe/internal/trace"
)

func TestRedactQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"target=x", "target=x"},
		{"token=secret", "token=REDACTED"},
		{"target=x&token=secret&proto=2", "target=x&token=REDACTED&proto=2"},
		// Parsed the way the server reads it, so an escaped or repeated
		// token cannot slip past.
		{"tok%65n=secret", "token=REDACTED"},
		{"token=a&token=b", "token=REDACTED&token=REDACTED"},
		{"token=secret;x=1", "token=REDACTED"},
		// A stream ticket and a pairing code are credentials too.
		{"target=x&ticket=secret", "target=x&ticket=REDACTED"},
		{"code=secret&target=x", "code=REDACTED&target=x"},
	}
	for _, c := range cases {
		got := redactQuery(c.in)
		if strings.Contains(got, "secret") {
			t.Errorf("redactQuery(%q) = %q, still holds the token", c.in, got)
		}
		if got != c.want {
			t.Errorf("redactQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestHTTPTraceHoldsNoToken: the request trace is a file on disk that outlives
// the run, and the first page load of every phone carries ?token=.
func TestHTTPTraceHoldsNoToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{trace: tw, web: fstest.MapFS{"x.css": {Data: []byte("x")}}}
	s.handleRoot(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x.css?token=s3cr3t&v=1", nil))
	_ = tw.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "s3cr3t") {
		t.Errorf("trace holds the token:\n%s", b)
	}
	if !strings.Contains(string(b), `"query":"token=REDACTED`) || !strings.Contains(string(b), `v=1"`) {
		t.Errorf("trace lost the rest of the query:\n%s", b)
	}
}

// A browser that already holds the cookie can still land on a ?token= URL --
// a Home Screen launch, or a page the strip redirect never saw -- and the
// startup probe posts location.href to /api/log, which goes to the trace.
func TestClientLogRedactsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{trace: tw}
	body := `{"href":"https://mini.ts.net:18456/?target=dev1&token=s3cr3t#peek","note":"plain"}`
	s.handleClientLog(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/log", strings.NewReader(body)))
	_ = tw.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "s3cr3t") {
		t.Errorf("trace holds the token:\n%s", b)
	}
	if !strings.Contains(string(b), `"href":"https://mini.ts.net:18456/?target=dev1\u0026token=REDACTED#peek"`) {
		t.Errorf("trace lost the rest of href:\n%s", b)
	}
}

// TestRedactURLCoversTheFragment: a pairing link carries its code after #,
// and a client record's href holds the whole link.
func TestRedactURLCoversTheFragment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://h/pair?target=a#code=secret", "https://h/pair?target=a#code=REDACTED"},
		{"https://h/pair#code=secret", "https://h/pair#code=REDACTED"},
		{"https://h/?token=secret#x", "https://h/?token=REDACTED#x"},
		{"https://h/", "https://h/"},
	}
	for _, c := range cases {
		if got := redactURL(c.in); got != c.want {
			t.Errorf("redactURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
