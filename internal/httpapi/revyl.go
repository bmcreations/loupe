package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// Revyl cloud devices: a session started through the user's own revyl CLI
// and account becomes a target.RevylTarget. The CLI's shapes are read from
// revyl-cli v0.2.27; internal/target/revyl.go says which were checked
// against a live account.

const (
	revylStarting = "starting"
	revylActive   = "active"
	revylFailed   = "failed"

	// revylStartTimeout covers a queued device: `revyl device start` waits
	// for the device itself.
	revylStartTimeout = 6 * time.Minute
	// revylStartGrace is how long an interrupted `revyl device start` has
	// to cancel its device at Revyl before it is killed.
	revylStartGrace    = 5 * time.Second
	revylCallTimeout   = 45 * time.Second
	revylSignInTimeout = 15 * time.Minute
	// revylLoginWait is how long POST /api/revyl/account waits for the CLI
	// to print the approval URL and code.
	revylLoginWait  = 15 * time.Second
	revylWatchEvery = time.Minute
	// revylListGrace is how long Sync trusts loupe over `revyl device
	// list` after a start or a stop, in case the list lags either.
	revylListGrace = 2 * time.Minute
)

var (
	validRevylPlatform = map[string]bool{"android": true, "ios": true}
	// Model and OS names go to the CLI as flag values. The leading letter
	// or digit keeps one from reading as a flag, and the rest keeps them
	// to what the targets list prints ("Pixel 7", "iPhone 16 Pro",
	// "Android 14", "iOS 18.5").
	validRevylName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()+-]{0,63}$`)
	revylLoginURL  = regexp.MustCompile(`https://\S+`)
	revylLoginCode = regexp.MustCompile(`Code:\s*(\S+)`)

	errRevylNoCLI    = errors.New("the revyl CLI is not installed")
	errRevylBusy     = errors.New("another Revyl device is starting")
	errRevylNoDevice = errors.New("no such Revyl device")
	errRevylStarting = errors.New("the Revyl device is still starting; stop it once it is up")
	errRevylHolding  = errors.New("stop the Revyl devices loupe started before signing out")
	errRevylClosing  = errors.New("loupe is shutting down")
)

