package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// Claims and hands-off keep agents off each other's devices, and all of them
// off a device the person has marked.
//
// A claim is one agent saying "this device is mine for now". While it lasts,
// any other agent's input on that target answers 409 claimedBy, so a second
// Claude session cannot tap through the middle of the first one's test. It is
// a lease: every call the holder makes renews it, and claimLease after the
// last one it lapses on its own, so a session that ended without releasing
// does not lock the device until the server restarts. Claims live in memory
// and a restart drops them.
//
// Hands-off is the person saying "no agent on this one". It refuses every
// agent, the holder of a claim included, and it is written to disk, because
// every `make build` restarts the server.
//
// Neither ever refuses a person at a viewer. Like the human hold, this is
// coordination between callers that name themselves honestly, not a security
// boundary: the token is shared, and a caller that sends another's name is
// that caller as far as this file knows.

// HeaderSession names the agent's session, so two sessions sharing an agent
// name are still told apart. A claim made with one is held by that session.
const HeaderSession = "X-Loupe-Session"

// claimLease is how long a claim lasts after the holder's last call. Long
// enough for an agent to think between steps, short enough that a crashed
// session frees the device before anyone goes looking for why.
const claimLease = 10 * time.Minute

// drivingShown matches the arbiter's agentShown: a target reads as driven for
// this long after an agent's input went through.
const drivingShown = 10 * time.Second

// claimLinkMax bounds the link a claim carries, which is drawn in the picker.
const claimLinkMax = 512

// ClaimInfo is a claim as /api/targets, /api/claims and a claimedBy 409 show
// it. Agent and Session are what the holder sent; Link, when present, is an
// https URL the holder says leads back to it.
type ClaimInfo struct {
	Target  string    `json:"target"`
	Agent   string    `json:"agent,omitempty"`
	Session string    `json:"session,omitempty"`
	Link    string    `json:"link,omitempty"`
	Since   time.Time `json:"since"`
	// IdleMs is how long since the holder's last call; the claim lapses at
	// claimLease.
	IdleMs int64 `json:"idleMs"`
}

type claim struct {
	agent, session, link string
	since, seen          time.Time
}

// key is who holds the claim: the session when one was named, else the agent.
func (c *claim) key() string { return callerKey(c.agent, c.session) }

func callerKey(agent, session string) string {
	if session != "" {
		return "s:" + session
	}
	if agent != "" {
		return "a:" + agent
	}
	return ""
}

type driver struct {
	agent string
	at    time.Time
}

// claimBook holds every claim, the hands-off set and who last drove each
// target. It is apart from the session's arbiter because a session exists
// only while something streams, and an agent driving a device nobody is
// watching is exactly the case this has to cover.
type claimBook struct {
	mu       sync.Mutex
	now      func() time.Time
	record   func(map[string]any)
	claims   map[string]*claim
	handsOff map[string]bool
	driving  map[string]driver
	// path is the hands-off file. Empty keeps the set in memory only.
	path string
}

func newClaimBook(record func(map[string]any)) *claimBook {
	return &claimBook{
		now:      time.Now,
		record:   record,
		claims:   map[string]*claim{},
		handsOff: map[string]bool{},
		driving:  map[string]driver{},
	}
}

