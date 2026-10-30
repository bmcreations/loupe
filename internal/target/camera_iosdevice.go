package target

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A phone's camera is fed by the same shim as a simulator's, which works
// because dyld honours DYLD_INSERT_LIBRARIES in a development-signed app
// (one with get-task-allow), the way Xcode's Main Thread Checker gets in.
// devicectl passes DEVICECTL_CHILD_* to the launched app as its environment,
// and the shim and the image go into the app's data container with
// `devicectl device copy to`. Measured on an iPhone 16 Pro on iOS 27.2:
//
//   - dyld refuses a library signed by a team other than the app's (library
//     validation), and skips it without a word, as it does a path that is
//     not there. So the shim is signed once per local Apple Development
//     identity's team, and every copy goes into the insert list: the one
//     that matches loads.
//   - DYLD_INSERT_LIBRARIES wants an absolute path, and devicectl does not
//     say where a data container is. dyld does: with DYLD_PRINT_ENV set it
//     prints the app's environment, HOME included, to the console.
//   - An App Store, TestFlight or enterprise build has no get-task-allow, so
//     dyld ignores the whole environment and prints nothing.
//
// The shim writes the pid it loaded into next to the image, and loupe reads
// it back after the launch: dyld's silence is otherwise the only answer.

const (
	// phoneCameraFile is the image, relative to the app's home, where
	// the shim resolves LOUPE_CAMERA_FILE.
	phoneCameraFile = "Library/Caches/loupe-camera.png"
	// phoneCameraMarker is what the shim writes its pid to.
	phoneCameraMarker = phoneCameraFile + ".loaded"
)

// ErrNotDevSigned is a phone app that dyld will not inject into.
var ErrNotDevSigned = errors.New("the app is not development-signed (no get-task-allow), so iOS ignores the launch environment and the camera cannot be fed to it")

// ErrShimNotLoaded is a launch that went through but without the shim.
var ErrShimNotLoaded = errors.New("the app launched without the camera shim: no local Apple Development identity matches the team that signed it")

// phoneCam is what loupe knows about one app on a phone.
type phoneCam struct {
	home string // the data container's absolute path on the phone
	libs string // the shim build whose copies are in the container
	pid  int    // the process launched with the shim
}

// phoneShims is the shim built for iphoneos and signed by each local Apple
// Development identity, one copy per team.
type phoneShims struct {
	key   string            // the unsigned build these were signed from
	teams map[string]string // team ID -> signed copy
}

// identityRE matches a line of `security find-identity -v -p codesigning`.
var identityRE = regexp.MustCompile(`^\s*\d+\)\s+([0-9A-F]{40})\s+"Apple Development: `)

// parseIdentities returns the Apple Development identities' hashes.
func parseIdentities(out string) []string {
	var ids []string
	for _, l := range strings.Split(out, "\n") {
		if m := identityRE.FindStringSubmatch(l); m != nil {
			ids = append(ids, m[1])
		}
	}
	return ids
}

// teamRE is codesign -dv's TeamIdentifier line.
var teamRE = regexp.MustCompile(`(?m)^TeamIdentifier=(\w+)$`)

var (
	phoneShimMu sync.Mutex
	phoneShimAt phoneShims
)

// loupecamPhoneShims builds the shim for iphoneos and signs a copy with each
// local Apple Development identity, keeping one per team.
func loupecamPhoneShims(ctx context.Context) (phoneShims, error) {
	lib, err := loupecamBuild(ctx, "iphoneos")
	if err != nil {
		return phoneShims{}, err
	}
	phoneShimMu.Lock()
	defer phoneShimMu.Unlock()
	if phoneShimAt.key == lib && len(phoneShimAt.teams) > 0 {
		return phoneShimAt, nil
	}
	out, err := exec.CommandContext(ctx, "security", "find-identity", "-v", "-p", "codesigning").Output()
	if err != nil {
		return phoneShims{}, fmt.Errorf("%w: list signing identities: %v", ErrCameraShim, err)
	}
	ids := parseIdentities(string(out))
	if len(ids) == 0 {
		return phoneShims{}, fmt.Errorf("%w: there is no Apple Development identity in the keychain to sign the camera shim with", ErrCameraShim)
	}
	shims := phoneShims{key: lib, teams: map[string]string{}}
	base := strings.TrimSuffix(lib, ".dylib")
	for _, id := range ids {
		tmp := base + "-" + id[:8] + ".dylib"
		raw, err := os.ReadFile(lib)
		if err != nil {
			return phoneShims{}, err
		}
		if err := os.WriteFile(tmp, raw, 0o755); err != nil {
			return phoneShims{}, err
		}
		if b, err := exec.CommandContext(ctx, "codesign", "-f", "-s", id, tmp).CombinedOutput(); err != nil {
			os.Remove(tmp)
			return phoneShims{}, fmt.Errorf("%w: sign with %s: %v: %s", ErrCameraShim, id[:8], err, strings.TrimSpace(string(b)))
		}
		b, _ := exec.CommandContext(ctx, "codesign", "-dv", tmp).CombinedOutput()
		m := teamRE.FindSubmatch(b)
		if m == nil || shims.teams[string(m[1])] != "" {
			os.Remove(tmp)
			continue
		}
		shims.teams[string(m[1])] = tmp
	}
	if len(shims.teams) == 0 {
		return phoneShims{}, fmt.Errorf("%w: no identity gave a team-signed shim", ErrCameraShim)
	}
	phoneShimAt = shims
	return shims, nil
}

