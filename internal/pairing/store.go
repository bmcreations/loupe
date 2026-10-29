// Package pairing keeps device sessions and the one-time codes that create
// them.
//
// A pairing code is minted by the master token or by a session allowed to
// pair, and a person moves it to a new device, which exchanges it for a
// session of its own. Only hashes are stored: a copy of sessions.json holds
// no usable credential.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// SessionPrefix starts every session token, so the middleware tells one
	// from the master token without trying each check, and a secret scanner
	// can match a pasted one.
	SessionPrefix = "lsn_"

	// CodeTTL is how long a minted code can be redeemed.
	CodeTTL = 5 * time.Minute
	// SessionTTL is how long a session lives past its last use. It slides,
	// so a peer hub or a remote agent that keeps using its session never
	// has to be re-paired by hand.
	SessionTTL = 30 * 24 * time.Hour

	// touchEvery bounds how often a session's lastUsedAt reaches the disk.
	// Every request moves it in memory; a hub polling every 2 seconds must
	// not turn into a write every 2 seconds.
	touchEvery = 5 * time.Minute
	// keepRevoked is how long a revoked row stays listed, so the list can
	// show what was signed out.
	keepRevoked = 7 * 24 * time.Hour

	// ParentMaster is the parent of a session paired through a code the
	// master token minted.
	ParentMaster = "master"

	// codeAlphabet has no 0, 1, I or O, because the code is typed on a phone.
	codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	codeLen      = 12
)

// Kinds of session. A browser holds its session in a cookie; the others
// hold the token and send it as Authorization: Bearer.
const (
	KindBrowser = "browser"
	KindBearer  = "bearer"
	KindPeer    = "peer"
)

// ErrBadCode is every reason a redemption fails: unknown, expired, already
// spent, or minted by a session since revoked. One error for all of them, so
// a guesser learns nothing from which it was.
var ErrBadCode = errors.New("pairing code is not valid")

// ErrParentGone refuses a mint by a session that is no longer live.
var ErrParentGone = errors.New("the session minting this code is no longer valid")

// Session is one row of sessions.json.
type Session struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
	Kind string `json:"kind"`
	// Label names the device or client, as the person or client gave it.
	Label string `json:"label,omitempty"`
	// Parent is the session that minted the code this one was paired with,
	// or ParentMaster.
	Parent string `json:"parent"`
	// CanPair lets this session mint codes of its own.
	CanPair    bool       `json:"canPair,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt time.Time  `json:"lastUsedAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	// UserAgent and RemoteAddr are the redeeming request's, at pairing.
	UserAgent  string `json:"userAgent,omitempty"`
	RemoteAddr string `json:"remoteAddr,omitempty"`
}

