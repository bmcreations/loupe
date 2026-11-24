package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
// does not lock the device for good. Claims are written to their own file,
// because every `make build` restarts the server and an agent mid-test should
// not lose its device to that.
//
// Hands-off is the person saying "no agent on this one". It refuses every
// agent, the holder of a claim included, and it is written to disk for the
// same reason.
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

// claimLapsedFor is how long loupe remembers that a holder's claim lapsed.
// It outlasts the lease several times over, so an agent that waits on a long
// build or on a person still hears about it, and it is bounded so a holder
// that never comes back leaves nothing behind.
const claimLapsedFor = time.Hour

// drivingShown matches the arbiter's agentShown: a target reads as driven for
// this long after an agent's input went through.
const drivingShown = 10 * time.Second

// waitingShown is how long a waiting agent stays on the row after its last
// refusal: the arbiter's hold plus the retry an agent makes after it, so an
// agent that is still retrying never drops off between refusals.
const waitingShown = 20 * time.Second

// refusedShown is how long the last refusal stays on the row.
const refusedShown = time.Minute

// claimLinkMax bounds the link a claim carries, which is drawn in the picker.
const claimLinkMax = 512

// claimSaveEvery bounds how often a renewal alone rewrites the file. A claim
// restored after a restart can be this much older than it was, which the
// lease absorbs.
const claimSaveEvery = time.Minute

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
	// RecentHumanInputMs is set only on a claim's 200: how long ago a person
	// last used the device through a viewer. It is missing when nobody has
	// the device open, or nobody has touched it since it was opened, because
	// only an open session sees a person's input.
	RecentHumanInputMs *int64 `json:"recentHumanInputMs,omitempty"`
}

// WaitingInfo is an agent refused because a person holds the target, from
// its first refusal in a run. It stays on the row until the agent's next
// action goes through, or waitingShown after its last refusal.
type WaitingInfo struct {
	Agent   string    `json:"agent"`
	Session string    `json:"session,omitempty"`
	Since   time.Time `json:"since"`
}

// RefusedInfo is the last refusal of an agent on a target, for refusedShown.
type RefusedInfo struct {
	Agent string    `json:"agent"`
	Code  string    `json:"code"`
	At    time.Time `json:"at"`
}

// claimHoldMax bounds a hold as the claims file records it, so a server that
// dies mid-boot keeps the device from other agents no longer than the boot
// could have run. A boot is the only request that holds a claim.
const claimHoldMax = bootWaitMax

type claim struct {
	agent, session, link string
	since, seen          time.Time
	// saved is the seen last written to or read from the file.
	saved time.Time
	// holds counts the holder's requests still running on this server that
	// keep the claim alive however long they take, such as a boot waiting on
	// ?wait=.
	holds int
	// holdUntil is the latest a hold on any server ends, as the file says,
	// so a boot running through one server keeps the claim alive on all.
	holdUntil time.Time
}

// key is who holds the claim: the session when one was named, else the agent.
func (c *claim) key() string { return callerKey(c.agent, c.session) }

// runOut reports whether the claim has lapsed: no call from the holder for
// claimLease, and no hold on it anywhere.
func (c *claim) runOut(now time.Time) bool {
	return c.holds == 0 && !now.Before(c.holdUntil) && now.Sub(c.seen) >= claimLease
}

func callerKey(agent, session string) string {
	if session != "" {
		return "s:" + session
	}
	if agent != "" {
		return "a:" + agent
	}
	return ""
}

// splitKey is callerKey backwards.
func splitKey(key string) (agent, session string) {
	if s, ok := strings.CutPrefix(key, "s:"); ok {
		return "", s
	}
	return strings.TrimPrefix(key, "a:"), ""
}

// machineWide reports whether id names the same device on every loupe server
// on this machine, so its claim belongs in the file they share. The synthetic
// target is each server's own test pattern.
func machineWide(id string) bool { return id != "synthetic" }

type driver struct {
	agent string
	at    time.Time
}

