package httpapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// Input arbitration on /api/input. A person at a viewer wins: while they are
// using the target, an agent's action is refused with ErrCodeHumanHeld and
// nothing reaches the device. The session's Arbiter decides; this file works
// out who is asking and turns a refusal into the 409.
//
// Auth is one shared token, so the caller is told apart by what it sends,
// not by who it is. A viewer names itself with X-Loupe-Viewer, the id its
// init carried; an agent may name itself with X-Loupe-Agent for the trace and
// the viewer's banner. This coordinates cooperating callers. It is not a
// security boundary, and a caller forging a live viewer id is let through.

// Request headers /api/input reads to tell a viewer from an agent.
const (
	HeaderViewer = "X-Loupe-Viewer"
	HeaderAgent  = "X-Loupe-Agent"
)

// agentNameMax bounds X-Loupe-Agent, which is drawn in a banner on the
// phone and written to every trace record the agent causes.
const agentNameMax = 64

// inputCaller is who sent one /api/input. viewer is non-zero only when the
// request named a viewer attached to the target's session now; anything else
// is an agent, named or not.
type inputCaller struct {
	viewer uint64
	agent  string
	// session is X-Loupe-Session, which tells two sessions of one agent
	// apart for a claim.
	session string
	// person is a viewer attached to some session, though not this target's.
	// The human hold does not apply to it, but claims and hands-off never
	// refuse it: it is the picker acting for the person.
	person bool
}

// annotate adds the caller to a kind:"agent" trace record.
func (c inputCaller) annotate(rec map[string]any) {
	if c.viewer != 0 {
		rec["caller"] = "viewer"
		rec["viewer"] = c.viewer
		return
	}
	rec["caller"] = "agent"
	if c.agent != "" {
		rec["agent"] = c.agent
	}
}

// inputCaller reads the headers. A viewer id that is missing, malformed or
// no longer attached makes the caller an agent rather than an error: a page
// that reconnected is sent a fresh id, and until it uses it, it is told
// humanHeld like any agent, which says what happened.
func (s *Server) inputCaller(r *http.Request, targetID string) inputCaller {
	c := inputCaller{agent: agentName(r.Header.Get(HeaderAgent)), session: agentName(r.Header.Get(HeaderSession))}
	if v := r.Header.Get(HeaderViewer); v != "" {
		if id, err := strconv.ParseUint(v, 10, 64); err == nil && id != 0 {
			if sess, ok := s.mgr.Session(targetID); ok && sess.HasViewer(id) {
				return inputCaller{viewer: id}
			}
			c.person = s.mgr.HasViewer(id)
		}
	}
	return c
}

// agentName trims a caller-supplied name and drops it when it is too long
// or holds anything but printable ASCII, so it cannot break the banner or a
// trace line.
func agentName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > agentNameMax {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return ""
		}
	}
	return v
}

// arbitrated reports whether an action is subject to the human hold. A
// hardware keyboard switch moves nothing on the screen, so an agent may
// prepare one while a person is at the phone. A wait only reads the screen.
// reclaim restarts SpringBoard under them, so it is held like any touch.
func arbitrated(action string) bool {
	return action != "hardwareKeyboard" && action != "wait"
}

// held reports whether an arbitrated action also waits out the human hold
// and shows its agent driving. A move only draws the agent's cursor, so a
// person at the phone sees where the agent is pointing without the agent
// being refused or the banner claiming it drove; another agent's claim
// still refuses it, so no one points at a device someone else holds.
func held(action string) bool {
	return arbitrated(action) && action != "move"
}

// arbitrate records the caller with the target's arbiter and reports whether
// the action may go ahead. When it may not, it has written the 409 and
// traced the refusal. With no session for the target there is no viewer to
// hold it, and everything goes ahead as it did before arbitration.
func (s *Server) arbitrate(w http.ResponseWriter, r *http.Request, targetID, action string, c inputCaller) bool {
	refused := s.mayAct(r, targetID, action, c)
	if refused == nil {
		return true
	}
	if refused.RetryAfterMs > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(float64(refused.RetryAfterMs)/1000))))
	}
	writeJSON(w, http.StatusConflict, *refused)
	return false
}