// libName is a team's shim copy's name in the container.
func (p phoneShims) libName(team string) string { return "Library/loupecam-" + team + ".dylib" }

// sortedTeams is the teams in a stable order.
func (p phoneShims) sortedTeams() []string {
	t := make([]string, 0, len(p.teams))
	for k := range p.teams {
		t = append(t, k)
	}
	sort.Strings(t)
	return t
}

// homeRE is dyld's DYLD_PRINT_ENV line for HOME.
var homeRE = regexp.MustCompile(`dyld\[\d+\]: HOME=(/\S+)`)

// phoneHome finds bundle's data container by launching it once with
// DYLD_PRINT_ENV and reading HOME off the console. Stopping devicectl ends
// that launch too; the real one follows. An app dyld will not take an
// environment from prints nothing, which is ErrNotDevSigned.
func (d *IOSDevice) phoneHome(ctx context.Context, bundle string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "process", "launch",
		"--device", d.device(), "--terminate-existing", "--console", bundle)
	// devicectl block-buffers the console into a pipe, and a kill loses
	// the buffer: NSUnbufferedIO makes it write each line as it comes.
	cmd.Env = append(os.Environ(), "DEVICECTL_CHILD_DYLD_PRINT_ENV=1", "NSUnbufferedIO=YES")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return "", err
	}
	var home string
	var said bytes.Buffer
	sc := bufio.NewScanner(pipe)
	for sc.Scan() {
		l := sc.Text()
		if m := homeRE.FindStringSubmatch(l); m != nil {
			home = m[1]
			break
		}
		if said.Len() < 4096 {
			said.WriteString(l + "\n")
		}
		// The app is up and dyld has had its say without HOME.
		if strings.Contains(l, "Waiting for the application to terminate") {
			go func() { time.Sleep(3 * time.Second); cancel() }()
		}
	}
	cancel()
	cmd.Wait()
	if home != "" {
		return home, nil
	}
	if devicectlNotInstalled(said.Bytes()) {
		return "", fmt.Errorf("%w: %s", ErrAppNotInstalled, bundle)
	}
	if strings.Contains(said.String(), "Waiting for the application") {
		return "", fmt.Errorf("%s: %w", bundle, ErrNotDevSigned)
	}
	if said.Len() == 0 {
		// Seen with the phone at its passcode screen: devicectl neither
		// launches nor says why.
		return "", fmt.Errorf("devicectl launch %s: no answer in 30s; is the phone locked?", bundle)
	}
	return "", fmt.Errorf("devicectl launch %s: %s", bundle, strings.TrimSpace(said.String()))
}

// copyToApp copies a local file into bundle's data container at rel.
func (d *IOSDevice) copyToApp(ctx context.Context, bundle, src, rel string) error {
	b, err := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "copy", "to",
		"--device", d.device(), "--domain-type", "appDataContainer", "--domain-identifier", bundle,
		"--source", src, "--destination", rel).CombinedOutput()
	if err != nil {
		return fmt.Errorf("devicectl copy to %s: %v: %s", bundle, err, strings.TrimSpace(string(b)))
	}
	return nil
}

