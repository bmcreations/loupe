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

	"github.com/bmcreations/loupe/internal/target/android"
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

// AVDCreatorConfig says where an AVDCreator finds the CLI and its AVDs.
// FindCLI, SDK and CLIAVDHome run on every request, so a CLI or SDK installed
// while loupe runs needs no restart.
type AVDCreatorConfig struct {
	// FindCLI locates the android CLI. "" means it is not installed, and
	// /api/avds answers 404.
	FindCLI func() string
	// SDK is the SDK root passed as --sdk. Nil or "" lets the CLI choose,
	// which is ~/Library/Android/sdk on a Mac with none.
	SDK func() string
	// AVDHome is where loupe reads AVDs. CLIAVDHome, when set, is where the
	// CLI writes them; it ignores ANDROID_AVD_HOME, so when the two differ a
	// created AVD would never appear, and /api/avds answers 404 saying so.
	AVDHome    string
	CLIAVDHome func() string
	// Registered is called with every installed AVD after a creation ends,
	// and when that scan began, so the new one becomes a target without a
	// restart. It must skip names already registered, and names Unregister
	// forgot after the scan began (Manager.RegisterScanned).
	Registered func(scannedAt time.Time, installed []android.AVD)
	Trace      func(map[string]any)
}

// AVDCreator runs `android emulator create <profile>` in the background and
// reports it as a target row until it ends. Only one runs at a time.
type AVDCreator struct {
	cfg AVDCreatorConfig

	// run, when set, executes the CLI in place of the one FindCLI found;
	// tests set it.
	run func(ctx context.Context, args ...string) (stdout, stderr []byte, err error)

	slot *createSlot

	mu sync.Mutex
	// profiles is the last successful listing, and profilesFrom the CLI
	// that gave it. A failed listing is never kept.
	profiles     []string
	profilesFrom string
}

// NewAVDCreator returns a creator, or nil when cfg.FindCLI is nil and there
// is nothing to look up.
func NewAVDCreator(cfg AVDCreatorConfig) *AVDCreator {
	if cfg.FindCLI == nil {
		return nil
	}
	return &AVDCreator{cfg: cfg, slot: &createSlot{}}
}

// errNoAndroidCLI is the 404 of /api/avds while no CLI is found.
const errNoAndroidCLI = "creating devices needs the android CLI, which this server did not find"

// offered is the CLI to run, or "" and why /api/avds answers 404. It is
// asked on every request.
func (c *AVDCreator) offered() (cli, why string) {
	if c == nil {
		return "", errNoAndroidCLI
	}
	cli = c.cfg.FindCLI()
	if cli == "" {
		return "", errNoAndroidCLI
	}
	if c.cfg.CLIAVDHome != nil {
		if home := c.cfg.CLIAVDHome(); filepath.Clean(home) != filepath.Clean(c.cfg.AVDHome) {
			return "", AVDHomeMismatch(c.cfg.AVDHome, home)
		}
	}
	return cli, ""
}

// AVDHomeMismatch says why the CLI's profiles are not offered when AVDs are
// read from avdHome and the CLI creates them in cliHome.
func AVDHomeMismatch(avdHome, cliHome string) string {
	return fmt.Sprintf("not offering the android CLI's New device profiles: AVDs are read from %s and the android CLI creates them in %s", avdHome, cliHome)
}

// SetAVDCreator enables /api/avds. Nil leaves both routes answering 404, as
// does a creator whose CLI is not found. The creator shares its one-at-a-time
// slot with the DeviceCreator, whichever is set first.
func (s *Server) SetAVDCreator(c *AVDCreator) {
	s.avds = c
	if c != nil {
		c.slot = s.sharedSlot(c.slot)
	}
}

// available reports whether /api/avds is offered right now.
func (c *AVDCreator) available() bool {
	cli, _ := c.offered()
	return cli != ""
}

