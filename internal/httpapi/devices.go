package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// androidNameRule and iosNameRule are the nameRule each platform reports. An
// AVD name becomes a directory and an `emulator -avd` argument, so it stays
// to the characters avdmanager takes without quoting, and starts with a letter
// or digit so `-n` and `emulator -avd` cannot read it as a flag. simctl takes any name,
// including an empty one, "/" and emoji, and allows duplicates, so loupe sets
// its own rule: it starts with a letter or digit and allows spaces and
// parentheses.
const (
	androidNameRule = `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`
	iosNameRule     = `^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,63}$`
)

var (
	androidNameRe = regexp.MustCompile(androidNameRule)
	iosNameRe     = regexp.MustCompile(iosNameRule)
)

// DeviceOS is one entry of a platform's os list: an installed system image or
// simulator runtime.
type DeviceOS struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// DeviceModel is one entry of a platform's devices list.
type DeviceModel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Group is phone, foldable or tablet.
	Group string `json:"group"`
	Size  string `json:"size,omitempty"`
}

// DevicePlatform is one platform GET /api/devices/options offers.
type DevicePlatform struct {
	ID      string        `json:"id"`
	Label   string        `json:"label"`
	OS      []DeviceOS    `json:"os"`
	Devices []DeviceModel `json:"devices"`
	// Pairs maps an os id to the ids of the devices it can be created with.
	Pairs    map[string][]string `json:"pairs"`
	NameRule string              `json:"nameRule"`
}

// DeviceJob is the creation in flight or the last one.
type DeviceJob struct {
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Name     string `json:"name"`
	// State is creating, createFailed or done.
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	CreatedID string `json:"createdId,omitempty"`
	// Warning is set on a "done" job whose AVD exists and works but lacks the
	// keys loupe patches into config.ini (keyboard, GPU, Play Store flag).
	Warning string `json:"warning,omitempty"`
}

// DeviceOptionsResponse is GET /api/devices/options.
type DeviceOptionsResponse struct {
	Platforms     []DevicePlatform    `json:"platforms"`
	ExistingNames map[string][]string `json:"existingNames"`
	Job           *DeviceJob          `json:"job,omitempty"`
	// ProfilesFallback is true when avdmanager is absent and the android CLI
	// is not: the client then offers /api/avds/profiles for Android.
	ProfilesFallback bool `json:"profilesFallback"`
}

// CreateDeviceRequest is POST /api/devices.
type CreateDeviceRequest struct {
	Platform string `json:"platform"`
	OS       string `json:"os"`
	Device   string `json:"device"`
	Name     string `json:"name"`
}

// CreateDeviceResponse is POST /api/devices's 202. The creation goes on after
// it; its progress is the target row with this id, and its end is the job in
// GET /api/devices/options.
type CreateDeviceResponse struct {
	ID string `json:"id"`
}

// DeviceCreatorConfig says what a DeviceCreator can make.
type DeviceCreatorConfig struct {
	Manager *Manager
	// SDK is the Android SDK root; AVDManager is the avdmanager binary, ""
	// when there is none, and AVDHome where AVDs are installed. Android is
	// offered when AVDManager and RegisterAVDs are both set.
	SDK        string
	AVDManager string
	AVDHome    string
	// RegisterAVDs is called with every installed AVD after a creation, and
	// when that scan began, so the new one becomes a target. It must skip
	// names already registered, and names Unregister forgot after the scan
	// began (Manager.RegisterScanned).
	RegisterAVDs func(scannedAt time.Time, installed []target.AVD)
	// Simcap is the built simcap helper. iOS is offered when it is set, which
	// the caller does only once simctl lists simulators and simcap runs: a new
	// simulator would not reach the picker otherwise.
	Simcap string
	Trace  func(map[string]any)
}

// DeviceCreator creates an AVD with avdmanager or a simulator with simctl,
// from what is installed, and reports it as a target row until it ends.
type DeviceCreator struct {
	cfg  DeviceCreatorConfig
	slot *createSlot

	// Seams; tests replace them.
	run      func(ctx context.Context, env []string, name string, args ...string) (stdout, stderr []byte, err error)
	images   func() []target.SystemImage
	devices  func() ([]target.AVDDevice, error)
	runtimes func(ctx context.Context) ([]target.SimRuntime, error)
	sizes    func(ctx context.Context, types []target.SimDeviceType) map[string]target.SimTypeInfo
	listSims func(ctx context.Context) ([]target.SimInfo, error)
}

