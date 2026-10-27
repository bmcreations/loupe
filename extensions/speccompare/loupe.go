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

// ui reads the full tree, not ?interactive=1: a region is as often a label
// or an image as something tappable. A target with no tree (501) gives nil.
func (c *loupeClient) ui(target string, from http.Header) ([]byte, error) {
	b, err := c.do("GET", "/api/ui", url.Values{"target": {target}}, nil, from)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusNotImplemented {
		return nil, nil
	}
	return b, err
}

// overlay is the subset of POST /api/overlays's body this extension sends.
type overlay struct {
	ID      string       `json:"id"`
	Source  string       `json:"source"`
	Label   string       `json:"label,omitempty"`
	Image   string       `json:"image,omitempty"`
	Screen  *[2]int      `json:"screen,omitempty"`
	Bounds  *Rect        `json:"bounds,omitempty"`
	Opacity *float64     `json:"opacity,omitempty"`
	Boxes   []overlayBox `json:"boxes,omitempty"`
}

type overlayBox struct {
	Bounds Rect   `json:"bounds"`
	Label  string `json:"label,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

func (c *loupeClient) postOverlay(target string, o overlay, from http.Header) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = c.do("POST", "/api/overlays", url.Values{"target": {target}}, b, from)
	return err
}

func (c *loupeClient) clearOverlays(target, source string, from http.Header) error {
	_, err := c.do("DELETE", "/api/overlays", url.Values{"target": {target}, "source": {source}}, nil, from)
	return err
}