// readFromApp reads rel out of bundle's data container.
func (d *IOSDevice) readFromApp(ctx context.Context, bundle, rel string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "loupe-phonecam")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	dst := filepath.Join(dir, "f")
	b, err := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "copy", "from",
		"--device", d.device(), "--domain-type", "appDataContainer", "--domain-identifier", bundle,
		"--source", rel, "--destination", dst).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("devicectl copy from %s: %v: %s", bundle, err, strings.TrimSpace(string(b)))
	}
	return os.ReadFile(dst)
}

// emptyFile is a zero-byte file to copy over one on the phone: devicectl
// has no way to delete a file. The shim reads an empty image as none.
func emptyFile() (string, error) {
	dir, err := cameraDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "empty")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// launchPid reads the pid out of `devicectl device process launch -j`.
func launchPid(raw []byte) int {
	var v struct {
		Result struct {
			Process struct {
				ProcessIdentifier int `json:"processIdentifier"`
			} `json:"process"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	return v.Result.Process.ProcessIdentifier
}

// runningPids is the phone's running processes' pids.
func (d *IOSDevice) runningPids(ctx context.Context) (map[int]bool, error) {
	dir, err := os.MkdirTemp("", "loupe-phonecam")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "p.json")
	if b, err := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "info", "processes",
		"--device", d.device(), "-j", path).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("devicectl processes: %v: %s", err, strings.TrimSpace(string(b)))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseRunningPids(raw), nil
}

func parseRunningPids(raw []byte) map[int]bool {
	var v struct {
		Result struct {
			RunningProcesses []struct {
				ProcessIdentifier int `json:"processIdentifier"`
			} `json:"runningProcesses"`
		} `json:"result"`
	}
	pids := map[int]bool{}
	if json.Unmarshal(raw, &v) == nil {
		for _, p := range v.Result.RunningProcesses {
			pids[p.ProcessIdentifier] = true
		}
	}
	return pids
}

// liveCams is the apps launched with the shim that are still running,
// forgetting the pids of the ones that are not.
func (d *IOSDevice) liveCams(ctx context.Context) ([]string, error) {
	d.camMu.Lock()
	n := 0
	for _, c := range d.cams {
		if c.pid > 0 {
			n++
		}
	}
	d.camMu.Unlock()
	if n == 0 {
		return nil, nil
	}
	pids, err := d.runningPids(ctx)
	if err != nil {
		return nil, err
	}
	d.camMu.Lock()
	defer d.camMu.Unlock()
	var live []string
	for b, c := range d.cams {
		if c.pid > 0 && pids[c.pid] {
			live = append(live, b)
		} else {
			c.pid = 0
		}
	}
	sort.Strings(live)
	return live, nil
}

func (d *IOSDevice) cam(bundle string) *phoneCam {
	d.camMu.Lock()
	defer d.camMu.Unlock()
	if d.cams == nil {
		d.cams = map[string]*phoneCam{}
	}
	c := d.cams[bundle]
	if c == nil {
		c = &phoneCam{}
		d.cams[bundle] = c
	}
	return c
}

func (d *IOSDevice) cameraName() string { return "ios-" + d.Info().UDID }

// stagedCamera is the phone's staged image on this Mac, or "".
func (d *IOSDevice) stagedCamera() string {
	path, err := cameraFile(d.cameraName())
	if err != nil {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// SetCamera implements CameraFeeder. The image is staged on this Mac and
// copied into each running app that has the shim, which sees it within a
// quarter of a second; the next launch through loupe gives the shim to the
// app it opens.
func (d *IOSDevice) SetCamera(ctx context.Context, png []byte) (CameraApplies, error) {
	// Built and signed now, so a missing Xcode or identity fails here
	// rather than at the launch.
	if _, err := loupecamPhoneShims(ctx); err != nil {
		return "", err
	}
	path, err := cameraFile(d.cameraName())
	if err != nil {
		return "", err
	}
	if err := writeCameraFile(path, png); err != nil {
		return "", err
	}
	return d.pushCamera(ctx, path)
}

// ClearCamera implements CameraFeeder. Running apps with the shim see black;
// the next launch of each is a plain one.
func (d *IOSDevice) ClearCamera(ctx context.Context) (CameraApplies, error) {
	path, err := cameraFile(d.cameraName())
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	empty, err := emptyFile()
	if err != nil {
		return "", err
	}
	return d.pushCamera(ctx, empty)
}

// pushCamera copies src over the image in each running app with the shim.
func (d *IOSDevice) pushCamera(ctx context.Context, src string) (CameraApplies, error) {
	live, err := d.liveCams(ctx)
	if err != nil {
		return "", err
	}
	for _, b := range live {
		if err := d.copyToApp(ctx, b, src, phoneCameraFile); err != nil {
			return "", err
		}
	}
	if len(live) > 0 {
		return CameraLive, nil
	}
	return CameraOnLaunch, nil
}

// LaunchBudget implements LaunchBudgeter: a launch that gives an app the
// shim is a handful of devicectl calls, each a few seconds over Wi-Fi.
func (d *IOSDevice) LaunchBudget() time.Duration {
	if d.stagedCamera() == "" {
		return 0
	}
	return 2 * time.Minute
}

// cameraLaunch is how Launch starts bundle while an image is staged: the
// shims and the image are copied into its container and it is relaunched
// with them in its environment, then the shim's marker is read back. It
// reports false when there is nothing staged or the running app already has
// the shim, and the launch should be a plain one.
func (d *IOSDevice) cameraLaunch(ctx context.Context, bundle string) (bool, error) {
	file := d.stagedCamera()
	if file == "" {
		return false, nil
	}
	if live, err := d.liveCams(ctx); err == nil {
		for _, b := range live {
			if b == bundle {
				return false, nil
			}
		}
	}
	shims, err := loupecamPhoneShims(ctx)
	if err != nil {
		return true, err
	}
	err = d.shimLaunch(ctx, bundle, file, shims)
	if errors.Is(err, ErrShimNotLoaded) {
		// A reinstall moves the container, and dyld skips a path that is
		// gone, so look for it again once.
		d.cam(bundle).home = ""
		err = d.shimLaunch(ctx, bundle, file, shims)
	}
	return true, err
}

func (d *IOSDevice) shimLaunch(ctx context.Context, bundle, file string, shims phoneShims) error {
	c := d.cam(bundle)
	d.camMu.Lock()
	home, libs := c.home, c.libs
	d.camMu.Unlock()
	if home == "" {
		h, err := d.phoneHome(ctx, bundle)
		if err != nil {
			return err
		}
		home, libs = h, ""
	}
	if libs != shims.key {
		for _, team := range shims.sortedTeams() {
			if err := d.copyToApp(ctx, bundle, shims.teams[team], shims.libName(team)); err != nil {
				return err
			}
		}
	}
	d.camMu.Lock()
	c.home, c.libs = home, shims.key
	d.camMu.Unlock()
	if err := d.copyToApp(ctx, bundle, file, phoneCameraFile); err != nil {
		return err
	}
	empty, err := emptyFile()
	if err != nil {
		return err
	}
	if err := d.copyToApp(ctx, bundle, empty, phoneCameraMarker); err != nil {
		return err
	}

	var insert []string
	for _, team := range shims.sortedTeams() {
		insert = append(insert, home+"/"+shims.libName(team))
	}
	dir, err := os.MkdirTemp("", "loupe-phonecam")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	js := filepath.Join(dir, "launch.json")
	cmd := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "process", "launch",
		"--device", d.device(), "--terminate-existing", "-j", js, bundle)
	cmd.Env = append(os.Environ(),
		"DEVICECTL_CHILD_DYLD_INSERT_LIBRARIES="+strings.Join(insert, ":"),
		"DEVICECTL_CHILD_LOUPE_CAMERA_FILE="+phoneCameraFile,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		if devicectlNotInstalled(out) {
			return fmt.Errorf("%w: %s", ErrAppNotInstalled, bundle)
		}
		return fmt.Errorf("devicectl launch %s: %w: %s", bundle, err, strings.TrimSpace(string(out)))
	}
	raw, _ := os.ReadFile(js)
	pid := launchPid(raw)

	// The shim writes its marker before the app's main runs, so it is
	// there by the time devicectl answers; one retry covers a slow start.
	var got []byte
	for i := 0; i < 2; i++ {
		got, _ = d.readFromApp(ctx, bundle, phoneCameraMarker)
		if len(bytes.TrimSpace(got)) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if n, _ := strconv.Atoi(string(bytes.TrimSpace(got))); n == 0 || (pid != 0 && n != pid) {
		return fmt.Errorf("%s: %w", bundle, ErrShimNotLoaded)
	}
	d.camMu.Lock()
	c.pid = pid
	d.camMu.Unlock()
	return nil
}
