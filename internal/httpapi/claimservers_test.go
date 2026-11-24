package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// twoFlagServers is two servers whose claim books share one claims file, as
// two loupe servers on one machine do, each with the flag target dev1.
func twoFlagServers(t *testing.T) (s1, s2 *Server) {
	t.Helper()
	a, b, _, _ := twoServers(t)
	s1, _ = newFlagServer(t)
	s1.claims = a
	s2, _ = newFlagServer(t)
	s2.claims = b
	return s1, s2
}

func listenersOn(t *testing.T, s *Server, id string) FlagListenersResponse {
	t.Helper()
	var resp FlagListenersResponse
	w := serveOn(s, http.MethodGet, "/api/flags/listeners?target="+id, "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode listeners %s: %v", w.Body, err)
	}
	return resp
}

// A flag names the holder only on the server the holder claimed through,
// where its listener is. On the other server the claim still refuses other
// agents, but a flag there goes to whoever listens there, as it did before
// servers shared claims.
func TestAFlagNamesOnlyAHolderWhoClaimedThroughItsServer(t *testing.T) {
	s1, s2 := twoFlagServers(t)
	if _, holder, _ := s2.claims.take("dev1", "claude", "sB", ""); holder != nil {
		t.Fatalf("claim through s2 refused: %+v", holder)
	}

	w := serveOn(s1, http.MethodPost, "/api/flags?target=dev1", `{"note":"clipped"}`, nil)
	if f := decodeFlag(t, w.Body.Bytes()); f.ClaimedBy != "" {
		t.Errorf("flag on s1 claimedBy = %q, want none: the holder claimed through s2", f.ClaimedBy)
	}
	if l := listenersOn(t, s1, "dev1"); l.ClaimedBy != "" || l.Holder != nil {
		t.Errorf("s1 listeners claimedBy %q holder %+v, want none", l.ClaimedBy, l.Holder)
	}
	if e := s1.claims.refusal("dev1", "claude", "sC"); e == nil || e.Code != ErrCodeClaimed {
		t.Errorf("s1 refusal of another session = %+v, want claimedBy", e)
	}

	w = serveOn(s2, http.MethodPost, "/api/flags?target=dev1", `{"note":"clipped"}`, nil)
	if f := decodeFlag(t, w.Body.Bytes()); f.ClaimedBy != "claude" {
		t.Errorf("flag on s2 claimedBy = %q, want claude", f.ClaimedBy)
	}
	if l := listenersOn(t, s2, "dev1"); l.ClaimedBy != "claude" || l.Holder == nil || l.Holder.Session != "sB" {
		t.Errorf("s2 listeners claimedBy %q holder %+v, want claude sB", l.ClaimedBy, l.Holder)
	}
}

// An interrupt raised on a server the holder did not claim through stops
// every agent there, and any agent there that reads it lets it be deleted,
// as before servers shared claims. Gating it on the holder, who never reads
// it there, would leave it undeletable.
func TestAnInterruptAwayFromTheHolderCanBeReadAndDeleted(t *testing.T) {
	s1, s2 := twoFlagServers(t)
	s2.claims.take("dev1", "claude", "sB", "")
	serveOn(s1, http.MethodPost, "/api/flags?target=dev1", `{}`, nil)
	serveOn(s1, http.MethodPost, "/api/flags?target=dev1", `{"kind":"interrupt"}`, nil)
	serveOn(s1, http.MethodPost, "/api/flags/end?id=2", "", nil)
	serveOn(s1, http.MethodGet, "/api/flags?id=2", "", map[string]string{HeaderAgent: "claude", HeaderSession: "sC"})

	deleteFlags(t, s1, "?target=dev1", http.StatusOK)
	if ids := flagIDs(t, s1); len(ids) != 0 {
		t.Fatalf("flags left on s1 = %v, want none", ids)
	}
}

// holdClaimLock takes the lock beside path from the test, as a server stopped
// mid-change would hold it, and returns the func that lets go.
func holdClaimLock(t *testing.T, path string) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := tryLockFile(fh); !ok || err != nil {
		t.Fatalf("lock: ok %v err %v", ok, err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			unlockFile(fh)
			fh.Close()
		}
	})
	return func() {
		released = true
		unlockFile(fh)
		fh.Close()
	}
}

// shortLockWait shortens how long a change waits for the lock, for one test.
func shortLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := claimLockWait
	claimLockWait = d
	t.Cleanup(func() { claimLockWait = old })
}

// fast fails the test when fn takes as long as a change waits for the lock.
func fast(t *testing.T, what string, fn func()) {
	t.Helper()
	began := time.Now()
	fn()
	if took := time.Since(began); took >= claimLockWait/2 {
		t.Errorf("%s with the lock held took %s, want no wait for it", what, took)
	}
}