// Live reports whether the session can still authenticate at now.
func (s *Session) Live(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// Code is a pending pairing code. The code itself is never kept.
type Code struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	Target    []string  `json:"target,omitempty"`
	Label     string    `json:"label,omitempty"`
	CanPair   bool      `json:"canPair,omitempty"`
	Parent    string    `json:"parent"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// MintRequest is what a code is minted for.
type MintRequest struct {
	Target []string
	Label  string
	// CanPair is passed on to the session the code is redeemed for.
	CanPair bool
	// Parent is the minting session's id, or ParentMaster.
	Parent string
}

// RedeemRequest describes the device exchanging a code.
type RedeemRequest struct {
	Kind       string
	Label      string
	UserAgent  string
	RemoteAddr string
}

// Store holds sessions and pending codes behind one lock, and writes every
// change through to its file before the change is visible.
type Store struct {
	mu   sync.Mutex
	path string
	now  func() time.Time

	sessions map[string]*Session // by hash
	byID     map[string]*Session
	codes    map[string]*Code // by hash
	// flushed is the lastUsedAt each session last had on disk.
	flushed map[string]time.Time
}

// Open loads the store at path. A missing file is an empty store. An
// unreadable one is an error and no store, rather than an empty store whose
// first write would sign every device out. An empty path keeps nothing on
// disk, which is what a test wants.
func Open(path string) (*Store, error) {
	s := &Store{
		path:     path,
		now:      time.Now,
		sessions: map[string]*Session{},
		byID:     map[string]*Session{},
		codes:    map[string]*Code{},
		flushed:  map[string]time.Time{},
	}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, row := range f.Sessions {
		if row == nil || row.ID == "" || row.Hash == "" {
			continue
		}
		s.sessions[row.Hash] = row
		s.byID[row.ID] = row
		s.flushed[row.ID] = row.LastUsedAt
	}
	for _, c := range f.Codes {
		if c == nil || c.Hash == "" {
			continue
		}
		s.codes[c.Hash] = c
	}
	return s, nil
}

// SetClock replaces the store's clock, for tests.
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

type file struct {
	Sessions []*Session `json:"sessions"`
	Codes    []*Code    `json:"codes"`
}

// Mint makes a pairing code and returns its public id and the code itself,
// formatted XXXX-XXXX-XXXX. The code is returned once and never again.
func (s *Store) Mint(req MintRequest) (Code, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if req.Parent == "" {
		req.Parent = ParentMaster
	}
	if req.Parent != ParentMaster {
		if p := s.byID[req.Parent]; p == nil || !p.Live(now) || !p.CanPair {
			return Code{}, "", ErrParentGone
		}
	}
	raw := newCode()
	c := &Code{
		ID:        newID(),
		Hash:      hashString(raw),
		Target:    append([]string(nil), req.Target...),
		Label:     req.Label,
		CanPair:   req.CanPair,
		Parent:    req.Parent,
		CreatedAt: now,
		ExpiresAt: now.Add(CodeTTL),
	}
	s.codes[c.Hash] = c
	if err := s.saveLocked(); err != nil {
		delete(s.codes, c.Hash)
		return Code{}, "", err
	}
	return *c, formatCode(raw), nil
}

// Redeem exchanges a code for a new session and returns the session and its
// token. Check, spend and insert happen under one lock, so two redemptions of
// one code cannot both succeed, and a failed write leaves the code unspent.
func (s *Store) Redeem(code string, req RedeemRequest) (Session, string, Code, error) {
	norm, ok := NormalizeCode(code)
	if !ok {
		return Session{}, "", Code{}, ErrBadCode
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	h := hashString(norm)
	c := s.codes[h]
	if c == nil || !now.Before(c.ExpiresAt) {
		return Session{}, "", Code{}, ErrBadCode
	}
	if c.Parent != ParentMaster {
		// A code outlives nothing it was minted by: a session revoked after
		// minting must not reach a new device through a code it left behind.
		if p := s.byID[c.Parent]; p == nil || !p.Live(now) || !p.CanPair {
			return Session{}, "", Code{}, ErrBadCode
		}
	}
	token := SessionPrefix + base64.RawURLEncoding.EncodeToString(randomBytes(32))
	label := req.Label
	if label == "" {
		label = c.Label
	}
	row := &Session{
		ID:         newID(),
		Hash:       hashString(token),
		Kind:       req.Kind,
		Label:      label,
		Parent:     c.Parent,
		CanPair:    c.CanPair,
		CreatedAt:  now,
		LastUsedAt: now,
		ExpiresAt:  now.Add(SessionTTL),
		UserAgent:  req.UserAgent,
		RemoteAddr: req.RemoteAddr,
	}
	delete(s.codes, h)
	s.sessions[row.Hash] = row
	s.byID[row.ID] = row
	if err := s.saveLocked(); err != nil {
		s.codes[h] = c
		delete(s.sessions, row.Hash)
		delete(s.byID, row.ID)
		return Session{}, "", Code{}, err
	}
	s.flushed[row.ID] = now
	return *row, token, *c, nil
}

// Authenticate looks a session token up and, when it is live, slides its
// expiry. The lookup is by hash, so the comparison never touches the secret.
// The new lastUsedAt reaches the disk at most every touchEvery.
func (s *Store) Authenticate(token string) (Session, bool) {
	if !strings.HasPrefix(token, SessionPrefix) {
		return Session{}, false
	}
	h := hashString(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.sessions[h]
	now := s.now()
	if row == nil || !row.Live(now) {
		return Session{}, false
	}
	row.LastUsedAt = now
	row.ExpiresAt = now.Add(SessionTTL)
	if now.Sub(s.flushed[row.ID]) >= touchEvery {
		// A failed write costs only the slide since the last one.
		// Either way the next try waits touchEvery, so a failing disk is not
		// rewritten on every request under the store's lock.
		_ = s.saveLocked()
		s.flushed[row.ID] = now
	}
	return *row, true
}

// Get returns a session by id, live or not.
func (s *Store) Get(id string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.byID[id]
	if row == nil {
		return Session{}, false
	}
	return *row, true
}

// Sessions lists every row, oldest first: live ones, and revoked ones until
// they are pruned.
func (s *Store) Sessions() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	out := make([]Session, 0, len(s.byID))
	for _, row := range s.byID {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Codes lists pending codes, oldest first. Each carries its hash, never the
// code.
func (s *Store) Codes() []Code {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	out := make([]Code, 0, len(s.codes))
	for _, c := range s.codes {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// DeleteCode withdraws a pending code. It reports false when no pending code
// has that id.
func (s *Store) DeleteCode(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, c := range s.codes {
		if c.ID == id {
			delete(s.codes, h)
			if err := s.saveLocked(); err != nil {
				s.codes[h] = c
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// Revoke signs a session out, and with it every session paired through a
// code it minted, theirs in turn, and every code they left pending. Without
// the cascade a stolen session that can pair would survive its own
// revocation on the second device it paired. It returns the ids newly
// revoked; ok is false when no session has that id.
func (s *Store) Revoke(id string) (revoked []string, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID[id] == nil {
		return nil, false, nil
	}
	revoked, err = s.revokeLocked(s.subtreeLocked(id))
	return revoked, true, err
}

// RevokeDescendants revokes every session paired, directly or down the
// line, through codes that id minted, and keeps id itself.
func (s *Store) RevokeDescendants(id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.subtreeLocked(id)
	delete(set, id)
	return s.revokeLocked(set)
}

// Descends reports whether id was paired through a code ancestor minted,
// directly or down the line. A session does not descend from itself.
func (s *Store) Descends(id, ancestor string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for row := s.byID[id]; row != nil && !seen[row.ID]; row = s.byID[row.Parent] {
		seen[row.ID] = true
		if row.Parent == ancestor {
			return true
		}
	}
	return false
}

// subtreeLocked is id and every session paired through it, down the line.
func (s *Store) subtreeLocked(id string) map[string]bool {
	set := map[string]bool{id: true}
	for grew := true; grew; {
		grew = false
		for _, row := range s.byID {
			if !set[row.ID] && set[row.Parent] {
				set[row.ID] = true
				grew = true
			}
		}
	}
	return set
}

// RevokeOthers revokes every session but keep, which may be empty to revoke
// them all. keep survives even when its own parent goes.
func (s *Store) RevokeOthers(keep string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[string]bool{}
	for _, row := range s.byID {
		if row.ID != keep {
			set[row.ID] = true
		}
	}
	return s.revokeLocked(set)
}

func (s *Store) revokeLocked(set map[string]bool) ([]string, error) {
	now := s.now()
	type undo struct {
		row *Session
		was *time.Time
	}
	var undos []undo
	var revoked []string
	for id := range set {
		row := s.byID[id]
		if row == nil || row.RevokedAt != nil {
			continue
		}
		undos = append(undos, undo{row, row.RevokedAt})
		at := now
		row.RevokedAt = &at
		revoked = append(revoked, id)
	}
	dropped := map[string]*Code{}
	for h, c := range s.codes {
		if set[c.Parent] {
			dropped[h] = c
			delete(s.codes, h)
		}
	}
	if len(undos) == 0 && len(dropped) == 0 {
		return nil, nil
	}
	if err := s.saveLocked(); err != nil {
		for _, u := range undos {
			u.row.RevokedAt = u.was
		}
		for h, c := range dropped {
			s.codes[h] = c
		}
		return nil, err
	}
	sort.Strings(revoked)
	return revoked, nil
}

// pruneLocked drops spent-by-time codes, expired sessions, and revoked
// sessions older than keepRevoked.
func (s *Store) pruneLocked(now time.Time) {
	for h, c := range s.codes {
		if !now.Before(c.ExpiresAt) {
			delete(s.codes, h)
		}
	}
	dropped := map[string]*Session{}
	for _, row := range s.sessions {
		switch {
		case row.RevokedAt != nil:
			if now.Sub(*row.RevokedAt) > keepRevoked {
				dropped[row.ID] = row
			}
		case !now.Before(row.ExpiresAt):
			dropped[row.ID] = row
		}
	}
	if len(dropped) == 0 {
		return
	}
	// A session outlives a parent that expired from disuse. It takes the
	// nearest ancestor still kept as its parent, so revoking that ancestor
	// still reaches it.
	for _, row := range s.sessions {
		if dropped[row.ID] != nil {
			continue
		}
		for p := dropped[row.Parent]; p != nil; p = dropped[p.Parent] {
			row.Parent = p.Parent
		}
	}
	// A pending code dies with its minter rather than move to an ancestor
	// whose authority it never had.
	for h, c := range s.codes {
		if dropped[c.Parent] != nil {
			delete(s.codes, h)
		}
	}
	for h, row := range s.sessions {
		if dropped[row.ID] != nil {
			delete(s.sessions, h)
			delete(s.byID, row.ID)
			delete(s.flushed, row.ID)
		}
	}
}

// saveLocked prunes and writes the store atomically, 0600, the way
// peer.Store does. Nothing in the file is a credential, so the mode is
// defence in depth.
func (s *Store) saveLocked() error {
	s.pruneLocked(s.now())
	if s.path == "" {
		return nil
	}
	f := file{Sessions: []*Session{}, Codes: []*Code{}}
	for _, row := range s.byID {
		f.Sessions = append(f.Sessions, row)
	}
	for _, c := range s.codes {
		f.Codes = append(f.Codes, c)
	}
	sort.Slice(f.Sessions, func(i, j int) bool { return f.Sessions[i].ID < f.Sessions[j].ID })
	sort.Slice(f.Codes, func(i, j int) bool { return f.Codes[i].ID < f.Codes[j].ID })
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	// WriteFile keeps the mode of a tmp file left over by a crashed run.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// NormalizeCode reads a code the way a person types it: any case, with or
// without dashes and spaces. ok is false for anything that is not 12 symbols
// of the alphabet.
func NormalizeCode(in string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(in) {
		switch {
		case r == '-' || r == ' ':
			continue
		case strings.ContainsRune(codeAlphabet, r):
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	if b.Len() != codeLen {
		return "", false
	}
	return b.String(), true
}

func formatCode(norm string) string {
	return norm[0:4] + "-" + norm[4:8] + "-" + norm[8:12]
}

// newCode draws 12 symbols. 256 is a multiple of 32, so masking a byte to
// five bits is unbiased.
func newCode() string {
	raw := randomBytes(codeLen)
	out := make([]byte, codeLen)
	for i, b := range raw {
		out[i] = codeAlphabet[b&31]
	}
	return string(out)
}

func newID() string { return hex.EncodeToString(randomBytes(8)) }

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("loupe: no entropy: " + err.Error())
	}
	return b
}
