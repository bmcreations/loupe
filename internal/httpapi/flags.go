package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	imgpng "image/png"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// A flag is a person telling an agent "something is off on this screen".
// The viewer posts one with an optional note and region; the server keeps
// the screen as it was then, a screenshot and the interactive UI tree, and
// who held the target. With watch on, the flag stays open while the person
// reproduces the problem, and ending it adds their input in between and the
// screen as it was at the end. Agents read flags with GET, long-polling for
// new ones; loupe mcp pushes each to a Claude Code session (docs/agent-api.md).
//
// Flags are the person's signal, like the clipboard: neither claims nor the
// human hold gate them, and reading one claims nothing. An interrupt is the
// one flag that gates agents in turn: it holds the target for the person
// while they write it, and afterwards refuses the interrupted agent's input
// until it has read the flag (ErrCodeInterrupted).

const (
	// maxFlags is how many flags the server keeps, oldest dropped first.
	// Each holds up to two screenshots, a few MB on a high-resolution device.
	maxFlags = 32
	// maxFlagNote is the longest note, in runes.
	maxFlagNote = 2000
	// maxWatch ends a watch the person forgot to stop, and an interrupt
	// they never sent.
	maxWatch = 10 * time.Minute
	// FlagInterrupt is FlagRequest.Kind for stopping a driving agent.
	FlagInterrupt = "interrupt"
	// flagsWaitDefault and flagsWaitMax bound a long-poll.
	flagsWaitDefault = 25 * time.Second
	flagsWaitMax     = 60 * time.Second
)

// activeWindow is how long a session that called the agent API without
// waiting on flags is still offered as somewhere to send one.
var activeWindow = 15 * time.Minute

// listenerGrace is how long a session still counts as listening after its
// long-poll answers. loupe mcp polls again at once, or after flagRetry (2s)
// when the server fails to answer.
var listenerGrace = 5 * time.Second

// FlagRequest is POST /api/flags's body.
type FlagRequest struct {
	// Note is what the person typed, if anything.
	Note string `json:"note,omitempty"`
	// Region is what the person marked, as fractions of the screen:
	// [left, top, right, bottom], each in [0, 1]. Absent marks the whole
	// screen.
	Region []float64 `json:"region,omitempty"`
	// Watch keeps the flag open until POST /api/flags/end, collecting the
	// person's input in between.
	Watch bool `json:"watch,omitempty"`
	// Kind is empty, or "interrupt": the person is stopping an agent that
	// is driving. It is pushed at once, before the person has written
	// anything, and stays open like a watch: the posting viewer holds the
	// target until POST /api/flags/end, which may carry the note and region.
	Kind string `json:"kind,omitempty"`
	// To sends the flag to one session, as GET /api/flags/listeners names
	// it: loupe mcp pushes it to that session only, and an interrupt stops
	// that session rather than the claim's holder. Absent sends it to all.
	To *FlagRecipient `json:"to,omitempty"`
}

// FlagRecipient is who a flag was sent to: a loupe mcp session by its agent
// and session names, or a T3 Code thread, or a new thread in a T3 project
// from the t3Projects setting, or a command or webhook from
// destinations.json by its name. Exactly one of these.
type FlagRecipient struct {
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	// T3Thread is a T3 thread's id, as /api/flags/destinations lists it.
	T3Thread string `json:"t3Thread,omitempty"`
	// T3Project starts a new thread in that T3 project.
	T3Project string `json:"t3Project,omitempty"`
	// Command runs that program from destinations.json.
	Command string `json:"command,omitempty"`
	// Webhook posts to that webhook from destinations.json.
	Webhook string `json:"webhook,omitempty"`
}

// FlagEndRequest is POST /api/flags/end's optional body. An interrupt is
// posted the moment the person stops the agent, so what they then write
// arrives with the end, and so may who it is for: To then names the one
// session that must read it before driving again. On any other flag all
// three are ignored.
type FlagEndRequest struct {
	Note   string         `json:"note,omitempty"`
	Region []float64      `json:"region,omitempty"`
	To     *FlagRecipient `json:"to,omitempty"`
}

const errFlagTo = "to names an agent or a session, as /api/flags/listeners lists them, or a t3Thread or t3Project, as /api/flags/destinations lists them"