// NewDeviceCreator returns a creator, or nil when neither platform can create.
func NewDeviceCreator(cfg DeviceCreatorConfig) *DeviceCreator {
	androidOK := cfg.AVDManager != "" && cfg.RegisterAVDs != nil
	if !androidOK && cfg.Simcap == "" {
		return nil
	}
	if !androidOK {
		cfg.AVDManager = ""
	}
	return &DeviceCreator{
		cfg:  cfg,
		slot: &createSlot{},
		run:  runTool,
		images: func() []target.SystemImage {
			return target.InstalledSystemImages(cfg.SDK)
		},
		devices:  func() ([]target.AVDDevice, error) { return target.AVDDevices(cfg.SDK) },
		runtimes: target.ListSimRuntimes,
		sizes:    target.ReadSimTypeInfos,
		listSims: target.ListSimulators,
	}
}

// SetDeviceCreator enables /api/devices. Nil leaves both routes answering 404
// (GET /api/devices/options still answers, if the android CLI is there, with
// profilesFallback). The creator shares its slot with the AVDCreator.
func (s *Server) SetDeviceCreator(c *DeviceCreator) {
	s.devs = c
	if c != nil {
		c.slot = s.sharedSlot(c.slot)
	}
}

// runTool runs a binary with stdin closed: avdmanager prompts for a custom
// hardware profile without -d and fails on a closed stdin otherwise, and
// exec's nil Stdin is the null device. env is added to the server's own.
func runTool(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

func (c *DeviceCreator) androidOffered() bool { return c.cfg.AVDManager != "" }
func (c *DeviceCreator) iosOffered() bool     { return c.cfg.Simcap != "" }

var groupOrder = map[string]int{target.GroupPhone: 0, target.GroupFoldable: 1, target.GroupTablet: 2}

// androidPlatform lists the installed images and the devices each takes.
//
// Pair rules, each of which avdmanager does not check itself (it made a TV
// and a Wear device on a phone image without complaint): a device with a tag
// (TV, Wear, automotive, desktop) is never offered; an image whose tag is
// android-* is never offered; a `google_apis_tablet` image takes tablets
// only; a device's api-level floor must not exceed the image's API (pixel_fold
// needs 34). Whether a device ships with the Play Store is not checked.
func (c *DeviceCreator) androidPlatform() DevicePlatform {
	p := DevicePlatform{ID: "android", Label: "Android", OS: []DeviceOS{}, Devices: []DeviceModel{}, Pairs: map[string][]string{}, NameRule: androidNameRule}
	devs, err := c.devices()
	if err != nil {
		devs = nil
		fmt.Fprintf(os.Stderr, "warning: device definitions: %v\n", err)
	}
	// The files list oldest first; the picker wants the newest first.
	devs = slices.Clone(devs)
	slices.Reverse(devs)
	slices.SortStableFunc(devs, func(a, b target.AVDDevice) int { return groupOrder[a.Group] - groupOrder[b.Group] })
	used := map[string]bool{}
	for _, img := range c.images() {
		p.OS = append(p.OS, DeviceOS{ID: img.ID, Label: img.Label(), Detail: img.Detail()})
		ids := []string{}
		for _, d := range devs {
			if d.MinAPI > img.APIFloor() || (strings.Contains(img.Family, "tablet") && d.Group != target.GroupTablet) {
				continue
			}
			ids = append(ids, d.ID)
			used[d.ID] = true
		}
		p.Pairs[img.ID] = ids
	}
	for _, d := range devs {
		if used[d.ID] {
			p.Devices = append(p.Devices, DeviceModel{ID: d.ID, Label: d.Name, Group: d.Group, Size: d.Size()})
		}
	}
	return p
}

func runtimeVersionKey(v string) []int {
	var k []int
	for _, part := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(part)
		k = append(k, n)
	}
	return k
}

