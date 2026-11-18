package target

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// KindRevyl is a cloud Android emulator or iOS simulator on Revyl
// (revyl.com), reached through the user's own revyl CLI and account. Its
// video comes over WHEP and its input as CLI gestures; nothing about it is
// local, so it has no adb serial and no simctl UDID.
const KindRevyl Kind = "revyl"

// The shapes and flags below are read from revyl-cli v0.2.18
// (github.com/RevylAI/revyl-cli, tag v0.2.18, commit 381c4fcf). Paths in the
// comments are into that tree. None of it is checked against a live
// account yet.

// RevylCLI runs the revyl binary.
type RevylCLI struct {
	// Path is the binary.
	Path string
	// Dir is the working directory. revyl reads a project's .revyl/config.yaml
	// from the directory it runs in and caches sessions in
	// .revyl/device-sessions.json there (internal/mcp/device_session.go:2258),
	// so it runs in loupe's own directory rather than wherever loupe started.
	Dir string
}

// revylEnv turns off what the CLI does on its own account: usage analytics
// (internal/analytics/analytics.go:213), the update check that talks to
// GitHub on every run (cmd/revyl/version_check.go:135), and ANSI colour in
// the stderr that ends up in error messages.
var revylEnv = []string{"REVYL_TELEMETRY_DISABLED=1", "REVYL_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1"}

// Command builds a revyl invocation with the environment above plus extra.
func (c RevylCLI) Command(ctx context.Context, extra []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Path, args...)
	cmd.Dir = c.Dir
	cmd.Env = append(append(os.Environ(), revylEnv...), extra...)
	return cmd
}

// Run runs revyl and returns its stdout. --json output goes to stdout and
// progress to stderr (cmd/revyl/device.go:250 jsonOrPrint), so stderr is
// only read for the error.
func (c RevylCLI) Run(ctx context.Context, extra []string, args ...string) ([]byte, error) {
	if c.Path == "" {
		return nil, errors.New("revyl: CLI not installed")
	}
	cmd := c.Command(ctx, extra, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := lastLine(stderr.String())
		if msg == "" {
			msg = lastLine(stdout.String())
		}
		if msg != "" {
			// A session's viewer, worker and WHEP URLs can carry its
			// access, and errors reach the trace and the API.
			msg = revylURL.ReplaceAllString(msg, "<url>")
			return stdout.Bytes(), fmt.Errorf("revyl %s: %s", revylVerb(args), clip(msg, 300))
		}
		return stdout.Bytes(), fmt.Errorf("revyl %s: %w", revylVerb(args), err)
	}
	return stdout.Bytes(), nil
}

var revylURL = regexp.MustCompile(`(?i)\b(?:https?|wss?)://\S+`)

