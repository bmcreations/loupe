package target

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Headers a PeerTarget forwards so the owner arbitrates the caller as the hub
// saw it. They are the names /api/input reads on the owner.
const (
	peerHeaderAgent   = "X-Loupe-Agent"
	peerHeaderSession = "X-Loupe-Session"
	peerHeaderViewer  = "X-Loupe-Viewer"
)

// ErrPeerDown is a call that never got an answer from the owner: the dial
// failed, the connection dropped or the call timed out. The hub answers it
// 502 serverDown.
var ErrPeerDown = errors.New("serverDown")

// PeerError is the owner's own non-2xx answer to a forwarded call. The hub
// writes Status and Body back verbatim, so a caller sees the owner's 409
// claimedBy or humanHeld exactly as the owner sent it.
type PeerError struct {
	Status int
	Body   []byte
	// RetryAfter is the owner's Retry-After header, kept so a humanHeld
	// refusal still says when to retry.
	RetryAfter string
	// ContentType is the owner's Content-Type, "" when it sent none.
	ContentType string
}

func (e *PeerError) Error() string {
	var er struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(e.Body, &er) == nil && er.Error != "" {
		return fmt.Sprintf("peer answered %d: %s", e.Status, er.Error)
	}
	return fmt.Sprintf("peer answered %d: %s", e.Status, http.StatusText(e.Status))
}

// PeerRequest is one call to the owner's HTTP API. Path carries the query.
type PeerRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// PeerResponse is the owner's 2xx answer.
type PeerResponse struct {
	Status      int
	Body        []byte
	ContentType string
}

// PeerCaller sends req to the named peer with its bearer token. A non-2xx
// answer is a *PeerError; a call that got none is an error wrapping
// ErrPeerDown. The hub supplies it, so this package imports neither the peer
// registry nor httpapi.
type PeerCaller func(ctx context.Context, server string, req PeerRequest) (*PeerResponse, error)

// PeerAs is who a forwarded call acts for. The hub puts it in the request
// context before dispatching an Actuator method, since those take only a
// context. Viewer, when non-zero, is the viewer id the owner gave the hub's
// relay for this target: the owner then treats the call as that viewer's
// person rather than as an agent. Otherwise Agent and Session are forwarded.
type PeerAs struct {
	Agent   string
	Session string
	Viewer  uint64
}

type peerAsKey struct{}

// WithPeerAs returns ctx carrying who a peer call acts for.
func WithPeerAs(ctx context.Context, as PeerAs) context.Context {
	return context.WithValue(ctx, peerAsKey{}, as)
}

func peerAs(ctx context.Context) PeerAs {
	as, _ := ctx.Value(peerAsKey{}).(PeerAs)
	return as
}

// do calls the owner's route and returns the 2xx answer.
func (p *PeerTarget) do(ctx context.Context, method, route string, q url.Values, body any) (*PeerResponse, error) {
	if p.call == nil {
		return nil, fmt.Errorf("%w: this server cannot reach %s", ErrPeerDown, p.server)
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("target", p.ownerID)
	req := PeerRequest{Method: method, Path: route + "?" + q.Encode(), Header: http.Header{}}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req.Body = raw
		req.Header.Set("Content-Type", "application/json")
	}
	as := peerAs(ctx)
	switch {
	case as.Viewer != 0:
		req.Header.Set(peerHeaderViewer, strconv.FormatUint(as.Viewer, 10))
	default:
		if as.Agent != "" {
			req.Header.Set(peerHeaderAgent, as.Agent)
		}
		if as.Session != "" {
			req.Header.Set(peerHeaderSession, as.Session)
		}
	}
	resp, err := p.call(ctx, p.server, req)
	if err != nil {
		var pe *PeerError
		if errors.As(err, &pe) || errors.Is(err, ErrPeerDown) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrPeerDown, err)
	}
	return resp, nil
}

// Forward sends body to the owner's route for this target and returns the
// owner's answer whatever its status, for routes the hub passes through
// (claims) rather than turns into a capability. Only an exchange that failed
// is an error.
func (p *PeerTarget) Forward(ctx context.Context, method, route string, q url.Values, body []byte) (status int, out []byte, retryAfter string, err error) {
	var b any
	if len(body) > 0 {
		b = json.RawMessage(body)
	}
	resp, err := p.do(ctx, method, route, q, b)
	var pe *PeerError
	switch {
	case errors.As(err, &pe):
		return pe.Status, pe.Body, pe.RetryAfter, nil
	case err != nil:
		return 0, nil, "", err
	}
	return resp.Status, resp.Body, "", nil
}

