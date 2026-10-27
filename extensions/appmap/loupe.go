package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// loupeClient is the extension's only way into loupe: HTTP with the shared
// token. The caller's agent and session headers go through, so a claim the
// agent holds stays theirs. Same shape as hotswap's; extensions share no code.
type loupeClient struct {
	base, token string
	hc          *http.Client
}

func newLoupeClient(base, token string) *loupeClient {
	tr := &http.Transport{}
	if u, err := url.Parse(base); err == nil {
		h := u.Hostname()
		if h == "localhost" || net.ParseIP(h).IsLoopback() || strings.HasSuffix(h, ".ts.net") {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
	}
	return &loupeClient{base: base, token: token, hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

var passHeaders = []string{"X-Loupe-Agent", "X-Loupe-Session"}

func (c *loupeClient) do(method, path string, q url.Values, body []byte, from http.Header) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path+"?"+q.Encode(), rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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
	b, err := c.do("GET", "/api/screenshot", url.Values{"target": {target}}, nil, from)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, pngSig) {
		return nil, fmt.Errorf("/api/screenshot: not a PNG")
	}
	return b, nil
}

// ui reads the full tree. With compose it asks for composable names, and
// falls back to the plain tree when that fails: compose=1 only works on a
// debuggable Android app, and a map shouldn't stop on a release build.
func (c *loupeClient) ui(target string, compose bool, from http.Header) ([]byte, error) {
	if compose {
		if b, err := c.do("GET", "/api/ui", url.Values{"target": {target}, "compose": {"1"}}, nil, from); err == nil {
			return b, nil
		}
	}
	return c.do("GET", "/api/ui", url.Values{"target": {target}}, nil, from)
}

// input sends one /api/input body as given. loupe checks the claim and the
// human hold, so a held target refuses the step here with loupe's own code.
func (c *loupeClient) input(target string, body map[string]any, from http.Header) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = c.do("POST", "/api/input", url.Values{"target": {target}}, b, from)
	return err
}

// register tells loupe where this extension listens, so clients can read
// maps through /api/ext/appmap/ with loupe's token alone.
func (c *loupeClient) register(listen string) error {
	body, _ := json.Marshal(map[string]string{"name": "appmap", "url": "http://" + listen, "label": "App map"})
	_, err := c.do(http.MethodPost, "/api/extensions", nil, body, nil)
	return err
}

// keepRegistered registers now and then every 30s, inside loupe's 90s
// expiry, so a loupe restart or a missed renewal heals on its own.
func (c *loupeClient) keepRegistered(listen string, logf func(string, ...any)) {
	failing := false
	for {
		if err := c.register(listen); err != nil {
			if !failing {
				logf("appmap: registering with loupe: %v", err)
			}
			failing = true
		} else if failing {
			logf("appmap: registered with loupe")
			failing = false
		}
		time.Sleep(30 * time.Second)
	}
}