// revylVerb names a command by its subcommands, never its flag values,
// which can hold a session ID.
func revylVerb(args []string) string {
	var words []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || len(words) == 2 {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// RevylSession is one cloud device session, as `revyl device start|info
// --json` print it: mcp.DeviceSession, internal/mcp/device_session.go:107.
// Only the fields loupe reads are kept.
type RevylSession struct {
	SessionID     string `json:"session_id"`
	WorkerBaseURL string `json:"worker_base_url"`
	ViewerURL     string `json:"viewer_url"`
	// WHEPURL is the raw WebRTC playback URL. It is absent until the device
	// streams. It and the two URLs above give access to the session, so
	// none of them leaves this package's callers: not in a listing, an
	// error or a trace.
	WHEPURL  *string `json:"whep_url,omitempty"`
	Platform string  `json:"platform"` // "ios" or "android" ("web" is not offered)
	// ScreenWidth and ScreenHeight are in device units: points on iOS,
	// pixels on Android. 0 when Revyl does not know them.
	ScreenWidth  int       `json:"screen_width,omitempty"`
	ScreenHeight int       `json:"screen_height,omitempty"`
	DeviceReady  *bool     `json:"device_ready,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	// IdleTimeout is a time.Duration in nanoseconds; Revyl stops the device
	// after this long without a command.
	IdleTimeout time.Duration `json:"idle_timeout"`
	// Status is set by `device list` for a session that is not attached
	// yet, such as "queued" or "starting" (cmd/revyl/device.go:3219).
	Status string `json:"status,omitempty"`
}

// Ready reports whether the session can be streamed.
func (s RevylSession) Ready() bool {
	return s.WHEPURL != nil && *s.WHEPURL != "" && (s.DeviceReady == nil || *s.DeviceReady)
}

// RevylDeviceTarget is one model and OS a session can start on. The struct
// in internal/devicetargets/targets.go:25 has no JSON tags, so the keys are
// the Go field names.
type RevylDeviceTarget struct {
	Model   string `json:"Model"`
	Runtime string `json:"Runtime"`
}

// Targets lists the models and OS versions per platform, from `revyl device
// targets --json` (cmd/revyl/device.go:2687): {"android":[...],"ios":[...]}.
func (c RevylCLI) Targets(ctx context.Context) (map[string][]RevylDeviceTarget, error) {
	out, err := c.Run(ctx, nil, "device", "targets", "--json")
	if err != nil {
		return nil, err
	}
	var t map[string][]RevylDeviceTarget
	if err := json.Unmarshal(out, &t); err != nil {
		return nil, fmt.Errorf("revyl device targets: %w", err)
	}
	return t, nil
}

// RevylStart says what device to start.
type RevylStart struct {
	Platform  string // "android" or "ios"
	Model     string // optional, e.g. "Pixel 7"
	OSVersion string // optional, e.g. "Android 14"
	// IdleTimeout is how long Revyl keeps an untouched device. Zero leaves
	// the CLI's default, 900 s.
	IdleTimeout time.Duration
}

// Start starts a cloud device and waits for it, from `revyl device start
// --json` (cmd/revyl/device.go:174 deviceStartOutput, which embeds the
// session). --open=false keeps it from opening Revyl's own viewer in a
// browser, and --computer none from running inside a Revyl computer that
// REVYL_COMPUTER_ID names.
func (c RevylCLI) Start(ctx context.Context, s RevylStart) (RevylSession, error) {
	if s.Platform != "android" && s.Platform != "ios" {
		return RevylSession{}, fmt.Errorf("revyl: platform %q, want android or ios", s.Platform)
	}
	args := []string{"device", "start", "--platform", s.Platform, "--json", "--open=false", "--computer", "none"}
	if s.Model != "" {
		args = append(args, "--device-model", s.Model)
	}
	if s.OSVersion != "" {
		args = append(args, "--os-version", s.OSVersion)
	}
	if s.IdleTimeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(int(s.IdleTimeout/time.Second)))
	}
	out, err := c.Run(ctx, nil, args...)
	if err != nil {
		return RevylSession{}, err
	}
	var sess RevylSession
	if err := json.Unmarshal(out, &sess); err != nil || sess.SessionID == "" {
		return RevylSession{}, errors.New("revyl device start: no session in its output")
	}
	return sess, nil
}

// Info reads one session, from `revyl device info --json`. A session that
// has ended prints {"active":false,...} (cmd/revyl/device.go:3053), which
// is reported as ok == false.
func (c RevylCLI) Info(ctx context.Context, sessionID string) (s RevylSession, ok bool, err error) {
	out, err := c.Run(ctx, nil, "device", "info", "--session-id", sessionID, "--json")
	if err != nil {
		return RevylSession{}, false, err
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return RevylSession{}, false, fmt.Errorf("revyl device info: %w", err)
	}
	return s, s.SessionID != "", nil
}

// List reads the sessions the account has open, from `revyl device list
// --json` (cmd/revyl/device.go:3162).
func (c RevylCLI) List(ctx context.Context) ([]RevylSession, error) {
	out, err := c.Run(ctx, nil, "device", "list", "--json")
	if err != nil {
		return nil, err
	}
	var ss []RevylSession
	if err := json.Unmarshal(out, &ss); err != nil {
		return nil, fmt.Errorf("revyl device list: %w", err)
	}
	return ss, nil
}

// Stop ends a session, from `revyl device stop --json`: {"stopped":true},
// or stopped false with stop_requested true while it winds down
// (cmd/revyl/device.go, deviceStopCmd).
func (c RevylCLI) Stop(ctx context.Context, sessionID string) error {
	out, err := c.Run(ctx, nil, "device", "stop", "--session-id", sessionID, "--json")
	if err != nil {
		return err
	}
	var r struct {
		Stopped       bool   `json:"stopped"`
		StopRequested bool   `json:"stop_requested"`
		Error         string `json:"error"`
	}
	if json.Unmarshal(out, &r) == nil && !r.Stopped && !r.StopRequested && r.Error != "" {
		return fmt.Errorf("revyl device stop: %s", clip(r.Error, 300))
	}
	return nil
}

// RevylAuth is `revyl auth status --json` (cmd/revyl/auth.go:629). The
// account's key never appears in it.
type RevylAuth struct {
	Authenticated bool   `json:"authenticated"`
	Email         string `json:"email,omitempty"`
	OrgName       string `json:"org_name,omitempty"`
	AuthMethod    string `json:"auth_method,omitempty"`
	Expired       bool   `json:"expired,omitempty"`
}

// AuthStatus reports whether the CLI has credentials: a login in
// ~/.revyl/credentials.json, or REVYL_API_KEY in loupe's environment.
func (c RevylCLI) AuthStatus(ctx context.Context) (RevylAuth, error) {
	out, err := c.Run(ctx, nil, "auth", "status", "--json")
	var a RevylAuth
	if jerr := json.Unmarshal(out, &a); jerr == nil {
		// Signed out may exit non-zero with the JSON still printed.
		return a, nil
	}
	if err != nil {
		return RevylAuth{}, err
	}
	return RevylAuth{}, errors.New("revyl auth status: unreadable output")
}

// RevylTarget is one Revyl session as a loupe target.
type RevylTarget struct {
	CLI     RevylCLI
	Session RevylSession
	// Model and OSVersion are what was asked for at start; Revyl's session
	// does not echo them back. Either may be empty for an adopted session.
	Model, OSVersion string
	// WHEPHeader is sent with the WHEP offer. Revyl does not document
	// whether its whep_url needs one; nil sends none.
	WHEPHeader http.Header

	settings *webrtc.SettingEngine // tests: loopback candidates
	poll     time.Duration         // tests: how often Open re-reads info
}

func (t *RevylTarget) ID() string { return "revyl:" + t.Session.SessionID }
func (t *RevylTarget) Kind() Kind { return KindRevyl }

func (t *RevylTarget) Label() string {
	if t.Model != "" {
		return "Revyl " + t.Model
	}
	switch t.Session.Platform {
	case "ios":
		return "Revyl iOS"
	case "android":
		return "Revyl Android"
	}
	return "Revyl device"
}

// Describe needs no round trip: everything it says was known at start.
func (t *RevylTarget) Describe(context.Context) (Description, error) {
	d := Description{OS: t.Session.Platform, OSVersion: t.OSVersion, Form: "phone", Transport: "cloud"}
	if strings.Contains(strings.ToLower(t.Model), "tablet") || strings.Contains(t.Model, "iPad") {
		d.Form = "tablet"
	}
	return d, nil
}

// Open waits for the session to stream, then connects to its WHEP URL.
func (t *RevylTarget) Open(ctx context.Context, _ Options) (Stream, error) {
	s, err := t.readySession(ctx)
	if err != nil {
		return nil, err
	}
	rcv, err := dialWHEP(ctx, WHEPConfig{
		URL: *s.WHEPURL, Header: t.WHEPHeader, Name: t.Label(), settings: t.settings,
	})
	if err != nil {
		return nil, err
	}
	// t.Session is not updated: listing reads it from other goroutines, and
	// the stream keeps s itself.
	return &revylStream{whepReceiver: rcv, in: newRevylInput(t.CLI, s)}, nil
}

// readySession re-reads the session until it has a WHEP URL. A device
// that was just started can report ready before its stream is up.
func (t *RevylTarget) readySession(ctx context.Context) (RevylSession, error) {
	poll := t.poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		s, ok, err := t.CLI.Info(ctx, t.Session.SessionID)
		switch {
		case err != nil:
			return RevylSession{}, err
		case !ok:
			return RevylSession{}, errors.New("revyl: the session has ended")
		case s.Ready():
			return s, nil
		}
		select {
		case <-ctx.Done():
			return RevylSession{}, errors.New("revyl: the device has not started streaming")
		case <-time.After(poll):
		}
	}
}

