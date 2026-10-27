package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
)

// How the poller treats a peer that stops answering. They are vars so a test
// does not wait out a minute.
var (
	// pollEvery is the gap between polls of one peer.
	pollEvery = 2 * time.Second
	// staleAfter is how long a silent peer's rows stay as they were; a
	// reconnecting Wi-Fi link should not flicker the picker.
	staleAfter = 10 * time.Second
	// dropAfter is when a silent peer's rows leave the list.
	dropAfter = time.Minute
)

// Info is a peer as GET /api/servers shows it. It carries no token.
type Info struct {
	Name    string
	URL     string
	State   string
	Targets int
	RTTMs   int64
}

// Rows is one peer's last polled rows. Stale means the peer has stopped
// answering, and the rows are what it said last.
type Rows struct {
	Server string
	Stale  bool
	Rows   []json.RawMessage
}

// Registry is the set of peers, each polled on its own goroutine.
type Registry struct {
	store *Store
	trace func(map[string]any)

	// poll, stale and drop are the package vars as they stood when the
	// registry was made, so a test that restores them does not race the
	// pollers it left running.
	poll, stale, drop time.Duration

	// probe checks, when a server is added, that it will serve this
	// server's stream protocol. Nil skips the check.
	probe func(context.Context, Server) error

	mu       sync.Mutex
	peers    map[string]*poller
	onChange func()
	ctx      context.Context
}

// NewRegistry loads the stored peers. Nothing is polled until Start.
func NewRegistry(store *Store, trace func(map[string]any)) (*Registry, error) {
	if trace == nil {
		trace = func(map[string]any) {}
	}
	r := &Registry{store: store, trace: trace, peers: map[string]*poller{}, poll: pollEvery, stale: staleAfter, drop: dropAfter}
	servers, err := store.Load()
	if err != nil {
		return r, err
	}
	for _, s := range servers {
		r.peers[s.Name] = newPoller(r, s)
	}
	return r, nil
}

// SetOnChange registers f to run after every poll and whenever the list
// changes. It must not call back into the Registry while holding a lock the
// Registry's callers also take.
func (r *Registry) SetOnChange(f func()) {
	r.mu.Lock()
	r.onChange = f
	r.mu.Unlock()
}

// SetProbe sets the check Add makes before storing a server. An
// *UnsupportedProtocolError refuses the add; any other error is kept as the
// added server's Warning, since HTTP worked and UDP may only be blocked from
// here.
func (r *Registry) SetProbe(f func(context.Context, Server) error) {
	r.mu.Lock()
	r.probe = f
	r.mu.Unlock()
}

// Server returns the stored server named name, token and pin included, for a
// stream to dial. It is not for a response body.
func (r *Registry) Server(name string) (Server, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[name]
	if !ok {
		return Server{}, false
	}
	return p.srv, true
}

// Call sends one request to the named peer through its pinned, bearer
// client, and returns the answer whatever its status. ErrServerDown means
// there was none.
func (r *Registry) Call(ctx context.Context, name, method, path string, header http.Header, body []byte) (*Reply, error) {
	r.mu.Lock()
	p, ok := r.peers[name]
	r.mu.Unlock()
	if !ok {
		return nil, ErrServerDown
	}
	return p.client.Call(ctx, method, path, header, body)
}

// Start begins polling every stored peer, and any added later, until ctx ends.
func (r *Registry) Start(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	for _, p := range r.peers {
		p.start(ctx)
	}
	r.mu.Unlock()
}

func (r *Registry) changed() {
	r.mu.Lock()
	f := r.onChange
	r.mu.Unlock()
	if f != nil {
		f()
	}
}

// List describes every peer, by name.
func (r *Registry) List() []Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Info, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, p.info())
	}
	slices.SortFunc(out, func(a, b Info) int { return compareStrings(a.Name, b.Name) })
	return out
}

// Rows returns each peer's rows for the hub to merge, by name.
func (r *Registry) Rows() []Rows {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Rows, 0, len(r.peers))
	for _, p := range r.peers {
		if rows, ok := p.rows(); ok {
			out = append(out, rows)
		}
	}
	slices.SortFunc(out, func(a, b Rows) int { return compareStrings(a.Server, b.Server) })
	return out
}

