package target

import (
	"context"
	"fmt"
)

// PeerDialer opens a stream to the owner's target ownerID on the named server.
// The hub supplies it, so this package imports neither the peer registry nor
// the session protocol.
type PeerDialer func(ctx context.Context, server, ownerID string, opts Options) (Stream, error)

// PeerStream is a Stream that came from a peer. OwnerViewer is the viewer id
// the owner assigned this relay in its Init, which the hub forwards as
// X-Loupe-Viewer so the owner attributes a hub viewer's HTTP input to the
// relay's viewer.
type PeerStream interface {
	Stream
	OwnerViewer() uint64
}

// PeerTarget is a target another loupe server owns, listed by this one. Its
// id is "<server>/<owner's id>" and its kind is the owner's, since the client
// picks labels, frames and input rules by kind. It registers in the Manager
// like any target so every handler's s.mgr.Target(id) resolves it; each
// capability it grows calls the owner's matching route (docs/remote-targets.md).
//
// It lives here and holds no connection to the owner: the row's availability,
// claim and state come from the owner's own listing, which the hub overlays
// when it builds /api/targets.
type PeerTarget struct {
	id      string
	kind    Kind
	label   string
	server  string
	ownerID string
	dial    PeerDialer
	call    PeerCaller
}

// PeerID is the hub's id for a target the named server owns.
func PeerID(server, ownerID string) string { return server + "/" + ownerID }

// NewPeerTarget describes the owner's target ownerID, as seen from server.
// dial opens its streams and call reaches its HTTP routes; nil leaves the
// target unopenable or unreachable.
func NewPeerTarget(server, ownerID string, kind Kind, label string, dial PeerDialer, call PeerCaller) *PeerTarget {
	return &PeerTarget{id: PeerID(server, ownerID), kind: kind, label: label, server: server, ownerID: ownerID, dial: dial, call: call}
}

func (p *PeerTarget) ID() string    { return p.id }
func (p *PeerTarget) Kind() Kind    { return p.kind }
func (p *PeerTarget) Label() string { return p.label }

// Server is the peer's name, and OwnerID the id the owner knows the target by.
func (p *PeerTarget) Server() string  { return p.server }
func (p *PeerTarget) OwnerID() string { return p.ownerID }

// Open connects to the owner through the hub's dialer. opts do not reach the
// owner: it opens its target with its own options and the hub's session
// takes the stream as it comes.
func (p *PeerTarget) Open(ctx context.Context, opts Options) (Stream, error) {
	if p.dial == nil {
		return nil, fmt.Errorf("%s is on server %s, which this server cannot stream from", p.label, p.server)
	}
	return p.dial(ctx, p.server, p.ownerID, opts)
}
