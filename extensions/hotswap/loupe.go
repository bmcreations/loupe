package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// loupeClient is the extension's only way into loupe: HTTP with the shared
// token. The caller's agent and session headers go through, so a claim the
// agent holds stays theirs.
type loupeClient struct {
	base, token string
	hc          *http.Client
}

func newLoupeClient(base, token string) *loupeClient {
	tr := &http.Transport{}
	if u, err := url.Parse(base); err == nil {
		h := u.Hostname()
		if h == "localhost" || net.ParseIP(h).IsLoopback() {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
	}
	return &loupeClient{base: base, token: token, hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

var passHeaders = []string{"X-Loupe-Agent", "X-Loupe-Session"}

func (c *loupeClient) get(path string, q url.Values, from http.Header) ([]byte, error) {
	req, err := http.NewRequest("GET", c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for _, h := range passHeaders {
		if v := from.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, &statusError{path, resp.StatusCode, string(bytes.TrimSpace(b))}
	}
	return b, nil
}

type statusError struct {
	path   string
	status int
	body   string
}

func (e *statusError) Error() string { return fmt.Sprintf("%s: %d %s", e.path, e.status, e.body) }

var pngSig = []byte("\x89PNG\r\n\x1a\n")

func (c *loupeClient) screenshot(target string, from http.Header) ([]byte, error) {
	b, err := c.get("/api/screenshot", url.Values{"target": {target}}, from)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, pngSig) {
		return nil, fmt.Errorf("/api/screenshot: not a PNG")
	}
	return b, nil
}

func (c *loupeClient) ui(target string, from http.Header) (json.RawMessage, error) {
	b, err := c.get("/api/ui", url.Values{"target": {target}, "interactive": {"1"}}, from)
	// A physical iPhone has no tree to read: 501 is an absent tree, not a failed readback.
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusNotImplemented {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("/api/ui: not JSON")
	}
	return b, nil
}