// Names lists the peers' names.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.peers))
	for n := range r.peers {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// InvalidError is Add's refusal of a name, URL or token that cannot be used,
// before any call to the peer.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// NameTakenError is Add's refusal of a name already in the list.
type NameTakenError struct{ Name string }

func (e *NameTakenError) Error() string {
	return fmt.Sprintf("a server named %q already exists", e.Name)
}

// ConfirmFingerprintError is Add's refusal of a peer whose certificate does
// not verify against the system roots, until the person confirms
// Fingerprint, the certificate it presents, by passing it back as the Pin.
// No token has been sent when it is returned.
type ConfirmFingerprintError struct {
	Fingerprint string
	// Changed is set when a fingerprint was passed and the peer presents a
	// different one.
	Changed bool
}

func (e *ConfirmFingerprintError) Error() string {
	if e.Changed {
		return "the server now presents a different certificate from the one confirmed: " + e.Fingerprint
	}
	return "the server's certificate is not signed by a trusted authority; confirm its fingerprint: " + e.Fingerprint
}

// Add reaches the server, reads its targets, and only then stores it, so a
// typo never sits in the list looking healthy. An empty Name is derived from
// the host. The certificate is checked first, on a handshake that sends no
// token: one that verifies against the system roots is stored with no pin,
// and any other is refused with a *ConfirmFingerprintError until s.Pin
// carries its fingerprint, which is then pinned. The returned Server carries
// the pin and the name that was chosen.
func (r *Registry) Add(ctx context.Context, s Server) (Server, error) {
	url, err := NormalizeURL(s.URL)
	if err != nil {
		return s, &InvalidError{err.Error()}
	}
	s.URL = url
	if s.Token == "" {
		return s, &InvalidError{"a server needs a token"}
	}
	derived := s.Name == ""
	if derived {
		s.Name = r.freeName(DeriveName(s.URL))
	}
	if err := ValidName(s.Name); err != nil {
		return s, &InvalidError{err.Error()}
	}
	r.mu.Lock()
	_, taken := r.peers[s.Name]
	r.mu.Unlock()
	if taken {
		return s, &NameTakenError{s.Name}
	}

	confirmed := ""
	if s.Pin != "" {
		if confirmed = normalFingerprint(s.Pin); confirmed == "" {
			return s, &InvalidError{"a fingerprint is the certificate's SHA-256 in hex"}
		}
	}
	presented, err := probeCert(ctx, s.URL)
	if err != nil {
		return s, err
	}
	switch {
	case presented == "":
		// The chain verifies for the host: no pin, so a renewal is fine.
		s.Pin = ""
	case confirmed == "":
		return s, &ConfirmFingerprintError{Fingerprint: presented}
	case confirmed != presented:
		return s, &ConfirmFingerprintError{Fingerprint: presented, Changed: true}
	default:
		// The dial below requires exactly this certificate, so one swapped
		// in after the probe fails the handshake before the token is sent.
		s.Pin = confirmed
	}
	c := NewClient(s)
	defer c.Close()
	rows, err := c.Targets(ctx)
	if err != nil {
		return s, err
	}

	r.mu.Lock()
	probe := r.probe
	r.mu.Unlock()
	if probe != nil {
		if perr := probe(ctx, s); perr != nil {
			var up *UnsupportedProtocolError
			if errors.As(perr, &up) {
				return s, perr
			}
			s.Warning = "streaming may not work: " + perr.Error()
		}
	}

	r.mu.Lock()
	if _, taken := r.peers[s.Name]; taken {
		r.mu.Unlock()
		return s, &NameTakenError{s.Name}
	}
	p := newPoller(r, s)
	p.record(rows, true, 0)
	r.peers[s.Name] = p
	err = r.saveLocked()
	if err != nil {
		delete(r.peers, s.Name)
		r.mu.Unlock()
		return s, err
	}
	if r.ctx != nil {
		p.start(r.ctx)
	}
	r.mu.Unlock()
	r.trace(map[string]any{"kind": "peer", "server": s.Name, "url": s.URL, "state": "added", "pinned": s.Pin != ""})
	r.changed()
	return s, nil
}

// Remove drops a server and its rows. It reports whether there was one.
func (r *Registry) Remove(name string) (bool, error) {
	r.mu.Lock()
	p, ok := r.peers[name]
	if !ok {
		r.mu.Unlock()
		return false, nil
	}
	delete(r.peers, name)
	err := r.saveLocked()
	r.mu.Unlock()
	p.stop()
	r.trace(map[string]any{"kind": "peer", "server": name, "url": p.srv.URL, "state": "removed"})
	r.changed()
	return true, err
}

// freeName returns base, or base-2, base-3... when base is taken.
func (r *Registry) freeName(base string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := base
	for i := 2; ; i++ {
		if _, taken := r.peers[name]; !taken {
			return name
		}
		suffix := fmt.Sprintf("-%d", i)
		name = base[:min(len(base), 32-len(suffix))] + suffix
	}
}

// saveLocked persists the list. r.mu must be held.
func (r *Registry) saveLocked() error {
	servers := make([]Server, 0, len(r.peers))
	for _, p := range r.peers {
		servers = append(servers, p.srv)
	}
	slices.SortFunc(servers, func(a, b Server) int { return compareStrings(a.Name, b.Name) })
	return r.store.Save(servers)
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// poller polls one peer and remembers what it said.
type poller struct {
	reg    *Registry
	srv    Server
	client *Client

	mu     sync.Mutex
	rowsAt time.Time
	last   []json.RawMessage
	up     bool
	rtt    time.Duration
	cancel context.CancelFunc
	done   chan struct{}
}

func newPoller(r *Registry, s Server) *poller {
	return &poller{reg: r, srv: s, client: NewClient(s)}
}

func (p *poller) start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, p.cancel = context.WithCancel(ctx)
	p.done = make(chan struct{})
	go p.run(ctx, p.done)
}

func (p *poller) stop() {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	p.client.Close()
}

func (p *poller) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(p.reg.poll)
	defer t.Stop()
	for {
		p.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *poller) poll(ctx context.Context) {
	began := time.Now()
	rows, err := p.client.Targets(ctx)
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	was := p.up
	if err == nil {
		p.record(rows, true, time.Since(began))
	} else {
		p.up = false
	}
	p.mu.Unlock()
	if err == nil && !was {
		p.reg.trace(map[string]any{"kind": "peer", "server": p.srv.Name, "url": p.srv.URL, "state": "up"})
	}
	if err != nil && was {
		p.reg.trace(map[string]any{"kind": "peer", "server": p.srv.Name, "url": p.srv.URL, "state": "down", "error": err.Error()})
	}
	p.reg.changed()
}

// record stores a successful poll. p.mu must be held, or p not yet shared.
func (p *poller) record(rows []json.RawMessage, up bool, rtt time.Duration) {
	p.last, p.rowsAt, p.up, p.rtt = rows, time.Now(), up, rtt
}

func (p *poller) info() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := "down"
	if p.up {
		state = "up"
	}
	return Info{Name: p.srv.Name, URL: p.srv.URL, State: state, Targets: p.count(), RTTMs: p.rtt.Milliseconds()}
}

// rows is what the peer last said, aged: unchanged for staleAfter, then
// marked stale, then gone after dropAfter.
func (p *poller) rows() (Rows, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rowsAt.IsZero() {
		return Rows{}, false
	}
	age := time.Since(p.rowsAt)
	if age >= p.reg.drop {
		return Rows{}, false
	}
	return Rows{Server: p.srv.Name, Stale: age >= p.reg.stale, Rows: p.last}, true
}

// count is how many rows the peer is still listed with. p.mu must be held.
func (p *poller) count() int {
	if p.rowsAt.IsZero() || time.Since(p.rowsAt) >= p.reg.drop {
		return 0
	}
	return len(p.last)
}

// SetTimings replaces the poll interval and the stale and drop ages, and
// returns a func that puts the old ones back. Tests use it so a peer going
// quiet is seen in milliseconds, not a minute; nothing else should.
func SetTimings(poll, stale, drop time.Duration) (restore func()) {
	p, s, d := pollEvery, staleAfter, dropAfter
	pollEvery, staleAfter, dropAfter = poll, stale, drop
	return func() { pollEvery, staleAfter, dropAfter = p, s, d }
}