// iosPlatform lists the installed runtimes and, for each, exactly the device
// types its own supportedDeviceTypes names.
func (c *DeviceCreator) iosPlatform(ctx context.Context) DevicePlatform {
	p := DevicePlatform{ID: "ios", Label: "iOS", OS: []DeviceOS{}, Devices: []DeviceModel{}, Pairs: map[string][]string{}, NameRule: iosNameRule}
	rts, err := c.runtimes(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		return p
	}
	slices.SortStableFunc(rts, func(a, b target.SimRuntime) int {
		return slices.Compare(runtimeVersionKey(b.Version), runtimeVersionKey(a.Version))
	})
	var all []target.SimDeviceType
	seen := map[string]bool{}
	for _, rt := range rts {
		for _, t := range rt.Types {
			if !seen[t.ID] {
				seen[t.ID] = true
				all = append(all, t)
			}
		}
	}
	infos := c.sizes(ctx, all)
	slices.SortStableFunc(all, func(a, b target.SimDeviceType) int {
		return groupOrder[infos[a.ID].Group] - groupOrder[infos[b.ID].Group]
	})
	for _, rt := range rts {
		p.OS = append(p.OS, DeviceOS{ID: rt.ID, Label: rt.Name})
		ids := []string{}
		for _, t := range rt.Types {
			ids = append(ids, t.ID)
		}
		p.Pairs[rt.ID] = ids
	}
	for _, t := range all {
		p.Devices = append(p.Devices, DeviceModel{ID: t.ID, Label: t.Name, Group: infos[t.ID].Group, Size: infos[t.ID].Size})
	}
	return p
}

func (c *DeviceCreator) existingAndroid() []string {
	names := map[string]bool{}
	if avds, err := target.ScanAVDs(c.cfg.AVDHome); err == nil {
		for _, a := range avds {
			names[a.Name] = true
		}
	}
	if c.cfg.Manager != nil {
		for _, n := range c.cfg.Manager.AVDNames() {
			names[n] = true
		}
	}
	// A half-made AVD has the .ini or the directory and no scan entry.
	for _, pat := range []string{"*.ini", "*.avd"} {
		matches, _ := filepath.Glob(filepath.Join(c.cfg.AVDHome, pat))
		for _, m := range matches {
			names[strings.TrimSuffix(strings.TrimSuffix(filepath.Base(m), ".ini"), ".avd")] = true
		}
	}
	return sortedKeys(names)
}

func (c *DeviceCreator) existingIOS() []string {
	names := map[string]bool{}
	if c.cfg.Manager != nil {
		for _, n := range c.cfg.Manager.SimulatorNames() {
			names[n] = true
		}
	}
	return sortedKeys(names)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (c *DeviceCreator) nameTaken(platform, name string) bool {
	switch platform {
	case "android":
		// avdmanager compares names with equalsIgnoreCase, so Pixel_10 and
		// pixel_10 are one AVD.
		return slices.ContainsFunc(c.existingAndroid(), func(n string) bool { return strings.EqualFold(n, name) })
	default:
		for _, n := range c.existingIOS() {
			if strings.EqualFold(n, name) {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleDeviceOptions(w http.ResponseWriter, r *http.Request) {
	fallback := s.avds != nil && (s.devs == nil || !s.devs.androidOffered())
	if s.devs == nil && !fallback {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "creating devices needs avdmanager, the android CLI, or simctl with simcap built, and this server found none"})
		return
	}
	resp := DeviceOptionsResponse{Platforms: []DevicePlatform{}, ExistingNames: map[string][]string{"android": {}, "ios": {}}, ProfilesFallback: fallback}
	if c := s.devs; c != nil {
		start := time.Now()
		if c.androidOffered() {
			resp.Platforms = append(resp.Platforms, c.androidPlatform())
			resp.ExistingNames["android"] = c.existingAndroid()
		}
		if c.iosOffered() {
			resp.Platforms = append(resp.Platforms, c.iosPlatform(r.Context()))
			resp.ExistingNames["ios"] = c.existingIOS()
		}
		s.trace.Record(map[string]any{"kind": "agent", "action": "device-options", "platforms": len(resp.Platforms), "tookMs": time.Since(start).Milliseconds()})
	}
	if job, ok := s.slot.last(); ok {
		j := &DeviceJob{ID: job.id, Platform: job.platform, Name: job.name, State: stateCreating}
		switch {
		case job.done && job.err != "":
			j.State, j.Error = stateCreateFailed, job.err
		case job.done:
			j.State, j.CreatedID, j.Warning = "done", job.createdID, job.warning
		}
		resp.Job = j
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	c := s.devs
	if c == nil {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "creating devices needs avdmanager, or simctl with simcap built, and this server found neither"})
		return
	}
	var req CreateDeviceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: `body must be {"platform", "os", "device", "name"}`})
		return
	}
	bad := func(format string, args ...any) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf(format, args...)})
	}
	var plat DevicePlatform
	switch {
	case req.Platform == "android" && c.androidOffered():
		plat = c.androidPlatform()
	case req.Platform == "ios" && c.iosOffered():
		plat = c.iosPlatform(r.Context())
	default:
		bad("unknown platform %q", req.Platform)
		return
	}
	pair, ok := plat.Pairs[req.OS]
	if !ok {
		bad("unknown os %q; GET /api/devices/options lists them", req.OS)
		return
	}
	if !slices.ContainsFunc(plat.Devices, func(d DeviceModel) bool { return d.ID == req.Device }) {
		bad("unknown device %q; GET /api/devices/options lists them", req.Device)
		return
	}
	if !slices.Contains(pair, req.Device) {
		bad("%s cannot be created on %s", req.Device, req.OS)
		return
	}
	rule := androidNameRe
	if req.Platform == "ios" {
		rule = iosNameRe
	}
	if !rule.MatchString(req.Name) {
		bad("name must match %s", rule)
		return
	}
	if c.nameTaken(req.Platform, req.Name) {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: "a device named " + req.Name + " already exists", Code: ErrCodeAVDExists})
		return
	}
	id, err := c.start(req)
	if err != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error(), Code: ErrCodeCreating})
		return
	}
	writeJSON(w, http.StatusAccepted, CreateDeviceResponse{ID: id})
}