// FlagRect is a region in device pixels, as /api/ui reports them.
type FlagRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// FlagEvent is one input of the person's during a watch, in device pixels.
type FlagEvent struct {
	T    int64  `json:"t"` // unix milliseconds
	Kind string `json:"kind"`
	// From and To are [x, y] in device pixels; To only for a swipe.
	From    []int  `json:"from,omitempty"`
	To      []int  `json:"to,omitempty"`
	Text    string `json:"text,omitempty"`
	Button  string `json:"button,omitempty"`
	Keycode int    `json:"keycode,omitempty"`
}

// FlagSnapshot is the screen at one moment of a flag.
type FlagSnapshot struct {
	T int64 `json:"t"` // unix milliseconds
	// Screenshot is the path of the PNG, the region outlined in red;
	// empty when the capture failed, and ScreenshotError says why.
	Screenshot      string `json:"screenshot,omitempty"`
	ScreenshotError string `json:"screenshotError,omitempty"`
	// UI is the interactive tree, as /api/ui?interactive=1 returns it. Only
	// GET /api/flags?id= carries it; a list leaves it out.
	UI      []*target.Node `json:"ui,omitempty"`
	UIError string         `json:"uiError,omitempty"`
}

// Flag is one flag as the API returns it.
type Flag struct {
	ID     uint64 `json:"id"`
	Target string `json:"target"`
	// Kind is "interrupt" for a flag that stopped an agent, else empty.
	Kind string `json:"kind,omitempty"`
	// Rev changes whenever the flag does; GET ?after= compares it.
	Rev  uint64 `json:"rev"`
	Note string `json:"note,omitempty"`
	// Width and Height are the screen in device pixels when flagged, 0 when
	// the target could not say.
	Width  int       `json:"width,omitempty"`
	Height int       `json:"height,omitempty"`
	Region *FlagRect `json:"region,omitempty"`
	// ClaimedBy is the agent holding the target when it was flagged.
	ClaimedBy string `json:"claimedBy,omitempty"`
	// To is who the person sent it to; absent means every session.
	To *FlagRecipient `json:"to,omitempty"`
	// Delivery is how sending it to a T3 thread went, once it has gone.
	Delivery *FlagDelivery `json:"delivery,omitempty"`
	// Watching is true while the person is still reproducing.
	Watching bool         `json:"watching"`
	Start    FlagSnapshot `json:"start"`
	// End, Events and EventsTruncated arrive when a watch ends.
	End    *FlagSnapshot `json:"end,omitempty"`
	Events []FlagEvent   `json:"events,omitempty"`
	// EventsTruncated means some of the person's input is missing: the
	// session's event ring wrapped, or the session restarted, mid-watch.
	EventsTruncated bool `json:"eventsTruncated,omitempty"`
}

// FlagsResponse is GET /api/flags's list.
type FlagsResponse struct {
	// Rev is the newest revision on the server; pass it as ?after= to wait
	// for the next change.
	Rev   uint64 `json:"rev"`
	Flags []Flag `json:"flags"`
}

type storedFlag struct {
	Flag
	region   []float64 // as posted, for outlining the end screenshot
	shots    [2][]byte // start, end
	session  string    // event session key when the watch began
	startSeq uint64
	timer    *time.Timer
	// viewer is the viewer an interrupt pinned the hold for, 0 if none.
	viewer uint64
}

// interruptGate is an interrupt its agent has yet to read. gate is the
// claim's caller key when the target was claimed, so only the holder is
// stopped; empty stops every agent, since any of them may have been driving.
type interruptGate struct {
	id   uint64
	gate string
}

// FlagListener is a session waiting on GET /api/flags for new flags, as
// loupe mcp does.
type FlagListener struct {
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	// Holds is true when this session holds the target asked about.
	Holds bool `json:"holds,omitempty"`
	// Target and IdleMs, on an active session, are the device it last
	// called about and how long ago.
	Target string `json:"target,omitempty"`
	IdleMs int64  `json:"idleMs,omitempty"`
}