type waiter struct {
	agent, session string
	since, last    time.Time
}

// lapseKey is a target and the callerKey that held it.
type lapseKey struct{ target, caller string }

type refusalNote struct {
	agent, code string
	at          time.Time
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
	waiting  map[string]waiter
	refused  map[string]refusalNote
	// lapsed remembers who held a target whose claim ran out, until the
	// holder claims it again or claimLapsedFor passes. Guarded by mu with
	// claims.
	lapsed map[lapseKey]time.Time
	// path is the hands-off file. Empty keeps it in memory only. It is apart
	// from the claims files so it says only what the person marked, and
	// nothing an agent did.
	path string
	// shared is the claims file every loupe server on the machine reads, so
	// a claim taken through one refuses other agents through all of them.
	// local is this server's own, beside its config, for the targets only it
	// has, and the one claims were kept in before they were shared. Either
	// nil keeps those claims in memory only; with no shared file, local
	// keeps them all.
	shared, local *claimFile
}

func newClaimBook(record func(map[string]any)) *claimBook {
	return &claimBook{
		now:      time.Now,
		record:   record,
		claims:   map[string]*claim{},
		handsOff: map[string]bool{},
		driving:  map[string]driver{},
		waiting:  map[string]waiter{},
		refused:  map[string]refusalNote{},
	}
}

// load reads the hands-off set from path and keeps writing it there. A
// missing file is an empty set.
func (b *claimBook) load(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.path = path
	var f struct {
		HandsOff []string `json:"handsOff"`
	}
	if err := readJSONFile(path, &f); err != nil {
		return err
	}
	for _, id := range f.HandsOff {
		b.handsOff[id] = true
	}
	return nil
}

// loadClaims reads the claims from the local and shared files and keeps them
// there; see claimBook.shared. A missing file is no claims, and a claim that
// lapsed while no server was running is dropped. Claims a server from before
// the shared file left in the local one move to the shared file, unless it
// already has a claim on that target.
func (b *claimBook) loadClaims(local, shared string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if shared != "" {
		b.shared = &claimFile{path: shared}
	}
	if local != "" && local != shared {
		b.local = &claimFile{path: local}
	}
	var errs []error
	for _, f := range b.files() {
		d, changed, err := f.read()
		if err != nil {
			errs = append(errs, err)
		}
		if changed {
			b.fold(f, d)
		}
	}
	var old claimData
	if b.local != nil && b.shared != nil {
		if err := readJSONFile(b.local.path, &old); err != nil {
			old = claimData{}
		}
	}
	b.update(func() bool {
		now := b.now()
		moved := false
		for _, sc := range old.Claims {
			if !machineWide(sc.Target) || callerKey(sc.Agent, sc.Session) == "" {
				continue
			}
			// The local file is rewritten without it either way.
			moved = true
			if b.claims[sc.Target] == nil && now.Sub(sc.Seen) < claimLease {
				b.claims[sc.Target] = &claim{
					agent: sc.Agent, session: sc.Session, link: sc.Link,
					since: sc.Since, seen: sc.Seen, saved: sc.Seen,
				}
			}
		}
		return moved
	})
	now := b.now()
	for id, c := range b.claims {
		b.trace(map[string]any{"action": "claimRestored", "target": id, "agent": c.agent, "session": c.session, "idleMs": now.Sub(c.seen).Milliseconds()})
	}
	return errors.Join(errs...)
}

// files is the claims files this book keeps, shared first, which is the
// order they are locked in.
func (b *claimBook) files() []*claimFile {
	var out []*claimFile
	for _, f := range []*claimFile{b.shared, b.local} {
		if f != nil {
			out = append(out, f)
		}
	}
	return out
}

// fileFor is the file id's claim is kept in, or nil for memory only.
func (b *claimBook) fileFor(id string) *claimFile {
	if machineWide(id) && b.shared != nil {
		return b.shared
	}
	return b.local
}