// HubID rewrites the owner's id for this target in an owner response body to
// the hub's: the "target" field, and the one inside a "claim". Anything else
// passes through untouched.
func (p *PeerTarget) HubID(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	fix := func(m map[string]json.RawMessage) bool {
		var id string
		if json.Unmarshal(m["target"], &id) != nil || id != p.ownerID {
			return false
		}
		m["target"], _ = json.Marshal(p.id)
		return true
	}
	changed := fix(m)
	if raw, ok := m["claim"]; ok {
		var c map[string]json.RawMessage
		if json.Unmarshal(raw, &c) == nil && fix(c) {
			m["claim"], _ = json.Marshal(c)
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// input posts one /api/input action.
func (p *PeerTarget) input(ctx context.Context, body map[string]any) error {
	_, err := p.do(ctx, http.MethodPost, "/api/input", nil, body)
	return err
}

// Tap, Swipe, TypeText and PressKey implement Actuator. Coordinates are
// device pixels on both servers and pass unchanged.
func (p *PeerTarget) Tap(ctx context.Context, x, y int) error {
	return p.input(ctx, map[string]any{"action": "tap", "x": x, "y": y})
}

func (p *PeerTarget) Swipe(ctx context.Context, x1, y1, x2, y2 int, ms int) error {
	return p.input(ctx, map[string]any{"action": "swipe", "x": x1, "y": y1, "x2": x2, "y2": y2, "ms": ms})
}

func (p *PeerTarget) TypeText(ctx context.Context, s string) error {
	return p.input(ctx, map[string]any{"action": "text", "text": s})
}

func (p *PeerTarget) PressKey(ctx context.Context, keycode int) error {
	return p.input(ctx, map[string]any{"action": "key", "keycode": keycode})
}

// PressButton implements ButtonPresser, so the owner names its own buttons:
// an iOS owner's home is not keycode 3.
func (p *PeerTarget) PressButton(ctx context.Context, name string) error {
	return p.input(ctx, map[string]any{"action": "button", "button": name})
}

// Buttons implements ButtonPresser. The owner's list is not known here, and
// /api/input only uses it to word an error the owner words itself.
func (p *PeerTarget) Buttons() []string { return nil }

// OpenURL implements URLOpener.
func (p *PeerTarget) OpenURL(ctx context.Context, u string) (*LaunchResult, error) {
	if err := p.input(ctx, map[string]any{"action": "openURL", "url": u}); err != nil {
		return nil, err
	}
	return &LaunchResult{}, nil
}

// CheckURL is the owner's check without the owner: the same I/O-free test, so
// a malformed URL is a 400 before arbitration.
func (p *PeerTarget) CheckURL(u string) error { return CheckURL(u) }

// Inspect implements Inspector from the owner's /api/ui. It asks for the full
// tree, which is one root, and not for compose names.
func (p *PeerTarget) Inspect(ctx context.Context) (*UI, error) {
	resp, err := p.do(ctx, http.MethodGet, "/api/ui", nil, nil)
	if err != nil {
		return nil, err
	}
	var in struct {
		Width    int     `json:"width"`
		Height   int     `json:"height"`
		Rotation int     `json:"rotation"`
		Nodes    []*Node `json:"nodes"`
	}
	if err := json.NewDecoder(bytes.NewReader(resp.Body)).Decode(&in); err != nil {
		return nil, fmt.Errorf("%s answered /api/ui with something else: %w", p.server, err)
	}
	ui := &UI{Width: in.Width, Height: in.Height, Rotation: in.Rotation}
	if len(in.Nodes) > 0 {
		ui.Root = in.Nodes[0]
	}
	return ui, nil
}

// Screenshot implements Screenshotter.
func (p *PeerTarget) Screenshot(ctx context.Context) ([]byte, error) {
	resp, err := p.do(ctx, http.MethodGet, "/api/screenshot", nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// DeviceFrame implements Framer.
func (p *PeerTarget) DeviceFrame(ctx context.Context) (*DeviceFrame, error) {
	resp, err := p.do(ctx, http.MethodGet, "/api/frame", nil, nil)
	if err != nil {
		return nil, err
	}
	var f DeviceFrame
	if err := json.Unmarshal(resp.Body, &f); err != nil {
		return nil, fmt.Errorf("%s answered /api/frame with something else: %w", p.server, err)
	}
	return &f, nil
}

// FrameAsset implements Framer.
func (p *PeerTarget) FrameAsset(ctx context.Context, name string) ([]byte, string, error) {
	resp, err := p.do(ctx, http.MethodGet, "/api/frame/asset", url.Values{"file": {name}}, nil)
	if err != nil {
		return nil, "", err
	}
	return resp.Body, resp.ContentType, nil
}

var (
	_ Actuator      = (*PeerTarget)(nil)
	_ ButtonPresser = (*PeerTarget)(nil)
	_ URLOpener     = (*PeerTarget)(nil)
	_ Inspector     = (*PeerTarget)(nil)
	_ Screenshotter = (*PeerTarget)(nil)
	_ Framer        = (*PeerTarget)(nil)
)