// FlagListenersResponse is GET /api/flags/listeners: who would hear a flag
// raised now, and who holds the target.
type FlagListenersResponse struct {
	Listeners []FlagListener `json:"listeners"`
	// Active is the sessions that called the agent API in the last 15
	// minutes but are not waiting on flags, as a session driving over HTTP
	// is not: nothing is pushed to them, but a flag can be sent to one.
	Active []FlagListener `json:"active"`
	// ClaimedBy is the agent holding ?target=, listening or not.
	ClaimedBy string `json:"claimedBy,omitempty"`
	// Holder is the session holding ?target=, listening or not, to send a
	// flag to with to.
	Holder *FlagRecipient `json:"holder,omitempty"`
}

type flagListener struct {
	agent, session string
	polls          int       // long-polls in flight
	seen           time.Time // when the last one answered
}

// activeSession is an agent session's last call to the agent API.
type activeSession struct {
	agent, session, target string
	seen                   time.Time
}

type flagStore struct {
	active map[string]*activeSession
	// interrupts is each target's interrupt still to be read, live or ended.
	interrupts map[string]interruptGate
	mu         sync.Mutex
	next       uint64
	rev        uint64
	flags      []*storedFlag // oldest first
	changed    chan struct{} // closed and replaced on every change
	listeners  map[string]*flagListener
}

func newFlagStore() *flagStore {
	return &flagStore{changed: make(chan struct{}), listeners: map[string]*flagListener{}, active: map[string]*activeSession{}, interrupts: map[string]interruptGate{}}
}