// fold makes memory's claims and lapse markers for f's targets what f says.
// A claim this server already has keeps its holds and, when it heard from
// the holder after the file was written, its seen. Called with mu held.
func (b *claimBook) fold(f *claimFile, d claimData) {
	now := b.now()
	inFile := map[string]bool{}
	for _, sc := range d.Claims {
		key := callerKey(sc.Agent, sc.Session)
		if sc.Target == "" || key == "" || b.fileFor(sc.Target) != f {
			continue
		}
		inFile[sc.Target] = true
		c := b.claims[sc.Target]
		if c == nil || c.key() != key || !c.since.Equal(sc.Since) {
			c = &claim{session: sc.Session, since: sc.Since}
			b.claims[sc.Target] = c
		}
		c.agent, c.link, c.saved, c.holdUntil = sc.Agent, sc.Link, sc.Seen, sc.HoldUntil
		if sc.Seen.After(c.seen) {
			c.seen = sc.Seen
		}
	}
	for id := range b.claims {
		if b.fileFor(id) == f && !inFile[id] {
			delete(b.claims, id)
		}
	}
	for k := range b.lapsed {
		if b.fileFor(k.target) == f {
			delete(b.lapsed, k)
		}
	}
	for _, l := range d.Lapsed {
		key := callerKey(l.Agent, l.Session)
		if l.Target == "" || key == "" || b.fileFor(l.Target) != f || now.Sub(l.At) >= claimLapsedFor {
			continue
		}
		if b.lapsed == nil {
			b.lapsed = map[lapseKey]time.Time{}
		}
		b.lapsed[lapseKey{l.Target, key}] = l.At
	}
}

// sync folds in whatever another server changed in the files since this one
// last read them. Called with mu held.
func (b *claimBook) sync() {
	for _, f := range b.files() {
		d, changed, err := f.read()
		if b.failing(f, "claimReadFailed", err) {
			continue
		}
		if changed {
			b.fold(f, d)
		}
	}
}

// failing traces err once while it lasts, and reports whether there was one.
func (b *claimBook) failing(f *claimFile, action string, err error) bool {
	if err == nil {
		f.failed = ""
		return false
	}
	if msg := err.Error(); msg != f.failed {
		f.failed = msg
		b.trace(map[string]any{"action": action, "error": msg})
	}
	return true
}

// view brings memory up to date for a read: it folds in other servers'
// changes and lapses claims that have run out, writing the lapse so every
// server sees it. Called with mu held.
func (b *claimBook) view() {
	b.sync()
	now := b.now()
	for _, c := range b.claims {
		if c.runOut(now) {
			b.update(func() bool { return false })
			return
		}
	}
}

// update makes a change with the files locked: it reads them again, lapses
// claims that have run out, runs fn, and writes the files when either changed
// anything. A file that cannot be locked is changed anyway, and the change
// stands in memory when it cannot be written. Called with mu held.
func (b *claimBook) update(fn func() bool) {
	for _, f := range b.files() {
		unlock, err := f.lock()
		if b.failing(f, "claimLockFailed", err) {
			continue
		}
		defer unlock()
	}
	b.sync()
	lapsed := b.expire()
	if fn() || lapsed {
		b.write()
	}
}

// expire drops every claim that has run out and remembers who held it.
// Called with mu held.
func (b *claimBook) expire() bool {
	now := b.now()
	dropped := false
	for id, c := range b.claims {
		if !c.runOut(now) {
			continue
		}
		delete(b.claims, id)
		b.noteLapse(id, c.key())
		b.trace(map[string]any{"action": "claimExpired", "target": id, "agent": c.agent, "session": c.session, "idleMs": now.Sub(c.seen).Milliseconds()})
		dropped = true
	}
	return dropped
}

