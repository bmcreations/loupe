package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// ErrCodeAVDExists is the 409 of POST /api/avds and POST /api/devices when a
// device of that name exists. The CLI names an AVD after its profile and has
// no flag to choose another name, so a profile can be created once; on
// /api/devices it means a device with the requested name exists on that
// platform, an AVD or a simulator.
const ErrCodeAVDExists = "avdExists"

// ErrCodeCreating is the 409 of POST /api/avds and POST /api/devices while
// another creation runs, from either. Two CLI creates would share one system
// image download, and one slot keeps the target list's single pending row
// honest.
const ErrCodeCreating = "creating"

// Target states for the row a creation puts in /api/targets.
const (
	stateCreating     = "creating"
	stateCreateFailed = "createFailed"
)

// createTimeout bounds one `android emulator create`. The first one here took
// about four and a half minutes, most of it a 2.3 GB system image.
const createTimeout = 30 * time.Minute

// AVDProfile is one entry of GET /api/avds/profiles.
type AVDProfile struct {
	// Name is what the CLI takes, and what the AVD it creates is called.
	Name  string `json:"name"`
	Label string `json:"label"`
	// Exists is true when an AVD of that name is installed, which the CLI
	// refuses to create again.
	Exists bool `json:"exists"`
}

// AVDProfilesResponse is GET /api/avds/profiles.
type AVDProfilesResponse struct {
	Profiles []AVDProfile `json:"profiles"`
	// Creating names the profile being created, when one is.
	Creating string `json:"creating,omitempty"`
}

// CreateAVDRequest is POST /api/avds.
type CreateAVDRequest struct {
	Profile string `json:"profile"`
}

// CreateAVDResponse is POST /api/avds's 202. The creation goes on after it;
// its progress is the target row with this id.
type CreateAVDResponse struct {
	ID string `json:"id"`
}

// validProfile is the CLI's own naming: the six profiles it lists are lower
// case with underscores. Anything else never reaches exec.
var validProfile = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// AVDCreator runs `android emulator create <profile>` in the background and
// reports it as a target row until it ends. Only one runs at a time.
type AVDCreator struct {
	cli     string
	sdk     string
	avdHome string
	// registered is called with every installed AVD after a creation ends,
	// and when that scan began, so the new one becomes a target without a
	// restart. It must skip names already registered, and names Unregister
	// forgot after the scan began (Manager.RegisterScanned).
	registered func(scannedAt time.Time, installed []target.AVD)
	trace      func(map[string]any)

	// run executes the CLI; tests replace it.
	run func(ctx context.Context, args ...string) (stdout, stderr []byte, err error)

	slot *createSlot

	mu       sync.Mutex
	profiles []string
}

// NewAVDCreator returns a creator for the CLI at cli, or nil when cli is ""
// and there is nothing to run.
func NewAVDCreator(cli, sdk, avdHome string, registered func(time.Time, []target.AVD), trace func(map[string]any)) *AVDCreator {
	if cli == "" {
		return nil
	}
	return &AVDCreator{cli: cli, sdk: sdk, avdHome: avdHome, registered: registered, trace: trace, slot: &createSlot{}}
}

// SetAVDCreator enables /api/avds. Nil leaves both routes answering 404. The
// creator shares its one-at-a-time slot with the DeviceCreator, whichever is
// set first.
func (s *Server) SetAVDCreator(c *AVDCreator) {
	s.avds = c
	if c != nil {
		c.slot = s.sharedSlot(c.slot)
	}
}