func (c *AVDCreator) exec(ctx context.Context, cli string, args ...string) ([]byte, []byte, error) {
	if c.run != nil {
		return c.run(ctx, args...)
	}
	return runCLI(ctx, cli, args...)
}

func runCLI(ctx context.Context, cli string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, cli, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// listProfiles asks the CLI once per process: the list ships with the CLI,
// and asking takes about 0.4s. Only a listing that named profiles is kept,
// and only for the CLI that gave it, so a failure, or a CLI installed or
// moved since, asks again.
func (c *AVDCreator) listProfiles(ctx context.Context, cli string) ([]string, error) {
	c.mu.Lock()
	cached, from := c.profiles, c.profilesFrom
	c.mu.Unlock()
	if cached != nil && from == cli {
		return cached, nil
	}
	// A CLI's first-ever run spends about 12s setting itself up before it
	// answers, so a 10s limit failed the first New Device sheet on a new Mac.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, stderr, err := c.exec(ctx, cli, "emulator", "create", "--list-profiles")
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
	c.profiles, c.profilesFrom = names, cli
	c.mu.Unlock()
	return names, nil
}

func (c *AVDCreator) avdExists(name string) bool {
	_, err := os.Stat(filepath.Join(c.cfg.AVDHome, name+".avd", "config.ini"))
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

// start begins a creation with the CLI at cli, or says why it cannot.
func (c *AVDCreator) start(cli, profile string) error {
	job := &createJob{
		id: "avd:" + profile, label: profileLabel(profile), platform: "android", name: profile,
		started:  time.Now(),
		progress: func(elapsed time.Duration) string { return creatingReason(elapsed, c.avdExists(profile)) },
	}
	if err := c.slot.begin(job); err != nil {
		return err
	}
	go c.create(job, cli)
	return nil
}

// create runs the CLI and settles the job. The verdict is the AVD's
// config.ini appearing with no "Error:" line, never the exit status: the CLI
// exits 0 after "Error: AVD small_phone already exists".
func (c *AVDCreator) create(job *createJob, cli string) {
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()
	args := append(sdkArgs(c.cfg.SDK), "emulator", "create", job.name)
	out, stderr, runErr := c.exec(ctx, cli, args...)
	failure := cliErrorLine(out, stderr)
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
		installed, err := android.ScanAVDs(c.cfg.AVDHome)
		if err != nil {
			failure = "listing AVDs: " + err.Error()
		} else if c.cfg.Registered != nil {
			c.cfg.Registered(scannedAt, installed)
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
	if c.cfg.Trace != nil {
		c.cfg.Trace(rec)
	}
}

// sdkArgs is --sdk=<root> when sdk names one. It is one of the CLI's own
// options, so it goes before the command.
func sdkArgs(sdk func() string) []string {
	if sdk == nil {
		return nil
	}
	if root := sdk(); root != "" {
		return []string{"--sdk=" + root}
	}
	return nil
}

// cliErrorLine is the text of the CLI's first "Error:" line, or "". The CLI
// can exit 0 after one, so this is read before the exit status.
func cliErrorLine(out, stderr []byte) string {
	for _, line := range strings.Split(string(out)+"\n"+string(stderr), "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "Error:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "Error:"))
		}
	}
	return ""
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
	cli, why := s.avds.offered()
	if cli == "" {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: why})
		return
	}
	names, err := s.avds.listProfiles(r.Context(), cli)
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
	cli, why := s.avds.offered()
	if cli == "" {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: why})
		return
	}
	var req CreateAVDRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "body must be {\"profile\": ...}"})
		return
	}
	names, err := s.avds.listProfiles(r.Context(), cli)
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
	if err := s.avds.start(cli, req.Profile); err != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: ErrCodeCreating})
		return
	}
	s.trace.Record(map[string]any{"kind": "avd", "action": "start", "profile": req.Profile})
	writeJSON(w, http.StatusAccepted, CreateAVDResponse{ID: "avd:" + req.Profile})
}
