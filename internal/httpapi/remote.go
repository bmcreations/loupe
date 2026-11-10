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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Remote devices are Firebase Device Streaming reservations, made through
// Google's android CLI (`android device remote ...`). A reservation is billed
// by the minute once a project's free minutes are used, so loupe:
//
//   - reserves only when a client asks, in the project that client names;
//   - never extends on its own;
//   - releases what it reserved when it exits, and at the next start for a
//     reservation a crash left behind.
//
// A connected reservation is an ordinary adb device at localhost:<port>, so
// streaming and input need nothing from this file. It only marks that row.

const (
	remoteReserving = "reserving"
	remoteActive    = "active"
	remoteEnded     = "ended"

	remoteCreateTimeout = 3 * time.Minute
	remoteCallTimeout   = 45 * time.Second
	remoteSignInTimeout = 5 * time.Minute
	// remoteMaxExtend is the most one extend call may add. A larger number
	// is far more likely a client bug than a wish.
	remoteMaxExtend = 30
)

var (
	validProject  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,29}$`)
	validCodename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,63}$`)
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
	State  string `json:"state"`
	// Reason says how an ended reservation ended: released, expired, failed.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
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

// RemoteModel is one reservable device and the API levels it comes with.
type RemoteModel struct {
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Codename     string `json:"codename"`
	APIs         []int  `json:"apis"`
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
	// their ids.
	creating sync.WaitGroup
	// signingIn is true while `android auth login` waits on the browser.
	signingIn bool
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
	var orphans, unnamed []*RemoteReservation
	for _, it := range r.items {
		if it.State == remoteEnded {
			continue
		}
		if it.Reservation == "" {
			cp := *it
			unnamed = append(unnamed, &cp)
		}
		// Usage counts up to the expiry at most: nobody knows when the old
		// process died, and the reservation could not outlive its expiry.
		end := r.now()
		if !it.ExpiresAt.IsZero() && it.ExpiresAt.Before(end) {
			end = it.ExpiresAt
		}
		it.State, it.Reason, it.EndedAt = remoteEnded, "released", end
		if it.Reservation != "" {
			cp := *it
			orphans = append(orphans, &cp)
		}
	}
	r.saveLocked()
	r.mu.Unlock()
	go func() {
		for _, it := range orphans {
			r.releaseAtGoogle(it)
			r.record(map[string]any{"action": "reconcile", "reservation": it.Reservation, "project": it.Project})
		}
		// The old process quit mid-reserve. What the project holds now and
		// did not hold before is what that reserve made.
		for _, it := range unnamed {
			ctx, cancel := context.WithTimeout(context.Background(), remoteCallTimeout)
			now := r.held(ctx, it.Project)
			cancel()
			for _, id := range now {
				if slices.Contains(it.Before, id) {
					continue
				}
				cp := *it
				cp.Reservation = id
				r.releaseAtGoogle(&cp)
				r.record(map[string]any{"action": "reconcile", "reservation": id, "project": it.Project, "midReserve": true})
			}
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
	if !validProject.MatchString(project) {
		return "", errors.New("project is not a Google Cloud project id")
	}
	if !validCodename.MatchString(codename) || api <= 0 {
		return "", errors.New("device must be a codename and an API level")
	}
	if !r.Installed() {
		return "", errNoCLI
	}
	r.mu.Lock()
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
	r.mu.Unlock()
	r.creating.Add(1)
	go r.create(it.Key, project, codename, api)
	return it.Key, nil
}

var errBusy = errors.New("another reservation is being made")

func (r *RemoteDevices) create(key, project, codename string, api int) {
	defer r.creating.Done()
	ctx, cancel := context.WithTimeout(context.Background(), remoteCreateTimeout)
	defer cancel()
	before := r.held(ctx, project)
	r.mu.Lock()
	if it := r.findLocked(key); it != nil {
		it.Before = before
		r.saveLocked()
	}
	r.mu.Unlock()
	out, stderr, runErr := r.exec(ctx, "device", "remote", "create", "--connect", "--project="+project, codename+"/"+strconv.Itoa(api))
	res := parseRemoteCreate(string(out) + "\n" + string(stderr))

	r.mu.Lock()
	it := r.findLocked(key)
	now := r.now()
	it.Reservation = res.id
	failure := ""
	switch {
	case res.id != "" && res.port > 0:
		it.State, it.ActiveAt = remoteActive, now
		it.Target = "localhost:" + strconv.Itoa(res.port)
		it.ExpiresAt = resolveClock(res.expires, now)
	default:
		failure = remoteFailure(string(out), string(stderr), runErr)
		it.State, it.Reason, it.Error, it.EndedAt = remoteEnded, "failed", failure, now
		it.APIDisabled = isAPIDisabled(string(out) + string(stderr))
	}
	cp := *it
	r.saveLocked()
	r.mu.Unlock()

	if failure != "" && cp.Reservation != "" {
		// Reserved but never connected: it would bill until it expired.
		r.releaseAtGoogle(&cp)
	}
	rec := map[string]any{"action": "reserve", "project": project, "device": codename, "api": api, "reservation": cp.Reservation, "tookMs": now.Sub(cp.StartedAt).Milliseconds()}
	if failure != "" {
		rec["error"] = failure
	}
	r.record(rec)
}

// held is the ids of the project's reservations right now, never nil.
func (r *RemoteDevices) held(ctx context.Context, project string) []string {
	out, _, _ := r.exec(ctx, "device", "remote", "list", "--project="+project)
	ids := []string{}
	for _, row := range parseRemoteList(string(out)) {
		ids = append(ids, row.id)
	}
	return ids
}

func (r *RemoteDevices) findLocked(key string) *RemoteReservation {
	for _, it := range r.items {
		if it.Key == key {
			return it
		}
	}
	return nil
}

// releaseAtGoogle disconnects, then removes. The connect daemon is a process
// of its own and outlives loupe, so removing alone would leave it running.
func (r *RemoteDevices) releaseAtGoogle(it *RemoteReservation) error {
	ctx, cancel := context.WithTimeout(context.Background(), remoteCallTimeout)
	defer cancel()
	_, _, _ = r.exec(ctx, "device", "remote", "disconnect", "--project="+it.Project, it.Reservation)
	out, stderr, err := r.exec(ctx, "device", "remote", "remove", "--project="+it.Project, it.Reservation)
	if err != nil {
		return errors.New(remoteFailure(string(out), string(stderr), err))
	}
	return nil
}

// Release ends a reservation now. The row is forgotten whether or not Google
// answered, and the error says when it did not.
func (r *RemoteDevices) Release(key string) error { return r.end(key, "released") }

func (r *RemoteDevices) end(key, reason string) error {
	r.mu.Lock()
	it := r.findLocked(key)
	if it == nil {
		r.mu.Unlock()
		return errNoReservation
	}
	if it.State != remoteActive {
		r.mu.Unlock()
		if it.State == remoteReserving {
			return errors.New("still reserving; release it once it connects")
		}
		return nil
	}
	it.State, it.Reason, it.EndedAt = remoteEnded, reason, r.now()
	cp := *it
	r.saveLocked()
	r.mu.Unlock()

	if r.endTarget != nil && cp.Target != "" {
		r.endTarget(cp.Target, reason)
	}
	err := r.releaseAtGoogle(&cp)
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
		return errors.New(remoteFailure(string(out), string(stderr), err))
	}
	// The new expiry is read back, not computed, so the timer shows what
	// Google holds. If the read fails the old expiry plus the minutes stands.
	expires := cp.ExpiresAt.Add(time.Duration(minutes) * time.Minute)
	if out, _, err := r.exec(ctx, "device", "remote", "list", "--project="+cp.Project, cp.Reservation); err == nil {
		for _, row := range parseRemoteList(string(out)) {
			if row.id == cp.Reservation {
				if t := resolveClock(row.expires, r.now()); !t.IsZero() {
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
		_ = r.end(key, "expired")
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

// ReleaseAll releases every reservation this loupe holds. Called at exit.
func (r *RemoteDevices) ReleaseAll() {
	// A reserve in flight has no id to release yet. Wait for it: quitting
	// now would leave the device billing until it expired.
	done := make(chan struct{})
	go func() { r.creating.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(remoteCreateTimeout):
	}
	r.mu.Lock()
	var keys []string
	for _, it := range r.items {
		if it.State == remoteActive {
			keys = append(keys, it.Key)
		}
	}
	r.mu.Unlock()
	for _, key := range keys {
		_ = r.end(key, "released")
	}
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

// ---- reading the CLI ----

type remoteCreated struct {
	id      string
	port    int
	expires string
}

var (
	reCreatedID = regexp.MustCompile(`New reservation created with id (\S+?)\.`)
	reEndsAt    = regexp.MustCompile(`Reservation ends at (\d{1,2}:\d{2}[\s\x{a0}\x{202f}]*[AP]M)`)
	rePort      = regexp.MustCompile(`Device connected on port (\d+)`)
	reClock     = regexp.MustCompile(`^(\d{1,2}):(\d{2})[\s\x{a0}\x{202f}]*([AP]M)$`)
	// Two or more plain spaces: the CLI writes "10:56\u202fAM" with a narrow
	// no-break space, which must stay inside its column.
	reColumns = regexp.MustCompile(` {2,}`)
	reAccount = regexp.MustCompile(`(?m)^Logged in as (.+)$`)
)

func parseRemoteCreate(out string) remoteCreated {
	var c remoteCreated
	if m := reCreatedID.FindStringSubmatch(out); m != nil {
		c.id = m[1]
	}
	if m := reEndsAt.FindStringSubmatch(out); m != nil {
		c.expires = m[1]
	}
	if m := rePort.FindStringSubmatch(out); m != nil {
		c.port, _ = strconv.Atoi(m[1])
	}
	return c
}

// resolveClock turns the CLI's "10:56 AM" into a time: that clock time today,
// or tomorrow when today's is more than a minute past. Zero when unreadable.
func resolveClock(clock string, now time.Time) time.Time {
	m := reClock.FindStringSubmatch(strings.TrimSpace(clock))
	if m == nil {
		return time.Time{}
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	h %= 12
	if m[3] == "PM" {
		h += 12
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
	if t.Before(now.Add(-time.Minute)) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

type remoteListRow struct {
	id, state, expires, manufacturer, model, codename string
	api                                               int
}

func parseRemoteList(out string) []remoteListRow {
	var rows []remoteListRow
	for _, line := range strings.Split(out, "\n") {
		f := reColumns.Split(strings.TrimSpace(line), -1)
		if len(f) < 7 || f[0] == "Reservation" {
			continue
		}
		api, _ := strconv.Atoi(f[6])
		rows = append(rows, remoteListRow{f[0], f[1], f[2], f[3], f[4], f[5], api})
	}
	return rows
}

func parseRemoteModels(out string) []RemoteModel {
	var models []RemoteModel
	maker := ""
	for _, line := range strings.Split(out, "\n") {
		body := strings.TrimLeft(line, "│├└─ \t")
		if body == "" {
			continue
		}
		indented := strings.HasPrefix(line, "│") || strings.HasPrefix(line, " ")
		f := reColumns.Split(strings.TrimSpace(body), -1)
		if !indented {
			// "Google [37]"
			if i := strings.LastIndex(body, " ["); i > 0 {
				maker = body[:i]
			}
			continue
		}
		if len(f) < 2 || maker == "" {
			continue
		}
		m := RemoteModel{Manufacturer: maker, Model: f[0]}
		for _, pair := range f[1:] {
			i := strings.LastIndex(pair, "/")
			if i <= 0 {
				continue
			}
			api, err := strconv.Atoi(pair[i+1:])
			if err != nil {
				continue
			}
			m.Codename = pair[:i]
			m.APIs = append(m.APIs, api)
		}
		if m.Codename != "" {
			sort.Sort(sort.Reverse(sort.IntSlice(m.APIs)))
			models = append(models, m)
		}
	}
	return models
}

func parseRemoteProjects(out string) []string {
	projects := []string{}
	listing := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Projects available") {
			listing = true
			continue
		}
		if !listing {
			continue
		}
		if f := strings.Fields(line); len(f) > 0 && validProject.MatchString(f[0]) {
			projects = append(projects, f[0])
		}
	}
	return projects
}

func isAPIDisabled(out string) bool {
	return strings.Contains(out, "SERVICE_DISABLED") || strings.Contains(out, "has not been used in project") || strings.Contains(out, "it is disabled")
}

// remoteFailure picks the line worth showing from a failed CLI call.
func remoteFailure(out, stderr string, runErr error) string {
	for _, text := range []string{stderr, out} {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		for _, l := range lines {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "Error:") {
				return strings.TrimSpace(strings.TrimPrefix(l, "Error:"))
			}
		}
		if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
			return last
		}
	}
	if runErr != nil {
		return runErr.Error()
	}
	return "the CLI finished without connecting a device"
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
	Models []RemoteModel `json:"models"`
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
	CLIInstalled bool     `json:"cliInstalled"`
	SignedIn     bool     `json:"signedIn"`
	Account      string   `json:"account,omitempty"`
	SigningIn    bool     `json:"signingIn,omitempty"`
	Projects     []string `json:"projects"`
}

func (r *RemoteDevices) account(ctx context.Context) RemoteAccountResponse {
	r.mu.Lock()
	acct := RemoteAccountResponse{SigningIn: r.signingIn, Projects: []string{}}
	r.mu.Unlock()
	if acct.CLIInstalled = r.Installed(); !acct.CLIInstalled {
		return acct
	}
	out, _, _ := r.exec(ctx, "auth", "status")
	m := reAccount.FindStringSubmatch(string(out))
	if m == nil {
		return acct
	}
	acct.SignedIn, acct.Account = true, strings.TrimSpace(m[1])
	if out, _, err := r.exec(ctx, "device", "remote", "projects"); err == nil {
		acct.Projects = parseRemoteProjects(string(out))
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
			rec["error"] = remoteFailure("", string(stderr), err)
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
		if errors.Is(err, errNoCLI) {
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
		if _, stderr, err := rd.exec(ctx, "auth", "logout"); err != nil {
			http.Error(w, remoteFailure("", string(stderr), err), http.StatusBadGateway)
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
	if !validProject.MatchString(project) {
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
	models := parseRemoteModels(string(out))
	if err != nil || len(models) == 0 {
		writeJSON(w, http.StatusBadGateway, RemoteModelsError{
			Error:       remoteFailure(string(out), string(stderr), err),
			APIDisabled: isAPIDisabled(string(out) + string(stderr)),
		})
		return
	}
	writeJSON(w, http.StatusOK, RemoteModelsResponse{Models: models})
}
