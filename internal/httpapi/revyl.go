package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/target/revyl"
)

// Revyl cloud devices: a session started through the user's own revyl CLI
// and account becomes a revyl.Target. The CLI's shapes are read from
// revyl-cli v0.2.27; internal/target/revyl.go says which were checked
// against a live account.

const (
	revylStarting = "starting"
	revylActive   = "active"
	revylFailed   = "failed"
	revylStopping = "stopping"

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
	// revylBillingTTL is how long one `revyl billing status` answers GET
	// /api/revyl/billing.
	revylBillingTTL = 30 * time.Second
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
	errRevylStopping = errors.New("the device is already stopping")
	errRevylUnread   = errors.New("loupe has not read the account's Revyl devices while one starts; try again once it is up")
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
	// State is "starting", "active", "stopping" or "failed"; Error says
	// why for failed. StopError says why the last stop failed; the device
	// is still active and can be stopped again.
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	StopError string `json:"stopError,omitempty"`
	// Adopted is true for a session loupe found on the account rather than
	// started. loupe leaves it running when it exits.
	Adopted   bool      `json:"adopted,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	// IdleTimeoutSec is how long Revyl keeps the device without a command.
	IdleTimeoutSec int `json:"idleTimeoutSec,omitempty"`

	// activated is when the row became active, for Sync. stopDone closes
	// when the stop in progress ends.
	activated time.Time
	stopDone  chan struct{}
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

// RevylBillingResponse is GET /api/revyl/billing: the account's plan, the
// free minutes it has left this period, and what a minute costs once they
// run out. Each field is absent when the CLI does not report it. FreeMinutes
// may be absent on any plan; revyl-cli v0.2.27 reports it only on Free.
type RevylBillingResponse struct {
	// Plan is Revyl's plan ID, such as "free"; PlanName is its display name.
	Plan     string `json:"plan,omitempty"`
	PlanName string `json:"planName,omitempty"`
	// PeriodEnd is RFC 3339, when the free minutes reset.
	PeriodEnd    string            `json:"periodEnd,omitempty"`
	FreeMinutes  *RevylFreeMinutes `json:"freeMinutes,omitempty"`
	USDPerMinute *RevylRates       `json:"usdPerMinute,omitempty"`
	// BillingURL is where the account adds a card or changes plan.
	BillingURL string `json:"billingURL,omitempty"`
}

// RevylFreeMinutes is the free minutes per platform: an Android emulator
// and an iOS simulator each draw on their own.
type RevylFreeMinutes struct {
	Android *RevylMinutes `json:"android,omitempty"`
	IOS     *RevylMinutes `json:"ios,omitempty"`
}

// RevylMinutes is one platform's free minutes. Left is 0, not absent, once
// they are used up.
type RevylMinutes struct {
	Left  *int `json:"left,omitempty"`
	Total *int `json:"total,omitempty"`
}

// RevylRates is the price of a minute in US dollars, for an Android
// emulator and an iOS simulator.
type RevylRates struct {
	Android *float64 `json:"android,omitempty"`
	IOS     *float64 `json:"ios,omitempty"`
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
	newTarget func(cli revyl.CLI, s revyl.Session, model, os string) target.Target

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
	stopped map[string]time.Time
	// listed is set once a `revyl device list` has succeeded. Before that
	// loupe does not know the devices it left running before a restart.
	listed       bool
	login        *revylLogin
	lastLoginErr string

	installer  *RevylInstaller
	installing bool
	installErr string

	// rest sends targets' input over Revyl's device proxy; nil leaves it
	// to the CLI. Set before Watch, read by newTarget.
	rest *revyl.REST

	// billing is the last billing status read, at billingAt, which answers
	// for billingTTL (revylBillingTTL; tests shorten it). billingGen counts
	// what drops it, so a read that began before a stop is not kept.
	billing    *RevylBillingResponse
	billingAt  time.Time
	billingGen int
	billingTTL time.Duration
}

type revylLogin struct {
	url, code string
	cancel    context.CancelFunc
	shown     chan struct{} // closed once url and code are read, or the CLI exits
}

// NewRevylDevices looks the CLI up with find on every call, so one
// installed later needs no restart. dir is where the CLI runs
// (revyl.WorkDir).
func NewRevylDevices(find func() string, dir string, register func(target.Target), unregister func(id, reason string), trace func(map[string]any)) *RevylDevices {
	r := &RevylDevices{find: find, dir: dir, register: register, unregister: unregister, trace: trace,
		listGrace: revylListGrace, startGrace: revylStartGrace, billingTTL: revylBillingTTL,
		stopped: map[string]time.Time{}}
	r.newTarget = func(cli revyl.CLI, s revyl.Session, model, os string) target.Target {
		return &revyl.Target{CLI: cli, Session: s, Model: model, OSVersion: os, REST: r.rest, Trace: r.record}
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
func (r *RevylDevices) SetREST(rest *revyl.REST) { r.rest = rest }

func (r *RevylDevices) cli() (revyl.CLI, bool) {
	path := ""
	if r.find != nil {
		path = r.find()
	}
	if path != "" && r.dir != "" {
		// The CLI runs in this directory, and exec fails on one that is
		// missing.
		_ = os.MkdirAll(r.dir, 0o755)
	}
	return revyl.CLI{Path: path, Dir: r.dir}, path != ""
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

func (r *RevylDevices) start(ctx context.Context, cancel context.CancelFunc, done chan struct{}, cli revyl.CLI, key string, req StartRevylRequest) {
	defer close(done)
	defer cancel()
	began := time.Now()
	sess, err := cli.Start(ctx, revyl.Start{Platform: req.Platform, Model: req.Model, OSVersion: req.OSVersion, Grace: r.startGrace})
	rec := map[string]any{"action": "start", "platform": req.Platform, "model": req.Model, "tookMs": time.Since(began).Milliseconds()}
	r.mu.Lock()
	r.starting = false
	r.cancelStart, r.startDone = nil, nil
	if sess.SessionID != "" {
		// A device came up, kept or not, and draws on the minutes.
		r.dropBillingLocked()
	}
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
	} else {
		r.mu.Lock()
		r.dropBillingLocked()
		r.mu.Unlock()
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

// stopRowLocked finds a row by key or target ID.
func (r *RevylDevices) stopRowLocked(key string) *RevylDevice {
	var it *RevylDevice
	for _, c := range r.items {
		if c.Key == key || (c.Target != "" && c.Target == key) {
			it = c
		}
	}
	return it
}

// Stop ends a device by key or target ID. A failed row is forgotten. An
// active one shows as stopping until the CLI answers; the row and the
// target go once it confirms, and a stop that fails leaves both, active
// with StopError.
func (r *RevylDevices) Stop(ctx context.Context, key string) error {
	r.mu.Lock()
	it, listed := r.stopRowLocked(key), r.listed
	if it == nil && !listed {
		// A device left running before a restart is not known until the
		// account is read, and a 404 meanwhile tells the client it is gone
		// while it bills. Read it now; a failed read is not a 404.
		r.mu.Unlock()
		if err := r.sync(ctx); err != nil {
			return fmt.Errorf("could not read the account's Revyl sessions: %w", err)
		}
		r.mu.Lock()
		it, listed = r.stopRowLocked(key), r.listed
	}
	if it == nil {
		r.mu.Unlock()
		if !listed {
			return errRevylUnread
		}
		return errRevylNoDevice
	}
	switch it.State {
	case revylStarting:
		r.mu.Unlock()
		return errRevylStarting
	case revylStopping:
		r.mu.Unlock()
		return errRevylStopping
	case revylActive:
	default:
		// Failed: nothing runs at Revyl.
		r.removeLocked(it)
		r.mu.Unlock()
		return nil
	}
	// The row and the target stay until the CLI reports the stop, so one
	// that fails or is cut short leaves the device where a client can see
	// it and stop it again.
	it.State, it.StopError = revylStopping, ""
	done := make(chan struct{})
	it.stopDone = done
	sessionID, tid := it.SessionID, it.Target
	r.mu.Unlock()
	defer close(done)

	cli, _ := r.cli()
	err := cli.Stop(ctx, sessionID)
	rec := map[string]any{"action": "stop", "target": tid}
	r.mu.Lock()
	it.stopDone = nil
	if err != nil {
		it.State, it.StopError = revylActive, err.Error()
		rec["error"] = err.Error()
	} else {
		r.removeLocked(it)
		r.stopped[sessionID] = time.Now()
		r.dropBillingLocked()
	}
	r.mu.Unlock()
	if err == nil && r.unregister != nil {
		r.unregister(tid, "stopped")
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
func (r *RevylDevices) Sync(ctx context.Context) { _ = r.sync(ctx) }

// sync is Sync, returning why it read nothing.
func (r *RevylDevices) sync(ctx context.Context) error {
	cli, ok := r.cli()
	if !ok {
		return errRevylNoCLI
	}
	began := time.Now()
	sessions, err := cli.List(ctx)
	if err != nil {
		return err
	}
	live := map[string]revyl.Session{}
	for _, s := range sessions {
		if s.SessionID != "" && validRevylPlatform[s.Platform] {
			live[s.SessionID] = s
		}
	}
	var adopt []target.Target
	var ended []*RevylDevice
	r.mu.Lock()
	skipped := false
	for id, at := range r.stopped {
		if time.Since(at) >= r.listGrace {
			delete(r.stopped, id)
		}
	}
	known := map[string]bool{}
	for _, it := range append([]*RevylDevice(nil), r.items...) {
		if it.State == revylStopping {
			// Its stop removes it, or puts it back to active if it fails.
			known[it.SessionID] = true
			continue
		}
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
			skipped = true
			continue
		}
		it := &RevylDevice{Key: newRevylKey(), SessionID: id, Target: "revyl:" + id, Platform: s.Platform,
			State: revylActive, Adopted: true, StartedAt: s.StartedAt, IdleTimeoutSec: int(s.IdleTimeout / time.Second),
			activated: time.Now()}
		r.items = append(r.items, it)
		adopt = append(adopt, r.newTarget(cli, s, "", ""))
	}
	// A session left to a start may be one loupe has never seen.
	if !skipped {
		r.listed = true
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
	return nil
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

// holding reports whether any device is active or stopping, or with
// startedOnly, any loupe started itself.
func (r *RevylDevices) holding(startedOnly bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, it := range r.items {
		live := it.State == revylActive || it.State == revylStopping
		if (live && (!startedOnly || !it.Adopted)) || it.State == revylStarting {
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
	var stopping []chan struct{}
	for _, it := range r.items {
		switch {
		case it.State == revylActive && !it.Adopted:
			keys = append(keys, it.Key)
		case it.State == revylStopping:
			// A client's stop, on its own context: wait for it too.
			stopping = append(stopping, it.stopDone)
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
	for _, done := range append(stopping, startDone) {
		if done == nil {
			continue
		}
		select {
		case <-done:
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
	if err == nil {
		r.dropBillingLocked()
	}
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
		// Not cancelled with the request, so a client that hangs up
		// mid-stop does not cut the stop short and leave the device billing.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), revylCallTimeout)
		defer cancel()
		err := rv.Stop(ctx, key)
		switch {
		case errors.Is(err, errRevylNoDevice):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, errRevylStarting), errors.Is(err, errRevylStopping), errors.Is(err, errRevylUnread):
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
		rv.mu.Lock()
		rv.dropBillingLocked()
		rv.mu.Unlock()
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

var errRevylSignedOut = errors.New("the revyl CLI is not signed in")

// dropBillingLocked forgets the billing status read, and keeps a read in
// flight from storing its own.
func (r *RevylDevices) dropBillingLocked() {
	r.billing = nil
	r.billingGen++
}

// billingStatus reads `revyl billing status`, or answers with the last read
// for billingTTL unless fresh. A read replaces the last, and one that fails
// leaves none. It records nothing to the trace.
func (r *RevylDevices) billingStatus(ctx context.Context, fresh bool) (RevylBillingResponse, error) {
	r.mu.Lock()
	if !fresh && r.billing != nil && time.Since(r.billingAt) < r.billingTTL {
		b := *r.billing
		r.mu.Unlock()
		return b, nil
	}
	gen := r.billingGen
	r.mu.Unlock()
	cli, ok := r.cli()
	if !ok {
		return RevylBillingResponse{}, errRevylNoCLI
	}
	st, err := cli.BillingStatus(ctx)
	var b RevylBillingResponse
	if err == nil {
		b = revylBilling(st)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.billingGen != gen {
		return b, err
	}
	if err != nil {
		r.billing = nil
		return b, err
	}
	r.billing, r.billingAt = &b, time.Now()
	return b, nil
}

// revylBilling is the part of the CLI's billing status the route answers
// with: the free minutes and the rates of the device types loupe starts.
func revylBilling(st revyl.Billing) RevylBillingResponse {
	b := RevylBillingResponse{Plan: st.Plan, PlanName: st.PlanName, PeriodEnd: st.PeriodEnd, BillingURL: st.BillingURL}
	if f := st.FreeMinutes; f != nil {
		m := RevylFreeMinutes{Android: revylMinutes(f.AndroidLeft, f.AndroidTotal), IOS: revylMinutes(f.IOSLeft, f.IOSTotal)}
		if m.Android != nil || m.IOS != nil {
			b.FreeMinutes = &m
		}
	}
	var rates RevylRates
	for _, rt := range st.Rates {
		switch {
		case rt.Platform == "android" && rt.DeviceType == "Emulator" && rates.Android == nil:
			rates.Android = rt.USDPerMinute
		case rt.Platform == "ios" && rt.DeviceType == "Simulator" && rates.IOS == nil:
			rates.IOS = rt.USDPerMinute
		}
	}
	if rates.Android != nil || rates.IOS != nil {
		b.USDPerMinute = &rates
	}
	return b
}

func revylMinutes(left, total *int) *RevylMinutes {
	if left == nil && total == nil {
		return nil
	}
	return &RevylMinutes{Left: left, Total: total}
}

// handleRevylBilling is GET /api/revyl/billing. ?fresh=1 reads the CLI
// rather than the last 30 s's answer.
func (s *Server) handleRevylBilling(w http.ResponseWriter, r *http.Request) {
	rv := s.revyl
	if rv == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), revylCallTimeout)
	defer cancel()
	b, err := rv.billingStatus(ctx, r.URL.Query().Get("fresh") == "1")
	switch {
	case errors.Is(err, errRevylNoCLI):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error()})
	case errors.Is(err, revyl.ErrSignedOut):
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: errRevylSignedOut.Error()})
	case errors.Is(err, revyl.ErrUnsupported):
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: "unsupported"})
	case err != nil:
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusOK, b)
	}
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