// start claims the slot and creates in the background. The request was
// validated against the options.
func (c *DeviceCreator) start(req CreateDeviceRequest) (string, error) {
	job := &createJob{platform: req.Platform, name: req.Name, label: req.Name, started: time.Now()}
	if req.Platform == "ios" {
		job.id = "sim-new:" + req.Name
		job.progress = func(e time.Duration) string { return "Creating the simulator" }
	} else {
		job.id = "avd:" + req.Name
		job.progress = func(e time.Duration) string { return "Creating the device" }
	}
	if err := c.slot.begin(job); err != nil {
		return "", err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var createdID, failure string
		extra := map[string]any{}
		if req.Platform == "ios" {
			createdID, failure = c.createSimulator(ctx, req)
		} else {
			createdID, failure = c.createAVD(ctx, req, extra)
		}
		c.slot.finish(job, failure, createdID)
		if w, ok := extra["patchError"].(string); ok && failure == "" {
			c.slot.warn(job, "created, but its config.ini could not be updated: "+w)
		}
		rec := map[string]any{"kind": "agent", "action": "create", "platform": req.Platform, "name": req.Name,
			"tookMs": time.Since(job.started).Milliseconds(), "outcome": "ok"}
		for k, v := range extra {
			rec[k] = v
		}
		if failure != "" {
			rec["outcome"], rec["error"] = "failed", failure
		}
		if c.cfg.Trace != nil {
			c.cfg.Trace(rec)
		}
	}()
	return job.id, nil
}

// toolNoise are lines avdmanager prints that say nothing about the outcome.
var toolNoise = []string{"SDK XML versions up to", "Auto-selecting single ABI", "Loading local repository", "Fetch remote repository"}

// toolMessage reads a tool's failure out of its output: the "Error:" lines,
// else whatever is left once progress bars (written with \r), warnings and
// the stray "null" avdmanager ends on are removed.
func toolMessage(out, errOut []byte) string {
	var errors, rest []string
	text := strings.ReplaceAll(string(out)+"\n"+string(errOut), "\r", "\n")
lines:
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case l == "" || l == "null" || strings.HasPrefix(l, "["):
			continue
		}
		for _, n := range toolNoise {
			if strings.Contains(l, n) {
				continue lines
			}
		}
		if e, ok := strings.CutPrefix(l, "Error:"); ok {
			errors = append(errors, strings.TrimSpace(e))
		} else {
			rest = append(rest, l)
		}
	}
	if len(errors) > 0 {
		return strings.Join(errors, " ")
	}
	return strings.Join(rest, " ")
}