// interrupted returns the interrupt that stops caller key on targetID, if
// any. A nil store, as in tests that build a bare Server, stops no one. An
// interrupt replaced by a newer one on the same target, or dropped
// from the store, stops no one.
func (st *flagStore) interrupted(targetID, key string) (uint64, bool) {
	if st == nil {
		return 0, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	g, ok := st.interrupts[targetID]
	if !ok {
		return 0, false
	}
	if st.get(g.id) == nil {
		delete(st.interrupts, targetID)
		return 0, false
	}
	if g.gate != "" && g.gate != key {
		return 0, false
	}
	return g.id, true
}

// read lifts interrupt id for caller key once its end has been captured: an
// agent that read it mid-hold has yet to see the screen the person left.
func (st *flagStore) read(id uint64, key string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	f := st.get(id)
	if f == nil || f.Kind != FlagInterrupt || f.Watching {
		return
	}
	if g, ok := st.interrupts[f.Target]; ok && g.id == id && (g.gate == "" || g.gate == key) {
		delete(st.interrupts, f.Target)
	}
}

// listen marks a long-poll from agent and session as begun, and returns the
// call that marks it done. A caller naming neither is not counted.
func (st *flagStore) listen(agent, session string) func() {
	agent, session = agentName(agent), agentName(session)
	key := callerKey(agent, session)
	if key == "" {
		return func() {}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	l := st.listeners[key]
	if l == nil {
		l = &flagListener{agent: agent, session: session}
		st.listeners[key] = l
	}
	l.polls++
	return func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		l.polls--
		l.seen = time.Now()
	}
}

// saw notes an agent API call from agent and session about targetID.
func (st *flagStore) saw(agent, session, targetID string) {
	key := callerKey(agent, session)
	if st == nil || key == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.active[key] = &activeSession{agent: agent, session: session, target: targetID, seen: time.Now()}
}

// activeOnly returns the sessions seen within activeWindow that are not
// in listening, most recent first, dropping older ones.
func (st *flagStore) activeOnly(listening []FlagListener) []FlagListener {
	st.mu.Lock()
	defer st.mu.Unlock()
	skip := map[string]bool{}
	for _, l := range listening {
		skip[callerKey(l.Agent, l.Session)] = true
	}
	out := []FlagListener{}
	for key, a := range st.active {
		idle := time.Since(a.seen)
		if idle > activeWindow {
			delete(st.active, key)
			continue
		}
		if !skip[key] {
			out = append(out, FlagListener{Agent: a.agent, Session: a.session, Target: a.target, IdleMs: idle.Milliseconds()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IdleMs < out[j].IdleMs })
	return out
}

// listening returns the sessions polling now or within listenerGrace,
// dropping the rest.
func (st *flagStore) listening() []FlagListener {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := []FlagListener{}
	for key, l := range st.listeners {
		if l.polls == 0 && time.Since(l.seen) > listenerGrace {
			delete(st.listeners, key)
			continue
		}
		out = append(out, FlagListener{Agent: l.agent, Session: l.session})
	}
	sort.Slice(out, func(i, j int) bool {
		return callerKey(out[i].Agent, out[i].Session) < callerKey(out[j].Agent, out[j].Session)
	})
	return out
}

// bumpLocked gives f a new revision and wakes every waiter. Called with mu held.
func (st *flagStore) bumpLocked(f *storedFlag) {
	st.rev++
	f.Rev = st.rev
	close(st.changed)
	st.changed = make(chan struct{})
}

func (st *flagStore) add(f *storedFlag) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.next++
	f.ID = st.next
	f.Start.Screenshot = flagShotPath(f, 0)
	if f.shots[0] == nil {
		f.Start.Screenshot = ""
	}
	st.flags = append(st.flags, f)
	if len(st.flags) > maxFlags {
		if old := st.flags[0]; old.timer != nil {
			old.timer.Stop()
		}
		st.flags = st.flags[1:]
	}
	st.bumpLocked(f)
}

func (st *flagStore) get(id uint64) *storedFlag {
	for _, f := range st.flags {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// list returns the flags revised after after, oldest first, without their
// UI trees, and the store's revision. target filters when not empty.
func (st *flagStore) list(after uint64, targetID string) FlagsResponse {
	st.mu.Lock()
	defer st.mu.Unlock()
	resp := FlagsResponse{Rev: st.rev, Flags: []Flag{}}
	for _, f := range st.flags {
		if f.Rev <= after || (targetID != "" && f.Target != targetID) {
			continue
		}
		g := f.Flag
		g.Start.UI = nil
		if g.End != nil {
			end := *g.End
			end.UI = nil
			g.End = &end
		}
		resp.Flags = append(resp.Flags, g)
	}
	return resp
}

// wait blocks until a flag is revised after after, or ctx ends.
func (st *flagStore) wait(ctx context.Context, after uint64, targetID string) (FlagsResponse, error) {
	for {
		st.mu.Lock()
		ch := st.changed
		st.mu.Unlock()
		if resp := st.list(after, targetID); len(resp.Flags) > 0 {
			return resp, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return FlagsResponse{}, ctx.Err()
		}
	}
}

func flagShotPath(f *storedFlag, which int) string {
	name := "start"
	if which == 1 {
		name = "end"
	}
	return fmt.Sprintf("/api/flags/shot?id=%d&which=%s", f.ID, name)
}

// handleFlags lists flags (GET) and takes a new one (POST).
func (s *Server) handleFlags(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.listFlags(w, r)
		return
	}
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	var req FlagRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad flag JSON: " + err.Error()})
		return
	}
	if !utf8.ValidString(req.Note) || utf8.RuneCountInString(req.Note) > maxFlagNote {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("note must be UTF-8, at most %d characters", maxFlagNote)})
		return
	}
	if req.Region != nil && !validRegion(req.Region) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: errFlagRegion})
		return
	}
	if req.Kind != "" && req.Kind != FlagInterrupt {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: `kind is empty or "interrupt"`})
		return
	}
	interrupt := req.Kind == FlagInterrupt
	to, ok := s.recipient(req.To)
	if !ok {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: errFlagTo})
		return
	}
	req.To = to

	f := &storedFlag{region: req.Region}
	f.Target = t.ID()
	f.Kind = req.Kind
	f.Note = req.Note
	f.To = req.To
	f.Watching = req.Watch || interrupt

	gate := ""
	if s.claims != nil {
		s.claims.mu.Lock()
		if c := s.claims.live(t.ID()); c != nil {
			f.ClaimedBy = c.agent
			gate = c.key()
		}
		s.claims.mu.Unlock()
	}
	if f.To != nil && !f.To.IsExternal() {
		// Sent to one session: that one is stopped, whoever holds the claim.
		// A T3 thread or a program may never have driven the device, so the
		// holder still re-checks the screen.
		gate = callerKey(f.To.Agent, f.To.Session)
	}
	if interrupt {
		// Hold the device first: the agent's next action must not land
		// while the screenshot is taken.
		if c := s.inputCaller(r, t.ID()); c.viewer != 0 {
			if sess, ok := s.mgr.Session(t.ID()); ok && sess.PinHold(c.viewer, time.Now().Add(maxWatch)) == nil {
				f.viewer = c.viewer
			}
		}
	}

	ctx := s.peerReadCtx(r.Context(), r, t)
	sctx, cancel := context.WithTimeout(ctx, agentTimeout)
	f.Width, f.Height = agentScreenSize(sctx, t)
	cancel()
	if req.Region != nil && f.Width > 0 {
		f.Region = regionPixels(req.Region, f.Width, f.Height)
	}
	f.Start, f.shots[0] = captureFlag(ctx, t, req.Region)

	if f.Watching {
		if sess, ok := s.mgr.Session(t.ID()); ok {
			page := sess.EventsSince(^uint64(0))
			f.session, f.startSeq = page.Session, page.Latest
		}
	}
	s.flags.add(f)
	if interrupt {
		s.flags.mu.Lock()
		s.flags.interrupts[f.Target] = interruptGate{id: f.ID, gate: gate}
		s.flags.mu.Unlock()
	}
	if f.Watching {
		id := f.ID
		f.timer = time.AfterFunc(maxWatch, func() { s.endFlag(context.Background(), nil, id) })
	}
	s.traceAgent(r, t.ID(), "flag", nil)
	if !f.Watching {
		s.deliverFlag(f.ID)
	}
	writeJSON(w, http.StatusOK, s.flagView(f.ID))
}

