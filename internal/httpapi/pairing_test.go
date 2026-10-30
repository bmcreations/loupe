package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

func pairingServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := &Server{mgr: NewManager(target.Options{}), token: "secret"}
	if err := s.SetSessionsPath(filepath.Join(t.TempDir(), "sessions.json")); err != nil {
		t.Fatal(err)
	}
	return s, s.Handler()
}

type call struct {
	method, path, body string
	auth               string // full Authorization header value
	cookie             *http.Cookie
	origin             string
	remote             string
}

func pdo(h http.Handler, c call) *httptest.ResponseRecorder {
	r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	if c.auth != "" {
		r.Header.Set("Authorization", c.auth)
	}
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	if c.origin != "" {
		r.Header.Set("Origin", c.origin)
	}
	if c.remote != "" {
		r.RemoteAddr = c.remote
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

const master = "Bearer secret"

func mintCode(t *testing.T, h http.Handler, auth, body string) PairingResponse {
	t.Helper()
	rec := pdo(h, call{method: http.MethodPost, path: "/api/pairings", body: body, auth: auth})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/pairings = %d %s", rec.Code, rec.Body)
	}
	return decode[PairingResponse](t, rec)
}

func pairBearer(t *testing.T, h http.Handler, code string) PairResponse {
	t.Helper()
	rec := pdo(h, call{method: http.MethodPost, path: "/api/pair", body: `{"code":"` + code + `","kind":"bearer","label":"ci"}`})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/pair = %d %s", rec.Code, rec.Body)
	}
	return decode[PairResponse](t, rec)
}

func TestPairABearerSessionAndUseIt(t *testing.T) {
	_, h := pairingServer(t)
	code := mintCode(t, h, master, `{"label":"mcp on ci"}`)
	sess := pairBearer(t, h, code.Code)
	if !strings.HasPrefix(sess.Token, "lsn_") {
		t.Fatalf("token %q has no lsn_ prefix", sess.Token)
	}

	rec := pdo(h, call{method: http.MethodGet, path: "/api/auth", auth: "Bearer " + sess.Token})
	got := decode[AuthResponse](t, rec)
	if got.Method != "session" || got.Session == nil || got.Session.ID != sess.ID || got.CanPair || !got.Pairing {
		t.Errorf("GET /api/auth = %+v", got)
	}
	if got.Session.Label != "ci" {
		t.Errorf("label = %q, want the redeemer's", got.Session.Label)
	}
	if rec := pdo(h, call{method: http.MethodGet, path: "/api/sessions", auth: "Bearer " + sess.Token}); rec.Code != http.StatusOK {
		t.Errorf("a session's GET /api/sessions = %d", rec.Code)
	}
}