// load reads the hands-off set from path and keeps writing it there. A
// missing file is an empty set.
func (b *claimBook) load(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.path = path
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var f struct {
		HandsOff []string `json:"handsOff"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, id := range f.HandsOff {
		b.handsOff[id] = true
	}
	return nil
}

// save writes the hands-off set atomically. Called with mu held.
func (b *claimBook) save() error {
	if b.path == "" {
		return nil
	}
	ids := make([]string, 0, len(b.handsOff))
	for id := range b.handsOff {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	raw, err := json.MarshalIndent(map[string]any{"handsOff": ids}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

// live returns the target's claim, dropping it first if it has lapsed.
// Called with mu held.
func (b *claimBook) live(id string) *claim {
	c := b.claims[id]
	if c == nil {
		return nil
	}
	if idle := b.now().Sub(c.seen); idle >= claimLease {
		delete(b.claims, id)
		b.trace(map[string]any{"action": "claimExpired", "target": id, "agent": c.agent, "session": c.session, "idleMs": idle.Milliseconds()})
		return nil
	}
	return c
}

func (b *claimBook) info(id string, c *claim) *ClaimInfo {
	return &ClaimInfo{
		Target: id, Agent: c.agent, Session: c.session, Link: c.link,
		Since: c.since, IdleMs: b.now().Sub(c.seen).Milliseconds(),
	}
}

func (b *claimBook) trace(rec map[string]any) {
	if b.record == nil {
		return
	}
	rec["kind"] = "agent"
	for k, v := range rec {
		if v == "" {
			delete(rec, k)
		}
	}
	b.record(rec)
}

// take claims id for the caller, or renews the caller's own claim and
// replaces its link. It fails with the holder when someone else holds it.
func (b *claimBook) take(id, agent, session, link string) (*ClaimInfo, *ClaimInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	key := callerKey(agent, session)
	if c := b.live(id); c != nil {
		if c.key() != key {
			return nil, b.info(id, c)
		}
		c.agent, c.seen = agent, now
		if link != "" {
			c.link = link
		}
		return b.info(id, c), nil
	}
	c := &claim{agent: agent, session: session, link: link, since: now, seen: now}
	b.claims[id] = c
	b.trace(map[string]any{"action": "claim", "target": id, "agent": agent, "session": session})
	return b.info(id, c), nil
}

// release ends the target's claim. A caller that does not hold it is refused
// with the holder unless force is set, which is for a person in the picker.
// Releasing a target nobody holds succeeds.
func (b *claimBook) release(id, agent, session string, force bool) *ClaimInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.live(id)
	if c == nil {
		return nil
	}
	if !force && c.key() != callerKey(agent, session) {
		return b.info(id, c)
	}
	delete(b.claims, id)
	b.trace(map[string]any{"action": "release", "target": id, "agent": c.agent, "session": c.session, "forced": force && c.key() != callerKey(agent, session)})
	return nil
}

// seen renews the claim when the caller holds it. Every agent endpoint calls
// it, reads included, so an agent reading the screen keeps its claim.
func (b *claimBook) seen(id, agent, session string) {
	if b == nil {
		return
	}
	key := callerKey(agent, session)
	if key == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.live(id); c != nil && c.key() == key {
		c.seen = b.now()
	}
}

// refusal says why an agent may not act on id, or nil when it may. Hands-off
// comes first: it refuses the holder of a claim too.
func (b *claimBook) refusal(id, agent, session string) *ErrorResponse {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handsOff[id] {
		return &ErrorResponse{
			Error: fmt.Sprintf("%s is marked hands off; only a person at a viewer may drive it", id),
			Code:  ErrCodeHandsOff,
		}
	}
	c := b.live(id)
	if c == nil || c.key() == callerKey(agent, session) {
		return nil
	}
	who := c.agent
	if who == "" {
		who = "another agent"
	}
	return &ErrorResponse{
		Error: fmt.Sprintf("%s is claimed by %s; pick another device or wait for the claim to end", id, who),
		Code:  ErrCodeClaimed,
		Claim: b.info(id, c),
	}
}

// acted notes that an agent's input went through on id, for the picker's
// driving badge.
func (b *claimBook) acted(id, agent string) {
	if b == nil {
		return
	}
	if agent == "" {
		agent = "agent"
	}
	b.mu.Lock()
	b.driving[id] = driver{agent: agent, at: b.now()}
	b.mu.Unlock()
}

// setHandsOff marks or clears id and writes the set. The change stands in
// memory even when the write fails, and the error says it will not survive a
// restart.
func (b *claimBook) setHandsOff(id string, on bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handsOff[id] == on {
		return nil
	}
	if on {
		b.handsOff[id] = true
	} else {
		delete(b.handsOff, id)
	}
	b.trace(map[string]any{"action": "handsOff", "target": id, "on": on})
	return b.save()
}

// list returns every live claim, oldest first.
func (b *claimBook) list() []ClaimInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []ClaimInfo{}
	for id := range b.claims {
		if c := b.live(id); c != nil {
			out = append(out, *b.info(id, c))
		}
	}
	slices.SortFunc(out, func(a, b ClaimInfo) int { return a.Since.Compare(b.Since) })
	return out
}

// decorate fills each row's claim, hands-off and driving fields.
func (b *claimBook) decorate(rows []TargetInfo) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for i := range rows {
		// A peer's claim, hands-off and driving are the owner's, which
		// arbitrates them; this book has nothing under a namespaced id.
		if rows[i].Server != "" {
			continue
		}
		id := rows[i].ID
		if c := b.live(id); c != nil {
			rows[i].Claim = b.info(id, c)
		}
		rows[i].HandsOff = b.handsOff[id]
		if d, ok := b.driving[id]; ok && now.Sub(d.at) < drivingShown {
			rows[i].DrivenBy = d.agent
		}
	}
}

// validLink keeps a claim's link to an absolute https URL, the only kind the
// picker draws as a link.
func validLink(v string) bool {
	if v == "" {
		return true
	}
	if len(v) > claimLinkMax {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// ClaimRequest is POST /api/claims' optional body. Agent and Session default
// to the X-Loupe-Agent and X-Loupe-Session headers.
type ClaimRequest struct {
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	Link    string `json:"link,omitempty"`
}

// handleClaims serves /api/claims: GET lists every claim, POST ?target=
// claims or renews one, DELETE ?target= releases one.
func (s *Server) handleClaims(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.claims.list())
		return
	}
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	id := t.ID()
	agent, session := agentName(r.Header.Get(HeaderAgent)), agentName(r.Header.Get(HeaderSession))
	if pt, ok := t.(*target.PeerTarget); ok {
		s.forwardToOwner(w, r, pt, "/api/claims", agent, session)
		return
	}
	if r.Method == http.MethodDelete {
		force := r.URL.Query().Get("force") == "1"
		if holder := s.claims.release(id, agent, session, force); holder != nil {
			writeJSON(w, http.StatusConflict, ErrorResponse{
				Error: fmt.Sprintf("%s is claimed by someone else; ?force=1 releases it anyway", id),
				Code:  ErrCodeClaimed, Claim: holder,
			})
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var req ClaimRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "body: " + err.Error()})
			return
		}
	}
	if req.Agent != "" {
		agent = agentName(req.Agent)
	}
	if req.Session != "" {
		session = agentName(req.Session)
	}
	if agent == "" && session == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "a claim needs a name: send X-Loupe-Agent (and X-Loupe-Session), or agent in the body; printable ASCII, 64 bytes at most",
		})
		return
	}
	if !validLink(req.Link) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "link must be an absolute https URL of at most 512 bytes"})
		return
	}
	got, holder := s.claims.take(id, agent, session, req.Link)
	if holder != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf("%s is already claimed by %s", id, cmpOr(holder.Agent, "another agent")),
			Code:  ErrCodeClaimed, Claim: holder,
		})
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// HandsOffResponse is POST /api/devices/handsoff's 200. Warning is set when
// the change took effect but could not be saved.
type HandsOffResponse struct {
	Target   string `json:"target"`
	HandsOff bool   `json:"handsOff"`
	Warning  string `json:"warning,omitempty"`
}

// handleHandsOff is POST /api/devices/handsoff?target=&on=1|0.
func (s *Server) handleHandsOff(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	if pt, ok := t.(*target.PeerTarget); ok {
		s.forwardToOwner(w, r, pt, "/api/devices/handsoff", "", "")
		return
	}
	on := r.URL.Query().Get("on")
	if on != "1" && on != "0" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "?on= must be 1 or 0"})
		return
	}
	resp := HandsOffResponse{Target: t.ID(), HandsOff: on == "1"}
	if err := s.claims.setHandsOff(t.ID(), on == "1"); err != nil {
		resp.Warning = "applied, but not saved, so a restart forgets it: " + err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// SetHandsOffPath loads the hands-off set from path and keeps it there.
func (s *Server) SetHandsOffPath(path string) error { return s.claims.load(path) }

func cmpOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// forwardToOwner sends a claims or hands-off request for a peer target to the
// owner, which arbitrates them, and writes its answer back with the owner's
// target id turned into this server's. This server's book never holds a peer
// id.
func (s *Server) forwardToOwner(w http.ResponseWriter, r *http.Request, pt *target.PeerTarget, route, agent, session string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "body: " + err.Error()})
		return
	}
	q := r.URL.Query()
	q.Del("target")
	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
	defer cancel()
	ctx = target.WithPeerAs(ctx, target.PeerAs{Agent: agent, Session: session})
	status, out, retry, err := pt.Forward(ctx, r.Method, route, q, body)
	if err != nil {
		if !writeOwnerError(w, err) {
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		}
		return
	}
	if retry != "" {
		w.Header().Set("Retry-After", retry)
	}
	if len(out) == 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(pt.HubID(out))
}
