package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrServerDown is the error for a peer that could not be reached: a refused
// or timed-out dial, or a connection that dropped mid-request.
var ErrServerDown = errors.New("serverDown")

// StatusError is a peer's own non-2xx answer, passed on with its body.
type StatusError struct {
	Status int
	Body   []byte
}

func (e *StatusError) Error() string {
	var er struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(e.Body, &er) == nil && er.Error != "" {
		return fmt.Sprintf("peer answered %d: %s", e.Status, er.Error)
	}
	return fmt.Sprintf("peer answered %d: %s", e.Status, http.StatusText(e.Status))
}

// requestTimeout bounds one call to a peer. It is a var so a test with a
// stopped owner does not wait on it.
var requestTimeout = 5 * time.Second

// Client calls one peer, sending its token as a bearer on every request.
type Client struct {
	srv Server
	hc  *http.Client
	// long shares hc's connections but has no client-wide timeout, for the
	// calls that act on a device: a screenshot or an iOS tap outlasts a
	// poll. Its context bounds it.
	long *http.Client
}

// NewClient returns a client for s, verifying the peer as TLSConfig does.
func NewClient(s Server) *Client {
	tr := &http.Transport{
		TLSClientConfig:     TLSConfig(peerHost(s.URL), s.Pin, nil),
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
	// A peer that redirects is not the peer that was pinned, and a
	// redirect would carry the token to wherever it points.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		srv:  s,
		hc:   &http.Client{Timeout: requestTimeout, Transport: tr, CheckRedirect: noRedirect},
		long: &http.Client{Transport: tr, CheckRedirect: noRedirect},
	}
}

// Close drops the client's idle connections.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// Do sends a request to path on the peer. A 2xx comes back as its body; any
// other status is a *StatusError, and a failed dial is ErrServerDown (or
// ErrCertChanged, when that is why).
func (c *Client) Do(ctx context.Context, method, path string) ([]byte, error) {
	r, err := c.send(c.hc, ctx, method, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return r.Body, nil
}

// Reply is a peer's answer to Call, whatever its status.
type Reply struct {
	Status      int
	Body        []byte
	ContentType string
	RetryAfter  string
}

// callMaxBody bounds a forwarded answer: a screenshot is a few megabytes.
const callMaxBody = 32 << 20

// Call sends header and body to path and returns the peer's answer with its
// status, not judged. Only a failed exchange is an error (ErrServerDown, or
// ErrCertChanged). ctx bounds it; there is no client-wide timeout, so the
// caller sets one.
func (c *Client) Call(ctx context.Context, method, path string, header http.Header, body []byte) (*Reply, error) {
	return c.send(c.long, ctx, method, path, header, body)
}

func (c *Client) send(hc *http.Client, ctx context.Context, method, path string, header http.Header, body []byte) (*Reply, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.srv.URL+path, rd)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.srv.Token)
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, ErrCertChanged) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrServerDown, err)
	}
	defer resp.Body.Close()
	limit := int64(8 << 20)
	if hc == c.long {
		limit = callMaxBody
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrServerDown, err)
	}
	r := &Reply{Status: resp.StatusCode, Body: b, ContentType: resp.Header.Get("Content-Type"), RetryAfter: resp.Header.Get("Retry-After")}
	if resp.StatusCode/100 != 2 {
		if hc == c.long {
			return r, nil
		}
		return nil, &StatusError{Status: resp.StatusCode, Body: b}
	}
	return r, nil
}

// Targets reads the peer's own rows, undecoded: httpapi owns the row type.
func (c *Client) Targets(ctx context.Context) ([]json.RawMessage, error) {
	body, err := c.Do(ctx, http.MethodGet, "/api/targets?local=1")
	if err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("peer's /api/targets is not a target list: %w", err)
	}
	return rows, nil
}