func (s *Server) listFlags(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if v := q.Get("id"); v != "" {
		id, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "id must be a flag id"})
			return
		}
		f := s.flagView(id)
		if f == nil {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: fmt.Sprintf("no flag %d; the server keeps the last %d", id, maxFlags)})
			return
		}
		// A person's app names its viewer; its reads, such as watching a
		// delivery, are not the agent catching up on an interrupt.
		if r.Header.Get(HeaderViewer) == "" {
			s.flags.read(id, callerKey(agentName(r.Header.Get(HeaderAgent)), agentName(r.Header.Get(HeaderSession))))
		}
		writeJSON(w, http.StatusOK, f)
		return
	}
	var after uint64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "after must be a revision"})
			return
		}
		after = n
	}
	targetID := q.Get("target")
	v := q.Get("waitMs")
	if v == "" {
		writeJSON(w, http.StatusOK, s.flags.list(after, targetID))
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "waitMs must be a number of milliseconds"})
		return
	}
	wait := min(time.Duration(n)*time.Millisecond, flagsWaitMax)
	if n == 0 {
		wait = flagsWaitDefault
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	defer s.flags.listen(r.Header.Get(HeaderAgent), r.Header.Get(HeaderSession))()
	resp, err := s.flags.wait(ctx, after, targetID)
	if err != nil {
		// No change in time is an empty list, so a caller can keep its rev.
		if r.Context().Err() == nil {
			writeJSON(w, http.StatusOK, s.flags.list(^uint64(0), ""))
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleFlagListeners answers who is waiting for flags, so the person can
// see whether a flag will reach anyone. ?target= adds who holds it.
func (s *Server) handleFlagListeners(w http.ResponseWriter, r *http.Request) {
	resp := FlagListenersResponse{Listeners: s.flags.listening()}
	resp.Active = s.flags.activeOnly(resp.Listeners)
	if id := r.URL.Query().Get("target"); id != "" && s.claims != nil {
		s.claims.mu.Lock()
		if c := s.claims.live(id); c != nil {
			resp.ClaimedBy = c.agent
			resp.Holder = &FlagRecipient{Agent: c.agent, Session: c.session}
			for _, ls := range [][]FlagListener{resp.Listeners, resp.Active} {
				for i := range ls {
					ls[i].Holds = callerKey(ls[i].Agent, ls[i].Session) == c.key()
				}
			}
		}
		s.claims.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, resp)
}

// flagView is flag id in full, UI trees included, or nil.
func (s *Server) flagView(id uint64) *Flag {
	s.flags.mu.Lock()
	defer s.flags.mu.Unlock()
	f := s.flags.get(id)
	if f == nil {
		return nil
	}
	g := f.Flag
	return &g
}

// handleFlagEnd ends a watch: POST /api/flags/end?id=.
func (s *Server) handleFlagEnd(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing ?id= of the flag to end"})
		return
	}
	var req FlagEndRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil && err != io.EOF {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad flag end JSON: " + err.Error()})
			return
		}
	}
	if !utf8.ValidString(req.Note) || utf8.RuneCountInString(req.Note) > maxFlagNote {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("note must be UTF-8, at most %d characters", maxFlagNote)})
		return
	}
	if req.Region != nil && !validRegion(req.Region) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: errFlagRegion})
		return
	}
	to, ok := s.recipient(req.To)
	if !ok {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: errFlagTo})
		return
	}
	s.flags.mu.Lock()
	if f := s.flags.get(id); f != nil && f.Kind == FlagInterrupt && f.Watching {
		if to != nil {
			// Picked once the agents were already stopped: from here on
			// only this session must read it, and the others go on.
			f.To = to
			if g, ok := s.flags.interrupts[f.Target]; ok && g.id == f.ID && !to.IsExternal() {
				g.gate = callerKey(to.Agent, to.Session)
				s.flags.interrupts[f.Target] = g
			}
		}
		if req.Note != "" {
			f.Note = req.Note
		}
		if req.Region != nil {
			f.region = req.Region
			if f.Width > 0 {
				f.Region = regionPixels(req.Region, f.Width, f.Height)
			}
		}
	}
	s.flags.mu.Unlock()
	f, status, msg := s.endFlag(r.Context(), r, id)
	if f == nil {
		writeJSON(w, status, ErrorResponse{Error: msg})
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// endFlag closes flag id's watch, capturing the screen again and the
// person's input since it began. r is nil when maxWatch ends it.
func (s *Server) endFlag(ctx context.Context, r *http.Request, id uint64) (*Flag, int, string) {
	s.flags.mu.Lock()
	f := s.flags.get(id)
	if f == nil {
		s.flags.mu.Unlock()
		return nil, http.StatusNotFound, fmt.Sprintf("no flag %d", id)
	}
	if !f.Watching {
		s.flags.mu.Unlock()
		return s.flagView(id), 0, ""
	}
	// Claimed for this end while the capture runs without the lock.
	f.Watching = false
	if f.timer != nil {
		f.timer.Stop()
	}
	targetID, region, sessKey, startSeq, pinned := f.Target, f.region, f.session, f.startSeq, f.viewer
	s.flags.mu.Unlock()
	if pinned != 0 {
		if sess, ok := s.mgr.Session(targetID); ok {
			sess.UnpinHold()
		}
	}

	var end FlagSnapshot
	var shot []byte
	var events []FlagEvent
	truncated := false
	if t, ok := s.mgr.Target(targetID); ok {
		if r != nil {
			ctx = s.peerReadCtx(ctx, r, t)
		}
		end, shot = captureFlag(ctx, t, region)
		if sess, ok := s.mgr.Session(targetID); ok {
			after := startSeq
			page := sess.EventsSince(after)
			if page.Session != sessKey {
				truncated = true
				page = sess.EventsSince(0)
			} else if page.Oldest > after+1 {
				truncated = true
			}
			sctx, cancel := context.WithTimeout(ctx, agentTimeout)
			sw, sh := agentScreenSize(sctx, t)
			cancel()
			events = flagEvents(page.Events, sw, sh)
		}
	} else {
		end = FlagSnapshot{T: time.Now().UnixMilli(), ScreenshotError: "target is gone", UIError: "target is gone"}
	}

	s.flags.mu.Lock()
	f = s.flags.get(id)
	if f != nil {
		f.End = &end
		f.shots[1] = shot
		if shot != nil {
			f.End.Screenshot = flagShotPath(f, 1)
		}
		f.Events = events
		f.EventsTruncated = truncated
		s.flags.bumpLocked(f)
	}
	s.flags.mu.Unlock()
	if r != nil {
		s.traceAgent(r, targetID, "flagEnd", nil)
	}
	s.deliverFlag(id)
	return s.flagView(id), 0, ""
}

// handleFlagShot serves a flag's screenshot: GET /api/flags/shot?id=&which=start|end.
func (s *Server) handleFlagShot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, err := strconv.ParseUint(q.Get("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing ?id="})
		return
	}
	which := 0
	switch q.Get("which") {
	case "", "start":
	case "end":
		which = 1
	default:
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "which is start or end"})
		return
	}
	s.flags.mu.Lock()
	var shot []byte
	if f := s.flags.get(id); f != nil {
		shot = f.shots[which]
	}
	s.flags.mu.Unlock()
	if shot == nil {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no such screenshot"})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(shot)
}

