package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func row(s *Server, id string) TargetInfo {
	rows := []TargetInfo{{ID: id}}
	s.claims.decorate(rows)
	return rows[0]
}

// A person's hold shows the agent waiting from its first refusal, the
// refusal itself, and a refused event in the session's Activity.
func TestAHeldAgentIsShownWaitingAndRefused(t *testing.T) {
	s, sess, _ := heldSession(t)
	s.claims = newClaimBook(nil)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s.claims.now = func() time.Time { return now }
	a := map[string]string{HeaderAgent: "claude", HeaderSession: "s1"}

	postInputAs(s, "dev1", tap, a)
	first := now
	now = now.Add(3 * time.Second)
	postInputAs(s, "dev1", tap, a)

	r := row(s, "dev1")
	if r.Waiting == nil || r.Waiting.Agent != "claude" || r.Waiting.Session != "s1" || !r.Waiting.Since.Equal(first) {
		t.Fatalf("waiting = %+v; want claude/s1 since the first refusal", r.Waiting)
	}
	if r.Refused == nil || r.Refused.Code != ErrCodeHumanHeld || !r.Refused.At.Equal(now) {
		t.Fatalf("refused = %+v; want the last humanHeld", r.Refused)
	}
	var refused int
	for _, e := range sess.EventsAfter(0) {
		if e.Kind == "refused" {
			refused++
			if e.Agent != "claude" || e.Action != "tap" || e.Err != ErrCodeHumanHeld {
				t.Errorf("event = %+v", e)
			}
		}
	}
	if refused != 2 {
		t.Errorf("%d refused events; want 2", refused)
	}
}

// Waiting ends when the agent's action goes through, or waitingShown after
// its last refusal; the refusal stays for refusedShown.
func TestWaitingEndsOnActingOrAfterAWhile(t *testing.T) {
	s, now := claimServer(t)
	s.claims.refusedAgent("dev1", "claude", "s1", ErrCodeHumanHeld)
	s.claims.acted("dev1", "claude")
	if r := row(s, "dev1"); r.Waiting != nil {
		t.Fatalf("waiting after acting = %+v", r.Waiting)
	}

	s.claims.refusedAgent("dev1", "claude", "s1", ErrCodeHumanHeld)
	*now = now.Add(waitingShown)
	r := row(s, "dev1")
	if r.Waiting != nil {
		t.Fatalf("waiting %v after the last refusal = %+v", waitingShown, r.Waiting)
	}
	if r.Refused == nil {
		t.Fatal("the refusal went with the waiting")
	}
	*now = now.Add(refusedShown)
	if r := row(s, "dev1"); r.Refused != nil {
		t.Fatalf("refused after %v = %+v", refusedShown, r.Refused)
	}
}

// A refusal for a claim or hands-off is on the row with no session open,
// and does not show the agent waiting.
func TestAClaimRefusalIsOnTheRowWithoutASession(t *testing.T) {
	s, _ := claimServer(t)
	claimReq(s, http.MethodPost, "target=dev1", "", map[string]string{HeaderAgent: "a"})
	postInputAs(s, "dev1", tap, map[string]string{HeaderAgent: "b"})
	r := row(s, "dev1")
	if r.Refused == nil || r.Refused.Agent != "b" || r.Refused.Code != ErrCodeClaimed {
		t.Fatalf("refused = %+v; want b claimedBy", r.Refused)
	}
	if r.Waiting != nil {
		t.Fatalf("waiting = %+v for a claim refusal", r.Waiting)
	}
}

// A claim on a device a person just used says how long ago; one nobody has
// open leaves the field out.
func TestAClaimSaysWhenAPersonLastUsedTheDevice(t *testing.T) {
	s, _, _ := heldSession(t)
	s.claims = newClaimBook(nil)
	rec := claimReq(s, http.MethodPost, "target=dev1", "", map[string]string{HeaderAgent: "claude"})
	var got ClaimInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("claim = %d %s", rec.Code, rec.Body.String())
	}
	if got.RecentHumanInputMs == nil || *got.RecentHumanInputMs < 0 || *got.RecentHumanInputMs > 5000 {
		t.Fatalf("recentHumanInputMs = %v; want the viewer's touch just now", got.RecentHumanInputMs)
	}

	c, _ := claimServer(t)
	rec = claimReq(c, http.MethodPost, "target=dev2", "", map[string]string{HeaderAgent: "claude"})
	var none ClaimInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &none); err != nil || none.RecentHumanInputMs != nil {
		t.Fatalf("claim with no session = %s", rec.Body.String())
	}
}
