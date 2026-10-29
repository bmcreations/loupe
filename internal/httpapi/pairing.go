package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/pairing"
)

// sessionCookie holds a browser's session. HttpOnly, unlike loupe_token:
// nothing in the page reads it, and /wt does not take it.
const sessionCookie = "loupe_session"

// sessionCookieResend is how long a session cookie's Max-Age is left alone.
// The session slides on every use; re-sending the cookie on every response
// would only add bytes, so it is re-sent once a day.
const sessionCookieResend = 24 * time.Hour

// Credential methods, as GET /api/auth reports them.
const (
	credMaster  = "master"
	credSession = "session"
	credNone    = "none"
)

// credential is how a request authenticated. authed puts it on the
// request's context.
type credential struct {
	method  string
	session *pairing.Session
}

type credKey struct{}

func withCred(r *http.Request, c credential) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), credKey{}, c))
}

func credOf(r *http.Request) credential {
	if c, ok := r.Context().Value(credKey{}).(credential); ok {
		return c
	}
	return credential{method: credNone}
}

// canPair reports whether the credential may mint pairing codes: the master
// token, or a session paired with canPair.
func (c credential) canPair() bool {
	return c.method == credMaster || (c.session != nil && c.session.CanPair)
}

// parent is the id recorded as a new code's minter.
func (c credential) parent() string {
	if c.session != nil {
		return c.session.ID
	}
	return pairing.ParentMaster
}

// SetSessionsPath loads the session store from path and keeps it there. An
// unreadable file leaves pairing off rather than starting empty, since the
// first write would then sign every device out; the master token still works.
func (s *Server) SetSessionsPath(path string) error {
	st, err := pairing.Open(path)
	if err != nil {
		return err
	}
	s.pairing = st
	s.pairLimit = newPairLimiter()
	return nil
}

// isOpenAuthPath names the routes authed passes through with no credential:
// the code exchange, which is how a device without one gets one, and the
// probe that tells a client how it is authenticated, which has to answer
// "none".
func isOpenAuthPath(path string) bool {
	return path == "/api/pair" || path == "/api/auth"
}

// sessionFromBearer returns the session an lsn_ Bearer names, if it is live.
func (s *Server) sessionFromBearer(r *http.Request) *pairing.Session {
	tok := bearer(r)
	if s.pairing == nil || !strings.HasPrefix(tok, pairing.SessionPrefix) {
		return nil
	}
	if row, ok := s.pairing.Authenticate(tok); ok {
		return &row
	}
	return nil
}

// sessionFromCookie returns the session the loupe_session cookie names.
func (s *Server) sessionFromCookie(r *http.Request) *pairing.Session {
	if s.pairing == nil {
		return nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	if row, ok := s.pairing.Authenticate(c.Value); ok {
		return &row
	}
	return nil
}

// setSessionCookie writes the browser's session cookie.
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		// As for loupe_token: Secure on a plain-http run would make the
		// cookie unstorable.
		Secure:   r.TLS != nil,
		HttpOnly: true,
		MaxAge:   int(pairing.SessionTTL / time.Second),
	})
}

// refreshSessionCookie re-sends a cookie session's cookie when it was last
// set more than a day ago, so its Max-Age follows the sliding expiry.
func (s *Server) refreshSessionCookie(w http.ResponseWriter, r *http.Request, id string) {
	now := time.Now()
	s.cookieMu.Lock()
	last, ok := s.cookieSet[id]
	stale := !ok || now.Sub(last) > sessionCookieResend
	if stale {
		if s.cookieSet == nil {
			s.cookieSet = map[string]time.Time{}
		}
		s.cookieSet[id] = now
	}
	s.cookieMu.Unlock()
	if !stale {
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		setSessionCookie(w, r, c.Value)
	}
}