// mayAct is arbitrate without the response: it records the caller with the
// arbiter, traces a refusal, and returns the refusal, or nil when the action
// may go ahead. Hands-off comes first, then another agent's claim, then the
// human hold, so an agent learns the reason that will outlast the others.
// /api/install?launch=1 uses it directly, because a launch refused there
// follows an install that did happen.
func (s *Server) mayAct(r *http.Request, targetID, action string, c inputCaller) *ErrorResponse {
	// A peer target's claims and hands-off are the owner's: the forwarded
	// call reaches its arbiter, and this server's book has nothing under a
	// namespaced id.
	if _, peer := s.mgr.peerTarget(targetID); c.viewer == 0 && !c.person && arbitrated(action) && !peer {
		if refused := s.claims.refusal(targetID, c.agent, c.session); refused != nil {
			rec := map[string]any{
				"kind": "agent", "target": targetID, "action": action,
				"ua": r.UserAgent(), "refused": refused.Code,
			}
			if refused.Claim != nil {
				rec["claimedBy"] = refused.Claim.Agent
			}
			c.annotate(rec)
			s.trace.Record(rec)
			return refused
		}
		if id, stopped := s.flags.interrupted(targetID, callerKey(c.agent, c.session)); stopped && held(action) {
			rec := map[string]any{
				"kind": "agent", "target": targetID, "action": action,
				"ua": r.UserAgent(), "refused": ErrCodeInterrupted, "flag": id,
			}
			c.annotate(rec)
			s.trace.Record(rec)
			return &ErrorResponse{
				Error: fmt.Sprintf("the person interrupted you with flag %d; read it (GET /api/flags?id=%d, or the flag tool) and check the screen before acting again", id, id),
				Code:  ErrCodeInterrupted,
				ID:    strconv.FormatUint(id, 10),
			}
		}
	}
	sess, ok := s.mgr.Session(targetID)
	if !ok {
		return nil
	}
	if c.viewer != 0 {
		sess.HumanInput(c.viewer)
		return nil
	}
	if !held(action) {
		return nil
	}
	ok, retry, holder := sess.AgentInput(c.agent)
	if ok {
		return nil
	}
	rec := map[string]any{
		"kind": "agent", "target": targetID, "action": action,
		"ua": r.UserAgent(), "refused": ErrCodeHumanHeld,
		"retryAfterMs": retry.Milliseconds(), "heldBy": holder,
	}
	c.annotate(rec)
	s.trace.Record(rec)
	return &ErrorResponse{
		Error:        fmt.Sprintf("a person is using this device; retry in %.1fs", retry.Seconds()),
		Code:         ErrCodeHumanHeld,
		RetryAfterMs: max(retry.Milliseconds(), 1),
	}
}

// agentActed shows an agent driving once its action has gone through. It is
// not done in arbitrate, which runs before the target is asked: an action the
// target cannot do (501) or tried and failed (502) moved nothing, and a
// banner saying the agent is driving would be wrong.
func (s *Server) agentActed(targetID, action string, c inputCaller) {
	if c.viewer != 0 || c.person || !held(action) {
		return
	}
	s.claims.acted(targetID, c.agent)
	if sess, ok := s.mgr.Session(targetID); ok {
		sess.AgentActed(c.agent)
	}
}

// agentFailed settles an agent that was allowed and whose action did not go
// through: one still shown waiting goes idle. arbitrate leaves it waiting
// rather than idle so that a successful action is one change, waiting to
// driving, on the viewer's banner.
func (s *Server) agentFailed(targetID, action string, c inputCaller) {
	if c.viewer != 0 || !held(action) {
		return
	}
	if sess, ok := s.mgr.Session(targetID); ok {
		sess.AgentFailed()
	}
}

// peerInputAs is who a forwarded /api/input acts for on the owner. A viewer on
// this hub, or a person acting for one, reaches the owner as the viewer id the
// owner gave this hub's relay for the target, so the owner takes it as the
// person it is and not as an agent its human hold refuses. With no relay open
// there is no such id, and the call goes as an agent.
func (s *Server) peerInputAs(t *target.PeerTarget, c inputCaller) target.PeerAs {
	if c.viewer != 0 || c.person {
		if sess, ok := s.mgr.Session(t.ID()); ok {
			if ps, ok := sess.Stream().(target.PeerStream); ok && ps.OwnerViewer() != 0 {
				return target.PeerAs{Viewer: ps.OwnerViewer()}
			}
		}
	}
	return target.PeerAs{Agent: c.agent, Session: c.session}
}

// peerReadCtx marks a read on a peer target with the caller's agent headers,
// so the owner renews that caller's claim.
func (s *Server) peerReadCtx(ctx context.Context, r *http.Request, t target.Target) context.Context {
	if _, ok := t.(*target.PeerTarget); !ok {
		return ctx
	}
	return target.WithPeerAs(ctx, target.PeerAs{
		Agent: agentName(r.Header.Get(HeaderAgent)), Session: agentName(r.Header.Get(HeaderSession)),
	})
}
