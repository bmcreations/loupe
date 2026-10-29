package pairing

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openAt(t *testing.T, path string) (*Store, *clock) {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	s.SetClock(c.now)
	return s, c
}

func mint(t *testing.T, s *Store, req MintRequest) string {
	t.Helper()
	_, code, err := s.Mint(req)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func redeem(t *testing.T, s *Store, code string) (Session, string) {
	t.Helper()
	row, tok, _, err := s.Redeem(code, RedeemRequest{Kind: KindBearer})
	if err != nil {
		t.Fatal(err)
	}
	return row, tok
}

func TestCodeFormatAndNormalizing(t *testing.T) {
	s, _ := openAt(t, "")
	code := mint(t, s, MintRequest{})
	if len(code) != 14 || code[4] != '-' || code[9] != '-' {
		t.Fatalf("code %q is not XXXX-XXXX-XXXX", code)
	}
	for _, r := range strings.ReplaceAll(code, "-", "") {
		if !strings.ContainsRune(codeAlphabet, r) {
			t.Fatalf("code %q has %q, outside the alphabet", code, r)
		}
	}
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	if _, _, _, err := s.Redeem(typed, RedeemRequest{Kind: KindBearer}); err != nil {
		t.Errorf("lower case with spaces: %v", err)
	}
	for _, bad := range []string{"", "K7QM-4XHT-9PZ", "K7QM-4XHT-9PZA0", "O7QM-4XHT-9PZA", "K7QM_4XHT_9PZA"} {
		if _, ok := NormalizeCode(bad); ok {
			t.Errorf("NormalizeCode(%q) accepted it", bad)
		}
	}
}

func TestACodeIsSpentOnce(t *testing.T) {
	s, _ := openAt(t, "")
	code := mint(t, s, MintRequest{})
	redeem(t, s, code)
	if _, _, _, err := s.Redeem(code, RedeemRequest{Kind: KindBearer}); err != ErrBadCode {
		t.Errorf("second redemption: err = %v, want ErrBadCode", err)
	}
}

func TestConcurrentRedemptionsOfOneCodeHaveOneWinner(t *testing.T) {
	s, _ := openAt(t, filepath.Join(t.TempDir(), "sessions.json"))
	code := mint(t, s, MintRequest{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, _, err := s.Redeem(code, RedeemRequest{Kind: KindBearer}); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d redemptions succeeded, want 1", wins)
	}
}

func TestACodeExpiresAfterFiveMinutes(t *testing.T) {
	s, c := openAt(t, "")
	code := mint(t, s, MintRequest{})
	c.add(CodeTTL)
	if _, _, _, err := s.Redeem(code, RedeemRequest{Kind: KindBearer}); err != ErrBadCode {
		t.Errorf("err = %v, want ErrBadCode", err)
	}
}

func TestAFailedWriteLeavesTheCodeUnspent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	s, _ := openAt(t, path)
	code := mint(t, s, MintRequest{})
	// A directory where the temp file goes makes the write fail.
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Redeem(code, RedeemRequest{Kind: KindBearer}); err == nil || err == ErrBadCode {
		t.Fatalf("redeem with an unwritable store: err = %v, want a write error", err)
	}
	if len(s.Sessions()) != 0 {
		t.Error("a session was kept although the write failed")
	}
	_ = os.Remove(path + ".tmp")
	if _, _, _, err := s.Redeem(code, RedeemRequest{Kind: KindBearer}); err != nil {
		t.Errorf("the code was spent by the failed redemption: %v", err)
	}
}

func TestAuthenticateSlidesExpiry(t *testing.T) {
	s, c := openAt(t, "")
	_, tok := redeem(t, s, mint(t, s, MintRequest{}))
	for i := 0; i < 3; i++ {
		c.add(SessionTTL - time.Hour)
		if _, ok := s.Authenticate(tok); !ok {
			t.Fatalf("use %d, %v after the last: session lapsed", i, SessionTTL-time.Hour)
		}
	}
	c.add(SessionTTL)
	if _, ok := s.Authenticate(tok); ok {
		t.Error("a session unused for 30 days still authenticated")
	}
}

func TestAuthenticateRejectsUnknownAndMasterShapedTokens(t *testing.T) {
	s, _ := openAt(t, "")
	redeem(t, s, mint(t, s, MintRequest{}))
	for _, tok := range []string{"", "lsn_", "lsn_nope", strings.Repeat("a", 64)} {
		if _, ok := s.Authenticate(tok); ok {
			t.Errorf("Authenticate(%q) accepted it", tok)
		}
	}
}

func TestLastUsedReachesTheDiskAtMostEveryFiveMinutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, c := openAt(t, path)
	_, tok := redeem(t, s, mint(t, s, MintRequest{}))
	before, _ := os.ReadFile(path)
	c.add(time.Minute)
	s.Authenticate(tok)
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a use one minute after the last was written to disk")
	}
	c.add(touchEvery)
	s.Authenticate(tok)
	if after, _ := os.ReadFile(path); string(after) == string(before) {
		t.Error("a use five minutes after the last write never reached the disk")
	}
}

func TestTheFileHoldsOnlyHashesAndIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, _ := openAt(t, path)
	code := mint(t, s, MintRequest{})
	_, tok := redeem(t, s, mint(t, s, MintRequest{}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), tok) || strings.Contains(string(raw), strings.ReplaceAll(code, "-", "")) {
		t.Errorf("sessions.json holds a credential:\n%s", raw)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestSessionsAndCodesSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, _ := openAt(t, path)
	_, tok := redeem(t, s, mint(t, s, MintRequest{}))
	pending := mint(t, s, MintRequest{Label: "phone"})

	s2, _ := openAt(t, path)
	if _, ok := s2.Authenticate(tok); !ok {
		t.Error("a session did not survive a restart")
	}
	row, _ := redeem(t, s2, pending)
	if row.Label != "phone" {
		t.Errorf("label = %q, want the code's", row.Label)
	}
}

func TestAnUnreadableFileIsAnErrorNotAnEmptyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil || s != nil {
		t.Errorf("Open = %v, %v; want an error and no store", s, err)
	}
}

func TestOnlyACanPairSessionMints(t *testing.T) {
	s, _ := openAt(t, "")
	plain, _ := redeem(t, s, mint(t, s, MintRequest{}))
	if _, _, err := s.Mint(MintRequest{Parent: plain.ID}); err != ErrParentGone {
		t.Errorf("a session without canPair minted: err = %v", err)
	}
	pairer, _ := redeem(t, s, mint(t, s, MintRequest{CanPair: true}))
	if !pairer.CanPair {
		t.Fatal("canPair was not passed from the code to the session")
	}
	child, _ := redeem(t, s, mint(t, s, MintRequest{Parent: pairer.ID}))
	if child.Parent != pairer.ID || child.CanPair {
		t.Errorf("child = %+v; want parent %s and no canPair", child, pairer.ID)
	}
}

func TestRevokeCascadesToDescendantsAndTheirCodes(t *testing.T) {
	s, _ := openAt(t, "")
	a, aTok := redeem(t, s, mint(t, s, MintRequest{CanPair: true}))
	b, bTok := redeem(t, s, mint(t, s, MintRequest{Parent: a.ID, CanPair: true}))
	c, cTok := redeem(t, s, mint(t, s, MintRequest{Parent: b.ID}))
	_, otherTok := redeem(t, s, mint(t, s, MintRequest{}))
	pending := mint(t, s, MintRequest{Parent: b.ID})

	revoked, ok, err := s.Revoke(a.ID)
	if err != nil || !ok {
		t.Fatalf("Revoke: %v %v", ok, err)
	}
	if len(revoked) != 3 {
		t.Errorf("revoked %v, want %s, %s and %s", revoked, a.ID, b.ID, c.ID)
	}
	for _, tok := range []string{aTok, bTok, cTok} {
		if _, ok := s.Authenticate(tok); ok {
			t.Error("a descendant of a revoked session still authenticates")
		}
	}
	if _, ok := s.Authenticate(otherTok); !ok {
		t.Error("an unrelated session was revoked")
	}
	if _, _, _, err := s.Redeem(pending, RedeemRequest{Kind: KindBearer}); err != ErrBadCode {
		t.Errorf("a code minted by a revoked session still redeems: %v", err)
	}
}

