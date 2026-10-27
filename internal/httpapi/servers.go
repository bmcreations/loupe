package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bmcreations/loupe/internal/peer"
	"github.com/bmcreations/loupe/internal/target"
)

// ServerInfo is a peer as GET /api/servers shows it. It never carries the
// token.
type ServerInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// State is "up" while the last poll of the peer succeeded, else "down".
	State   string `json:"state"`
	Targets int    `json:"targets"`
	// RTTMs is how long the last successful poll took.
	RTTMs int64 `json:"rttMs"`
	// Warning is set on the add response when the owner answered over HTTP
	// but the QUIC probe could not confirm streaming works.
	Warning string `json:"warning,omitempty"`
}

// ServerAddRequest is POST /api/servers' body. Name is optional and is
// derived from the host when empty. Fingerprint repeats the one a 409
// confirmFingerprint answer carried, once the person has checked it; the
// peer's certificate is then pinned to it.
type ServerAddRequest struct {
	Name        string `json:"name,omitempty"`
	URL         string `json:"url"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// addServerTimeout bounds POST /api/servers. The peer is reached before the
// answer, so this is a dial, a handshake and one request.
const addServerTimeout = 15 * time.Second

// SetPeers attaches the peer registry and keeps this server's merged rows in
// step with it: every poll, add and remove re-merges.
func (s *Server) SetPeers(reg *peer.Registry) {
	s.peers = reg
	reg.SetProbe(peer.ProbeProtocol)
	s.mgr.SetPeerDialer(func(ctx context.Context, server, ownerID string, opts target.Options) (target.Stream, error) {
		srv, ok := reg.Server(server)
		if !ok {
			return nil, peer.ErrServerDown
		}
		return peer.DialStream(ctx, srv, ownerID, opts)
	})
	s.mgr.SetPeerCaller(func(ctx context.Context, server string, req target.PeerRequest) (*target.PeerResponse, error) {
		return callPeer(ctx, reg, server, req)
	})
	reg.SetOnChange(s.syncPeers)
	s.syncPeers()
}

// syncPeers turns the peers' polled rows into this server's rows. An id is
// namespaced with the peer's name, the owner's kind stays, and everything
// else about the row is the owner's. A row that already carries a server is
// the owner's own peer's, and is dropped so servers that peer with each other
// never list a device twice. A peer that has stopped answering keeps its rows
// but marks them unavailable.
func (s *Server) syncPeers() {
	s.peerSync.Lock()
	defer s.peerSync.Unlock()
	var out []TargetInfo
	for _, p := range s.peers.Rows() {
		for _, raw := range p.Rows {
			var ti TargetInfo
			if err := json.Unmarshal(raw, &ti); err != nil || ti.ID == "" || ti.Server != "" {
				continue
			}
			ti.ID = peerRowID(p.Server, ti.ID)
			ti.Server = p.Server
			if p.Stale {
				ti.Available = false
				ti.Reason = ErrCodeServerDown
			}
			out = append(out, ti)
		}
	}
	s.mgr.SetPeerRows(out)
}

func peerRowID(server, ownerID string) string { return server + "/" + ownerID }

// handleServers serves /api/servers: GET lists, POST adds, DELETE removes.
func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	if s.peers == nil {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "servers are not enabled"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		infos := s.peers.List()
		out := make([]ServerInfo, 0, len(infos))
		for _, i := range infos {
			out = append(out, ServerInfo{Name: i.Name, URL: i.URL, State: i.State, Targets: i.Targets, RTTMs: i.RTTMs})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		s.addServer(w, r)
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		ok, err := s.peers.Remove(name)
		switch {
		case !ok:
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no server named " + name})
		case err != nil:
			// Removed from memory, but the file still lists it: it would be
			// back after a restart.
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "removed, but servers.json was not updated: " + err.Error()})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
	}
}

func (s *Server) addServer(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req ServerAddRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "body must be {name?, url, token, fingerprint?}: " + err.Error()})
		return
	}
	// Not r.Context(): a phone that drops the connection must not leave the
	// pin half taken, and the timeout bounds it anyway.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), addServerTimeout)
	defer cancel()
	added, err := s.peers.Add(ctx, peer.Server{Name: req.Name, URL: req.URL, Token: req.Token, Pin: req.Fingerprint})
	if err != nil {
		writePeerError(w, err)
		return
	}
	info := ServerInfo{Name: added.Name, URL: added.URL, State: "up", Warning: added.Warning}
	for _, i := range s.peers.List() {
		if i.Name == added.Name {
			info = ServerInfo{Name: i.Name, URL: i.URL, State: i.State, Targets: i.Targets, RTTMs: i.RTTMs, Warning: added.Warning}
		}
	}
	writeJSON(w, http.StatusCreated, info)
}

// writePeerError answers with what went wrong reaching a peer. The peer's own
// 4xx or 5xx passes through with its body; a dial failure is 502 serverDown.
func writePeerError(w http.ResponseWriter, err error) {
	var invalid *peer.InvalidError
	var taken *peer.NameTakenError
	var status *peer.StatusError
	var unsupported *peer.UnsupportedProtocolError
	var confirm *peer.ConfirmFingerprintError
	switch {
	case errors.As(err, &confirm):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: ErrCodeConfirmFingerprint, Fingerprint: confirm.Fingerprint})
	case errors.As(err, &unsupported):
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error(), Code: ErrCodeUnsupportedProtocol})
	case errors.As(err, &invalid):
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	case errors.As(err, &taken):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error()})
	case errors.Is(err, peer.ErrCertChanged):
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error(), Code: ErrCodeCertChanged})
	case errors.Is(err, peer.ErrServerDown):
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error(), Code: ErrCodeServerDown})
	case errors.As(err, &status) && status.Status >= 400:
		if json.Valid(status.Body) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status.Status)
			_, _ = w.Write(status.Body)
			return
		}
		writeJSON(w, status.Status, ErrorResponse{Error: status.Error()})
	default:
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
	}
}

// peerCallTimeout bounds a forwarded call whose context has no deadline.
const peerCallTimeout = 30 * time.Second

// callPeer sends req to the named peer. The peer's own non-2xx answer is a
// *target.PeerError; no answer is target.ErrPeerDown.
func callPeer(ctx context.Context, reg *peer.Registry, server string, req target.PeerRequest) (*target.PeerResponse, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, peerCallTimeout)
		defer cancel()
	}
	rep, err := reg.Call(ctx, server, req.Method, req.Path, req.Header, req.Body)
	if err != nil {
		if errors.Is(err, peer.ErrServerDown) {
			return nil, fmt.Errorf("%w: %v", target.ErrPeerDown, err)
		}
		return nil, err
	}
	if rep.Status/100 != 2 {
		return nil, &target.PeerError{Status: rep.Status, Body: rep.Body, RetryAfter: rep.RetryAfter, ContentType: rep.ContentType}
	}
	return &target.PeerResponse{Status: rep.Status, Body: rep.Body, ContentType: rep.ContentType}, nil
}

// writeOwnerError answers a call to a peer target's owner that failed, and
// reports whether it did. The owner's own refusal goes back verbatim, status
// and body, so a caller sees its 409 claimedBy or humanHeld unchanged; no
// answer at all is 502 serverDown. Callers check it before any other error.
func writeOwnerError(w http.ResponseWriter, err error) bool {
	var pe *target.PeerError
	if errors.As(err, &pe) {
		ct := pe.ContentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		if pe.RetryAfter != "" {
			w.Header().Set("Retry-After", pe.RetryAfter)
		}
		w.WriteHeader(pe.Status)
		_, _ = w.Write(pe.Body)
		return true
	}
	if errors.Is(err, target.ErrPeerDown) {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error(), Code: ErrCodeServerDown})
		return true
	}
	return false
}
