package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

func requestEvents(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleEvents(w, httptest.NewRequest(method, path, nil))
	return w
}

func getEvents(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return requestEvents(t, s, http.MethodGet, path)
}

// decodePage unmarshals a handleEvents body into an EventPage, failing the
// test on decode error so callers do not have to repeat the boilerplate.
func decodePage(t *testing.T, rec *httptest.ResponseRecorder) session.EventPage {
	t.Helper()
	var got session.EventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	return got
}

// TestEventsAfterReturnsOnlyTheLaterOnes exercises the boundary at the seq the
// caller already has: after=1 must exclude seq 1 and include everything past
// it, in order.
func TestEventsAfterReturnsOnlyTheLaterOnes(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	sess, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	for i := 0; i < 3; i++ {
		sess.PublishEvent(session.Event{Source: "agent", Kind: "tap"})
	}

	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?target=stub&after=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodePage(t, rec)
	if len(got.Events) != 2 || got.Events[0].Seq != 2 || got.Events[1].Seq != 3 {
		t.Fatalf("events = %+v; want seqs 2, 3", got.Events)
	}
}

// TestEventsAfterZeroReturnsEverythingKept pins the lower boundary: after=0
// (also what an empty ?after= parses to) must include the oldest event still
// in the ring, seq 1.
func TestEventsAfterZeroReturnsEverythingKept(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	sess, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	sess.PublishEvent(session.Event{Source: "agent", Kind: "tap"})
	sess.PublishEvent(session.Event{Source: "agent", Kind: "tap"})

	s := &Server{mgr: m}

	rec := getEvents(t, s, "/api/events?target=stub&after=0")
	got := decodePage(t, rec)
	if len(got.Events) != 2 || got.Events[0].Seq != 1 {
		t.Fatalf("after=0: events = %+v; want seqs 1, 2", got.Events)
	}

	// An empty after= must behave the same as after=0, not as a parse error.
	recEmpty := getEvents(t, s, "/api/events?target=stub&after=")
	if recEmpty.Code != http.StatusOK {
		t.Fatalf("after=: status = %d: %s", recEmpty.Code, recEmpty.Body.String())
	}
	gotEmpty := decodePage(t, recEmpty)
	if len(gotEmpty.Events) != 2 || gotEmpty.Events[0].Seq != 1 {
		t.Fatalf("after=: events = %+v; want seqs 1, 2", gotEmpty.Events)
	}
}

// TestEventsAfterTheNewestSeqReturnsNothing pins the upper boundary: asking
// for events after the newest seq the ring holds must come back empty, not
// error and not replay anything already seen.
func TestEventsAfterTheNewestSeqReturnsNothing(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	sess, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	sess.PublishEvent(session.Event{Source: "agent", Kind: "tap"})
	sess.PublishEvent(session.Event{Source: "agent", Kind: "tap"})

	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?target=stub&after=2")
	got := decodePage(t, rec)
	if len(got.Events) != 0 {
		t.Fatalf("after=2 (the newest seq): events = %+v; want none", got.Events)
	}
}

// TestEventsWithNoSessionIsNotAnError is the spec's explicit case: a target
// that exists but has never been streamed to has no session, and that is not
// the same as a failure. It reports 200 with an empty list, since nothing has
// happened yet, rather than 404, and the session/oldest/latest fields report
// their zero values rather than being omitted.
func TestEventsWithNoSessionIsNotAnError(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})

	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?target=stub&after=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodePage(t, rec)
	if got.Events == nil || len(got.Events) != 0 {
		t.Fatalf("events = %+v; want an empty, non-null list", got.Events)
	}
	if got.Session != "" || got.Oldest != 0 || got.Latest != 0 {
		t.Fatalf("page = %+v; want the zero session/oldest/latest", got)
	}
}

