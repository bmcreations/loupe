package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// claimServer has dev1 and dev2 registered, nothing open, and a clock the
// test moves.
func claimServer(t *testing.T) (*Server, *time.Time) {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(&fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400})
	m.Register(&fakeAgentTarget{id: "dev2", screenW: 1080, screenH: 2400})
	s := &Server{mgr: m, claims: newClaimBook(nil)}
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	s.claims.now = func() time.Time { return now }
	return s, &now
}

func claimReq(s *Server, method, query, body string, hdr map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/api/claims?"+query, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	s.handleClaims(w, r)
	return w
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var e ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return e
}

const tap = `{"action":"tap","x":1,"y":1}`

func TestClaimCodesAreStable(t *testing.T) {
	if ErrCodeClaimed != "claimedBy" || ErrCodeHandsOff != "handsOff" || ErrCodeClaimLapsed != "claimLapsed" {
		t.Fatalf("codes changed: %q %q %q", ErrCodeClaimed, ErrCodeHandsOff, ErrCodeClaimLapsed)
	}
}

func TestAClaimRefusesOtherAgentsWithNoSessionOpen(t *testing.T) {
	s, _ := claimServer(t)
	a := map[string]string{HeaderAgent: "loupe: xctest", HeaderSession: "local_a"}
	b := map[string]string{HeaderAgent: "loupe: picker", HeaderSession: "local_b"}

	if rec := claimReq(s, http.MethodPost, "target=dev1", `{"link":"https://claude.ai/code/x"}`, a); rec.Code != http.StatusOK {
		t.Fatalf("claim = %d: %s", rec.Code, rec.Body.String())
	}
	rec := postInputAs(s, "dev1", tap, b)
	if rec.Code != http.StatusConflict {
		t.Fatalf("other agent's tap = %d, want 409", rec.Code)
	}
	e := decodeErr(t, rec)
	if e.Code != ErrCodeClaimed || e.Claim == nil || e.Claim.Agent != "loupe: xctest" || e.Claim.Link != "https://claude.ai/code/x" {
		t.Fatalf("refusal = %+v (claim %+v)", e, e.Claim)
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("a claim has no retry time, so no Retry-After")
	}
	if rec := postInputAs(s, "dev1", tap, a); rec.Code != http.StatusOK {
		t.Fatalf("holder's tap = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postInputAs(s, "dev2", tap, b); rec.Code != http.StatusOK {
		t.Fatalf("a claim on dev1 refused dev2: %d", rec.Code)
	}
	if rec := claimReq(s, http.MethodPost, "target=dev1", "", b); rec.Code != http.StatusConflict {
		t.Fatalf("second claim = %d, want 409", rec.Code)
	}
}

// Another agent's claim refuses a move too: no one points at a device
// someone else holds.
func TestAClaimRefusesAnotherAgentsMove(t *testing.T) {
	s, _ := claimServer(t)
	a := map[string]string{HeaderAgent: "loupe: xctest", HeaderSession: "local_a"}
	b := map[string]string{HeaderAgent: "loupe: picker", HeaderSession: "local_b"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	rec := postInputAs(s, "dev1", `{"action":"move","x":1,"y":1}`, b)
	if rec.Code != http.StatusConflict || decodeErr(t, rec).Code != ErrCodeClaimed {
		t.Fatalf("other agent's move = %d %s; want 409 claimedBy", rec.Code, rec.Body.String())
	}
}

// Two sessions of the same agent are two callers.
func TestTheSessionNotTheNameHoldsTheClaim(t *testing.T) {
	s, _ := claimServer(t)
	claimReq(s, http.MethodPost, "target=dev1", "", map[string]string{HeaderAgent: "claude", HeaderSession: "local_a"})
	rec := postInputAs(s, "dev1", tap, map[string]string{HeaderAgent: "claude", HeaderSession: "local_b"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("same name, other session = %d, want 409", rec.Code)
	}
	if rec := postInputAs(s, "dev1", tap, map[string]string{HeaderAgent: "claude"}); rec.Code != http.StatusConflict {
		t.Fatalf("same name, no session = %d, want 409", rec.Code)
	}
}

func TestAClaimLapsesAfterTheLeaseAndCallsRenewIt(t *testing.T) {
	s, now := claimServer(t)
	a := map[string]string{HeaderAgent: "a"}
	b := map[string]string{HeaderAgent: "b"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)

	// A read from the holder renews it.
	*now = now.Add(claimLease - time.Minute)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/ui?target=dev1", nil)
	r.Header.Set(HeaderAgent, "a")
	if _, ok := s.agentTarget(w, r); !ok {
		t.Fatal("agentTarget refused dev1")
	}
	*now = now.Add(claimLease - time.Minute)
	if rec := postInputAs(s, "dev1", tap, b); rec.Code != http.StatusConflict {
		t.Fatalf("renewed claim did not hold: %d", rec.Code)
	}
	// Another agent's calls do not.
	*now = now.Add(2 * time.Minute)
	if rec := postInputAs(s, "dev1", tap, b); rec.Code != http.StatusOK {
		t.Fatalf("lapsed claim still refused: %d %s", rec.Code, rec.Body.String())
	}
	if got := s.claims.list(); len(got) != 0 {
		t.Fatalf("lapsed claim still listed: %+v", got)
	}
}

// A holder that stops calling for claimLease loses the claim. Its next input
// must say so, in a code of its own, instead of passing through unclaimed.
func TestALapsedHolderIsToldItsClaimLapsed(t *testing.T) {
	s, now := claimServer(t)
	a := map[string]string{HeaderAgent: "a", HeaderSession: "sa"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)

	*now = now.Add(claimLease)
	rec := postInputAs(s, "dev1", tap, a)
	if rec.Code != http.StatusConflict {
		t.Fatalf("lapsed holder's tap = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if code := decodeErr(t, rec).Code; code != "claimLapsed" {
		t.Fatalf("code = %q, want claimLapsed", code)
	}
}

func TestLapseRefusalOnlyReachesTheFormerHolder(t *testing.T) {
	s, now := claimServer(t)
	a := map[string]string{HeaderAgent: "a", HeaderSession: "sa"}
	b := map[string]string{HeaderAgent: "b", HeaderSession: "sb"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	*now = now.Add(claimLease)
	if rec := postInputAs(s, "dev1", tap, b); rec.Code != http.StatusOK {
		t.Fatalf("caller that never held it = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := postInputAs(s, "dev2", tap, a); rec.Code != http.StatusOK {
		t.Fatalf("holder on another target = %d, want 200", rec.Code)
	}
}

func TestAReleaseLeavesNoLapseMarker(t *testing.T) {
	s, now := claimServer(t)
	a := map[string]string{HeaderAgent: "a", HeaderSession: "sa"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	if rec := claimReq(s, http.MethodDelete, "target=dev1", "", a); rec.Code != http.StatusNoContent {
		t.Fatalf("release = %d", rec.Code)
	}
	*now = now.Add(2 * claimLease)
	if rec := postInputAs(s, "dev1", tap, a); rec.Code != http.StatusOK {
		t.Fatalf("input after own release = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// The refusal repeats until the holder claims again, so an agent that ignores
// it once does not drive an unclaimed device on its retry.
func TestReclaimingClearsTheLapseMarker(t *testing.T) {
	s, now := claimServer(t)
	a := map[string]string{HeaderAgent: "a", HeaderSession: "sa"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	*now = now.Add(claimLease)
	for i := 0; i < 2; i++ {
		if rec := postInputAs(s, "dev1", tap, a); rec.Code != http.StatusConflict || decodeErr(t, rec).Code != ErrCodeClaimLapsed {
			t.Fatalf("input %d = %d %s, want 409 claimLapsed", i, rec.Code, rec.Body.String())
		}
	}
	if rec := claimReq(s, http.MethodPost, "target=dev1", "", a); rec.Code != http.StatusOK {
		t.Fatalf("reclaim = %d", rec.Code)
	}
	if rec := postInputAs(s, "dev1", tap, a); rec.Code != http.StatusOK {
		t.Fatalf("input after reclaim = %d %s, want 200", rec.Code, rec.Body.String())
	}
	// Released after the reclaim, the old marker must not come back.
	claimReq(s, http.MethodDelete, "target=dev1", "", a)
	if rec := postInputAs(s, "dev1", tap, a); rec.Code != http.StatusOK {
		t.Fatalf("input after reclaim and release = %d, want 200", rec.Code)
	}
}

func TestLapseMarkerExpires(t *testing.T) {
	s, now := claimServer(t)
	a := map[string]string{HeaderAgent: "a", HeaderSession: "sa"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	*now = now.Add(claimLease)
	if rec := postInputAs(s, "dev1", tap, a); rec.Code != http.StatusConflict {
		t.Fatalf("lapsed input = %d, want 409", rec.Code)
	}
	*now = now.Add(claimLapsedFor)
	if rec := postInputAs(s, "dev1", tap, a); rec.Code != http.StatusOK {
		t.Fatalf("input after the marker expired = %d, want 200", rec.Code)
	}
}

func TestAnotherAgentsClaimAfterTheLapseStillAnswersClaimedBy(t *testing.T) {
	s, now := claimServer(t)
	a := map[string]string{HeaderAgent: "a", HeaderSession: "sa"}
	b := map[string]string{HeaderAgent: "b", HeaderSession: "sb"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	*now = now.Add(claimLease)
	// A list call observes the lapse before anyone refuses.
	s.claims.list()
	if rec := claimReq(s, http.MethodPost, "target=dev1", "", b); rec.Code != http.StatusOK {
		t.Fatalf("b's claim = %d", rec.Code)
	}
	rec := postInputAs(s, "dev1", tap, a)
	if rec.Code != http.StatusConflict || decodeErr(t, rec).Code != ErrCodeClaimed {
		t.Fatalf("former holder after b's claim = %d %s, want 409 claimedBy", rec.Code, rec.Body.String())
	}
}

func TestOnlyTheHolderReleasesUnlessForced(t *testing.T) {
	s, _ := claimServer(t)
	a := map[string]string{HeaderAgent: "a"}
	b := map[string]string{HeaderAgent: "b"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	if rec := claimReq(s, http.MethodDelete, "target=dev1", "", b); rec.Code != http.StatusConflict {
		t.Fatalf("other's release = %d, want 409", rec.Code)
	}
	if rec := claimReq(s, http.MethodDelete, "target=dev1&force=1", "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("forced release = %d", rec.Code)
	}
	if rec := claimReq(s, http.MethodDelete, "target=dev1", "", b); rec.Code != http.StatusNoContent {
		t.Fatalf("releasing a free target = %d, want 204", rec.Code)
	}
}

func TestAClaimNeedsANameAndAnHTTPSLink(t *testing.T) {
	s, _ := claimServer(t)
	if rec := claimReq(s, http.MethodPost, "target=dev1", "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("nameless claim = %d, want 400", rec.Code)
	}
	for _, link := range []string{"javascript:alert(1)", "http://x", "claude.ai/code", "https://" + strings.Repeat("a", 600)} {
		rec := claimReq(s, http.MethodPost, "target=dev1", `{"agent":"a","link":`+strconv.Quote(link)+`}`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("link %.30q = %d, want 400", link, rec.Code)
		}
	}
	if rec := claimReq(s, http.MethodPost, "target=nope", `{"agent":"a"}`, nil); rec.Code == http.StatusOK {
		t.Fatal("claimed an unknown target")
	}
}

// Hands-off refuses every agent, the claim's holder too, and never a person:
// neither a viewer of this target nor the picker on another one.
func TestHandsOffRefusesEveryAgentAndNoPerson(t *testing.T) {
	s, _ := claimServer(t)
	a := map[string]string{HeaderAgent: "a"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	if err := s.claims.setHandsOff("dev1", true); err != nil {
		t.Fatal(err)
	}
	rec := postInputAs(s, "dev1", tap, a)
	if rec.Code != http.StatusConflict || decodeErr(t, rec).Code != ErrCodeHandsOff {
		t.Fatalf("holder on hands-off = %d %s", rec.Code, rec.Body.String())
	}

	other, err := s.mgr.Acquire(t.Context(), "dev2")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	vid, _ := other.AddViewer()
	person := map[string]string{HeaderViewer: strconv.FormatUint(vid, 10)}
	if rec := postInputAs(s, "dev1", tap, person); rec.Code != http.StatusOK {
		t.Fatalf("person from another target's viewer = %d %s", rec.Code, rec.Body.String())
	}

	own, err := s.mgr.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	vid1, _ := own.AddViewer()
	if rec := postInputAs(s, "dev1", tap, map[string]string{HeaderViewer: strconv.FormatUint(vid1, 10)}); rec.Code != http.StatusOK {
		t.Fatalf("viewer of the target = %d", rec.Code)
	}

	// The viewer's tap above holds dev1, so the holder is now refused for
	// that, not for hands-off.
	s.claims.setHandsOff("dev1", false)
	if e := decodeErr(t, postInputAs(s, "dev1", tap, a)); e.Code != ErrCodeHumanHeld {
		t.Fatalf("holder after hands-off cleared: %+v", e)
	}
}

func TestHandsOffSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "handsoff.json")
	b := newClaimBook(nil)
	if err := b.load(path); err != nil {
		t.Fatal(err)
	}
	b.setHandsOff("ios:phone", true)
	b.setHandsOff("avd:Pixel_10", true)
	b.setHandsOff("avd:Pixel_10", false)

	again := newClaimBook(nil)
	if err := again.load(path); err != nil {
		t.Fatal(err)
	}
	rows := []TargetInfo{{ID: "ios:phone"}, {ID: "avd:Pixel_10"}}
	again.decorate(rows)
	if !rows[0].HandsOff || rows[1].HandsOff {
		t.Fatalf("after reload: %+v", rows)
	}
}

// A claim survives a restart with its holder, link and lease; one that lapsed
// while the server was down, or was released, does not come back.
func TestClaimsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claims.json")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	b := newClaimBook(nil)
	b.now = clock
	if err := b.load(filepath.Join(dir, "handsoff.json")); err != nil {
		t.Fatal(err)
	}
	if err := b.loadClaims("", path); err != nil {
		t.Fatal(err)
	}
	b.take("dev1", "claude", "s1", "https://example.com/s1")
	b.take("dev2", "claude", "s2", "")
	b.take("dev3", "claude", "s3", "")
	b.release("dev3", "claude", "s3", false)
	// dev1 is renewed past claimSaveEvery, which writes the newer seen; dev2
	// is not, so its saved seen is from the take.
	now = now.Add(claimLease - time.Minute)
	b.seen("dev1", "claude", "s1")
	now = now.Add(time.Minute + time.Second)

	again := newClaimBook(nil)
	again.now = clock
	if err := again.loadClaims("", path); err != nil {
		t.Fatal(err)
	}
	got := again.list()
	if len(got) != 1 || got[0].Target != "dev1" || got[0].Session != "s1" || got[0].Link != "https://example.com/s1" {
		t.Fatalf("restored = %+v", got)
	}
	if e := again.refusal("dev1", "claude", "s2"); e == nil || e.Code != ErrCodeClaimed {
		t.Fatalf("another session on the restored claim: %+v", e)
	}
	if e := again.refusal("dev1", "claude", "s1"); e != nil {
		t.Fatalf("holder on the restored claim: %+v", e)
	}
	// The hands-off file says only what the person marked.
	if _, err := os.Stat(filepath.Join(dir, "handsoff.json")); !os.IsNotExist(err) {
		t.Fatalf("claims touched the hands-off file: %v", err)
	}
}

// Two servers on one machine share the claims file, so a claim taken through
// one refuses another session through the other.
func TestAClaimRefusesOnEveryServerSharingTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	a, b := newClaimBook(nil), newClaimBook(nil)
	for _, book := range []*claimBook{a, b} {
		book.now = clock
		if err := book.loadClaims("", path); err != nil {
			t.Fatal(err)
		}
	}
	if _, holder := a.take("avd:Pixel_9", "claude", "review-A", ""); holder != nil {
		t.Fatalf("first claim refused: %+v", holder)
	}
	if _, holder := b.take("avd:Pixel_9", "claude", "review-B", ""); holder == nil || holder.Session != "review-A" {
		t.Fatalf("second server's claim: holder %+v, want review-A", holder)
	}
	if e := b.refusal("avd:Pixel_9", "claude", "review-B"); e == nil || e.Code != ErrCodeClaimed {
		t.Fatalf("second server's refusal = %+v, want claimedBy", e)
	}
}

// twoServers is two claim books sharing one claims file, as two loupe
// servers on one machine do, each with its own file beside its config.
func twoServers(t *testing.T) (a, b *claimBook, now *time.Time, dir string) {
	t.Helper()
	dir = t.TempDir()
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	now = &at
	var books [2]*claimBook
	for i := range books {
		books[i] = newClaimBook(nil)
		books[i].now = func() time.Time { return *now }
		local := filepath.Join(dir, "config"+strconv.Itoa(i), "claims.json")
		if err := books[i].loadClaims(local, filepath.Join(dir, "cache", "claims.json")); err != nil {
			t.Fatal(err)
		}
	}
	return books[0], books[1], now, dir
}

// The holder can release through another server, and a person's force
// release there ends another session's claim too.
func TestAClaimReleasesThroughAnotherServer(t *testing.T) {
	a, b, _, _ := twoServers(t)
	a.take("avd:x", "claude", "s1", "")
	if _, _, released := b.release("avd:x", "claude", "s1", false); !released || a.held("avd:x") {
		t.Fatalf("holder's release through b: released %v, a still holds %v", released, a.held("avd:x"))
	}
	a.take("avd:x", "claude", "s1", "")
	if holder, _, released := b.release("avd:x", "claude", "s2", false); holder == nil || released {
		t.Fatalf("another session's release through b: holder %+v released %v", holder, released)
	}
	if _, gone, released := b.release("avd:x", "", "", true); !released || gone != "claude" || a.held("avd:x") {
		t.Fatalf("forced release through b: released %v gone %q, a still holds %v", released, gone, a.held("avd:x"))
	}
}

// A boot holding the claim on one server keeps it from other agents on every
// server past the lease, and the holder's claim lapses as usual after it.
func TestAHoldKeepsTheClaimOnEveryServer(t *testing.T) {
	a, b, now, _ := twoServers(t)
	a.take("avd:x", "claude", "s1", "")
	unhold := a.hold("avd:x", "claude", "s1")
	*now = now.Add(claimLease + time.Minute)
	if _, holder := b.take("avd:x", "claude", "s2", ""); holder == nil || holder.IdleMs != 0 {
		t.Fatalf("another session's claim during the hold: holder %+v, want s1 and idle 0", holder)
	}
	unhold()
	*now = now.Add(claimLease - time.Second)
	if _, holder := b.take("avd:x", "claude", "s2", ""); holder == nil {
		t.Fatal("the hold's end did not renew the claim on b")
	}
	*now = now.Add(time.Second)
	if _, holder := b.take("avd:x", "claude", "s2", ""); holder != nil {
		t.Fatalf("claim outlived its lease after the hold: %+v", holder)
	}
}

// A server that dies mid-boot holds the device on the others no longer than
// a boot could have run.
func TestAHoldFromADeadServerEnds(t *testing.T) {
	a, b, now, _ := twoServers(t)
	a.take("avd:x", "claude", "s1", "")
	a.hold("avd:x", "claude", "s1") // never let go
	*now = now.Add(claimHoldMax - time.Second)
	if _, holder := b.take("avd:x", "claude", "s2", ""); holder == nil {
		t.Fatal("hold ended early on b")
	}
	*now = now.Add(time.Second)
	if _, holder := b.take("avd:x", "claude", "s2", ""); holder != nil {
		t.Fatalf("hold from a dead server outlived claimHoldMax: %+v", holder)
	}
}

// Once another server gives the claim to someone else, the holder's own
// server goes by the file: a renewal it has not saved yet, or a hold still
// running on it, does not keep the claim there.
func TestTheFileDecidesWhoHoldsAClaim(t *testing.T) {
	takenOver := func(t *testing.T, a, b *claimBook) {
		t.Helper()
		if _, holder := b.take("avd:x", "claude", "s2", ""); holder != nil {
			t.Fatalf("b refused s2 after the lapse: holder %+v", holder)
		}
		if e := a.refusal("avd:x", "claude", "s1"); e == nil || e.Code != ErrCodeClaimed {
			t.Fatalf("a's refusal of the old holder = %+v, want claimedBy", e)
		}
		if _, holder := a.take("avd:x", "claude", "s1", ""); holder == nil || holder.Session != "s2" {
			t.Fatalf("a let the old holder take it back: holder %+v", holder)
		}
		if got := a.list(); len(got) != 1 || got[0].Session != "s2" {
			t.Fatalf("a lists %+v, want s2's claim", got)
		}
	}

	t.Run("unsaved renewal", func(t *testing.T) {
		a, b, now, _ := twoServers(t)
		a.take("avd:x", "claude", "s1", "")
		*now = now.Add(claimSaveEvery / 2)
		a.seen("avd:x", "claude", "s1") // newer than the file, not written
		*now = now.Add(claimLease - claimSaveEvery/4)
		takenOver(t, a, b)
	})

	t.Run("hold past its bound", func(t *testing.T) {
		a, b, now, _ := twoServers(t)
		a.take("avd:x", "claude", "s1", "")
		unhold := a.hold("avd:x", "claude", "s1")
		*now = now.Add(claimHoldMax)
		takenOver(t, a, b)
		unhold()
		if got := b.list(); len(got) != 1 || got[0].Session != "s2" {
			t.Fatalf("the old hold's end changed b's claim: %+v", got)
		}
	})
}

// Calls through one server keep a claim taken through another, and a lapse
// one server notices is told to the holder on every server.
func TestALeaseIsSharedAcrossServers(t *testing.T) {
	a, b, now, _ := twoServers(t)
	a.take("avd:x", "claude", "s1", "")
	*now = now.Add(claimLease - time.Minute)
	b.seen("avd:x", "claude", "s1")
	*now = now.Add(2 * time.Minute)
	if !a.held("avd:x") {
		t.Fatal("a renewal through b did not reach a")
	}
	*now = now.Add(claimLease)
	if got := b.list(); len(got) != 0 {
		t.Fatalf("b lists %+v after the lease", got)
	}
	for name, book := range map[string]*claimBook{"a": a, "b": b} {
		if e := book.refusal("avd:x", "claude", "s1"); e == nil || e.Code != ErrCodeClaimLapsed {
			t.Fatalf("%s's refusal of the lapsed holder = %+v, want claimLapsed", name, e)
		}
		if e := book.refusal("avd:x", "claude", "s2"); e != nil {
			t.Fatalf("%s refused another session after the lapse: %+v", name, e)
		}
	}
}

// The synthetic target is each server's own, so its claims stay in each
// server's own file.
func TestSyntheticClaimsStayOnTheirServer(t *testing.T) {
	a, b, _, dir := twoServers(t)
	a.take("synthetic", "claude", "s1", "")
	if _, holder := b.take("synthetic", "claude", "s2", ""); holder != nil {
		t.Fatalf("b's synthetic refused by a's claim: %+v", holder)
	}
	var shared claimData
	if err := readJSONFile(filepath.Join(dir, "cache", "claims.json"), &shared); err != nil {
		t.Fatal(err)
	}
	if len(shared.Claims) != 0 {
		t.Fatalf("shared file has %+v", shared.Claims)
	}
}

// A server from before the shared file kept every claim beside its config.
// Loading moves a live one to the shared file once, and the file beside the
// config keeps only its own targets', so a released claim does not come back.
func TestClaimsBesideTheConfigMoveToTheSharedFile(t *testing.T) {
	dir := t.TempDir()
	local, shared := filepath.Join(dir, "config", "claims.json"), filepath.Join(dir, "cache", "claims.json")
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	old := claimData{Claims: []savedClaim{
		{Target: "avd:x", Session: "s1", Since: now, Seen: now},
		{Target: "avd:stale", Session: "s2", Since: now.Add(-time.Hour), Seen: now.Add(-claimLease)},
		{Target: "synthetic", Session: "s3", Since: now, Seen: now},
	}}
	if err := writeJSONFile(local, old); err != nil {
		t.Fatal(err)
	}
	load := func() *claimBook {
		b := newClaimBook(nil)
		b.now = func() time.Time { return now }
		if err := b.loadClaims(local, shared); err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := load()
	var ids []string
	for _, c := range b.list() {
		ids = append(ids, c.Target)
	}
	if strings.Join(ids, ",") != "avd:x,synthetic" {
		t.Fatalf("loaded %v, want avd:x and synthetic", ids)
	}
	for path, want := range map[string]string{shared: "avd:x", local: "synthetic"} {
		var d claimData
		if err := readJSONFile(path, &d); err != nil {
			t.Fatal(err)
		}
		if len(d.Claims) != 1 || d.Claims[0].Target != want {
			t.Fatalf("%s holds %+v, want only %s", path, d.Claims, want)
		}
	}
	b.release("avd:x", "", "s1", false)
	if load().held("avd:x") {
		t.Fatal("a released claim came back from the file beside the config")
	}
}

func TestTargetsShowTheClaimAndWhoIsDriving(t *testing.T) {
	s, now := claimServer(t)
	a := map[string]string{HeaderAgent: "loupe: xctest", HeaderSession: "local_a"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	postInputAs(s, "dev1", tap, a)

	rows := []TargetInfo{{ID: "dev1"}, {ID: "dev2"}}
	s.claims.decorate(rows)
	if rows[0].Claim == nil || rows[0].Claim.Session != "local_a" || rows[0].DrivenBy != "loupe: xctest" {
		t.Fatalf("dev1 = %+v claim %+v", rows[0], rows[0].Claim)
	}
	if rows[1].Claim != nil || rows[1].DrivenBy != "" {
		t.Fatalf("dev2 = %+v", rows[1])
	}

	*now = now.Add(drivingShown)
	rows = []TargetInfo{{ID: "dev1"}}
	s.claims.decorate(rows)
	if rows[0].DrivenBy != "" || rows[0].Claim == nil {
		t.Fatalf("10s on: %+v", rows[0])
	}
}

// A refused tap is not driving.
func TestARefusedAgentIsNotShownDriving(t *testing.T) {
	s, _ := claimServer(t)
	claimReq(s, http.MethodPost, "target=dev1", "", map[string]string{HeaderAgent: "a"})
	postInputAs(s, "dev1", tap, map[string]string{HeaderAgent: "b"})
	rows := []TargetInfo{{ID: "dev1"}}
	s.claims.decorate(rows)
	if rows[0].DrivenBy != "" {
		t.Fatalf("drivenBy = %q after a refusal", rows[0].DrivenBy)
	}
}

// A device that went away under its claim, deleted outside loupe or never
// coming back, still lets the holder release the claim.
func TestAClaimOnATargetThatIsGoneCanBeReleased(t *testing.T) {
	s, _ := claimServer(t)
	a := map[string]string{HeaderAgent: "claude", HeaderSession: "s1"}
	b := map[string]string{HeaderAgent: "claude", HeaderSession: "s2"}
	s.claims.take("avd:gone", "claude", "s1", "")

	if rec := claimReq(s, http.MethodDelete, "target=avd:gone", "", b); rec.Code != http.StatusConflict {
		t.Fatalf("another session's release = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if rec := claimReq(s, http.MethodDelete, "target=avd:gone", "", a); rec.Code != http.StatusNoContent {
		t.Fatalf("holder's release = %d %s, want 204", rec.Code, rec.Body.String())
	}
	if got := s.claims.list(); len(got) != 0 {
		t.Fatalf("claims after release = %+v", got)
	}
	if rec := claimReq(s, http.MethodDelete, "target=avd:gone", "", a); rec.Code != http.StatusNotFound {
		t.Fatalf("release with no target and no claim = %d, want 404", rec.Code)
	}
}

// Releasing a claim ends its holder's "driving" at once, on the row and on the
// session's banner. Both used to run out their 10 seconds, so a viewer was
// offered Take Over against an agent that had already let go.
func TestAReleaseEndsItsHoldersDriving(t *testing.T) {
	s, _ := claimServer(t)
	sess, err := s.mgr.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.AddViewer()
	a := map[string]string{HeaderAgent: "claude", HeaderSession: "local_a"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	postInputAs(s, "dev1", tap, a)
	if st := sess.ControlState(); st.Agent != session.AgentDriving {
		t.Fatalf("after the tap: %+v; want driving", st)
	}

	if rec := claimReq(s, http.MethodDelete, "target=dev1", "", a); rec.Code != http.StatusNoContent {
		t.Fatalf("release = %d: %s", rec.Code, rec.Body.String())
	}
	rows := []TargetInfo{{ID: "dev1"}}
	s.claims.decorate(rows)
	if rows[0].DrivenBy != "" {
		t.Errorf("row after release: drivenBy %q; want none", rows[0].DrivenBy)
	}
	if st := sess.ControlState(); st.Agent != session.AgentIdle {
		t.Errorf("control after release: %+v; want idle", st)
	}
}

// A release leaves another agent's driving alone: only the claim's holder let go.
func TestAReleaseLeavesAnotherAgentDriving(t *testing.T) {
	s, _ := claimServer(t)
	sess, err := s.mgr.Acquire(t.Context(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.AddViewer()
	b := map[string]string{HeaderAgent: "codex", HeaderSession: "local_b"}
	postInputAs(s, "dev1", tap, b)
	a := map[string]string{HeaderAgent: "claude", HeaderSession: "local_a"}
	claimReq(s, http.MethodPost, "target=dev1", "", a)
	if rec := claimReq(s, http.MethodDelete, "target=dev1", "", a); rec.Code != http.StatusNoContent {
		t.Fatalf("release = %d: %s", rec.Code, rec.Body.String())
	}

	rows := []TargetInfo{{ID: "dev1"}}
	s.claims.decorate(rows)
	if rows[0].DrivenBy != "codex" {
		t.Errorf("row: drivenBy %q; want codex", rows[0].DrivenBy)
	}
	if st := sess.ControlState(); st.Agent != session.AgentDriving || st.AgentName != "codex" {
		t.Errorf("control: %+v; want codex driving", st)
	}
}
