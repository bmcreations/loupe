package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A loupe run without TLS answers https://localhost:18456 in plain HTTP;
// the client follows it there, a body included.
func TestClientFallsBackToPlainHTTPOnLoopback(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c, err := NewClient(strings.Replace(srv.URL, "http://", "https://", 1), "tok", "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.postJSON(ctx, "/api/claims", url.Values{"target": {"x"}}, map[string]string{"a": "b"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := c.getJSON(ctx, "/api/targets", nil); err != nil {
		t.Fatalf("get: %v", err)
	}
	if c.base.Scheme != "http" || len(got) != 2 {
		t.Errorf("scheme %s, requests %v", c.base.Scheme, got)
	}
}

func TestPlainHTTPAnsweredOnlyOnLoopback(t *testing.T) {
	e := errors.New(`Get "https://x/": http: server gave HTTP response to HTTPS client`)
	for raw, want := range map[string]bool{
		"https://localhost:18456":  true,
		"https://127.0.0.1:18456":  true,
		"https://mac.ts.net:18456": false,
		"http://localhost:18456":   false,
	} {
		u, _ := url.Parse(raw)
		if got := PlainHTTPAnswered(u, e); got != want {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
}