// TestEventsMissingTargetIs400 mirrors agentTarget's own behaviour for every
// other agent endpoint: a request naming no target at all is a caller bug,
// not "no session yet".
func TestEventsMissingTargetIs400(t *testing.T) {
	m := NewManager(target.Options{})
	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?after=0")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestEventsNonNumericAfterIs400 pins the parse-failure branch distinctly
// from the empty-string branch above, which must succeed.
func TestEventsNonNumericAfterIs400(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?target=stub&after=nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestEventsAfterMaxUint64IsValid pins the top of the parseable range: the
// largest uint64 must parse (not overflow) and simply return no events, since
// nothing can have a seq past it. A ParseUint bit size of 32 fails this,
// because the value does not fit a uint32.
func TestEventsAfterMaxUint64IsValid(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?target=stub&after=18446744073709551615")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodePage(t, rec)
	if len(got.Events) != 0 {
		t.Fatalf("events = %+v; want none past the maximum seq", got.Events)
	}
}

// TestEventsAfterNegativeIs400 pins that after is unsigned: a ParseInt in
// place of ParseUint would accept -1 and this must not.
func TestEventsAfterNegativeIs400(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?target=stub&after=-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestEventsAfterOverflowIs400WithMessage pins one past the top of the range,
// 2^64, and the wording of the error -- a caller integrating against this
// needs to know what kind of value was rejected.
func TestEventsAfterOverflowIs400WithMessage(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?target=stub&after=18446744073709551616")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got["error"], "unsigned 64-bit integer") {
		t.Fatalf("error = %q; want it to name the type", got["error"])
	}
}

// TestEventsBadAfterBodyHasNoEventsKey pins the 400 response's shape: an
// error key and nothing else. A handler that forgets to return after writing
// the 400 falls through to writeJSON(200, page) and writes a second JSON
// value onto the same body, which this catches two ways -- the concatenated
// bytes fail to decode as one JSON value at all, and even if they somehow
// didn't, an "events" key would appear in a body that must not have one.
func TestEventsBadAfterBodyHasNoEventsKey(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?target=stub&after=nope")

	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	if _, ok := got["error"]; !ok {
		t.Fatalf("missing error key: %s", rec.Body.String())
	}
	if _, ok := got["events"]; ok {
		t.Fatalf("unexpected events key in an error body: %s", rec.Body.String())
	}
}

// TestEventsRejectsAnythingButGetAndHead mirrors handleInput's own
// method-check pattern for the endpoints going the other way: a caller
// sending anything but GET or HEAD gets 405 with the allowed method named.
func TestEventsRejectsAnythingButGetAndHead(t *testing.T) {
	m := NewManager(target.Options{})
	s := &Server{mgr: m}
	rec := requestEvents(t, s, http.MethodPost, "/api/events?target=stub")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow = %q, want %q", got, http.MethodGet)
	}
}

// TestEventsAcceptsHead pins the other side of the same boundary: HEAD is not
// rejected the way every other non-GET method is.
func TestEventsAcceptsHead(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	rec := requestEvents(t, s, http.MethodHead, "/api/events?target=stub")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestEventsDetectsARestartedSession is the case the plain seq gap cannot
// express: the caller was reading a session that ended and a different one
// took its place. A restart resets seq to 1, so the new session's latest is
// lower than an `after` the caller was already holding from the old one --
// and, independently, the session id itself has changed.
func TestEventsDetectsARestartedSession(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})

	first, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i := 0; i < 3; i++ {
		first.PublishEvent(session.Event{Source: "agent", Kind: "tap"})
	}
	firstPage := first.EventsSince(0)
	if firstPage.Latest != 3 {
		t.Fatalf("firstPage.Latest = %d, want 3", firstPage.Latest)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("Acquire (2): %v", err)
	}
	defer second.Close()
	second.PublishEvent(session.Event{Source: "agent", Kind: "tap"})

	s := &Server{mgr: m}
	// The caller was holding after=3 from the first session.
	rec := getEvents(t, s, "/api/events?target=stub&after=3")
	got := decodePage(t, rec)

	if got.Session == firstPage.Session {
		t.Fatalf("session id did not change across a restart: %q", got.Session)
	}
	if got.Latest >= 3 {
		t.Fatalf("second session's latest = %d, want it below the stale after=3", got.Latest)
	}
	// Either signal alone is what a caller checks before deciding to refetch
	// from after=0; both hold here.
	if !(got.Session != firstPage.Session && 3 > got.Latest) {
		t.Fatalf("no restart signal present: page = %+v, firstPage = %+v", got, firstPage)
	}
}

// TestEventsWrappedRingReportsOldestPastAfter pins the ring-wrap signal: once
// more than eventRingSize events have been published, the oldest one the
// ring still holds has moved past what an old caller was asking for, and
// Oldest says so rather than silently returning nothing.
func TestEventsWrappedRingReportsOldestPastAfter(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	sess, err := m.Acquire(t.Context(), "stub")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer sess.Close()

	// The ring holds 256; publishing past that wraps it, so seq 1 is long
	// gone by the time this asks for it.
	for i := 0; i < 300; i++ {
		sess.PublishEvent(session.Event{Source: "agent", Kind: "tap"})
	}

	s := &Server{mgr: m}
	rec := getEvents(t, s, "/api/events?target=stub&after=1")
	got := decodePage(t, rec)
	if got.Oldest <= 2 {
		t.Fatalf("oldest = %d; want > after(1)+1, the ring should have wrapped past it", got.Oldest)
	}
}

// TestEventsThroughTheRealMuxNeedsAuth runs the endpoint through the actual
// mux and auth middleware, not handleEvents called directly -- the only test
// here that would fail if the route were dropped from Handler() or if
// /api/events were added to the auth exemption list alongside /healthz and
// the transport endpoints.
func TestEventsThroughTheRealMuxNeedsAuth(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&stubTarget{})
	s := &Server{mgr: m, token: "secret"}
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events?target=stub", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/events?target=stub", nil)
	r.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer token: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"session", "oldest", "latest", "events"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing %q in response: %s", key, rec.Body.String())
		}
	}
}