// pairLimiter counts failed POST /api/pair attempts over the last minute,
// per remote address. Only failures count, so a person who types a code
// right is never slowed by others, and an attacker gets 5 guesses a minute
// from one address. There is no cap across all addresses: it would let
// anyone who can reach the port keep pairing shut for everyone by sending
// junk, and a code's 60 bits make the per-address cap enough.
type pairLimiter struct {
	mu     sync.Mutex
	perIP  map[string][]time.Time
	window time.Duration
	ipMax  int
}

func newPairLimiter() *pairLimiter {
	return &pairLimiter{perIP: map[string][]time.Time{}, window: time.Minute, ipMax: 5}
}

func (l *pairLimiter) prune(now time.Time) {
	cut := now.Add(-l.window)
	for ip, ts := range l.perIP {
		i := 0
		for i < len(ts) && !ts[i].After(cut) {
			i++
		}
		if ts = ts[i:]; len(ts) == 0 {
			delete(l.perIP, ip)
		} else {
			l.perIP[ip] = ts
		}
	}
}

// take reports whether ip may try a code now and, if so, counts the attempt
// as a failure up front, so concurrent attempts from one address cannot all
// pass before any is counted. A right code gives it back with refund.
func (l *pairLimiter) take(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	if len(l.perIP[ip]) >= l.ipMax {
		return false
	}
	l.perIP[ip] = append(l.perIP[ip], now)
	return true
}

// refund uncounts an attempt take counted at now.
func (l *pairLimiter) refund(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.perIP[ip]
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].Equal(now) {
			l.perIP[ip] = append(ts[:i], ts[i+1:]...)
			return
		}
	}
}

func remoteHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// PairingRequest is POST /api/pairings' body.
type PairingRequest struct {
	// Target is the targets the paired device should open, carried to the
	// pairing link. Not secret, and not a restriction.
	Target []string `json:"target,omitempty"`
	// Label names the device the code is for. The redeemer's own label wins.
	Label string `json:"label,omitempty"`
	// CanPair lets the session the code is redeemed for mint codes itself.
	CanPair bool `json:"canPair,omitempty"`
}

// PairingResponse is a freshly minted code. Code is returned here and never
// again.
type PairingResponse struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// PairingInfo is a pending code as GET /api/pairings lists it, without the
// code.
type PairingInfo struct {
	ID        string    `json:"id"`
	Target    []string  `json:"target,omitempty"`
	Label     string    `json:"label,omitempty"`
	CanPair   bool      `json:"canPair,omitempty"`
	Parent    string    `json:"parent"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// PairRequest is POST /api/pair's body.
type PairRequest struct {
	Code string `json:"code"`
	// Kind is browser, bearer or peer. A browser gets its session as a
	// cookie and a 204; the others get the token in the body.
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
}

// PairResponse is a bearer or peer session, returned once.
type PairResponse struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// SessionInfo is a session row without its hash.
type SessionInfo struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Label      string     `json:"label,omitempty"`
	Parent     string     `json:"parent"`
	CanPair    bool       `json:"canPair,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt time.Time  `json:"lastUsedAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	UserAgent  string     `json:"userAgent,omitempty"`
	RemoteAddr string     `json:"remoteAddr,omitempty"`
	// Current marks the caller's own session.
	Current bool `json:"current,omitempty"`
}

func sessionInfo(row pairing.Session, current string) SessionInfo {
	return SessionInfo{
		ID: row.ID, Kind: row.Kind, Label: row.Label, Parent: row.Parent,
		CanPair: row.CanPair, CreatedAt: row.CreatedAt, LastUsedAt: row.LastUsedAt,
		ExpiresAt: row.ExpiresAt, RevokedAt: row.RevokedAt, UserAgent: row.UserAgent,
		RemoteAddr: row.RemoteAddr, Current: current != "" && row.ID == current,
	}
}

// AuthResponse is GET /api/auth: how the caller is authenticated, and
// whether this server pairs at all. A server from before pairing answers 404.
type AuthResponse struct {
	// Method is master, session or none.
	Method  string       `json:"method"`
	Session *SessionInfo `json:"session,omitempty"`
	// Pairing is false when the session store could not be read and only
	// the master token works.
	Pairing bool `json:"pairing"`
	// CanPair says whether the caller may mint codes.
	CanPair bool `json:"canPair"`
}

