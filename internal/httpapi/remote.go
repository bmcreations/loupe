package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target/firebase"
)

// Remote devices are Firebase Device Streaming reservations, made through
// Google's android CLI (`android device remote ...`). A reservation is billed
// by the minute once a project's free minutes are used, so loupe:
//
//   - reserves only when a client asks, in the project that client names;
//   - never extends on its own;
//   - releases what it reserved when it exits, and at the next start for a
//     reservation a crash left behind;
//   - marks a reservation ended only once Google confirms the release. Until
//     then it stays releasing in the state file, and the next DELETE, start
//     or exit tries again.
//
// A connected reservation is an ordinary adb device at localhost:<port>, so
// streaming and input need nothing from this file. It only marks that row.

const (
	remoteReserving = "reserving"
	remoteActive    = "active"
	// remoteReleasing is a release Google has not confirmed. The device may
	// still bill, so the row is kept and retried.
	remoteReleasing = "releasing"
	remoteEnded     = "ended"

	remoteCreateTimeout = 3 * time.Minute
	remoteCallTimeout   = 45 * time.Second
	remoteSignInTimeout = 5 * time.Minute
	// remoteMaxExtend is the most one extend call may add. A larger number
	// is far more likely a client bug than a wish.
	remoteMaxExtend = 30
)