func runCLI(ctx context.Context, cli string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, cli, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

func (c *AVDCreator) exec(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if c.run != nil {
		return c.run(ctx, args...)
	}
	return runCLI(ctx, c.cli, args...)
}

// listProfiles asks the CLI once per process: the list ships with the CLI,
// and asking takes about 0.4s.
func (c *AVDCreator) listProfiles(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	cached := c.profiles
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, stderr, err := c.exec(ctx, "emulator", "create", "--list-profiles")
	if err != nil {
		return nil, fmt.Errorf("android emulator create --list-profiles: %v: %s", err, strings.TrimSpace(string(stderr)))
	}
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if n := strings.TrimSpace(sc.Text()); validProfile.MatchString(n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("android emulator create --list-profiles listed nothing")
	}
	c.mu.Lock()
	c.profiles = names
	c.mu.Unlock()
	return names, nil
}

func (c *AVDCreator) avdExists(name string) bool {
	_, err := os.Stat(filepath.Join(c.avdHome, name+".avd", "config.ini"))
	return err == nil
}

// profileLabel is how the CLI's displayname reads: small_phone is the AVD
// "Small Phone".
func profileLabel(name string) string {
	words := strings.Split(name, "_")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

var errCreating = errors.New("a device is already being created")

// start begins a creation, or says why it cannot.
func (c *AVDCreator) start(profile string) error {
	job := &createJob{
		id: "avd:" + profile, label: profileLabel(profile), platform: "android", name: profile,
		started:  time.Now(),
		progress: func(elapsed time.Duration) string { return creatingReason(elapsed, c.avdExists(profile)) },
	}
	if err := c.slot.begin(job); err != nil {
		return err
	}
	go c.create(job)
	return nil
}

// create runs the CLI and settles the job. The verdict is the AVD's
// config.ini appearing with no "Error:" line, never the exit status: the CLI
// exits 0 after "Error: AVD small_phone already exists".
func (c *AVDCreator) create(job *createJob) {
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()
	// --sdk is one of the CLI's own options, so it goes before the command.
	var args []string
	if c.sdk != "" {
		args = append(args, "--sdk="+c.sdk)
	}
	args = append(args, "emulator", "create", job.name)
	out, stderr, runErr := c.exec(ctx, args...)
	var failure string
	for _, line := range strings.Split(string(out)+"\n"+string(stderr), "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "Error:") {
			failure = strings.TrimSpace(strings.TrimPrefix(l, "Error:"))
			break
		}
	}
	if failure == "" && !c.avdExists(job.name) {
		failure = strings.TrimSpace(string(stderr))
		if failure == "" && runErr != nil {
			failure = runErr.Error()
		}
		if failure == "" {
			failure = "the CLI finished without creating " + job.name
		}
	}
	if failure == "" {
		scannedAt := time.Now()
		installed, err := target.ScanAVDs(c.avdHome)
		if err != nil {
			failure = "listing AVDs: " + err.Error()
		} else if c.registered != nil {
			c.registered(scannedAt, installed)
		}
	}
	took := time.Since(job.started)
	createdID := ""
	if failure == "" {
		createdID = job.id
	}
	c.slot.finish(job, failure, createdID)

	rec := map[string]any{"kind": "avd", "action": "create", "profile": job.name, "tookMs": took.Milliseconds()}
	if failure != "" {
		rec["error"] = failure
	}
	if c.trace != nil {
		c.trace(rec)
	}
}

// downloadAfter is how long a create runs without its .avd before the row
// calls it a download. The directory appears only once the system image is
// in place, so a long wait without it is the download; a short one may be a
// create whose image is already here, which has not been timed.
const downloadAfter = 15 * time.Second

// creatingReason says which part of a create is running. The CLI prints one
// URL line for the whole download, so this reads the filesystem and the clock.
func creatingReason(elapsed time.Duration, avdExists bool) string {
	switch {
	case avdExists:
		return "Creating the device"
	case elapsed < downloadAfter:
		return fmt.Sprintf("Preparing · %s", roundElapsed(elapsed))
	default:
		return fmt.Sprintf("Getting the system image · %s", roundElapsed(elapsed))
	}
}

func roundElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func (s *Server) handleAVDProfiles(w http.ResponseWriter, r *http.Request) {
	if s.avds == nil {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "creating devices needs the android CLI, which this server did not find"})
		return
	}
	names, err := s.avds.listProfiles(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	resp := AVDProfilesResponse{Profiles: make([]AVDProfile, 0, len(names))}
	for _, n := range names {
		resp.Profiles = append(resp.Profiles, AVDProfile{Name: n, Label: profileLabel(n), Exists: s.avds.avdExists(n)})
	}
	if row, ok := s.avds.slot.row(); ok && row.State == stateCreating && strings.HasPrefix(row.ID, "avd:") {
		resp.Creating = strings.TrimPrefix(row.ID, "avd:")
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCreateAVD(w http.ResponseWriter, r *http.Request) {
	if s.avds == nil {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "creating devices needs the android CLI, which this server did not find"})
		return
	}
	var req CreateAVDRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "body must be {\"profile\": ...}"})
		return
	}
	names, err := s.avds.listProfiles(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	known := false
	for _, n := range names {
		known = known || n == req.Profile
	}
	if !known {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("unknown profile %q; GET /api/avds/profiles lists them", req.Profile)})
		return
	}
	if s.avds.avdExists(req.Profile) {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: "an AVD named " + req.Profile + " already exists", Code: ErrCodeAVDExists})
		return
	}
	if err := s.avds.start(req.Profile); err != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: ErrCodeCreating})
		return
	}
	s.trace.Record(map[string]any{"kind": "avd", "action": "start", "profile": req.Profile})
	writeJSON(w, http.StatusAccepted, CreateAVDResponse{ID: "avd:" + req.Profile})
}