// RevokeResponse lists the sessions a DELETE signed out, cascade included.
type RevokeResponse struct {
	Revoked []string `json:"revoked"`
}

func (s *Server) pairingOff(w http.ResponseWriter) bool {
	if s.pairing != nil {
		return false
	}
	writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "pairing is off: the session store could not be read at startup"})
	return true
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	c := credOf(r)
	resp := AuthResponse{Method: c.method, Pairing: s.pairing != nil, CanPair: c.canPair() && s.pairing != nil}
	if c.session != nil {
		info := sessionInfo(*c.session, c.session.ID)
		resp.Session = &info
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePairings mints and lists pairing codes. Only the master token and
// a canPair session may, so a stolen phone session cannot add devices.
func (s *Server) handlePairings(w http.ResponseWriter, r *http.Request) {
	if s.pairingOff(w) {
		return
	}
	c := credOf(r)
	if !c.canPair() {
		writeJSON(w, http.StatusForbidden, ErrorResponse{Error: "this session cannot pair devices; pair from a device paired with canPair, or with the master token"})
		return
	}
	if r.Method == http.MethodGet {
		codes := s.pairing.Codes()
		out := make([]PairingInfo, 0, len(codes))
		for _, p := range codes {
			out = append(out, PairingInfo{ID: p.ID, Target: p.Target, Label: p.Label, CanPair: p.CanPair, Parent: p.Parent, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var req PairingRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "body: " + err.Error()})
		return
	}
	code, raw, err := s.pairing.Mint(pairing.MintRequest{Target: req.Target, Label: req.Label, CanPair: req.CanPair, Parent: c.parent()})
	if errors.Is(err, pairing.ErrParentGone) {
		writeJSON(w, http.StatusForbidden, ErrorResponse{Error: err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "saving the code: " + err.Error()})
		return
	}
	s.trace.Record(map[string]any{"kind": "pairing", "op": "mint", "id": code.ID, "parent": code.Parent, "canPair": code.CanPair, "targets": code.Target})
	writeJSON(w, http.StatusCreated, PairingResponse{ID: code.ID, Code: raw, ExpiresAt: code.ExpiresAt})
}

// handlePairingItem withdraws one pending code: DELETE /api/pairings/{id}.
func (s *Server) handlePairingItem(w http.ResponseWriter, r *http.Request) {
	if s.pairingOff(w) {
		return
	}
	if !credOf(r).canPair() {
		writeJSON(w, http.StatusForbidden, ErrorResponse{Error: "this session cannot pair devices"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/pairings/")
	ok, err := s.pairing.DeleteCode(id)
	switch {
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	case !ok:
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no pending code " + id})
	default:
		s.trace.Record(map[string]any{"kind": "pairing", "op": "withdraw", "id": id})
		writeJSON(w, http.StatusOK, OKResponse{OK: true})
	}
}

// handlePair exchanges a code for a session. It is the one write route that
// takes no credential, which is why it is rate limited and why the code is
// spent under the store's lock.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if s.pairingOff(w) {
		return
	}
	var req PairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "body: " + err.Error()})
		return
	}
	switch req.Kind {
	case pairing.KindBrowser, pairing.KindBearer, pairing.KindPeer:
	default:
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: `kind must be "browser", "bearer" or "peer"`})
		return
	}
	// A browser exchange sets a cookie, so a page elsewhere must not be able
	// to pair a visitor's browser into a session the page's author chose.
	if req.Kind == pairing.KindBrowser && !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, ErrorResponse{Error: "loupe: a browser pairing was sent from another origin"})
		return
	}
	ip := remoteHost(r)
	now := time.Now()
	if !s.pairLimit.take(ip, now) {
		s.trace.Record(map[string]any{"kind": "pairing", "op": "limited", "remote": ip})
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, ErrorResponse{Error: "too many failed pairing attempts; wait a minute"})
		return
	}
	row, token, code, err := s.pairing.Redeem(req.Code, pairing.RedeemRequest{
		Kind: req.Kind, Label: req.Label, UserAgent: r.UserAgent(), RemoteAddr: ip,
	})
	if !errors.Is(err, pairing.ErrBadCode) {
		s.pairLimit.refund(ip, now)
	}
	if errors.Is(err, pairing.ErrBadCode) {
		s.trace.Record(map[string]any{"kind": "pairing", "op": "refused", "remote": ip, "pairKind": req.Kind})
		writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "that code is not valid: it may have expired or been used. Ask for a new one."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "saving the session: " + err.Error()})
		return
	}
	s.trace.Record(map[string]any{"kind": "pairing", "op": "redeem", "code": code.ID, "session": row.ID, "parent": row.Parent, "pairKind": row.Kind, "label": row.Label, "remote": ip})
	if req.Kind == pairing.KindBrowser {
		setSessionCookie(w, r, token)
		s.cookieMu.Lock()
		if s.cookieSet == nil {
			s.cookieSet = map[string]time.Time{}
		}
		s.cookieSet[row.ID] = now
		s.cookieMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusCreated, PairResponse{ID: row.ID, Token: token, ExpiresAt: row.ExpiresAt})
}