// RemoteInfo marks a target row as a remote reservation.
type RemoteInfo struct {
	// Key is loupe's own name for the reservation, known before Google
	// issues an id. It is the same on the reserving row and the attached one.
	Key         string `json:"key"`
	Reservation string `json:"reservation,omitempty"`
	Project     string `json:"project"`
	// ExpiresAt is RFC 3339. The CLI prints a wall-clock time with no date,
	// so this is that time on the day it next occurs.
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// RemoteReservation is one reservation, as /api/remote lists it and as the
// state file keeps it.
type RemoteReservation struct {
	Key         string `json:"key"`
	Reservation string `json:"reservation,omitempty"`
	Project     string `json:"project"`
	Codename    string `json:"codename"`
	API         int    `json:"api"`
	Model       string `json:"model,omitempty"`
	// Target is the id of the attached row, localhost:<port>.
	Target string `json:"target,omitempty"`
	// State is reserving, active, releasing or ended.
	State string `json:"state"`
	// Reason says how a releasing or ended reservation ended: released,
	// expired, failed.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	// ReleaseError is why Google has not confirmed a releasing row's release.
	ReleaseError string `json:"releaseError,omitempty"`
	// APIDisabled is true when the failure is the project not having the
	// Device Streaming or Cloud Testing API turned on.
	APIDisabled bool      `json:"apiDisabled,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	// The three below are the zero time (year 1) until they happen; read
	// State before trusting them.
	ActiveAt  time.Time `json:"activeAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	EndedAt   time.Time `json:"endedAt"`
	// Before is the reservation ids the project held when this reserve
	// began. A reserve has no id of its own until the CLI returns, so a
	// start after a quit in that window releases whatever is not in here.
	Before []string `json:"before,omitempty"`
}

// RemoteDevices makes and ends reservations through the android CLI.
type RemoteDevices struct {
	// find locates the android CLI on each call, so one installed while
	// loupe runs is picked up without a restart. "" means not installed.
	find  func() string
	trace func(map[string]any)
	// run executes the CLI; tests replace it. Nothing here may reach runCLI
	// in a test: the CLI on a developer's Mac is signed in and would bill.
	run func(ctx context.Context, args ...string) (stdout, stderr []byte, err error)
	now func() time.Time
	// endTarget ends the attached row's session with reason and forgets the
	// row, so a released device does not linger as "missing".
	endTarget func(id, reason string)

	mu    sync.Mutex
	path  string
	items []*RemoteReservation
	// creating counts reserves still inside the CLI, so exit can wait for
	// their ids. closing is set by ReleaseAll, after which none begins.
	creating sync.WaitGroup
	closing  bool
	// releasing holds the keys whose release is in the CLI right now, so a
	// DELETE, the start's retry and exit do not run the same one twice.
	releasing map[string]bool
	// signingIn is true while `android auth login` waits on the browser.
	signingIn bool
	// tools installs the CLI on request, sharing one install at a time with
	// /api/android/cli; nil means it cannot.
	tools *AndroidTools
}

// NewRemoteDevices takes the CLI lookup, not a path: the CLI may be installed
// after loupe starts.
func NewRemoteDevices(find func() string, endTarget func(id, reason string), trace func(map[string]any)) *RemoteDevices {
	return &RemoteDevices{find: find, endTarget: endTarget, trace: trace, now: time.Now}
}

// errNoCLI is what every CLI call fails with while the android CLI is not
// installed. Handlers answer it 503 so a client can tell it from the 404 of
// a server that predates /api/remote.
var errNoCLI = errors.New("the android CLI is not installed")

// Installed reports whether there is a CLI to run.
func (r *RemoteDevices) Installed() bool {
	return r.run != nil || (r.find != nil && r.find() != "")
}

// SetAndroidTools lets POST /api/remote/cli install the CLI. Without it the
// route answers 501. Pass the Server's own, so the two routes share one
// install.
func (r *RemoteDevices) SetAndroidTools(t *AndroidTools) { r.tools = t }

// SetRemoteDevices enables /api/remote. Nil leaves its routes answering 404.
func (s *Server) SetRemoteDevices(r *RemoteDevices) { s.remote = r }

func (r *RemoteDevices) exec(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if r.run != nil {
		return r.run(ctx, args...)
	}
	cli := ""
	if r.find != nil {
		cli = r.find()
	}
	if cli == "" {
		return nil, nil, errNoCLI
	}
	return runCLI(ctx, cli, args...)
}

func (r *RemoteDevices) record(rec map[string]any) {
	if r.trace != nil {
		rec["kind"] = "remote"
		r.trace(rec)
	}
}

// Load reads the state file and releases every reservation it lists as still
// held: those belong to a loupe that did not get to release them. It returns
// once they are marked; the CLI calls run in the background.
func (r *RemoteDevices) Load(path string) error {
	r.mu.Lock()
	r.path = path
	raw, err := os.ReadFile(path)
	if err != nil {
		r.mu.Unlock()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var items []*RemoteReservation
	if err := json.Unmarshal(raw, &items); err != nil {
		r.mu.Unlock()
		return fmt.Errorf("%s: %w", path, err)
	}
	r.items = items
	var retry []*RemoteReservation
	for _, it := range r.items {
		switch it.State {
		case remoteEnded:
			continue
		case remoteReleasing:
			// A release an earlier run could not confirm.
		default:
			// Usage counts up to the expiry at most: nobody knows when the
			// old process died, and the reservation could not outlive its
			// expiry.
			end := r.now()
			if !it.ExpiresAt.IsZero() && it.ExpiresAt.Before(end) {
				end = it.ExpiresAt
			}
			it.State, it.Reason, it.EndedAt = remoteReleasing, "released", end
		}
		cp := *it
		retry = append(retry, &cp)
	}
	r.saveLocked()
	r.mu.Unlock()
	go func() {
		for _, it := range retry {
			rec := map[string]any{"action": "reconcile", "reservation": it.Reservation, "project": it.Project}
			if err := r.release(context.Background(), it.Key); err != nil {
				rec["error"] = err.Error()
			}
			r.record(rec)
		}
	}()
	return nil
}

func (r *RemoteDevices) saveLocked() {
	if r.path == "" {
		return
	}
	if err := writeJSONFile(r.path, r.items); err != nil {
		r.record(map[string]any{"action": "save", "error": err.Error()})
	}
}

func newRemoteKey() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Reserve starts a reservation and returns its key. One runs at a time: the
// CLI's connect step picks the adb port, and two at once have not been tried.
func (r *RemoteDevices) Reserve(project, codename string, api int, model string) (string, error) {
	if !firebase.ValidProject(project) {
		return "", errors.New("project is not a Google Cloud project id")
	}
	if !firebase.ValidCodename(codename) || api <= 0 {
		return "", errors.New("device must be a codename and an API level")
	}
	if !r.Installed() {
		return "", errNoCLI
	}
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return "", errClosing
	}
	for _, it := range r.items {
		if it.State == remoteReserving {
			r.mu.Unlock()
			return "", errBusy
		}
	}
	it := &RemoteReservation{
		Key: newRemoteKey(), Project: project, Codename: codename, API: api, Model: model,
		State: remoteReserving, StartedAt: r.now(),
	}
	r.items = append(r.items, it)
	r.saveLocked()
	// Under the lock, so ReleaseAll's wait sees every reserve it lets begin.
	r.creating.Add(1)
	r.mu.Unlock()
	go r.create(it.Key, project, codename, api)
	return it.Key, nil
}

var (
	errBusy    = errors.New("another reservation is being made")
	errClosing = errors.New("loupe is shutting down")
)

var errHolding = errors.New("release the reserved device before signing out")

// holding reports whether a device is reserved or being reserved.
func (r *RemoteDevices) holding() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, it := range r.items {
		if it.State == remoteReserving || it.State == remoteActive || it.State == remoteReleasing {
			return true
		}
	}
	return false
}

