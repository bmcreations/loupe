package revyl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// REST sends gestures to a Revyl session over the backend's
// device-proxy endpoint, the one revyl-cli's own device commands use
// (internal/mcp/device_session.go:3547 proxyWorkerRequestForSession):
//
//	POST <base>/api/v1/execution/device-proxy/<workflow_run_id>/<action>
//
// (internal/api/client.go:6603). The endpoint is not documented. Its shape
// was checked against a live account on 2026-11-18 with revyl-cli v0.2.18
// (commit 381c4fcf): a tap took a median of 500 to 640 ms on one kept-alive
// HTTPS connection, against 860 to 1100 ms for one `revyl device tap`.
//
// One REST serves every session, so its connections are shared and
// kept alive between gestures.
type REST struct {
	// BaseURL is the backend, e.g. https://backend.revyl.ai.
	BaseURL string
	// UserAgent is sent as is. It names loupe and its version: a request
	// that claimed to be the CLI (X-Revyl-Client: cli, or a revyl-cli
	// User-Agent) without the CLI's version was refused with 426
	// cli_upgrade_required, and an honest one was not.
	UserAgent string
	// Timeout bounds one request, from the dial to the end of the answer.
	Timeout time.Duration
	// Key returns the account's API key, or "" when there is none. It is
	// called for every request, so a new sign-in counts at once.
	Key func() string

	once   sync.Once
	client *http.Client
}

// revylBackendURL is the CLI's DefaultBaseURL (internal/api/client.go:71).
const revylBackendURL = "https://backend.revyl.ai"

// NewREST returns the client loupe uses, reading the key as the CLI
// does. version goes into the User-Agent.
func NewREST(version string) *REST {
	return &REST{
		BaseURL:   revylBackendURL,
		UserAgent: "loupe/" + version,
		Timeout:   10 * time.Second,
		Key:       func() string { return revylAPIKey(revylCredentialsPath()) },
	}
}

// revylCredentialsPath is where `revyl auth login` stores the account
// (internal/auth/manager.go:216 and :235).
func revylCredentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".revyl", "credentials.json")
}

// revylAPIKey reads the key the CLI would use: REVYL_API_KEY first, then
// api_key in the credentials file (internal/auth/manager.go:360
// ResolveCredentials). It is read on every call rather than kept.
func revylAPIKey(path string) string {
	if k := strings.TrimSpace(os.Getenv("REVYL_API_KEY")); k != "" {
		return k
	}
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c struct {
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(b, &c) != nil {
		return ""
	}
	return strings.TrimSpace(c.APIKey)
}

func (r *REST) httpClient() *http.Client {
	r.once.Do(func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		// Several sessions can each have a request in flight; keep a
		// connection warm for each.
		tr.MaxIdleConnsPerHost = 8
		r.client = &http.Client{Transport: tr}
	})
	return r.client
}

func (r *REST) key() string {
	if r == nil || r.Key == nil {
		return ""
	}
	return r.Key()
}

// revylRESTResult is a device's answer to one action.
type revylRESTResult struct {
	// DeviceMs is the device's own latency_ms, 0 when it gave none.
	DeviceMs float64
}

// revylRESTError is a device-proxy request that failed. Its message holds
// no URL, key or typed text.
type revylRESTError struct {
	action string
	// status is the HTTP status, 0 when no answer came.
	status int
	// wrote is set once the request's headers were written, after which
	// Revyl may have acted on it.
	wrote bool
	// code is the error code the proxy answered with, "" when it gave none.
	code string
	msg  string
}

func (e *revylRESTError) Error() string { return "revyl " + e.action + " over REST: " + e.msg }

// fallback reports whether the CLI may send the same action after this
// failure. It may when Revyl did not act on the request: it never left,
// or the backend refused it with 401, 403, 404 or 426, or the proxy
// answered 5xx worker_gone, which the Revyl CLI documents as the action
// never reaching the device (internal/mcp/device_session.go, v0.2.27).
//
// Every other failure is reported, not retried, a 5xx included: the
// proxy can answer 5xx after the device acted, and a second tap or Back
// does more harm than a lost one. After a timeout or a dropped
// connection the device may already have acted too.
func (e *revylRESTError) fallback() bool {
	switch {
	case e.status == 0:
		return !e.wrote
	case e.status == http.StatusUnauthorized, e.status == http.StatusForbidden,
		e.status == http.StatusNotFound, e.status == http.StatusUpgradeRequired:
		return true
	case e.status >= 500 && e.code == "worker_gone":
		return true
	}
	return false
}