// captureFlag reads t's screenshot and interactive UI tree. A failure of
// either is recorded on the snapshot rather than failing the flag: the
// person's note is worth keeping on a screen that cannot be captured.
func captureFlag(ctx context.Context, t target.Target, region []float64) (FlagSnapshot, []byte) {
	snap := FlagSnapshot{T: time.Now().UnixMilli()}
	var shot []byte
	var wg sync.WaitGroup
	if sc, ok := t.(target.Screenshotter); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, screenshotTimeout)
			defer cancel()
			raw, err := sc.Screenshot(c)
			if err != nil {
				snap.ScreenshotError = err.Error()
				return
			}
			shot = outlineRegion(raw, region)
		}()
	} else {
		snap.ScreenshotError = "target cannot be captured"
	}
	if insp, ok := t.(target.Inspector); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, agentTimeout)
			defer cancel()
			ui, err := insp.Inspect(c)
			if err != nil {
				snap.UIError = err.Error()
				return
			}
			snap.UI = target.Prune(ui.Root)
		}()
	} else {
		snap.UIError = "target has no view hierarchy"
	}
	wg.Wait()
	return snap, shot
}

const errFlagRegion = "region is [left, top, right, bottom], fractions in [0, 1], left < right and top < bottom"

func validRegion(r []float64) bool {
	if len(r) != 4 {
		return false
	}
	for _, v := range r {
		if !(v >= 0 && v <= 1) {
			return false
		}
	}
	return r[0] < r[2] && r[1] < r[3]
}