func (r *RemoteDevices) create(key, project, codename string, api int) {
	defer r.creating.Done()
	ctx, cancel := context.WithTimeout(context.Background(), remoteCreateTimeout)
	defer cancel()
	before, _ := r.held(ctx, project)
	r.mu.Lock()
	if it := r.findLocked(key); it != nil {
		it.Before = before
		r.saveLocked()
	}
	r.mu.Unlock()
	out, stderr, runErr := r.exec(ctx, "device", "remote", "create", "--connect", "--project="+project, codename+"/"+strconv.Itoa(api))
	res := firebase.ParseCreate(string(out) + "\n" + string(stderr))

	r.mu.Lock()
	it := r.findLocked(key)
	now := r.now()
	it.Reservation = res.ID
	failure := ""
	switch {
	case res.ID != "" && res.Port > 0:
		it.State, it.ActiveAt = remoteActive, now
		it.Target = "localhost:" + strconv.Itoa(res.Port)
		it.ExpiresAt = firebase.ResolveClock(res.Expires, now)
	default:
		failure = firebase.Failure(string(out), string(stderr), runErr)
		it.State, it.Reason, it.Error, it.EndedAt = remoteEnded, "failed", failure, now
		it.APIDisabled = firebase.APIDisabled(string(out) + string(stderr))
		if it.Reservation != "" {
			// Reserved but never connected: it bills until it expires
			// unless the release goes through.
			it.State = remoteReleasing
			if res.Expires != "" {
				it.ExpiresAt = firebase.ResolveClock(res.Expires, now)
			}
		}
	}
	cp := *it
	r.saveLocked()
	r.mu.Unlock()

	if cp.State == remoteReleasing {
		// Not ctx: a create that timed out has already spent it.
		_ = r.release(context.Background(), key)
	}
	rec := map[string]any{"action": "reserve", "project": project, "device": codename, "api": api, "reservation": cp.Reservation, "tookMs": now.Sub(cp.StartedAt).Milliseconds()}
	if failure != "" {
		rec["error"] = failure
	}
	r.record(rec)
}

// held is the ids of the project's reservations right now, never nil, and
// the error from listProject.
func (r *RemoteDevices) held(ctx context.Context, project string) ([]string, error) {
	out, err := r.listProject(ctx, project)
	ids := []string{}
	for _, row := range firebase.ParseList(out) {
		ids = append(ids, row.ID)
	}
	return ids, err
}

// listProject is what `device remote list` printed for project. The error is
// set unless the CLI printed a list it recognises, so an empty answer is never
// taken for an empty project.
func (r *RemoteDevices) listProject(ctx context.Context, project string) (string, error) {
	out, stderr, err := r.exec(ctx, "device", "remote", "list", "--project="+project)
	if err != nil {
		return "", errors.New(firebase.Failure(string(out), string(stderr), err))
	}
	if !firebase.IsList(string(out)) {
		return "", errors.New("the CLI printed no reservation list")
	}
	return string(out), nil
}

func (r *RemoteDevices) findLocked(key string) *RemoteReservation {
	for _, it := range r.items {
		if it.Key == key {
			return it
		}
	}
	return nil
}