// handleSessions lists sessions (GET) or revokes others (DELETE ?others=1):
// with the master token every session, with a session only itself (when
// listing) and those it paired,
// down the line. A stolen phone session must not be able to sign out every
// other device.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if s.pairingOff(w) {
		return
	}
	current := ""
	if c := credOf(r); c.session != nil {
		current = c.session.ID
	}
	if r.Method == http.MethodGet {
		// A session sees itself and the sessions it paired, not every
		// device's address and user agent.
		rows := s.pairing.Sessions()
		out := make([]SessionInfo, 0, len(rows))
		for _, row := range rows {
			if current != "" && row.ID != current && !s.pairing.Descends(row.ID, current) {
				continue
			}
			out = append(out, sessionInfo(row, current))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if r.URL.Query().Get("others") != "1" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "DELETE /api/sessions needs ?others=1; revoke one with DELETE /api/sessions/{id}"})
		return
	}
	var revoked []string
	var err error
	if current == "" {
		revoked, err = s.pairing.RevokeOthers("")
	} else {
		revoked, err = s.pairing.RevokeDescendants(current)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	s.recordRevoke(r, revoked)
	writeJSON(w, http.StatusOK, RevokeResponse{Revoked: nonNil(revoked)})
}

// handleSessionItem revokes one session and every session it paired:
// DELETE /api/sessions/{id}. A session may revoke itself or one it paired,
// down the line; only the master token may revoke any.
func (s *Server) handleSessionItem(w http.ResponseWriter, r *http.Request) {
	if s.pairingOff(w) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	if c := credOf(r); c.session != nil && id != c.session.ID && !s.pairing.Descends(id, c.session.ID) {
		if _, ok := s.pairing.Get(id); ok {
			writeJSON(w, http.StatusForbidden, ErrorResponse{Error: "a session can revoke only itself and the devices it paired; revoke " + id + " with the master token"})
			return
		}
	}
	revoked, ok, err := s.pairing.Revoke(id)
	switch {
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	case !ok:
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no session " + id})
	default:
		s.recordRevoke(r, revoked)
		writeJSON(w, http.StatusOK, RevokeResponse{Revoked: nonNil(revoked)})
	}
}

func (s *Server) recordRevoke(r *http.Request, ids []string) {
	s.cookieMu.Lock()
	for _, id := range ids {
		delete(s.cookieSet, id)
	}
	s.cookieMu.Unlock()
	rec := map[string]any{"kind": "pairing", "op": "revoke", "sessions": ids, "by": credOf(r).method}
	if c := credOf(r); c.session != nil {
		rec["bySession"] = c.session.ID
	}
	s.trace.Record(rec)
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}