func regionPixels(r []float64, w, h int) *FlagRect {
	x0, y0 := int(r[0]*float64(w)), int(r[1]*float64(h))
	x1, y1 := int(r[2]*float64(w)), int(r[3]*float64(h))
	return &FlagRect{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0}
}

// flagEvents keeps the person's input from events, in device pixels.
func flagEvents(events []session.Event, w, h int) []FlagEvent {
	px := func(p *session.Point) []int {
		if p == nil || w <= 0 || h <= 0 {
			return nil
		}
		return []int{int(p.X * float64(w)), int(p.Y * float64(h))}
	}
	var out []FlagEvent
	for _, e := range events {
		if e.Source != "viewer" {
			continue
		}
		out = append(out, FlagEvent{
			T: e.T, Kind: e.Kind, From: px(e.From), To: px(e.To),
			Text: e.Text, Button: e.Button, Keycode: e.Keycode,
		})
	}
	return out
}

// outlineRegion draws region on the PNG in red, so the agent sees what the
// person marked without mapping it. The PNG comes back unchanged when there
// is no region or it does not decode.
func outlineRegion(raw []byte, region []float64) []byte {
	if region == nil {
		return raw
	}
	src, err := imgpng.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw
	}
	b := src.Bounds()
	img := image.NewRGBA(b)
	draw.Draw(img, b, src, b.Min, draw.Src)
	x0 := b.Min.X + int(region[0]*float64(b.Dx()))
	y0 := b.Min.Y + int(region[1]*float64(b.Dy()))
	x1 := b.Min.X + int(region[2]*float64(b.Dx()))
	y1 := b.Min.Y + int(region[3]*float64(b.Dy()))
	stroke := max(b.Dx()/200, 3)
	red := image.NewUniform(color.RGBA{0xff, 0x2d, 0x2d, 0xff})
	for _, r := range []image.Rectangle{
		image.Rect(x0, y0, x1, y0+stroke),
		image.Rect(x0, y1-stroke, x1, y1),
		image.Rect(x0, y0, x0+stroke, y1),
		image.Rect(x1-stroke, y0, x1, y1),
	} {
		draw.Draw(img, r.Intersect(b), red, image.Point{}, draw.Src)
	}
	var out bytes.Buffer
	if err := imgpng.Encode(&out, img); err != nil {
		return raw
	}
	return out.Bytes()
}