// A claim that cannot get the lock is refused, not granted without it, so
// two servers never give one device to two sessions. Reads and renewals do
// not wait for it.
func TestAClaimFailsClosedWhenTheLockStaysHeld(t *testing.T) {
	shortLockWait(t, 300*time.Millisecond)
	a, b, now, dir := twoServers(t)
	if _, holder, err := a.take("avd:y", "claude", "sA", ""); holder != nil || err != nil {
		t.Fatalf("claim avd:y: holder %+v err %v", holder, err)
	}
	unlock := holdClaimLock(t, filepath.Join(dir, "cache", "claims.json"))

	for name, book := range map[string]*claimBook{"a": a, "b": b} {
		got, holder, err := book.take("avd:x", "claude", "s"+name, "")
		if got != nil || holder != nil || !errors.Is(err, errClaimLockBusy) {
			t.Fatalf("claim through %s with the lock held: got %+v holder %+v err %v, want errClaimLockBusy", name, got, holder, err)
		}
	}
	if _, _, released, err := a.release("avd:y", "claude", "sA", false); released || !errors.Is(err, errClaimLockBusy) {
		t.Fatalf("release with the lock held: released %v err %v, want errClaimLockBusy", released, err)
	}
	if _, err := a.hold("avd:y", "claude", "sA"); !errors.Is(err, errClaimLockBusy) {
		t.Fatalf("holder's hold with the lock held: err %v, want errClaimLockBusy", err)
	}
	fast(t, "a non-holder's hold", func() {
		if _, err := b.hold("avd:y", "claude", "sB"); err != nil {
			t.Errorf("non-holder's hold: err %v, want none: it holds nothing", err)
		}
	})
	fast(t, "refusal", func() {
		if e := b.refusal("avd:y", "claude", "sB"); e == nil || e.Code != ErrCodeClaimed {
			t.Errorf("refusal of another session = %+v, want claimedBy", e)
		}
	})
	*now = now.Add(claimSaveEvery)
	fast(t, "the holder's renewal", func() { a.seen("avd:y", "claude", "sA") })
	*now = now.Add(claimLease)
	fast(t, "list with a claim run out", func() {
		if l := b.list(); len(l) != 0 {
			t.Errorf("list = %+v, want the run-out claim gone", l)
		}
	})
	unlock()
	if got, holder, _ := a.take("avd:x", "claude", "sA", ""); got == nil || holder != nil {
		t.Fatalf("claim after the lock came free: got %+v holder %+v", got, holder)
	}
	if _, holder, _ := b.take("avd:x", "claude", "sB", ""); holder == nil || holder.Session != "sA" {
		t.Fatalf("b's claim after a's: holder %+v, want sA", holder)
	}
}

// POST /api/claims answers a busy lock with a 503 the caller can retry.
func TestAClaimRequestWithTheLockHeldIsA503(t *testing.T) {
	shortLockWait(t, 100*time.Millisecond)
	a, _, _, dir := twoServers(t)
	s, _ := newFlagServer(t)
	s.claims = a
	unlock := holdClaimLock(t, filepath.Join(dir, "cache", "claims.json"))
	agent := map[string]string{HeaderAgent: "claude", HeaderSession: "sA"}

	w := serveOn(s, http.MethodPost, "/api/claims?target=dev1", "", agent)
	var e ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != http.StatusServiceUnavailable || e.Code != ErrCodeClaimLockBusy || w.Header().Get("Retry-After") == "" {
		t.Fatalf("claim with the lock held: %d %s Retry-After %q, want 503 %s", w.Code, w.Body, w.Header().Get("Retry-After"), ErrCodeClaimLockBusy)
	}
	if a.held("dev1") {
		t.Fatal("dev1 held after a refused claim")
	}
	unlock()
	if w := serveOn(s, http.MethodPost, "/api/claims?target=dev1", "", agent); w.Code != http.StatusOK {
		t.Fatalf("claim after the lock came free: %d %s", w.Code, w.Body)
	}
}

// A claim's server survives a restart, so the holder's flags keep naming it
// on the server it claimed through.
func TestAClaimsServerSurvivesARestart(t *testing.T) {
	a, _, _, dir := twoServers(t)
	a.take("dev1", "claude", "sA", "")
	again := newClaimBook(nil)
	again.now = a.now
	again.self = a.self
	if err := again.loadClaims(filepath.Join(dir, "config0", "claims.json"), filepath.Join(dir, "cache", "claims.json")); err != nil {
		t.Fatal(err)
	}
	if agent, _, ok := again.holderHere("dev1"); !ok || agent != "claude" {
		t.Fatalf("after a restart holderHere = %q %v, want claude", agent, ok)
	}
	other := newClaimBook(nil)
	other.now = a.now
	other.self = ":18459"
	if err := other.loadClaims("", filepath.Join(dir, "cache", "claims.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := other.holderHere("dev1"); ok {
		t.Fatal("holderHere true on a server the claim was not taken through")
	}
}

// Writers that replace the claims file at once, as two servers' do, each get
// their own new file to rename over it: every write succeeds, the file always
// parses, and no new file is left behind.
func TestClaimFileWritersAtOnceNeverShareANewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claims.json")
	var wg sync.WaitGroup
	errs := make(chan error, 8*50*2) // a write and a read each loop
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				d := claimData{}
				for range (w*50 + i) % 7 {
					d.Claims = append(d.Claims, savedClaim{Target: "avd:x", Agent: strings.Repeat("a", 40)})
				}
				if err := writeJSONFile(path, d); err != nil {
					errs <- err
				}
				var got claimData
				if err := readJSONFile(path, &got); err != nil {
					errs <- fmt.Errorf("read between writes: %w", err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(left) != 0 {
		t.Errorf("new files left behind: %v", left)
	}
}
