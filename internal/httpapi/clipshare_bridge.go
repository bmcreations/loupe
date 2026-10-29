package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bmcreations/loupe/internal/clipshare"
)

// How a bridge paces itself against a peer. They are vars so a test does not
// wait out a long-poll or a backoff.
var (
	// bridgeWait is the waitMs a bridge asks a peer to hold its GET for.
	bridgeWait = sharedWaitDefault
	// bridgeRetry follows a failed exchange or an unexpected status.
	bridgeRetry = 5 * time.Second
	// bridgeOffRetry follows a peer answering 409: its sharedClipboard is
	// off, and a person turning it on is not urgent.
	bridgeOffRetry = 30 * time.Second
	// bridgeOldRetry follows a 404: a build without the route, which only
	// an upgrade changes.
	bridgeOldRetry = 5 * time.Minute
	// bridgePushTimeout bounds one POST of a value to a peer.
	bridgePushTimeout = 5 * time.Second
)

// syncClipBridges runs one bridge per peer and stops the bridge of a peer
// that was removed. It runs on every syncPeers, so it only acts on a change.
func (s *Server) syncClipBridges() {
	if s.clipShare == nil || s.peers == nil {
		return
	}
	want := map[string]bool{}
	for _, n := range s.peers.Names() {
		want[n] = true
	}
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	if s.bridges == nil {
		s.bridges = map[string]context.CancelFunc{}
	}
	for name, cancel := range s.bridges {
		if !want[name] {
			cancel()
			delete(s.bridges, name)
		}
	}
	for name := range want {
		if _, ok := s.bridges[name]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.bridges[name] = cancel
		go s.runClipBridge(ctx, name)
	}
}

// stopClipBridges stops every bridge. A test calls it so none outlives it.
func (s *Server) stopClipBridges() {
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	for name, cancel := range s.bridges {
		cancel()
		delete(s.bridges, name)
	}
}

// runClipBridge carries the shared clipboard both ways between this server
// and peer name until ctx ends. It long-polls the peer and publishes what
// arrives as peer:<name>, keeping the peer's id and At, and posts each of
// this server's changes that did not come from that peer. Ids stop the
// echoes: a value posted to the peer and pulled back carries the id this
// server already holds, and is dropped. Every exchange goes through the
// registry's client for the peer, which verifies its certificate before the
// token is sent.
//
// When the peer first answers, and again when it answers after being off or
// unreachable, this server's value is posted to it, so a peer that turned
// sharing on later still converges on it.
func (s *Server) runClipBridge(ctx context.Context, name string) {
	share := s.clipShare
	origin := "peer:" + name
	// peerOff is set while the peer says its sharedClipboard is off (or it
	// has no route), so changes are not posted to be refused.
	var peerOff atomic.Bool
	cancelPush := share.Subscribe(func(v clipshare.Value) {
		if v.Origin == origin || peerOff.Load() {
			return
		}
		s.pushClip(ctx, name, v, &peerOff)
	})
	defer cancelPush()

	var after uint64
	// heard and heardSeq are the last answer from the peer and the seq this
	// server held then.
	var heard bool
	var heardSeq uint64
	for {
		if share.WaitEnabled(ctx) != nil {
			return
		}
		// The first poll of a run asks for no wait, so the peer's answer,
		// and the catch-up post after it, come at once.
		wait := time.Duration(0)
		on, stop := share.WhileEnabled(ctx)
		for on.Err() == nil {
			backoff, answered := s.pullClip(on, name, &after, wait, &peerOff)
			if answered && wait == 0 {
				if v, ok := share.Current(); ok && needsCatchUp(v, origin, heard, heardSeq) {
					s.pushClip(on, name, v, &peerOff)
				}
			}
			if answered {
				heard = true
				heardSeq = currentSeq(share)
			} else if peerOff.Load() {
				// Off or without the route: it forgot what it held.
				heard = false
			}
			if !answered {
				wait = 0
			} else {
				wait = bridgeWait
			}
			if backoff == 0 {
				continue
			}
			select {
			case <-on.Done():
			case <-time.After(backoff):
			}
		}
		stop()
		if ctx.Err() != nil {
			return
		}
	}
}