// avdManagerSDK is the SDK whose skins avdmanager will look in: the one the
// binary sits in (<sdk>/cmdline-tools/<version>/bin/avdmanager), which
// -android-avdmanager can point away from loupe's own SDK root. A path that is
// not in that layout says nothing, so the configured root stands.
func avdManagerSDK(avdmanager, fallback string) string {
	bin := filepath.Dir(avdmanager)
	ver := filepath.Dir(bin)
	tools := filepath.Dir(ver)
	if filepath.Base(bin) == "bin" && filepath.Base(tools) == "cmdline-tools" {
		return filepath.Dir(tools)
	}
	return fallback
}

// createAVD runs avdmanager. Its exit status is the verdict (0, or 1 for any
// error including a duplicate name), checked against the AVD's config.ini
// appearing. It returns the new target's id, or why not.
func (c *DeviceCreator) createAVD(ctx context.Context, req CreateDeviceRequest, extra map[string]any) (string, string) {
	args := []string{"create", "avd", "-n", req.Name, "-k", req.OS, "-d", req.Device}
	if target.SkinExists(avdManagerSDK(c.cfg.AVDManager, c.cfg.SDK), req.Device) {
		args = append(args, "--skin", req.Device)
	}
	// avdmanager reads ANDROID_AVD_HOME, unlike the android CLI, so the AVD
	// lands where loupe scans whatever the environment says.
	out, errOut, err := c.run(ctx, []string{"ANDROID_AVD_HOME=" + c.cfg.AVDHome}, c.cfg.AVDManager, args...)
	dir := filepath.Join(c.cfg.AVDHome, req.Name+".avd")
	_, statErr := os.Stat(filepath.Join(dir, "config.ini"))
	if err != nil || statErr != nil {
		msg := toolMessage(out, errOut)
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = "avdmanager finished without creating " + req.Name
		}
		return "", msg
	}
	// avdmanager's config.ini lacks what Android Studio writes.
	set := [][2]string{{"hw.keyboard", "yes"}, {"hw.gpu.enabled", "yes"}, {"avd.ini.displayname", req.Name}}
	for _, img := range c.images() {
		if img.ID == req.OS && strings.Contains(img.Family, "playstore") {
			set = append(set, [2]string{"PlayStore.enabled", "true"})
		}
	}
	if err := target.PatchAVDConfig(dir, set); err != nil {
		extra["patchError"] = err.Error()
	}
	scannedAt := time.Now()
	installed, err := target.ScanAVDs(c.cfg.AVDHome)
	if err != nil {
		return "", "listing AVDs: " + err.Error()
	}
	c.cfg.RegisterAVDs(scannedAt, installed)
	return "avd:" + req.Name, ""
}

// createSimulator runs `simctl create`. On success stdout is the UDID. A
// failure writes its main message to stdout ("Unable to create a device for
// device type: ...", exit 147 for an incompatible pair) and the cause to
// stderr, so both are kept. The simulator is then listed and handed to the
// manager at once, since the 5s poll is not triggered from here.
func (c *DeviceCreator) createSimulator(ctx context.Context, req CreateDeviceRequest) (string, string) {
	out, errOut, err := c.run(ctx, nil, "xcrun", "simctl", "create", req.Name, req.Device, req.OS)
	udid := strings.TrimSpace(string(out))
	if err != nil || udid == "" {
		primary := firstLine(string(out))
		secondary := lastLine(string(errOut))
		msg := primary
		switch {
		case msg == "":
			msg = secondary
		case secondary != "" && !strings.Contains(primary, secondary):
			msg += ": " + secondary
		}
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = "simctl create printed no UDID"
		}
		return "", msg
	}
	// Stamped before the listing starts, so a poll that began earlier cannot
	// overwrite this with a listing from before the create returned.
	listedAt := time.Now()
	if sims, err := c.listSims(ctx); err != nil {
		if c.cfg.Trace != nil {
			c.cfg.Trace(map[string]any{"kind": "agent", "action": "create", "platform": "ios", "name": req.Name, "syncError": err.Error()})
		}
	} else if c.cfg.Manager != nil {
		c.cfg.Manager.SyncSimulatorsAt(listedAt, sims, c.cfg.Simcap)
	}
	return target.SimulatorID(udid), ""
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
