// Package mcpserver is `loupe mcp`: a Model Context Protocol server over
// stdio that turns a running loupe server's HTTP API into tools. It holds no
// device logic of its own. Every tool is one or two requests to /api/*, so an
// agent gets schemas and discovery over what curl already reaches, and
// nothing from simcap or internal/target is reachable through it that the
// HTTP API does not already serve.
package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/httpapi"
)

// Client is the HTTP side: one loupe server, one token, and the claims this
// process holds.
type Client struct {
	base    *url.URL
	token   string
	agent   string
	session string
	http    *http.Client
	// noPush keeps Run from pushing flags, for tests that count requests.
	noPush bool

	mu     sync.Mutex
	claims map[string]bool
}

// NewClient builds a client for the server at rawURL. TLS is verified unless
// the host is loopback: the launchd server's certificate names its ts.net
// host only, so https://localhost would never verify, and skipping the check
// anywhere else would hand the bearer token to whatever answered.
func NewClient(rawURL, token, agent string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("server url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("server url %q: want https:// or http://", rawURL)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if isLoopback(u.Hostname()) {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{
		base:    u,
		token:   token,
		agent:   agent,
		session: newSessionID(),
		http:    &http.Client{Transport: tr},
		claims:  map[string]bool{},
	}, nil
}

// PlainHTTPAnswered reports whether err is a plain-HTTP server answering an
// https:// request to a loopback host. The clients default to
// https://localhost:18456, the launchd service's TLS listener; a server run
// without a certificate answers the same port in plain HTTP, and on loopback
// nothing is lost by following it. Off loopback the error stands, since
// dropping TLS there would send the token in the clear.
func PlainHTTPAnswered(base *url.URL, err error) bool {
	return base.Scheme == "https" && isLoopback(base.Hostname()) &&
		strings.Contains(err.Error(), "server gave HTTP response to HTTPS client")
}

// SetSession replaces the random session NewClient picks. The CLI sends a
// fixed one, or none, so that its claim outlives the process that took it:
// the next command, a new process, still holds it. With none the server keys
// claims by agent name alone.
func (c *Client) SetSession(s string) { c.session = s }

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newSessionID tells this process's claims apart from another session of the
// same agent on the same server.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// APIError is a non-2xx answer, with the server's JSON error when it sent one.
type APIError struct {
	Status int
	Body   httpapi.ErrorResponse
	Raw    string
}

func (e *APIError) Error() string {
	msg := e.Body.Error
	if msg == "" {
		msg = strings.TrimSpace(e.Raw)
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d: %s", e.Status, msg)
	if e.Body.Code != "" {
		fmt.Fprintf(&b, " (code %s", e.Body.Code)
		if e.Body.RetryAfterMs > 0 {
			fmt.Fprintf(&b, ", retry after %dms", e.Body.RetryAfterMs)
		}
		if c := e.Body.Claim; c != nil {
			fmt.Fprintf(&b, ", held by %s", c.Agent)
		}
		b.WriteString(")")
	}
	return b.String()
}

// do sends one request and returns the body of a 2xx answer, and its headers.
// Never X-Loupe-Viewer: that header makes a request the person's, which would
// walk past the human hold this process is meant to respect.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, contentType string, body io.Reader) ([]byte, http.Header, error) {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set(httpapi.HeaderAgent, c.agent)
	if c.session != "" {
		req.Header.Set(httpapi.HeaderSession, c.session)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if PlainHTTPAnswered(c.base, err) {
			// A loopback server without TLS, as `brew services` runs it.
			// Every later request goes to http:// too.
			c.base.Scheme = "http"
			if body == nil {
				return c.do(ctx, method, path, q, contentType, nil)
			}
			if sk, ok := body.(io.Seeker); ok {
				if _, serr := sk.Seek(0, io.SeekStart); serr == nil {
					return c.do(ctx, method, path, q, contentType, body)
				}
			}
		}
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode/100 != 2 {
		e := &APIError{Status: resp.StatusCode, Raw: string(b)}
		_ = json.Unmarshal(b, &e.Body)
		return nil, resp.Header, e
	}
	return b, resp.Header, nil
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values) (json.RawMessage, error) {
	b, _, err := c.do(ctx, http.MethodGet, path, q, "", nil)
	return b, err
}

func (c *Client) postJSON(ctx context.Context, path string, q url.Values, v any) (json.RawMessage, error) {
	var body io.Reader
	ct := ""
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	b, _, err := c.do(ctx, http.MethodPost, path, q, ct, body)
	return b, err
}

// holds reports whether this process has claimed target.
// is reports whether to names this process, the way the server keys
// callers: by session when there is one, else by agent.
func (c *Client) is(to httpapi.FlagRecipient) bool {
	if to.Session != "" || c.session != "" {
		return to.Session == c.session
	}
	return to.Agent == c.agent
}

func (c *Client) holds(target string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims[target]
}

// claim takes the target for this process before anything drives it. A claim
// already held is renewed by every call the server sees from this session, so
// it is posted once per target, and again only after the server says it has
// lapsed. A claim another agent holds, or a hands-off target, comes back as
// the server's 409 and nothing is sent to the device.
func (c *Client) claim(ctx context.Context, target string) error {
	c.mu.Lock()
	held := c.claims[target]
	c.mu.Unlock()
	if held {
		return nil
	}
	if _, err := c.postJSON(ctx, "/api/claims", url.Values{"target": {target}}, nil); err != nil {
		return err
	}
	c.mu.Lock()
	c.claims[target] = true
	c.mu.Unlock()
	return nil
}

// drive runs fn under this process's claim on target. A claim that lapsed
// after ten idle minutes reads as claimedBy or a 409 on the first call after,
// so a refusal drops the remembered claim and the next call claims again.
func (c *Client) drive(ctx context.Context, target string, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	if err := c.claim(ctx, target); err != nil {
		return nil, err
	}
	b, err := fn()
	if e, ok := err.(*APIError); ok && e.Status == http.StatusConflict && e.Body.Code == "claimedBy" {
		c.forget(target)
	}
	return b, err
}

func (c *Client) forget(target string) {
	c.mu.Lock()
	delete(c.claims, target)
	c.mu.Unlock()
}

// release gives back one claim this process holds. Never ?force=1: that is
// the person's Release in the picker, not an agent's.
func (c *Client) release(ctx context.Context, target string) error {
	_, _, err := c.do(ctx, http.MethodDelete, "/api/claims", url.Values{"target": {target}}, "", nil)
	c.forget(target)
	return err
}

// ReleaseAll gives back every claim this process holds, so a device is free
// for the next agent as soon as this one exits rather than ten minutes later.
func (c *Client) ReleaseAll(ctx context.Context) {
	c.mu.Lock()
	var ts []string
	for t := range c.claims {
		ts = append(ts, t)
	}
	c.mu.Unlock()
	for _, t := range ts {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_ = c.release(ctx, t)
		cancel()
	}
}