// revylStream is a WHEP video stream with Revyl CLI gestures for input.
type revylStream struct {
	*whepReceiver
	in *revylInput
}

func (s *revylStream) Touch(t Touch) error    { return s.in.touch(t, s.Meta()) }
func (s *revylStream) Scroll(Scroll) error    { return ErrNoControl }
func (s *revylStream) Key(k Key) error        { return s.in.key(k) }
func (s *revylStream) Text(text string) error { return s.in.text(text) }
func (s *revylStream) Close() error {
	s.in.close()
	return s.whepReceiver.Close()
}

// Android keycodes the viewer sends for its keyboard (web/keyboard.js).
const (
	keycodeEnter = 66
	keycodeDel   = 67
)

// revylTextEnv carries typed text to the CLI in the environment, so it is
// never in argv where ps shows it: `device type --text-env`
// (cmd/revyl/device.go:3960, internal/mcp/device_tools.go:1539).
const revylTextEnv = "LOUPE_REVYL_TEXT"

// Gesture thresholds, in stream pixels and time. A press that moves less
// than tapSlop is a tap or a long-press; one held at least longPress is a
// long-press.
const (
	revylLongPress = 500 * time.Millisecond
	revylQueue     = 32
)

// revylInput turns loupe's touch stream into Revyl gestures.
//
// Revyl takes whole gestures, not pointer events, so a press is buffered
// from Down to Up and sent as one tap, long-press or drag when it ends: a
// drag shows on the device only after the finger lifts. Only the first
// pointer is tracked; a second finger is ignored.
//
// Each gesture is one CLI process and a network round trip, so commands go
// to one worker that runs them in order, and the input methods return at
// once instead of holding up the viewer that sent them.
type revylInput struct {
	cli      RevylCLI
	session  string
	platform string
	screenW  int // device units; 0 when Revyl did not say
	screenH  int
	jobs     chan revylJob
	done     chan struct{}
	stopped  chan struct{}
	once     sync.Once
	now      func() time.Time

	mu      sync.Mutex
	press   *revylPress
	tapX    int // device units of the last tap, where typing goes
	tapY    int
	haveTap bool
	lastErr error
}