// write saves every file from memory. A claim that cannot be saved still
// holds in memory; the failure is traced, since the caller that changed it
// has nothing to do about it. Called with mu held and the files locked.
func (b *claimBook) write() {
	now := b.now()
	for _, f := range b.files() {
		d := claimData{Claims: []savedClaim{}}
		for id, c := range b.claims {
			if b.fileFor(id) == f {
				d.Claims = append(d.Claims, savedClaim{
					Target: id, Agent: c.agent, Session: c.session, Link: c.link,
					Since: c.since, Seen: c.seen, HoldUntil: c.holdUntil,
				})
			}
		}
		for k, at := range b.lapsed {
			if b.fileFor(k.target) == f && now.Sub(at) < claimLapsedFor {
				agent, session := splitKey(k.caller)
				d.Lapsed = append(d.Lapsed, savedLapse{Target: k.target, Agent: agent, Session: session, At: at})
			}
		}
		slices.SortFunc(d.Claims, func(a, b savedClaim) int { return strings.Compare(a.Target, b.Target) })
		slices.SortFunc(d.Lapsed, func(a, b savedLapse) int { return a.At.Compare(b.At) })
		if err := f.write(d); err != nil {
			b.trace(map[string]any{"action": "claimSaveFailed", "error": err.Error()})
			continue
		}
		for id, c := range b.claims {
			if b.fileFor(id) == f {
				c.saved = c.seen
			}
		}
	}
}