// pullClip makes one long-poll of the peer, holding it for wait, and returns
// how long to wait before the next one and whether the peer answered with
// its sharedClipboard on.
func (s *Server) pullClip(ctx context.Context, name string, after *uint64, wait time.Duration, peerOff *atomic.Bool) (time.Duration, bool) {
	path := fmt.Sprintf("/api/clipboard/shared?after=%d&waitMs=%d", *after, wait.Milliseconds())
	cctx, cancel := context.WithTimeout(ctx, wait+bridgePushTimeout)
	defer cancel()
	reply, err := s.peers.Call(cctx, name, http.MethodGet, path, nil, nil)
	if err != nil {
		if ctx.Err() != nil {
			// Sharing was turned off, or the peer removed.
			return 0, false
		}
		s.trace.Record(map[string]any{"kind": "clipboard", "from": "peer", "server": name, "error": err.Error()})
		return bridgeRetry, false
	}
	switch reply.Status {
	case http.StatusOK:
		peerOff.Store(false)
		var v clipshare.Value
		if err := json.Unmarshal(reply.Body, &v); err != nil || v.Text == "" {
			s.trace.Record(map[string]any{"kind": "clipboard", "from": "peer", "server": name, "error": "unreadable shared clipboard answer"})
			return bridgeRetry, false
		}
		*after = v.Seq
		if len(v.Text) > maxClipboardBytes || !utf8.ValidString(v.Text) {
			return 0, true
		}
		got, ok := s.clipShare.Publish(clipshare.Value{ID: v.ID, Text: v.Text, At: v.At, Origin: "peer:" + name})
		if ok {
			s.trace.Record(map[string]any{"kind": "clipboard", "from": "peer", "server": name, "textLen": utf8.RuneCountInString(got.Text), "seq": got.Seq})
		}
		return 0, true
	case http.StatusNoContent:
		peerOff.Store(false)
		return 0, true
	case http.StatusConflict:
		peerOff.Store(true)
		return bridgeOffRetry, false
	case http.StatusNotFound:
		peerOff.Store(true)
		return bridgeOldRetry, false
	default:
		s.trace.Record(map[string]any{"kind": "clipboard", "from": "peer", "server": name, "status": reply.Status})
		return bridgeRetry, false
	}
}

// pushClip posts v to the peer with its id and At, so the peer stores the
// same value rather than a new one.
func (s *Server) pushClip(ctx context.Context, name string, v clipshare.Value, peerOff *atomic.Bool) {
	body, _ := json.Marshal(SharedClipboardRequest{Text: v.Text, ID: v.ID, At: v.At})
	cctx, cancel := context.WithTimeout(ctx, bridgePushTimeout)
	defer cancel()
	rec := map[string]any{"kind": "clipboard", "target": "peer:" + name, "from": "shared", "seq": v.Seq}
	reply, err := s.peers.Call(cctx, name, http.MethodPost, "/api/clipboard/shared", http.Header{"Content-Type": {"application/json"}}, body)
	switch {
	case err != nil:
		if ctx.Err() != nil {
			return
		}
		rec["error"] = err.Error()
	case reply.Status == http.StatusConflict || reply.Status == http.StatusNotFound:
		peerOff.Store(true)
		rec["status"] = reply.Status
	case reply.Status/100 != 2:
		rec["status"] = reply.Status
	default:
		rec["textLen"] = utf8.RuneCountInString(v.Text)
	}
	s.trace.Record(rec)
}

// currentSeq is the seq of the value share holds, 0 when it holds none.
func currentSeq(share *clipshare.Service) uint64 {
	v, _ := share.Current()
	return v.Seq
}

// needsCatchUp reports whether v, this server's value, should be posted to a
// peer that has just answered: it did not come from that peer (origin), and
// the peer has either never answered or has not answered since this server
// accepted it. A value the peer already had a chance to take is not posted
// again, so a reconnect cannot put it over a newer copy of the peer's own.
func needsCatchUp(v clipshare.Value, origin string, heard bool, heardSeq uint64) bool {
	return v.Origin != origin && (!heard || v.Seq > heardSeq)
}