// releaseAtGoogle disconnects, then removes, and is nil once Google no longer
// holds the reservation. The connect daemon is a process of its own and
// outlives loupe, so removing alone would leave it running.
func (r *RemoteDevices) releaseAtGoogle(ctx context.Context, it *RemoteReservation) error {
	ctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	defer cancel()
	_, _, _ = r.exec(ctx, "device", "remote", "disconnect", "--project="+it.Project, it.Reservation)
	out, stderr, err := r.exec(ctx, "device", "remote", "remove", "--project="+it.Project, it.Reservation)
	if err == nil {
		return nil
	}
	failure := firebase.Failure(string(out), string(stderr), err)
	// "Could not find reservation" is the CLI's message for gRPC NOT_FOUND.
	// "It has already ended" is its message for an ended reservation, seen
	// in the connect path; what remove says for one is not on record.
	if text := string(out) + string(stderr); strings.Contains(text, "Could not find reservation") || strings.Contains(text, "It has already ended") {
		return nil
	}
	if list, lerr := r.listProject(ctx, it.Project); lerr == nil && !firebase.ListsID(list, it.Reservation) {
		return nil
	}
	// It cannot outlive its expiry. This also ends the retries for one
	// that can no longer be read, say after a sign-out outside loupe.
	if !it.ExpiresAt.IsZero() && !r.now().Before(it.ExpiresAt) {
		return nil
	}
	return errors.New(failure)
}

// releaseMidReserve releases what a reserve made before its process quit,
// with no id recorded: what the project holds now and did not hold when that
// reserve began, less the ids loupe's other rows own.
func (r *RemoteDevices) releaseMidReserve(ctx context.Context, it *RemoteReservation) error {
	listCtx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	ids, err := r.held(listCtx, it.Project)
	cancel()
	if err != nil {
		return err
	}
	r.mu.Lock()
	var known []string
	for _, other := range r.items {
		// A reserve in flight has no id on its row yet, so the device it is
		// making would look like this one's.
		if other.State == remoteReserving {
			r.mu.Unlock()
			return errBusy
		}
		if other.Reservation != "" {
			known = append(known, other.Reservation)
		}
	}
	r.mu.Unlock()
	var first error
	for _, id := range ids {
		if slices.Contains(it.Before, id) || slices.Contains(known, id) {
			continue
		}
		cp := *it
		cp.Reservation = id
		err := r.releaseAtGoogle(ctx, &cp)
		rec := map[string]any{"action": "reconcile", "reservation": id, "project": it.Project, "midReserve": true}
		if err != nil {
			rec["error"] = err.Error()
			if first == nil {
				first = err
			}
		}
		r.record(rec)
	}
	return first
}

// release asks Google to end a releasing row and marks it ended once Google
// has. Until then the row stays releasing, saved, with the reason.
func (r *RemoteDevices) release(ctx context.Context, key string) error {
	r.mu.Lock()
	it := r.findLocked(key)
	if it == nil || it.State != remoteReleasing {
		r.mu.Unlock()
		return nil
	}
	if r.releasing[key] {
		r.mu.Unlock()
		return errReleaseRunning
	}
	if r.releasing == nil {
		r.releasing = map[string]bool{}
	}
	r.releasing[key] = true
	cp := *it
	r.mu.Unlock()

	var err error
	if cp.Reservation == "" {
		err = r.releaseMidReserve(ctx, &cp)
	} else {
		err = r.releaseAtGoogle(ctx, &cp)
	}

	r.mu.Lock()
	delete(r.releasing, key)
	if it := r.findLocked(key); it != nil && it.State == remoteReleasing {
		if err == nil {
			it.State, it.ReleaseError = remoteEnded, ""
		} else {
			it.ReleaseError = err.Error()
		}
		r.saveLocked()
	}
	r.mu.Unlock()
	return err
}

var errReleaseRunning = errors.New("a release of this reservation is already running")

// Release ends a reservation now, or tries again to release one Google has
// not confirmed. The row stays releasing until Google confirms, and the error
// says why it has not.
func (r *RemoteDevices) Release(key string) error {
	return r.end(context.Background(), key, "released")
}

func (r *RemoteDevices) end(ctx context.Context, key, reason string) error {
	r.mu.Lock()
	it := r.findLocked(key)
	if it == nil {
		r.mu.Unlock()
		return errNoReservation
	}
	wasActive := it.State == remoteActive
	switch it.State {
	case remoteReserving:
		r.mu.Unlock()
		return errors.New("still reserving; release it once it connects")
	case remoteActive:
		it.State, it.Reason, it.EndedAt = remoteReleasing, reason, r.now()
		r.saveLocked()
	case remoteReleasing:
	default:
		r.mu.Unlock()
		return nil
	}
	cp := *it
	r.mu.Unlock()

	if wasActive && r.endTarget != nil && cp.Target != "" {
		r.endTarget(cp.Target, reason)
	}
	err := r.release(ctx, key)
	rec := map[string]any{"action": reason, "reservation": cp.Reservation, "project": cp.Project, "heldMs": cp.EndedAt.Sub(cp.ActiveAt).Milliseconds()}
	if err != nil {
		rec["error"] = err.Error()
	}
	r.record(rec)
	return err
}