func TestAuthProbeAnswersWithoutACredential(t *testing.T) {
	_, h := pairingServer(t)
	rec := pdo(h, call{method: http.MethodGet, path: "/api/auth"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := decode[AuthResponse](t, rec); got.Method != "none" || !got.Pairing || got.CanPair {
		t.Errorf("GET /api/auth = %+v", got)
	}
	got := decode[AuthResponse](t, pdo(h, call{method: http.MethodGet, path: "/api/auth", auth: master}))
	if got.Method != "master" || !got.CanPair {
		t.Errorf("with the master token: %+v", got)
	}
	// Open means /api/auth and /api/pair only.
	if rec := pdo(h, call{method: http.MethodGet, path: "/api/sessions"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/sessions with no credential = %d, want 401", rec.Code)
	}
	if rec := pdo(h, call{method: http.MethodPost, path: "/api/pairings", body: "{}"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/pairings with no credential = %d, want 401", rec.Code)
	}
}

func TestOnlyMasterOrACanPairSessionMintsCodes(t *testing.T) {
	_, h := pairingServer(t)
	plain := pairBearer(t, h, mintCode(t, h, master, `{}`).Code)
	rec := pdo(h, call{method: http.MethodPost, path: "/api/pairings", body: `{}`, auth: "Bearer " + plain.Token})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a plain session minted a code: %d", rec.Code)
	}

	pairer := pairBearer(t, h, mintCode(t, h, master, `{"canPair":true}`).Code)
	child := pairBearer(t, h, mintCode(t, h, "Bearer "+pairer.Token, `{}`).Code)

	rows := decode[[]SessionInfo](t, pdo(h, call{method: http.MethodGet, path: "/api/sessions", auth: "Bearer " + pairer.Token}))
	byID := map[string]SessionInfo{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if !byID[pairer.ID].CanPair || !byID[pairer.ID].Current {
		t.Errorf("pairer row = %+v; want canPair and current", byID[pairer.ID])
	}
	if byID[child.ID].Parent != pairer.ID || byID[child.ID].CanPair || byID[child.ID].Current {
		t.Errorf("child row = %+v", byID[child.ID])
	}
	if !strings.Contains(pdo(h, call{method: http.MethodGet, path: "/api/sessions", auth: master}).Body.String(), `"id"`) {
		t.Error("the list is empty for the master token")
	}
	if strings.Contains(pdo(h, call{method: http.MethodGet, path: "/api/sessions", auth: master}).Body.String(), `"hash"`) {
		t.Error("GET /api/sessions returned hashes")
	}
}

func TestRevokingASessionCascadesAndEndsItsAccess(t *testing.T) {
	_, h := pairingServer(t)
	pairer := pairBearer(t, h, mintCode(t, h, master, `{"canPair":true}`).Code)
	child := pairBearer(t, h, mintCode(t, h, "Bearer "+pairer.Token, `{}`).Code)

	rec := pdo(h, call{method: http.MethodDelete, path: "/api/sessions/" + pairer.ID, auth: master})
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
	}
	if got := decode[RevokeResponse](t, rec); len(got.Revoked) != 2 {
		t.Errorf("revoked %v, want the pairer and its child", got.Revoked)
	}
	for _, tok := range []string{pairer.Token, child.Token} {
		if rec := pdo(h, call{method: http.MethodGet, path: "/api/sessions", auth: "Bearer " + tok}); rec.Code != http.StatusUnauthorized {
			t.Errorf("a revoked session still authenticates: %d", rec.Code)
		}
	}
	if rec := pdo(h, call{method: http.MethodDelete, path: "/api/sessions/nope", auth: master}); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE an unknown session = %d, want 404", rec.Code)
	}
}

func TestASessionRevokesOnlyItselfAndWhatItPaired(t *testing.T) {
	_, h := pairingServer(t)
	a := pairBearer(t, h, mintCode(t, h, master, `{"canPair":true}`).Code)
	b := pairBearer(t, h, mintCode(t, h, master, `{}`).Code)
	child := pairBearer(t, h, mintCode(t, h, "Bearer "+a.Token, `{}`).Code)
	live := func(tok string) bool {
		rec := pdo(h, call{method: http.MethodGet, path: "/api/auth", auth: "Bearer " + tok})
		return decode[AuthResponse](t, rec).Method == "session"
	}

	if rec := pdo(h, call{method: http.MethodDelete, path: "/api/sessions", auth: "Bearer " + a.Token}); rec.Code != http.StatusBadRequest {
		t.Errorf("DELETE /api/sessions without ?others=1 = %d, want 400", rec.Code)
	}
	if rec := pdo(h, call{method: http.MethodDelete, path: "/api/sessions/" + b.ID, auth: "Bearer " + a.Token}); rec.Code != http.StatusForbidden {
		t.Errorf("a session revoking a sibling = %d, want 403", rec.Code)
	}
	if rec := pdo(h, call{method: http.MethodDelete, path: "/api/sessions/" + a.ID, auth: "Bearer " + child.Token}); rec.Code != http.StatusForbidden {
		t.Errorf("a session revoking its parent = %d, want 403", rec.Code)
	}
	rec := pdo(h, call{method: http.MethodDelete, path: "/api/sessions?others=1", auth: "Bearer " + a.Token})
	if got := decode[RevokeResponse](t, rec); len(got.Revoked) != 1 || got.Revoked[0] != child.ID {
		t.Errorf("?others=1 from a session revoked %v, want only its child %s", got.Revoked, child.ID)
	}
	if !live(a.Token) || !live(b.Token) || live(child.Token) {
		t.Errorf("after ?others=1: a=%v b=%v child=%v, want true true false", live(a.Token), live(b.Token), live(child.Token))
	}
	if rec := pdo(h, call{method: http.MethodDelete, path: "/api/sessions/" + b.ID, auth: "Bearer " + b.Token}); rec.Code != http.StatusOK || live(b.Token) {
		t.Errorf("a session revoking itself = %d, live after: %v", rec.Code, live(b.Token))
	}

	c := pairBearer(t, h, mintCode(t, h, master, `{}`).Code)
	rec = pdo(h, call{method: http.MethodDelete, path: "/api/sessions?others=1", auth: master})
	if got := decode[RevokeResponse](t, rec); len(got.Revoked) != 2 || live(a.Token) || live(c.Token) {
		t.Errorf("?others=1 with the master token revoked %v, want every live session", got.Revoked)
	}
}

func TestBrowserPairingSetsAnHttpOnlySessionCookie(t *testing.T) {
	_, h := pairingServer(t)
	code := mintCode(t, h, master, `{}`)
	rec := pdo(h, call{method: http.MethodPost, path: "/api/pair", body: `{"code":"` + code.Code + `","kind":"browser"}`, origin: "http://example.com"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /api/pair browser = %d %s", rec.Code, rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 30*24*3600 {
		t.Fatalf("cookie = %+v", cookie)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a browser pairing returned a body: %s", rec.Body)
	}

	if rec := pdo(h, call{method: http.MethodGet, path: "/api/auth", cookie: cookie}); decode[AuthResponse](t, rec).Method != "session" {
		t.Errorf("the cookie did not authenticate: %s", rec.Body)
	}
	// The cookie is something the browser attaches on its own, so it gets the
	// Origin check the master cookie gets.
	if rec := pdo(h, call{method: http.MethodPost, path: "/api/input", body: "{}", cookie: cookie, origin: "http://example.com:9000"}); rec.Code != http.StatusForbidden {
		t.Errorf("a cross-origin request with the session cookie = %d, want 403", rec.Code)
	}
}

func TestBrowserPairingFromAnotherOriginIsRefused(t *testing.T) {
	_, h := pairingServer(t)
	code := mintCode(t, h, master, `{}`)
	rec := pdo(h, call{method: http.MethodPost, path: "/api/pair", body: `{"code":"` + code.Code + `","kind":"browser"}`, origin: "http://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	// And the code is not spent by the refusal.
	pairBearer(t, h, code.Code)
}

func TestPairIsRateLimitedOnFailures(t *testing.T) {
	_, h := pairingServer(t)
	bad := `{"code":"AAAA-AAAA-AAAA","kind":"bearer"}`
	for i := 0; i < 5; i++ {
		if rec := pdo(h, call{method: http.MethodPost, path: "/api/pair", body: bad, remote: "10.0.0.1:1234"}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	good := mintCode(t, h, master, `{}`)
	rec := pdo(h, call{method: http.MethodPost, path: "/api/pair", body: `{"code":"` + good.Code + `","kind":"bearer"}`, remote: "10.0.0.1:1234"})
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("sixth attempt from one address = %d, want 429", rec.Code)
	}
	// Junk from many other addresses does not hold back a right code: a cap
	// across all addresses would let anyone shut pairing for everyone.
	for i := 0; i < 50; i++ {
		pdo(h, call{method: http.MethodPost, path: "/api/pair", body: bad, remote: "10.0.1." + string(rune('a'+i)) + ":1"})
	}
	if rec := pdo(h, call{method: http.MethodPost, path: "/api/pair", body: `{"code":"` + good.Code + `","kind":"bearer"}`, remote: "10.0.0.2:1234"}); rec.Code != http.StatusCreated {
		t.Errorf("another address after 50 junk attempts = %d, want 201", rec.Code)
	}
}

func TestPairLimiterCountsConcurrentAttempts(t *testing.T) {
	l := newPairLimiter()
	now := time.Now()
	var mu sync.Mutex
	passed := 0
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.take("10.0.0.1", now) {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if passed != 5 {
		t.Errorf("%d of 20 concurrent attempts passed, want 5", passed)
	}
	l.refund("10.0.0.1", now)
	if !l.take("10.0.0.1", now) || l.take("10.0.0.1", now) {
		t.Error("a refund did not give back exactly one attempt")
	}
}

func TestASessionListsOnlyItselfAndWhatItPaired(t *testing.T) {
	_, h := pairingServer(t)
	a := pairBearer(t, h, mintCode(t, h, master, `{"canPair":true}`).Code)
	b := pairBearer(t, h, mintCode(t, h, master, `{}`).Code)
	child := pairBearer(t, h, mintCode(t, h, "Bearer "+a.Token, `{}`).Code)
	ids := func(auth string) map[string]bool {
		got := map[string]bool{}
		for _, row := range decode[[]SessionInfo](t, pdo(h, call{method: http.MethodGet, path: "/api/sessions", auth: auth})) {
			got[row.ID] = true
		}
		return got
	}
	if got := ids("Bearer " + a.Token); len(got) != 2 || !got[a.ID] || !got[child.ID] {
		t.Errorf("a sees %v, want itself and %s", got, child.ID)
	}
	if got := ids("Bearer " + b.Token); len(got) != 1 || !got[b.ID] {
		t.Errorf("b sees %v, want only itself", got)
	}
	if got := ids(master); len(got) != 3 {
		t.Errorf("master sees %v, want all three", got)
	}
}

func TestPairingRoutesWithCodesNeverReachTheTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := pairingServer(t)
	s.trace = tw
	h := s.Handler()
	code := mintCode(t, h, master, `{}`)
	sess := pairBearer(t, h, code.Code)
	pdo(h, call{method: http.MethodPost, path: "/api/pair", body: `{"code":"` + code.Code + `","kind":"bearer"}`})
	pdo(h, call{method: http.MethodPost, path: "/api/log", auth: master,
		body: `{"href":"https://mac.example:18456/pair?target=avd%3Ax#code=` + code.Code + `"}`})
	_ = tw.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{code.Code, sess.Token, strings.TrimPrefix(sess.Token, "lsn_")} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the trace holds %q:\n%s", secret, b)
		}
	}
	if !strings.Contains(string(b), `"op":"redeem"`) {
		t.Errorf("the redemption was not traced:\n%s", b)
	}
}

func TestMasterTokenIsUnchangedWithPairingOn(t *testing.T) {
	_, h := pairingServer(t)
	for _, c := range []call{
		{method: http.MethodGet, path: "/api/sessions", auth: master},
		{method: http.MethodGet, path: "/api/sessions", cookie: &http.Cookie{Name: tokenCookie, Value: "secret"}},
	} {
		if rec := pdo(h, c); rec.Code != http.StatusOK {
			t.Errorf("%+v = %d", c, rec.Code)
		}
	}
}

func TestPairingRoutesAnswer404WhenTheStoreIsUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{mgr: NewManager(target.Options{}), token: "secret"}
	if err := s.SetSessionsPath(path); err == nil {
		t.Fatal("SetSessionsPath accepted a broken file")
	}
	h := s.Handler()
	if rec := pdo(h, call{method: http.MethodPost, path: "/api/pairings", body: "{}", auth: master}); rec.Code != http.StatusNotFound {
		t.Errorf("POST /api/pairings = %d, want 404", rec.Code)
	}
	if got := decode[AuthResponse](t, pdo(h, call{method: http.MethodGet, path: "/api/auth", auth: master})); got.Pairing {
		t.Errorf("GET /api/auth says pairing is on: %+v", got)
	}
	if b, _ := os.ReadFile(path); string(b) != "{" {
		t.Error("the unreadable file was overwritten")
	}
}

func TestQUICOpenAcceptsAPairedSession(t *testing.T) {
	s, h := pairingServer(t)
	sess := pairBearer(t, h, mintCode(t, h, master, `{"label":"phone"}`).Code)
	if s.sessionFromToken(sess.Token) == nil {
		t.Fatalf("a live session token was refused")
	}
	if s.sessionFromToken("lsn_not-a-session") != nil || s.sessionFromToken("") != nil {
		t.Fatalf("an unknown token was accepted")
	}
	rec := pdo(h, call{method: http.MethodDelete, path: "/api/sessions/" + sess.ID, auth: master})
	if rec.Code >= 300 {
		t.Fatalf("DELETE session = %d %s", rec.Code, rec.Body)
	}
	if s.sessionFromToken(sess.Token) != nil {
		t.Fatalf("a revoked session token was accepted")
	}
}