// RevylDevice is one Revyl session loupe started or found on the account.
type RevylDevice struct {
	// Key is loupe's handle, set from POST until the device is stopped.
	Key string `json:"key"`
	// SessionID is Revyl's, once the device has started.
	SessionID string `json:"sessionId,omitempty"`
	// Target is the target ID, revyl:<sessionId>, once there is one.
	Target    string `json:"target,omitempty"`
	Platform  string `json:"platform"`
	Model     string `json:"model,omitempty"`
	OSVersion string `json:"osVersion,omitempty"`
	// State is "starting", "active" or "failed"; Error says why for failed.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Adopted is true for a session loupe found on the account rather than
	// started. loupe leaves it running when it exits.
	Adopted   bool      `json:"adopted,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	// IdleTimeoutSec is how long Revyl keeps the device without a command.
	IdleTimeoutSec int `json:"idleTimeoutSec,omitempty"`

	// activated is when the row became active, for Sync.
	activated time.Time
}

// RevylListResponse is GET /api/revyl.
type RevylListResponse struct {
	Devices []RevylDevice `json:"devices"`
}

// StartRevylRequest is POST /api/revyl. Model and osVersion are optional
// and come from GET /api/revyl/targets.
type StartRevylRequest struct {
	Platform  string `json:"platform"`
	Model     string `json:"model,omitempty"`
	OSVersion string `json:"osVersion,omitempty"`
}

// StartRevylResponse is POST /api/revyl's 202. Poll GET /api/revyl for the
// device with this key to read its target once it is active.
type StartRevylResponse struct {
	Key string `json:"key"`
}

// RevylAccountResponse is whether the revyl CLI is installed and signed
// in. The account's key never appears here.
type RevylAccountResponse struct {
	CLIInstalled bool `json:"cliInstalled"`
	// CanInstallCLI is true when POST /api/revyl/cli can install it.
	CanInstallCLI   bool   `json:"canInstallCLI,omitempty"`
	CLIInstalling   bool   `json:"cliInstalling,omitempty"`
	CLIInstallError string `json:"cliInstallError,omitempty"`
	SignedIn        bool   `json:"signedIn"`
	Email           string `json:"email,omitempty"`
	Org             string `json:"org,omitempty"`
	// AuthMethod is the CLI's word for how it is signed in, such as a login
	// or REVYL_API_KEY in loupe's environment.
	AuthMethod string `json:"authMethod,omitempty"`
	// SigningIn is true while `revyl auth login` waits for approval. Open
	// LoginURL and confirm LoginCode there; poll until signedIn.
	SigningIn  bool   `json:"signingIn,omitempty"`
	LoginURL   string `json:"loginURL,omitempty"`
	LoginCode  string `json:"loginCode,omitempty"`
	LoginError string `json:"loginError,omitempty"`
}

// RevylModel is one model and OS version a device can start on.
type RevylModel struct {
	Model   string `json:"model"`
	Runtime string `json:"runtime"`
}

// RevylTargetsResponse is GET /api/revyl/targets: models per platform.
type RevylTargetsResponse struct {
	Platforms map[string][]RevylModel `json:"platforms"`
}

// RevylDevices starts and stops Revyl devices and registers each as a
// target.
type RevylDevices struct {
	find       func() string
	dir        string
	register   func(target.Target)
	unregister func(id, reason string)
	trace      func(map[string]any)
	// newTarget builds the target for a session; tests swap it to stream
	// from a local WHEP server.
	newTarget func(cli target.RevylCLI, s target.RevylSession, model, os string) target.Target

	// listGrace is revylListGrace; tests shorten it.
	listGrace time.Duration
	// startGrace is revylStartGrace; tests shorten it.
	startGrace time.Duration

	mu       sync.Mutex
	items    []*RevylDevice
	starting bool
	// cancelStart interrupts the start in flight, and startDone closes
	// once it has finished, including stopping a device it was left with.
	cancelStart context.CancelFunc
	startDone   chan struct{}
	// closing is set by StopAll, after which no start begins. exitCtx is
	// StopAll's, and bounds the stop of a device a start brings up after.
	closing bool
	exitCtx context.Context
	// stopped is when loupe stopped each session, so Sync does not adopt
	// one the list still shows.
	stopped      map[string]time.Time
	login        *revylLogin
	lastLoginErr string

	installer  *RevylInstaller
	installing bool
	installErr string

	// rest sends targets' input over Revyl's device proxy; nil leaves it
	// to the CLI. Set before Watch, read by newTarget.
	rest *target.RevylREST
}

type revylLogin struct {
	url, code string
	cancel    context.CancelFunc
	shown     chan struct{} // closed once url and code are read, or the CLI exits
}

// NewRevylDevices looks the CLI up with find on every call, so one
// installed later needs no restart. dir is where the CLI runs
// (target.RevylWorkDir).
func NewRevylDevices(find func() string, dir string, register func(target.Target), unregister func(id, reason string), trace func(map[string]any)) *RevylDevices {
	r := &RevylDevices{find: find, dir: dir, register: register, unregister: unregister, trace: trace,
		listGrace: revylListGrace, startGrace: revylStartGrace, stopped: map[string]time.Time{}}
	r.newTarget = func(cli target.RevylCLI, s target.RevylSession, model, os string) target.Target {
		return &target.RevylTarget{CLI: cli, Session: s, Model: model, OSVersion: os, REST: r.rest, Trace: r.record}
	}
	return r
}

// SetRevylDevices enables /api/revyl*. Without it those routes answer 404.
func (s *Server) SetRevylDevices(r *RevylDevices) { s.revyl = r }

// SetInstaller lets POST /api/revyl/cli download the CLI.
func (r *RevylDevices) SetInstaller(in *RevylInstaller) { r.installer = in }

// SetREST sends targets' gestures over Revyl's device proxy, with the CLI
// as the fallback. Without it every gesture is a CLI process. Call it
// before Watch.
func (r *RevylDevices) SetREST(rest *target.RevylREST) { r.rest = rest }

func (r *RevylDevices) cli() (target.RevylCLI, bool) {
	path := ""
	if r.find != nil {
		path = r.find()
	}
	if path != "" && r.dir != "" {
		// The CLI runs in this directory, and exec fails on one that is
		// missing.
		_ = os.MkdirAll(r.dir, 0o755)
	}
	return target.RevylCLI{Path: path, Dir: r.dir}, path != ""
}

func (r *RevylDevices) record(rec map[string]any) {
	if r.trace != nil {
		rec["kind"] = "revyl"
		r.trace(rec)
	}
}

func newRevylKey() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Start begins starting a device and returns its key at once.
func (r *RevylDevices) Start(req StartRevylRequest) (string, error) {
	if !validRevylPlatform[req.Platform] {
		return "", errors.New("platform must be android or ios")
	}
	for _, v := range []string{req.Model, req.OSVersion} {
		if v != "" && !validRevylName.MatchString(v) {
			return "", errors.New("model and osVersion are names from /api/revyl/targets")
		}
	}
	cli, ok := r.cli()
	if !ok {
		return "", errRevylNoCLI
	}
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return "", errRevylClosing
	}
	if r.starting {
		r.mu.Unlock()
		return "", errRevylBusy
	}
	r.starting = true
	ctx, cancel := context.WithTimeout(context.Background(), revylStartTimeout)
	r.cancelStart, r.startDone = cancel, make(chan struct{})
	it := &RevylDevice{Key: newRevylKey(), Platform: req.Platform, Model: req.Model, OSVersion: req.OSVersion,
		State: revylStarting, StartedAt: time.Now()}
	r.items = append(r.items, it)
	done := r.startDone
	r.mu.Unlock()
	go r.start(ctx, cancel, done, cli, it.Key, req)
	return it.Key, nil
}

func (r *RevylDevices) start(ctx context.Context, cancel context.CancelFunc, done chan struct{}, cli target.RevylCLI, key string, req StartRevylRequest) {
	defer close(done)
	defer cancel()
	began := time.Now()
	sess, err := cli.Start(ctx, target.RevylStart{Platform: req.Platform, Model: req.Model, OSVersion: req.OSVersion, Grace: r.startGrace})
	rec := map[string]any{"action": "start", "platform": req.Platform, "model": req.Model, "tookMs": time.Since(began).Milliseconds()}
	r.mu.Lock()
	r.starting = false
	r.cancelStart, r.startDone = nil, nil
	it := r.findLocked(key)
	var t target.Target
	var leftover string
	stopCtx := r.exitCtx
	switch {
	case sess.SessionID != "" && (err != nil || r.closing || it == nil):
		// The device came up, but the start was cut short, loupe is
		// exiting, or the row is gone. Nothing will hold it, so stop it
		// rather than leave it billing until it idles out.
		leftover = sess.SessionID
		r.stopped[leftover] = time.Now()
		msg := errRevylClosing.Error()
		if err != nil {
			msg = err.Error()
		}
		if it != nil {
			it.State, it.Error = revylFailed, msg
		}
		rec["error"], rec["target"] = msg, "revyl:"+leftover
	case it == nil:
		// Forgotten while starting; nothing to attach the session to.
	case err != nil:
		it.State, it.Error = revylFailed, err.Error()
		rec["error"] = it.Error
	default:
		it.State, it.SessionID, it.Target = revylActive, sess.SessionID, "revyl:"+sess.SessionID
		it.activated = time.Now()
		it.IdleTimeoutSec = int(sess.IdleTimeout / time.Second)
		t = r.newTarget(cli, sess, req.Model, req.OSVersion)
		rec["target"] = it.Target
	}
	r.mu.Unlock()
	if t != nil && r.register != nil {
		r.register(t)
	}
	r.record(rec)
	if leftover == "" {
		return
	}
	if stopCtx == nil {
		c, cancel := context.WithTimeout(context.Background(), revylCallTimeout)
		defer cancel()
		stopCtx = c
	}
	stop := map[string]any{"action": "stop", "target": "revyl:" + leftover}
	if err := cli.Stop(stopCtx, leftover); err != nil {
		stop["error"] = err.Error()
	}
	r.record(stop)
}

func (r *RevylDevices) findLocked(key string) *RevylDevice {
	for _, it := range r.items {
		if it.Key == key {
			return it
		}
	}
	return nil
}

// Stop ends a device by key or target ID. A failed row is forgotten. The
// target goes first, so its sessions end before the device does.
func (r *RevylDevices) Stop(ctx context.Context, key string) error {
	r.mu.Lock()
	var it *RevylDevice
	for _, c := range r.items {
		if c.Key == key || (c.Target != "" && c.Target == key) {
			it = c
		}
	}
	if it == nil {
		r.mu.Unlock()
		return errRevylNoDevice
	}
	if it.State == revylStarting {
		r.mu.Unlock()
		return errRevylStarting
	}
	r.removeLocked(it)
	if it.State == revylActive {
		r.stopped[it.SessionID] = time.Now()
	}
	r.mu.Unlock()
	if it.State != revylActive {
		return nil
	}
	if r.unregister != nil {
		r.unregister(it.Target, "stopped")
	}
	cli, _ := r.cli()
	err := cli.Stop(ctx, it.SessionID)
	rec := map[string]any{"action": "stop", "target": it.Target}
	if err != nil {
		rec["error"] = err.Error()
	}
	r.record(rec)
	return err
}

func (r *RevylDevices) removeLocked(it *RevylDevice) {
	for i, c := range r.items {
		if c == it {
			r.items = append(r.items[:i], r.items[i+1:]...)
			return
		}
	}
}

// List is every device loupe knows, oldest first.
func (r *RevylDevices) List() []RevylDevice {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RevylDevice, 0, len(r.items))
	for _, it := range r.items {
		out = append(out, *it)
	}
	return out
}

// Sync reads the account's sessions from `revyl device list`: one loupe
// does not know is adopted as a target, and an active one the list no
// longer has has ended. It does nothing when the CLI is missing or the
// list fails, such as when signed out.
func (r *RevylDevices) Sync(ctx context.Context) {
	cli, ok := r.cli()
	if !ok {
		return
	}
	began := time.Now()
	sessions, err := cli.List(ctx)
	if err != nil {
		return
	}
	live := map[string]target.RevylSession{}
	for _, s := range sessions {
		if s.SessionID != "" && validRevylPlatform[s.Platform] {
			live[s.SessionID] = s
		}
	}
	var adopt []target.Target
	var ended []*RevylDevice
	r.mu.Lock()
	for id, at := range r.stopped {
		if time.Since(at) >= r.listGrace {
			delete(r.stopped, id)
		}
	}
	known := map[string]bool{}
	for _, it := range append([]*RevylDevice(nil), r.items...) {
		if it.State != revylActive {
			continue
		}
		known[it.SessionID] = true
		// The list was read before this row went active, or so soon
		// after loupe started it that Revyl may not list it yet.
		fresh := it.activated.After(began) || (!it.Adopted && time.Since(it.activated) < r.listGrace)
		if _, ok := live[it.SessionID]; !ok && !fresh {
			r.removeLocked(it)
			ended = append(ended, it)
		}
	}
	for id, s := range live {
		if known[id] {
			continue
		}
		// Stopped by loupe while the list was read, or since.
		if _, ok := r.stopped[id]; ok {
			continue
		}
		// A start in flight has no session ID yet; its session can show up
		// here first. Leave it to the start rather than list it twice.
		if r.starting {
			continue
		}
		it := &RevylDevice{Key: newRevylKey(), SessionID: id, Target: "revyl:" + id, Platform: s.Platform,
			State: revylActive, Adopted: true, StartedAt: s.StartedAt, IdleTimeoutSec: int(s.IdleTimeout / time.Second),
			activated: time.Now()}
		r.items = append(r.items, it)
		adopt = append(adopt, r.newTarget(cli, s, "", ""))
	}
	r.mu.Unlock()
	for _, it := range ended {
		if r.unregister != nil {
			r.unregister(it.Target, "ended")
		}
		r.record(map[string]any{"action": "ended", "target": it.Target})
	}
	for _, t := range adopt {
		if r.register != nil {
			r.register(t)
		}
		r.record(map[string]any{"action": "adopt", "target": t.ID()})
	}
}

// Watch syncs once, then every minute while loupe holds a device, so one
// Revyl stopped (idle timeout, or from elsewhere) leaves the picker.
// `revyl device list` reads Revyl's backend, not the device, so it is not
// a command that keeps a device from going idle (cmd/revyl/device.go:46,
// syncDeviceSessions).
func (r *RevylDevices) Watch(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, revylCallTimeout)
	r.Sync(c)
	cancel()
	tick := time.NewTicker(revylWatchEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if r.holding(false) {
				c, cancel := context.WithTimeout(ctx, revylCallTimeout)
				r.Sync(c)
				cancel()
			}
		}
	}
}

// holding reports whether any device is active, or with startedOnly, any
// loupe started itself.
func (r *RevylDevices) holding(startedOnly bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, it := range r.items {
		if (it.State == revylActive && (!startedOnly || !it.Adopted)) || it.State == revylStarting {
			return true
		}
	}
	return false
}

// StopAll stops every device loupe started, at shutdown, so none keeps
// billing until its idle timeout. Adopted ones are left running. A start
// in flight is interrupted rather than waited out, since Revyl's queue can
// hold it for minutes, and a device it brings up anyway is stopped. No
// start begins after. It returns when all of that is done or ctx ends.
func (r *RevylDevices) StopAll(ctx context.Context) {
	r.mu.Lock()
	r.closing, r.exitCtx = true, ctx
	var keys []string
	for _, it := range r.items {
		if it.State == revylActive && !it.Adopted {
			keys = append(keys, it.Key)
		}
	}
	cancelStart, startDone := r.cancelStart, r.startDone
	r.mu.Unlock()
	if cancelStart != nil {
		cancelStart()
	}
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Stop(ctx, k)
		}()
	}
	if startDone != nil {
		select {
		case <-startDone:
		case <-ctx.Done():
		}
	}
	wg.Wait()
}

// decorate adds a row for each device still starting, so the picker shows
// it before it has a target.
func (r *RevylDevices) decorate(rows []TargetInfo) []TargetInfo {
	if r == nil {
		return rows
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, it := range r.items {
		if it.State != revylStarting {
			continue
		}
		label := "Revyl " + it.Model
		if it.Model == "" {
			label = map[string]string{"android": "Revyl Android", "ios": "Revyl iOS"}[it.Platform]
		}
		row := TargetInfo{ID: "revyl-starting:" + it.Key, Kind: string(target.KindRevyl), Label: label,
			State: revylStarting, Reason: "Starting…"}
		row.OS, row.OSVersion, row.Transport = it.Platform, it.OSVersion, "cloud"
		rows = append(rows, row)
	}
	return rows
}

// ---- account ----

func (r *RevylDevices) account(ctx context.Context) RevylAccountResponse {
	r.mu.Lock()
	acct := RevylAccountResponse{CanInstallCLI: r.installer != nil, CLIInstalling: r.installing,
		CLIInstallError: r.installErr, LoginError: r.lastLoginErr}
	if l := r.login; l != nil {
		acct.SigningIn, acct.LoginURL, acct.LoginCode = true, l.url, l.code
	}
	r.mu.Unlock()
	cli, ok := r.cli()
	if acct.CLIInstalled = ok; !ok {
		return acct
	}
	a, err := cli.AuthStatus(ctx)
	if err != nil {
		return acct
	}
	acct.SignedIn = a.Authenticated && !a.Expired
	acct.Email, acct.Org, acct.AuthMethod = a.Email, a.OrgName, a.AuthMethod
	return acct
}

// signIn runs `revyl auth login`, which prints an approval URL and code to
// stderr and waits for them to be approved in a browser
// (cmd/revyl/auth.go:340 loginWithDeviceApproval; it also opens the URL on
// this Mac). It returns once both are read, or the CLI exits, or
// revylLoginWait passes. Neither goes to the trace.
func (r *RevylDevices) signIn(ctx context.Context) error {
	cli, ok := r.cli()
	if !ok {
		return errRevylNoCLI
	}
	r.mu.Lock()
	l := r.login
	if l == nil {
		lctx, cancel := context.WithTimeout(context.Background(), revylSignInTimeout)
		l = &revylLogin{cancel: cancel, shown: make(chan struct{})}
		r.login, r.lastLoginErr = l, ""
		cmd := cli.Command(lctx, nil, "auth", "login")
		stderr, err := cmd.StderrPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			r.login = nil
			r.mu.Unlock()
			cancel()
			return err
		}
		go r.watchLogin(l, cmd.Wait, bufio.NewScanner(stderr))
	}
	r.mu.Unlock()
	select {
	case <-l.shown:
	case <-ctx.Done():
	case <-time.After(revylLoginWait):
	}
	return nil
}

func (r *RevylDevices) watchLogin(l *revylLogin, wait func() error, sc *bufio.Scanner) {
	var last string
	shown := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			last = line
		}
		r.mu.Lock()
		if l.url == "" {
			l.url = revylLoginURL.FindString(line)
		}
		if m := revylLoginCode.FindStringSubmatch(line); m != nil && l.code == "" {
			l.code = m[1]
		}
		ready := l.url != "" && l.code != ""
		r.mu.Unlock()
		if ready && !shown {
			shown = true
			close(l.shown)
		}
	}
	err := wait()
	l.cancel()
	r.mu.Lock()
	if r.login == l {
		r.login = nil
	}
	rec := map[string]any{"action": "signin"}
	if err != nil {
		// The CLI's own last word, minus the URL and code.
		msg := revylLoginURL.ReplaceAllString(last, "<url>")
		msg = revylLoginCode.ReplaceAllString(msg, "Code: <code>")
		if msg == "" {
			msg = err.Error()
		}
		r.lastLoginErr = msg
		rec["error"] = msg
	}
	r.mu.Unlock()
	if !shown {
		close(l.shown)
	}
	r.record(rec)
}

// cancelLogin stops a login that is waiting for approval.
func (r *RevylDevices) cancelLogin() {
	r.mu.Lock()
	l := r.login
	r.mu.Unlock()
	if l != nil {
		l.cancel()
	}
}

// installCLI starts a download and returns at once; GET /api/revyl/account
// reports cliInstalling and, after a failure, cliInstallError.
func (r *RevylDevices) installCLI() {
	r.mu.Lock()
	if r.installing {
		r.mu.Unlock()
		return
	}
	r.installing, r.installErr = true, ""
	r.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cliInstallTimeout)
		defer cancel()
		err := r.installer.Install(ctx)
		rec := map[string]any{"action": "install-cli", "version": revylCLIVersion}
		r.mu.Lock()
		r.installing = false
		if err != nil {
			r.installErr = err.Error()
			rec["error"] = r.installErr
		}
		r.mu.Unlock()
		r.record(rec)
	}()
}

// ---- handlers ----

func (s *Server) handleRevyl(w http.ResponseWriter, r *http.Request) {
	rv := s.revyl
	if rv == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("sync") == "1" {
			ctx, cancel := context.WithTimeout(r.Context(), revylCallTimeout)
			rv.Sync(ctx)
			cancel()
		}
		writeJSON(w, http.StatusOK, RevylListResponse{Devices: rv.List()})
	case http.MethodPost:
		var req StartRevylRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		key, err := rv.Start(req)
		switch {
		case errors.Is(err, errRevylBusy):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errRevylNoCLI), errors.Is(err, errRevylClosing):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			writeJSON(w, http.StatusAccepted, StartRevylResponse{Key: key})
		}
	case http.MethodDelete:
		key := r.URL.Query().Get("key")
		if key == "" {
			key = r.URL.Query().Get("target")
		}
		// Not cancelled with the request: by the time the CLI runs, the row
		// and the target are gone, so a client that hangs up mid-stop
		// would leave the device billing with nothing left to stop it.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), revylCallTimeout)
		defer cancel()
		err := rv.Stop(ctx, key)
		switch {
		case errors.Is(err, errRevylNoDevice):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, errRevylStarting):
			http.Error(w, err.Error(), http.StatusConflict)
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadGateway)
		default:
			writeJSON(w, http.StatusOK, RevylListResponse{Devices: rv.List()})
		}
	}
}

func (s *Server) handleRevylAccount(w http.ResponseWriter, r *http.Request) {
	rv := s.revyl
	if rv == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), revylCallTimeout)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, rv.account(ctx))
	case http.MethodPost:
		acct := rv.account(ctx)
		if !acct.CLIInstalled {
			http.Error(w, errRevylNoCLI.Error(), http.StatusServiceUnavailable)
			return
		}
		if acct.SignedIn {
			writeJSON(w, http.StatusOK, acct)
			return
		}
		if err := rv.signIn(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusAccepted, rv.account(ctx))
	case http.MethodDelete:
		cli, ok := rv.cli()
		if !ok {
			http.Error(w, errRevylNoCLI.Error(), http.StatusServiceUnavailable)
			return
		}
		rv.cancelLogin()
		// Stop runs as the signed-in account.
		if rv.holding(true) {
			http.Error(w, errRevylHolding.Error(), http.StatusConflict)
			return
		}
		if _, err := cli.Run(ctx, nil, "auth", "logout"); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, rv.account(ctx))
	}
}

func (s *Server) handleRevylCLI(w http.ResponseWriter, r *http.Request) {
	rv := s.revyl
	if rv == nil {
		http.NotFound(w, r)
		return
	}
	if rv.installer == nil {
		http.Error(w, "this server cannot install the revyl CLI", http.StatusNotImplemented)
		return
	}
	if _, ok := rv.cli(); !ok {
		rv.installCLI()
	}
	ctx, cancel := context.WithTimeout(r.Context(), revylCallTimeout)
	defer cancel()
	writeJSON(w, http.StatusAccepted, rv.account(ctx))
}

func (s *Server) handleRevylTargets(w http.ResponseWriter, r *http.Request) {
	rv := s.revyl
	if rv == nil {
		http.NotFound(w, r)
		return
	}
	cli, ok := rv.cli()
	if !ok {
		http.Error(w, errRevylNoCLI.Error(), http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), revylCallTimeout)
	defer cancel()
	t, err := cli.Targets(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp := RevylTargetsResponse{Platforms: map[string][]RevylModel{}}
	for p, models := range t {
		if !validRevylPlatform[p] {
			continue
		}
		list := make([]RevylModel, 0, len(models))
		for _, m := range models {
			list = append(list, RevylModel{Model: m.Model, Runtime: m.Runtime})
		}
		resp.Platforms[p] = list
	}
	writeJSON(w, http.StatusOK, resp)
}