var errNoReservation = errors.New("no such reservation")

// Extend adds minutes to a reservation, and only ever when a client asks.
func (r *RemoteDevices) Extend(key string, minutes int) error {
	if minutes <= 0 || minutes > remoteMaxExtend {
		return fmt.Errorf("minutes must be 1 to %d", remoteMaxExtend)
	}
	r.mu.Lock()
	it := r.findLocked(key)
	if it == nil || it.State != remoteActive {
		r.mu.Unlock()
		return errNoReservation
	}
	cp := *it
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), remoteCallTimeout)
	defer cancel()
	out, stderr, err := r.exec(ctx, "device", "remote", "extend", "--project="+cp.Project, "--duration="+strconv.Itoa(minutes), cp.Reservation)
	if err != nil {
		return errors.New(firebase.Failure(string(out), string(stderr), err))
	}
	// The new expiry is read back, not computed, so the timer shows what
	// Google holds. If the read fails the old expiry plus the minutes stands.
	expires := cp.ExpiresAt.Add(time.Duration(minutes) * time.Minute)
	if out, _, err := r.exec(ctx, "device", "remote", "list", "--project="+cp.Project, cp.Reservation); err == nil {
		for _, row := range firebase.ParseList(string(out)) {
			if row.ID == cp.Reservation {
				if t := firebase.ResolveClock(row.Expires, r.now()); !t.IsZero() {
					expires = t
				}
			}
		}
	}
	r.mu.Lock()
	if it := r.findLocked(key); it != nil && it.State == remoteActive {
		it.ExpiresAt = expires
		r.saveLocked()
	}
	r.mu.Unlock()
	r.record(map[string]any{"action": "extend", "reservation": cp.Reservation, "project": cp.Project, "minutes": minutes})
	return nil
}

// ExpireDue ends every reservation whose time has run out. Google has ended
// it already; this forgets the row and stops the connect daemon.
func (r *RemoteDevices) ExpireDue() {
	r.mu.Lock()
	now := r.now()
	var due []string
	for _, it := range r.items {
		if it.State == remoteActive && !it.ExpiresAt.IsZero() && !now.Before(it.ExpiresAt) {
			due = append(due, it.Key)
		}
	}
	r.mu.Unlock()
	for _, key := range due {
		_ = r.end(context.Background(), key, "expired")
	}
}

// Watch runs ExpireDue until ctx ends.
func (r *RemoteDevices) Watch(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ExpireDue()
		}
	}
}

// ReleaseAll releases every reservation this loupe holds, all at once.
// Called at exit; no reserve begins after. It returns when they are
// released or ctx ends.
func (r *RemoteDevices) ReleaseAll(ctx context.Context) {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	// A reserve in flight has no id to release yet. Wait for it: quitting
	// now would leave the device billing until it expired. If ctx ends
	// first, the next start's Load releases what it made.
	done := make(chan struct{})
	go func() { r.creating.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	// Held devices, and releases Google has not confirmed yet.
	r.mu.Lock()
	var keys []string
	for _, it := range r.items {
		if it.State == remoteActive || it.State == remoteReleasing {
			keys = append(keys, it.Key)
		}
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.end(ctx, key, "released")
		}()
	}
	wg.Wait()
}

// List returns the reservations still held, and those that ended in the last
// ten minutes so a client can say how one ended.
func (r *RemoteDevices) List() []RemoteReservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := []RemoteReservation{}
	for _, it := range r.items {
		if it.State == remoteEnded && now.Sub(it.EndedAt) > 10*time.Minute {
			continue
		}
		out = append(out, *it)
	}
	return out
}

// UsageMinutes is the minutes loupe has held devices this calendar month, by
// project, rounded up per reservation. It is this Mac's count only: Google
// documents no way to read what a project has left.
func (r *RemoteDevices) UsageMinutes() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	out := map[string]int{}
	for _, it := range r.items {
		if it.ActiveAt.IsZero() || it.ActiveAt.Before(first) {
			continue
		}
		end := it.EndedAt
		if it.State == remoteActive {
			end = now
		}
		if d := end.Sub(it.ActiveAt); d > 0 {
			out[it.Project] += int((d + time.Minute - 1) / time.Minute)
		}
	}
	return out
}