type revylPress struct {
	id     uint64
	x0, y0 int32
	x, y   int32
	at     time.Time
}

type revylJob struct {
	env  []string
	args []string
}

func newRevylInput(cli RevylCLI, s RevylSession) *revylInput {
	in := &revylInput{
		cli: cli, session: s.SessionID, platform: s.Platform,
		screenW: s.ScreenWidth, screenH: s.ScreenHeight,
		jobs: make(chan revylJob, revylQueue), done: make(chan struct{}), stopped: make(chan struct{}),
		now: time.Now,
	}
	go in.work()
	return in
}

func (in *revylInput) work() {
	defer close(in.stopped)
	for {
		select {
		case <-in.done:
			return
		case j := <-in.jobs:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := in.cli.Run(ctx, j.env, j.args...)
			cancel()
			in.mu.Lock()
			in.lastErr = err
			in.mu.Unlock()
		}
	}
}

func (in *revylInput) close() {
	in.once.Do(func() { close(in.done) })
	<-in.stopped
}

// enqueue queues a command against this session.
func (in *revylInput) enqueue(env []string, args ...string) error {
	args = append(args, "--session-id", in.session)
	select {
	case in.jobs <- revylJob{env: env, args: args}:
		return nil
	case <-in.done:
		return errors.New("revyl: stream closed")
	default:
		return errors.New("revyl: input queue full; the device is answering slowly")
	}
}

// errRevylNoPoints is returned for iOS input when Revyl reported no screen
// size. iOS takes points, and guessing the screen's scale factor would put
// every tap in the wrong place without saying so.
var errRevylNoPoints = errors.New("revyl: the session reports no screen size, so a stream pixel cannot be turned into an iOS point")