func TestRevokeOthersKeepsTheCaller(t *testing.T) {
	s, _ := openAt(t, "")
	a, _ := redeem(t, s, mint(t, s, MintRequest{CanPair: true}))
	b, bTok := redeem(t, s, mint(t, s, MintRequest{Parent: a.ID}))
	_, cTok := redeem(t, s, mint(t, s, MintRequest{}))
	if _, err := s.RevokeOthers(b.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Authenticate(bTok); !ok {
		t.Error("the caller was revoked along with its parent")
	}
	if _, ok := s.Authenticate(cTok); ok {
		t.Error("another session survived")
	}
	if _, err := s.RevokeOthers(""); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Authenticate(bTok); ok {
		t.Error("revoking all left the caller")
	}
}

func TestRevokedRowsAreKeptSevenDaysThenPruned(t *testing.T) {
	s, c := openAt(t, "")
	a, _ := redeem(t, s, mint(t, s, MintRequest{}))
	if _, _, err := s.Revoke(a.ID); err != nil {
		t.Fatal(err)
	}
	c.add(6 * 24 * time.Hour)
	if len(s.Sessions()) != 1 {
		t.Error("a revoked row was pruned before 7 days")
	}
	c.add(2 * 24 * time.Hour)
	if len(s.Sessions()) != 0 {
		t.Error("a revoked row outlived 7 days")
	}
}

func TestDeleteCode(t *testing.T) {
	s, _ := openAt(t, "")
	code := mint(t, s, MintRequest{})
	codes := s.Codes()
	if len(codes) != 1 {
		t.Fatalf("Codes = %v", codes)
	}
	if ok, err := s.DeleteCode(codes[0].ID); !ok || err != nil {
		t.Fatalf("DeleteCode: %v %v", ok, err)
	}
	if _, _, _, err := s.Redeem(code, RedeemRequest{Kind: KindBearer}); err != ErrBadCode {
		t.Errorf("a withdrawn code redeemed: %v", err)
	}
}

func TestRevokeDescendantsKeepsTheSessionAndItsSiblings(t *testing.T) {
	s, _ := openAt(t, "")
	a, aTok := redeem(t, s, mint(t, s, MintRequest{CanPair: true}))
	b, bTok := redeem(t, s, mint(t, s, MintRequest{Parent: a.ID, CanPair: true}))
	c, cTok := redeem(t, s, mint(t, s, MintRequest{Parent: b.ID}))
	sib, sibTok := redeem(t, s, mint(t, s, MintRequest{}))
	if !s.Descends(c.ID, a.ID) || !s.Descends(b.ID, a.ID) || s.Descends(a.ID, a.ID) || s.Descends(sib.ID, a.ID) || s.Descends(a.ID, c.ID) {
		t.Error("Descends got the tree wrong")
	}
	revoked, err := s.RevokeDescendants(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 2 {
		t.Errorf("revoked %v, want b and c", revoked)
	}
	for tok, want := range map[string]bool{aTok: true, sibTok: true, bTok: false, cTok: false} {
		if _, ok := s.Authenticate(tok); ok != want {
			t.Errorf("token live = %v, want %v (revoked %v, sibling %s)", ok, want, revoked, sib.ID)
		}
	}
}

func TestASessionOutlivingItsParentTakesTheGrandparent(t *testing.T) {
	s, c := openAt(t, "")
	a, _ := redeem(t, s, mint(t, s, MintRequest{CanPair: true}))
	b, _ := redeem(t, s, mint(t, s, MintRequest{Parent: a.ID, CanPair: true}))
	child, childTok := redeem(t, s, mint(t, s, MintRequest{Parent: b.ID}))
	// a and b go idle; child keeps being used, so only child is live after
	// SessionTTL.
	for i := 0; i < 4; i++ {
		c.add(SessionTTL / 3)
		if _, ok := s.Authenticate(childTok); !ok {
			t.Fatal("the used session expired")
		}
	}
	rows := s.Sessions()
	if len(rows) != 1 || rows[0].ID != child.ID {
		t.Fatalf("sessions = %v, want only %s", rows, child.ID)
	}
	if rows[0].Parent != ParentMaster {
		t.Errorf("parent = %q, want %q", rows[0].Parent, ParentMaster)
	}
}