// decorate marks the attached row of each held reservation and adds a row
// for one still being made.
func (r *RemoteDevices) decorate(rows []TargetInfo) []TargetInfo {
	if r == nil {
		return rows
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, it := range r.items {
		switch it.State {
		case remoteActive:
			for i := range rows {
				if rows[i].ID == it.Target {
					rows[i].Remote = &RemoteInfo{Key: it.Key, Reservation: it.Reservation, Project: it.Project}
					// adb has no model for a tunnelled device, so its row
					// would read localhost:<port>.
					if it.Model != "" {
						rows[i].Label = it.Model
					}
					if !it.ExpiresAt.IsZero() {
						rows[i].Remote.ExpiresAt = it.ExpiresAt.Format(time.RFC3339)
					}
				}
			}
		case remoteReserving:
			label := it.Model
			if label == "" {
				label = it.Codename
			}
			row := TargetInfo{ID: "remote:" + it.Key, Kind: "device", Label: label, State: remoteReserving, Reason: "Reserving…",
				Remote: &RemoteInfo{Key: it.Key, Project: it.Project}}
			row.OS, row.APILevel = "android", it.API
			rows = append(rows, row)
		}
	}
	return rows
}

// ---- HTTP ----

// RemoteListResponse answers GET /api/remote and POST /api/remote/extend.
type RemoteListResponse struct {
	Reservations []RemoteReservation `json:"reservations"`
	// UsageMinutes is minutes held this calendar month by project, counted
	// by this server only.
	UsageMinutes map[string]int `json:"usageMinutes"`
}

// ReserveRemoteRequest is the body of POST /api/remote.
type ReserveRemoteRequest struct {
	Project  string `json:"project"`
	Codename string `json:"codename"`
	API      int    `json:"api"`
	// Model labels the row while it reserves, e.g. "Pixel 9".
	Model string `json:"model,omitempty"`
}

// ReserveRemoteResponse names the reservation and its reserving row.
type ReserveRemoteResponse struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

// ExtendRemoteRequest is the body of POST /api/remote/extend.
type ExtendRemoteRequest struct {
	Key     string `json:"key"`
	Minutes int    `json:"minutes"`
}

// RemoteModelsResponse answers GET /api/remote/models.
type RemoteModelsResponse struct {
	Models []firebase.Model `json:"models"`
}

// RemoteModelsError is the 502 body of GET /api/remote/models.
type RemoteModelsError struct {
	Error       string `json:"error"`
	APIDisabled bool   `json:"apiDisabled,omitempty"`
}

// RemoteAccountResponse is the Google account the CLI is signed in as and
// the projects it may reserve in.
type RemoteAccountResponse struct {
	// CLIInstalled is false while the android CLI is missing on this host;
	// nothing else here is meaningful then.
	CLIInstalled bool   `json:"cliInstalled"`
	SignedIn     bool   `json:"signedIn"`
	Account      string `json:"account,omitempty"`
	SigningIn    bool   `json:"signingIn,omitempty"`
	// CanInstallCLI is true when POST /api/remote/cli can install it.
	CanInstallCLI bool `json:"canInstallCLI,omitempty"`
	// CLIInstalling is true while that install runs; CLIInstallError is why
	// the last one failed.
	CLIInstalling   bool     `json:"cliInstalling,omitempty"`
	CLIInstallError string   `json:"cliInstallError,omitempty"`
	Projects        []string `json:"projects"`
}

func (r *RemoteDevices) account(ctx context.Context) RemoteAccountResponse {
	r.mu.Lock()
	acct := RemoteAccountResponse{SigningIn: r.signingIn, Projects: []string{}, CanInstallCLI: r.tools.canInstallCLI()}
	r.mu.Unlock()
	acct.CLIInstalling, acct.CLIInstallError = r.tools.cliState()
	if acct.CLIInstalled = r.Installed(); !acct.CLIInstalled {
		return acct
	}
	out, _, _ := r.exec(ctx, "auth", "status")
	account := firebase.ParseAccount(string(out))
	if account == "" {
		return acct
	}
	acct.SignedIn, acct.Account = true, account
	if out, _, err := r.exec(ctx, "device", "remote", "projects"); err == nil {
		acct.Projects = firebase.ParseProjects(string(out))
	}
	return acct
}

// signIn runs `android auth login`, which opens the browser and waits.
func (r *RemoteDevices) signIn() error {
	r.mu.Lock()
	if r.signingIn {
		r.mu.Unlock()
		return nil
	}
	r.signingIn = true
	r.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), remoteSignInTimeout)
		defer cancel()
		_, stderr, err := r.exec(ctx, "auth", "login")
		r.mu.Lock()
		r.signingIn = false
		r.mu.Unlock()
		rec := map[string]any{"action": "signin"}
		if err != nil {
			rec["error"] = firebase.Failure("", string(stderr), err)
		}
		r.record(rec)
	}()
	return nil
}