// toDevice maps a stream pixel to Revyl's device units: pixels on Android,
// points on iOS (the flags' help text, cmd/revyl/device.go:3935).
func (in *revylInput) toDevice(x, y int32, m Meta) (int, int, error) {
	if m.Width <= 0 || m.Height <= 0 {
		return 0, 0, errors.New("revyl: stream size not known yet")
	}
	sw, sh := in.screenW, in.screenH
	if sw <= 0 || sh <= 0 {
		if in.platform == "ios" {
			return 0, 0, errRevylNoPoints
		}
		// Android counts in pixels. Without a size from Revyl, assume the
		// stream is the screen at full size.
		sw, sh = m.Width, m.Height
	}
	if (m.Width > m.Height) != (sw > sh) {
		// The device rotated since the session started: the stream is the
		// screen turned, and so are its units.
		sw, sh = sh, sw
	}
	dx := int(math.Round(float64(x) * float64(sw) / float64(m.Width)))
	dy := int(math.Round(float64(y) * float64(sh) / float64(m.Height)))
	return min(max(dx, 0), sw-1), min(max(dy, 0), sh-1), nil
}

func (in *revylInput) touch(t Touch, m Meta) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	switch t.Action {
	case TouchDown:
		if in.press != nil {
			return nil // a second finger: multi-touch is not supported
		}
		in.press = &revylPress{id: t.PointerID, x0: t.X, y0: t.Y, x: t.X, y: t.Y, at: in.now()}
		return nil
	case TouchMove:
		if in.press != nil && in.press.id == t.PointerID {
			in.press.x, in.press.y = t.X, t.Y
		}
		return nil
	case TouchCancel:
		if in.press != nil && in.press.id == t.PointerID {
			in.press = nil
		}
		return nil
	case TouchUp:
	default:
		return nil
	}
	p := in.press
	if p == nil || p.id != t.PointerID {
		return nil
	}
	in.press = nil
	p.x, p.y = t.X, t.Y
	x0, y0, err := in.toDevice(p.x0, p.y0, m)
	if err != nil {
		return err
	}
	slop := max(6, min(m.Width, m.Height)/50)
	moved := math.Hypot(float64(p.x-p.x0), float64(p.y-p.y0))
	if moved >= float64(slop) {
		x1, y1, err := in.toDevice(p.x, p.y, m)
		if err != nil {
			return err
		}
		return in.enqueue(nil, "device", "drag",
			"--start-x", strconv.Itoa(x0), "--start-y", strconv.Itoa(y0),
			"--end-x", strconv.Itoa(x1), "--end-y", strconv.Itoa(y1))
	}
	in.tapX, in.tapY, in.haveTap = x0, y0, true
	if held := in.now().Sub(p.at); held >= revylLongPress {
		return in.enqueue(nil, "device", "long-press",
			"--x", strconv.Itoa(x0), "--y", strconv.Itoa(y0),
			"--duration", strconv.Itoa(int(held/time.Millisecond)))
	}
	return in.enqueue(nil, "device", "tap", "--x", strconv.Itoa(x0), "--y", strconv.Itoa(y0))
}

// key sends a key on its down event; the up is ignored, since Revyl takes
// a whole press. Phones take ENTER and BACKSPACE only (cmd/revyl/device.go,
// deviceKeyCmd --key), and back is Android's.
func (in *revylInput) key(k Key) error {
	if k.Action != KeyDown {
		return nil
	}
	switch k.Keycode {
	case KeycodeHome:
		return in.enqueue(nil, "device", "home")
	case KeycodeBack:
		if in.platform != "android" {
			return ErrNoControl
		}
		return in.enqueue(nil, "device", "back")
	case keycodeEnter:
		return in.enqueue(nil, "device", "key", "--key", "ENTER")
	case keycodeDel:
		return in.enqueue(nil, "device", "key", "--key", "BACKSPACE")
	}
	return ErrNoControl
}

// text types at the last tap. `device type` needs a point or an element
// description to focus first (cmd/revyl/device.go:1824), and the last tap
// is the field the user picked. --clear-first defaults to true, which would
// wipe the field on every burst of typing.
func (in *revylInput) text(s string) error {
	if s == "" {
		return nil
	}
	in.mu.Lock()
	x, y, ok := in.tapX, in.tapY, in.haveTap
	in.mu.Unlock()
	if !ok {
		return errors.New("revyl: tap a text field first; Revyl types at a point")
	}
	return in.enqueue([]string{revylTextEnv + "=" + s}, "device", "type",
		"--text-env", revylTextEnv, "--clear-first=false",
		"--x", strconv.Itoa(x), "--y", strconv.Itoa(y))
}

// Err returns the last gesture's error, for tests and diagnostics.
func (in *revylInput) err() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.lastErr
}