// do sends one action. body is marshalled as JSON; nil sends none.
func (r *REST) do(ctx context.Context, key, runID, action string, body any) (revylRESTResult, error) {
	fail := func(status int, wrote bool, msg string) (revylRESTResult, error) {
		return revylRESTResult{}, &revylRESTError{action: action, status: status, wrote: wrote, msg: revylRedact(msg, key)}
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fail(0, false, "encoding the request: "+err.Error())
		}
		payload = bytes.NewReader(b)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }})
	u := strings.TrimRight(r.BaseURL, "/") + "/api/v1/execution/device-proxy/" + url.PathEscape(runID) + "/" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, payload)
	if err != nil {
		// A bad BaseURL; err would quote it.
		return fail(0, false, "the backend URL is not usable")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", r.UserAgent)
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return fail(0, wrote.Load(), revylTransportError(ctx, err))
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	// Read what is left so the connection goes back to the pool.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := "status " + fmt.Sprint(resp.StatusCode)
		c := revylErrorCode(data)
		if c != "" {
			msg += " " + c
		}
		_, err := fail(resp.StatusCode, true, msg)
		err.(*revylRESTError).code = c
		return revylRESTResult{}, err
	}
	if rerr != nil {
		return fail(resp.StatusCode, true, "reading the answer: "+revylTransportError(ctx, rerr))
	}
	var out struct {
		Success   *bool           `json:"success"`
		LatencyMs float64         `json:"latency_ms"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &out) != nil || out.Success == nil {
		return fail(resp.StatusCode, true, "the answer has no success field")
	}
	if !*out.Success {
		msg := "the device refused it"
		var s string
		if json.Unmarshal(out.Error, &s) == nil && s != "" {
			msg += ": " + clip(s, 200)
		}
		return fail(resp.StatusCode, true, msg)
	}
	return revylRESTResult{DeviceMs: out.LatencyMs}, nil
}

// workerSocket asks the backend for the session's worker websocket, the
// one Revyl's browser viewer sends live touches on:
//
//	GET <base>/api/v1/execution/streaming/worker-connection/<workflow_run_id>
//
// answers {"status": "ready", "worker_ws_url": "wss://...?token=...",
// "device_width": w, "device_height": h, ...}. The size is the one the
// socket's touches are in: on a live account (2026-11-19) it was 1080x2400
// pixels on Android and 402x874 points on an iPhone streamed at 1206x2622.
// The endpoint is not documented; its shape was read from a live account
// on 2026-11-19. The URL carries a token: it is never logged or put in an
// error, and the error holds no URL, key or run id.
func (r *REST) workerSocket(ctx context.Context, key, runID string) (revylWorkerSocket, error) {
	fail := func(msg string) (revylWorkerSocket, error) {
		msg = strings.ReplaceAll(revylRedact(msg, key), runID, "<run>")
		return revylWorkerSocket{}, errors.New("revyl worker-connection: " + msg)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := strings.TrimRight(r.BaseURL, "/") + "/api/v1/execution/streaming/worker-connection/" + url.PathEscape(runID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fail("the backend URL is not usable")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", r.UserAgent)
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return fail(revylTransportError(ctx, err))
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := "status " + fmt.Sprint(resp.StatusCode)
		if c := revylErrorCode(data); c != "" {
			msg += " " + c
		}
		return fail(msg)
	}
	if rerr != nil {
		return fail("reading the answer: " + revylTransportError(ctx, rerr))
	}
	var out struct {
		Status string `json:"status"`
		URL    string `json:"worker_ws_url"`
	}
	if json.Unmarshal(data, &out) != nil {
		return fail("the answer is not JSON")
	}
	if out.Status != "ready" {
		msg := "the worker is not ready"
		if revylCode.MatchString(out.Status) {
			msg += ": " + out.Status
		}
		return fail(msg)
	}
	if p, err := url.Parse(out.URL); err != nil || (p.Scheme != "wss" && p.Scheme != "ws") || p.Host == "" {
		return fail("the answer has no websocket URL")
	}
	ws := revylWorkerSocket{url: out.URL}
	// Read on its own, so a size it cannot read is no size rather than no
	// socket.
	var size struct {
		W float64 `json:"device_width"`
		H float64 `json:"device_height"`
	}
	_ = json.Unmarshal(data, &size)
	ws.w, ws.h, _ = revylScreenSize(size.W, size.H)
	return ws, nil
}

// revylScreenSize is a screen size Revyl reports, rounded. ok is false for
// a side under 1 or from 65536 up, which is taken as no size.
func revylScreenSize(w, h float64) (int, int, bool) {
	if w >= 1 && h >= 1 && w < 1<<16 && h < 1<<16 {
		return int(math.Round(w)), int(math.Round(h)), true
	}
	return 0, 0, false
}

// revylWorkerSocket is worker-connection's answer: the socket's URL, and
// the screen size its touches are in, 0 when it gave none.
type revylWorkerSocket struct {
	url  string
	w, h int
}

// revylTransportError describes a failed round trip without its URL: a
// *url.Error's message quotes the whole request URL.
func revylTransportError(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return clip(err.Error(), 200)
}

// revylCode is the shape of a machine-readable error code, the only part
// of a refusal's body that is repeated: the rest is free text.
var revylCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// revylErrorCode finds an error code such as cli_upgrade_required in a
// refusal: top-level code or error, or the same inside detail.
func revylErrorCode(body []byte) string {
	var b struct {
		Code   any `json:"code"`
		Error  any `json:"error"`
		Detail any `json:"detail"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	cands := []any{b.Code, b.Error}
	if d, ok := b.Detail.(map[string]any); ok {
		cands = append(cands, d["code"], d["error"])
	}
	for _, c := range cands {
		if s, ok := c.(string); ok && revylCode.MatchString(s) {
			return s
		}
	}
	return ""
}

var revylBearer = regexp.MustCompile(`(?i)bearer\s+\S+`)

// revylRedact cuts URLs, bearer tokens and the key itself from a message
// bound for an error or the trace.
func revylRedact(msg, key string) string {
	if key != "" {
		msg = strings.ReplaceAll(msg, key, "<key>")
	}
	msg = revylBearer.ReplaceAllString(msg, "Bearer <key>")
	return revylURL.ReplaceAllString(msg, "<url>")
}

// revylKeyID stands for a key in memory that outlives one request, so a
// refused key can be recognised without keeping a copy of it.
func revylKeyID(key string) [32]byte { return sha256.Sum256([]byte(key)) }
