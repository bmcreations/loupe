package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if ErrCodeClaimed != "claimedBy" || ErrCodeHandsOff != "handsOff" {
		t.Fatalf("codes changed: %q %q", ErrCodeClaimed, ErrCodeHandsOff)
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