func (s *Server) handleRemote(w http.ResponseWriter, r *http.Request) {
	rd := s.remote
	if rd == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, RemoteListResponse{Reservations: rd.List(), UsageMinutes: rd.UsageMinutes()})
	case http.MethodPost:
		var req ReserveRemoteRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		key, err := rd.Reserve(req.Project, req.Codename, req.API, req.Model)
		if errors.Is(err, errBusy) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, errNoCLI) || errors.Is(err, errClosing) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusAccepted, ReserveRemoteResponse{Key: key, ID: "remote:" + key})
	case http.MethodDelete:
		err := rd.Release(r.URL.Query().Get("key"))
		if errors.Is(err, errNoReservation) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if errors.Is(err, errReleaseRunning) || errors.Is(err, errBusy) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, RemoteListResponse{Reservations: rd.List(), UsageMinutes: rd.UsageMinutes()})
	}
}

func (s *Server) handleRemoteExtend(w http.ResponseWriter, r *http.Request) {
	rd := s.remote
	if rd == nil {
		http.NotFound(w, r)
		return
	}
	var req ExtendRemoteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	err := rd.Extend(req.Key, req.Minutes)
	switch {
	case errors.Is(err, errNoReservation):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil && strings.HasPrefix(err.Error(), "minutes must"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		writeJSON(w, http.StatusOK, RemoteListResponse{Reservations: rd.List(), UsageMinutes: rd.UsageMinutes()})
	}
}

func (s *Server) handleRemoteAccount(w http.ResponseWriter, r *http.Request) {
	rd := s.remote
	if rd == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteCallTimeout)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, rd.account(ctx))
	case http.MethodPost:
		if !rd.Installed() {
			http.Error(w, errNoCLI.Error(), http.StatusServiceUnavailable)
			return
		}
		_ = rd.signIn()
		writeJSON(w, http.StatusAccepted, RemoteAccountResponse{SigningIn: true, Projects: []string{}})
	case http.MethodDelete:
		if !rd.Installed() {
			http.Error(w, errNoCLI.Error(), http.StatusServiceUnavailable)
			return
		}
		// Release runs as the signed-in account, so signing out under a
		// held device would leave it running until it expires.
		if rd.holding() {
			http.Error(w, errHolding.Error(), http.StatusConflict)
			return
		}
		if _, stderr, err := rd.exec(ctx, "auth", "logout"); err != nil {
			http.Error(w, firebase.Failure("", string(stderr), err), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, RemoteAccountResponse{Projects: []string{}})
	}
}

func (s *Server) handleRemoteModels(w http.ResponseWriter, r *http.Request) {
	rd := s.remote
	if rd == nil {
		http.NotFound(w, r)
		return
	}
	project := r.URL.Query().Get("project")
	if !firebase.ValidProject(project) {
		http.Error(w, "project is not a Google Cloud project id", http.StatusBadRequest)
		return
	}
	if !rd.Installed() {
		http.Error(w, errNoCLI.Error(), http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteCallTimeout)
	defer cancel()
	out, stderr, err := rd.exec(ctx, "device", "remote", "models", "--project="+project)
	models := firebase.ParseModels(string(out))
	if err != nil || len(models) == 0 {
		writeJSON(w, http.StatusBadGateway, RemoteModelsError{
			Error:       firebase.Failure(string(out), string(stderr), err),
			APIDisabled: firebase.APIDisabled(string(out) + string(stderr)),
		})
		return
	}
	writeJSON(w, http.StatusOK, RemoteModelsResponse{Models: models})
}