// readJSONFile decodes path into v. A missing file leaves v as it is.
func readJSONFile(path string, v any) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// writeJSONFile writes v to path atomically.
func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// save writes the hands-off set. Called with mu held.
func (b *claimBook) save() error {
	if b.path == "" {
		return nil
	}
	ids := make([]string, 0, len(b.handsOff))
	for id := range b.handsOff {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return writeJSONFile(b.path, map[string]any{"handsOff": ids})
}

// noteLapse remembers that key's claim on id ran out, and forgets markers
// past claimLapsedFor. Called with mu held.
func (b *claimBook) noteLapse(id, key string) {
	now := b.now()
	for k, at := range b.lapsed {
		if now.Sub(at) >= claimLapsedFor {
			delete(b.lapsed, k)
		}
	}
	if key == "" {
		return
	}
	if b.lapsed == nil {
		b.lapsed = map[lapseKey]time.Time{}
	}
	b.lapsed[lapseKey{id, key}] = now
}

func (b *claimBook) info(id string, c *claim) *ClaimInfo {
	now := b.now()
	idle := now.Sub(c.seen)
	if c.holds > 0 || now.Before(c.holdUntil) {
		// The holder is waiting on loupe, not idle.
		idle = 0
	}
	return &ClaimInfo{
		Target: id, Agent: c.agent, Session: c.session, Link: c.link,
		Since: c.since, IdleMs: idle.Milliseconds(),
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
func (b *claimBook) take(id, agent, session, link string) (got, holder *ClaimInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := callerKey(agent, session)
	b.update(func() bool {
		now := b.now()
		if c := b.claims[id]; c != nil {
			if c.key() != key {
				holder = b.info(id, c)
				return false
			}
			changed := c.agent != agent || (link != "" && link != c.link)
			c.agent, c.seen = agent, now
			if link != "" {
				c.link = link
			}
			got = b.info(id, c)
			return changed || now.Sub(c.saved) >= claimSaveEvery
		}
		c := &claim{agent: agent, session: session, link: link, since: now, seen: now}
		b.claims[id] = c
		delete(b.lapsed, lapseKey{id, key})
		b.trace(map[string]any{"action": "claim", "target": id, "agent": agent, "session": session})
		got = b.info(id, c)
		return true
	})
	return got, holder
}

// release ends the target's claim. A caller that does not hold it is refused
// with the holder unless force is set, which is for a person in the picker.
// Releasing a target nobody holds succeeds. released reports whether a claim
// ended and gone is its holder's agent name; the holder's driving badge ends
// with the claim.
func (b *claimBook) release(id, agent, session string, force bool) (holder *ClaimInfo, gone string, released bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.update(func() bool {
		c := b.claims[id]
		if c == nil {
			return false
		}
		if !force && c.key() != callerKey(agent, session) {
			holder = b.info(id, c)
			return false
		}
		delete(b.claims, id)
		if d, ok := b.driving[id]; ok && d.agent == cmpOr(c.agent, "agent") {
			delete(b.driving, id)
		}
		b.trace(map[string]any{"action": "release", "target": id, "agent": c.agent, "session": c.session, "forced": force && c.key() != callerKey(agent, session)})
		gone, released = c.agent, true
		return true
	})
	return holder, gone, released
}

// has reports whether id has a live claim.
func (b *claimBook) has(id string) bool {
	if b == nil {
		return false
	}
	return b.held(id)
}

// drop removes id's claim, whoever holds it, because the target is gone.
func (b *claimBook) drop(id, reason string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.update(func() bool {
		c := b.claims[id]
		if c == nil {
			return false
		}
		delete(b.claims, id)
		b.trace(map[string]any{"action": "release", "target": id, "agent": c.agent, "session": c.session, "reason": reason})
		return true
	})
}

// seen renews the claim when the caller holds it. Every agent endpoint calls
// it, reads included, so an agent reading the screen keeps its claim. The
// renewal reaches the file at most every claimSaveEvery.
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
	b.view()
	c := b.claims[id]
	if c == nil || c.key() != key {
		return
	}
	c.seen = b.now()
	if c.seen.Sub(c.saved) >= claimSaveEvery {
		b.update(func() bool {
			c := b.claims[id]
			return c != nil && c.key() == key
		})
	}
}

// hold keeps the caller's claim on id from lapsing until the returned func
// runs, which renews it. A request that waits on the device longer than the
// lease, such as a boot with a long ?wait=, calls it so the holder does not
// lose the device to its own wait. The file records the hold as lasting
// claimHoldMax, so it keeps the claim alive on every server, and no longer
// than that if this one dies. A caller without the claim holds nothing.
func (b *claimBook) hold(id, agent, session string) func() {
	key := callerKey(agent, session)
	if b == nil || key == "" {
		return func() {}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var c *claim
	b.update(func() bool {
		c = b.claims[id]
		if c == nil || c.key() != key {
			c = nil
			return false
		}
		c.holds++
		if until := b.now().Add(claimHoldMax); until.After(c.holdUntil) {
			c.holdUntil = until
		}
		return true
	})
	if c == nil {
		return func() {}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.update(func() bool {
				c.holds--
				// A claim released or replaced during the hold is not renewed.
				if b.claims[id] != c {
					return false
				}
				c.seen = b.now()
				if c.holds == 0 {
					c.holdUntil = time.Time{}
				}
				return true
			})
		})
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
	b.view()
	c := b.claims[id]
	if c == nil {
		// A lapsed holder is told until it claims again; see ErrCodeClaimLapsed.
		key := callerKey(agent, session)
		at, ok := b.lapsed[lapseKey{id, key}]
		if !ok || b.now().Sub(at) >= claimLapsedFor {
			return nil
		}
		return &ErrorResponse{
			Error: fmt.Sprintf("your claim on %s lapsed after %s without a call; claim it again (POST /api/claims) before driving it", id, claimLease),
			Code:  ErrCodeClaimLapsed,
		}
	}
	if c.key() == callerKey(agent, session) {
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
	delete(b.waiting, id)
	b.mu.Unlock()
}

// refusedAgent notes that agent was refused on id with code. A humanHeld
// refusal also shows the agent waiting; one that was already waiting keeps
// its first refusal's time.
func (b *claimBook) refusedAgent(id, agent, session, code string) {
	if b == nil {
		return
	}
	if agent == "" {
		agent = "agent"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.refused[id] = refusalNote{agent: agent, code: code, at: now}
	if code != ErrCodeHumanHeld {
		return
	}
	w, ok := b.waiting[id]
	if !ok || w.agent != agent || w.session != session || now.Sub(w.last) >= waitingShown {
		w = waiter{agent: agent, session: session, since: now}
	}
	w.last = now
	b.waiting[id] = w
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

// list returns every live claim, oldest first: this server's and those taken
// through other servers on the machine.
func (b *claimBook) list() []ClaimInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.view()
	out := []ClaimInfo{}
	for id, c := range b.claims {
		out = append(out, *b.info(id, c))
	}
	slices.SortFunc(out, func(a, b ClaimInfo) int {
		if c := a.Since.Compare(b.Since); c != 0 {
			return c
		}
		return strings.Compare(a.Target, b.Target)
	})
	return out
}

// held reports whether id has a live claim. It does not renew it.
func (b *claimBook) held(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.view()
	return b.claims[id] != nil
}

// holderOf returns who holds id's claim, or ok false when nobody does.
func (b *claimBook) holderOf(id string) (agent, session string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.view()
	c := b.claims[id]
	if c == nil {
		return "", "", false
	}
	return c.agent, c.session, true
}

// decorate fills each row's claim, hands-off and driving fields.
func (b *claimBook) decorate(rows []TargetInfo) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.view()
	now := b.now()
	for i := range rows {
		// A peer's claim, hands-off and driving are the owner's, which
		// arbitrates them; this book has nothing under a namespaced id.
		if rows[i].Server != "" {
			continue
		}
		id := rows[i].ID
		if c := b.claims[id]; c != nil {
			rows[i].Claim = b.info(id, c)
		}
		rows[i].HandsOff = b.handsOff[id]
		if d, ok := b.driving[id]; ok && now.Sub(d.at) < drivingShown {
			rows[i].DrivenBy = d.agent
		}
		if w, ok := b.waiting[id]; ok && now.Sub(w.last) < waitingShown {
			rows[i].Waiting = &WaitingInfo{Agent: w.agent, Session: w.session, Since: w.since}
		}
		if r, ok := b.refused[id]; ok && now.Sub(r.at) < refusedShown {
			rows[i].Refused = &RefusedInfo{Agent: r.agent, Code: r.code, At: r.at}
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
	if r.Method == http.MethodDelete {
		// A claim outlives its target when the device goes away under it, so
		// it can be released without one.
		if id := r.URL.Query().Get("target"); id != "" {
			if _, listed := s.mgr.Target(id); !listed {
				s.releaseClaim(w, r, id)
				return
			}
		}
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
		s.releaseClaim(w, r, id)
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
	// A claim keeps the emulator out of the idle pause (see pauseIdle), so one
	// the clock paused before the claim is resumed now: raw `adb install` on
	// a paused VM hangs. Best effort; the pause pass never starts on a claimed
	// emulator, so a failed resume is not a claim failure.
	s.mgr.WakeEmulator(r.Context(), id)
	if sess, ok := s.mgr.Session(id); ok {
		if since, ok := sess.SinceHumanInput(); ok {
			ms := since.Milliseconds()
			got.RecentHumanInputMs = &ms
		}
	}
	writeJSON(w, http.StatusOK, got)
}

// releaseClaim is DELETE /api/claims for id. A target that is not listed
// and has no claim is 404, as any other call naming it is.
func (s *Server) releaseClaim(w http.ResponseWriter, r *http.Request, id string) {
	agent, session := agentName(r.Header.Get(HeaderAgent)), agentName(r.Header.Get(HeaderSession))
	if _, listed := s.mgr.Target(id); !listed && !s.claims.has(id) {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: fmt.Sprintf("no target %q", id)})
		return
	}
	force := r.URL.Query().Get("force") == "1"
	holder, gone, released := s.claims.release(id, agent, session, force)
	if holder != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf("%s is claimed by someone else; ?force=1 releases it anyway", id),
			Code:  ErrCodeClaimed, Claim: holder,
		})
		return
	}
	if released {
		if sess, ok := s.mgr.Session(id); ok {
			sess.AgentGone(gone)
		}
	}
	w.WriteHeader(http.StatusNoContent)
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

// SetClaimsPaths loads the claims from local, this server's own file, and
// shared, the file every loupe server on the machine reads, and keeps them
// there.
func (s *Server) SetClaimsPaths(local, shared string) error {
	return s.claims.loadClaims(local, shared)
}

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
