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

// The shapes and flags below are read from revyl-cli v0.2.27
// (github.com/RevylAI/revyl-cli, tag v0.2.27, commit 35b06ed8). Paths in the
// comments are into that tree. The device-proxy input in revylrest.go, the
// workflow_run_id it needs from `device start` and `device info`, and
// `device tap` were checked against a live account on 2026-11-18, with
// v0.2.18.

// RevylCLI runs the revyl binary.
type RevylCLI struct {
	// Path is the binary.
	Path string
	// Dir is the working directory. revyl reads a project's .revyl/config.yaml
	// from the directory it runs in and caches sessions in
	// .revyl/device-sessions.json there (internal/mcp/device_session.go:2263),
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
// progress to stderr (cmd/revyl/device.go:264 jsonOrPrint), so stderr is
// only read for the error.
func (c RevylCLI) Run(ctx context.Context, extra []string, args ...string) ([]byte, error) {
	if c.Path == "" {
		return nil, errors.New("revyl: CLI not installed")
	}
	return c.run(c.Command(ctx, extra, args...), args)
}

func (c RevylCLI) run(cmd *exec.Cmd, args []string) ([]byte, error) {
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
// --json` print it: mcp.DeviceSession, internal/mcp/device_session.go:109.
// Only the fields loupe reads are kept.
type RevylSession struct {
	SessionID string `json:"session_id"`
	// WorkflowRunID names the session to Revyl's device proxy
	// (internal/mcp/device_session.go:125), which input goes through. It
	// is not the session ID.
	WorkflowRunID string `json:"workflow_run_id"`
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
	// yet, such as "queued" or "starting" (cmd/revyl/device.go:3296).
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
// targets --json` (cmd/revyl/device.go:2708): {"android":[...],"ios":[...]}.
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
	// Grace is how long the CLI has to cancel the device once ctx ends,
	// before it is killed. Zero is 5 s.
	Grace time.Duration
}

// Start starts a cloud device and waits for it, from `revyl device start
// --json` (cmd/revyl/device.go:185 deviceStartOutput, which embeds the
// session). --open=false keeps it from opening Revyl's own viewer in a
// browser, and --computer none from running inside a Revyl computer that
// REVYL_COMPUTER_ID names.
//
// When ctx ends the CLI gets SIGINT rather than SIGKILL: `device start`
// catches it and cancels the device at Revyl (cmd/revyl/device.go:1111,
// internal/mcp/device_session.go:759 and :777) instead of leaving it to
// come up. It is killed if it is still running Grace later. A start
// interrupted just after it finished still prints its session, and Start
// returns that session with the error: the caller owns a device it must
// stop.
func (c RevylCLI) Start(ctx context.Context, s RevylStart) (RevylSession, error) {
	if s.Platform != "android" && s.Platform != "ios" {
		return RevylSession{}, fmt.Errorf("revyl: platform %q, want android or ios", s.Platform)
	}
	if c.Path == "" {
		return RevylSession{}, errors.New("revyl: CLI not installed")
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
	cmd := c.Command(ctx, nil, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = s.Grace
	if cmd.WaitDelay <= 0 {
		cmd.WaitDelay = 5 * time.Second
	}
	out, err := c.run(cmd, args)
	var sess RevylSession
	if json.Unmarshal(out, &sess) != nil || sess.SessionID == "" {
		if err != nil {
			return RevylSession{}, err
		}
		return RevylSession{}, errors.New("revyl device start: no session in its output")
	}
	return sess, err
}

// Info reads one session, from `revyl device info --json`. A session that
// has ended prints {"active":false,...} (cmd/revyl/device.go:3130), which
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
// --json` (cmd/revyl/device.go:3239).
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
	// REST sends input over Revyl's device proxy. Nil sends every gesture
	// through the CLI.
	REST *RevylREST
	// Trace gets one record per gesture: which way it went and how long it
	// took. Nil drops them.
	Trace func(map[string]any)

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
	return &revylStream{whepReceiver: rcv, in: newRevylInput(t.CLI, t.REST, s, t.inputTrace())}, nil
}

func (t *RevylTarget) inputTrace() func(map[string]any) {
	if t.Trace == nil {
		return nil
	}
	id := t.ID()
	return func(rec map[string]any) {
		rec["target"] = id
		t.Trace(rec)
	}
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

// revylStream is a WHEP video stream with Revyl gestures for input.
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

// revylTextEnv carries typed text to the CLI in the environment (see
// revylJob.argv).
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
// Each gesture is a network round trip, so gestures queue for one worker
// that sends them one at a time, in order, and the input methods return at
// once instead of holding up the viewer that sent them. The worker sends
// over the device proxy when it can and through the CLI otherwise.
type revylInput struct {
	cli      RevylCLI
	rest     *RevylREST // nil: the CLI sends everything
	runID    string     // the session's workflow_run_id; "" leaves REST out
	session  string
	platform string
	screenW  int // device units; 0 when Revyl did not say
	screenH  int
	trace    func(map[string]any)
	ctx      context.Context // cancelled at close, ending the gesture in flight
	cancel   context.CancelFunc
	wake     chan struct{}
	stopped  chan struct{}
	once     sync.Once
	now      func() time.Time

	qmu    sync.Mutex // after mu when both are held
	queue  []*revylJob
	closed bool

	mu      sync.Mutex
	press   *revylPress
	tapX    int // device units of the last tap, where typing goes
	tapY    int
	haveTap bool
	lastErr error

	// Set by the worker alone. A 426 turns REST off for the stream, since
	// Revyl will refuse every request the same way. A 401 or 403 does for
	// as long as the key is the one refused.
	restOff     bool
	refusedKey  [32]byte
	haveRefused bool
}

type revylPress struct {
	id     uint64
	x0, y0 int32
	x, y   int32
	at     time.Time
}

// revylJob is one gesture. It holds what the gesture is rather than a
// request, so the device proxy and the CLI fallback send the same thing.
type revylJob struct {
	// action is the device-proxy action: tap, drag, longpress, go_home,
	// back, key or input.
	action string
	x, y   int
	x1, y1 int    // drag's end
	ms     int    // longpress's hold
	key    string // ENTER or BACKSPACE
	text   string // input's text; never in argv, an error or the trace
}

// body is the device-proxy request body, as the CLI's MCP tools build it
// (internal/mcp/device_tools.go): tap :1450, longpress :1519, input :1600,
// drag :1700, and key in internal/mcp/device_web_actions.go:84. go_home
// (:2564) and back (:1864) send none.
func (j *revylJob) body() any {
	switch j.action {
	case "tap":
		return map[string]int{"x": j.x, "y": j.y}
	case "drag":
		return map[string]int{"start_x": j.x, "start_y": j.y, "end_x": j.x1, "end_y": j.y1}
	case "longpress":
		return map[string]int{"x": j.x, "y": j.y, "duration_ms": j.ms}
	case "key":
		return map[string]string{"key": j.key}
	case "input":
		// clear_first is always sent: the worker's default is true, which
		// would wipe the field on every burst of typing.
		return struct {
			X          int    `json:"x"`
			Y          int    `json:"y"`
			Text       string `json:"text"`
			ClearFirst bool   `json:"clear_first"`
		}{j.x, j.y, j.text, false}
	}
	return nil
}

// argv is the same gesture as a CLI command, without --session-id. Typed
// text goes in the environment, so it is never in argv where ps shows it:
// `device type --text-env` (cmd/revyl/device.go:4039,
// internal/mcp/device_tools.go:1549). `device type` needs a point to focus
// first (cmd/revyl/device.go:1836), and its --clear-first defaults to true.
func (j *revylJob) argv() (env, args []string) {
	x, y := strconv.Itoa(j.x), strconv.Itoa(j.y)
	switch j.action {
	case "tap":
		return nil, []string{"device", "tap", "--x", x, "--y", y}
	case "drag":
		return nil, []string{"device", "drag", "--start-x", x, "--start-y", y,
			"--end-x", strconv.Itoa(j.x1), "--end-y", strconv.Itoa(j.y1)}
	case "longpress":
		return nil, []string{"device", "long-press", "--x", x, "--y", y, "--duration", strconv.Itoa(j.ms)}
	case "go_home":
		return nil, []string{"device", "home"}
	case "back":
		return nil, []string{"device", "back"}
	case "key":
		return nil, []string{"device", "key", "--key", j.key}
	case "input":
		return []string{revylTextEnv + "=" + j.text},
			[]string{"device", "type", "--text-env", revylTextEnv, "--clear-first=false", "--x", x, "--y", y}
	}
	return nil, nil
}

func newRevylInput(cli RevylCLI, rest *RevylREST, s RevylSession, trace func(map[string]any)) *revylInput {
	ctx, cancel := context.WithCancel(context.Background())
	in := &revylInput{
		cli: cli, rest: rest, runID: s.WorkflowRunID, session: s.SessionID, platform: s.Platform,
		screenW: s.ScreenWidth, screenH: s.ScreenHeight, trace: trace,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), stopped: make(chan struct{}),
		now: time.Now,
	}
	go in.work()
	return in
}

func (in *revylInput) work() {
	defer close(in.stopped)
	for {
		j := in.next()
		if j == nil {
			return
		}
		in.run(j)
	}
}

// next takes the oldest gesture, waiting for one. It returns nil once the
// input is closed.
func (in *revylInput) next() *revylJob {
	for {
		in.qmu.Lock()
		if in.closed {
			in.qmu.Unlock()
			return nil
		}
		if len(in.queue) > 0 {
			j := in.queue[0]
			in.queue[0] = nil
			in.queue = in.queue[1:]
			in.qmu.Unlock()
			return j
		}
		in.qmu.Unlock()
		select {
		case <-in.wake:
		case <-in.ctx.Done():
			return nil
		}
	}
}

// run sends one gesture and records how it went.
func (in *revylInput) run(j *revylJob) {
	rec := map[string]any{"action": "input", "gesture": j.action}
	done, err := in.sendREST(j, rec)
	if !done {
		env, args := j.argv()
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(in.ctx, 30*time.Second)
		_, err = in.cli.Run(ctx, env, append(args, "--session-id", in.session)...)
		cancel()
		rec["via"], rec["ms"] = "cli", time.Since(t0).Milliseconds()
	}
	if err != nil {
		err = revylHideText(err, j.text)
		rec["error"] = err.Error()
	}
	in.mu.Lock()
	in.lastErr = err
	in.mu.Unlock()
	if in.trace != nil {
		in.trace(rec)
	}
}

// sendREST sends j over the device proxy. done is false when the CLI
// should send it instead: the proxy is not set up for this session, or the
// request failed in a way that shows Revyl did not act on it.
func (in *revylInput) sendREST(j *revylJob, rec map[string]any) (done bool, err error) {
	if in.rest == nil || in.runID == "" {
		return false, nil
	}
	key := in.rest.key()
	switch {
	case key == "":
		rec["rest"] = "no key"
		return false, nil
	case in.restOff:
		rec["rest"] = "refused earlier"
		return false, nil
	case in.haveRefused && in.refusedKey == revylKeyID(key):
		rec["rest"] = "key refused earlier"
		return false, nil
	}
	t0 := time.Now()
	res, err := in.rest.do(in.ctx, key, in.runID, j.action, j.body())
	ms := time.Since(t0).Milliseconds()
	if err == nil {
		rec["via"], rec["ms"] = "rest", ms
		if res.DeviceMs > 0 {
			rec["deviceMs"] = res.DeviceMs
		}
		return true, nil
	}
	var re *revylRESTError
	if in.ctx.Err() == nil && errors.As(err, &re) && re.fallback() {
		rec["restError"], rec["restMs"] = revylHideText(err, j.text).Error(), ms
		switch re.status {
		case http.StatusUpgradeRequired:
			in.restOff = true
		case http.StatusUnauthorized, http.StatusForbidden:
			in.refusedKey, in.haveRefused = revylKeyID(key), true
		}
		return false, nil
	}
	rec["via"], rec["ms"] = "rest", ms
	return true, err
}

// revylHideText keeps typed text out of an error that might echo it.
// Text this short is left alone: replacing it would garble the message
// more than repeating it reveals.
func revylHideText(err error, text string) error {
	if len(text) < 4 || !strings.Contains(err.Error(), text) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), text, "<text>"))
}

func (in *revylInput) close() {
	in.once.Do(func() {
		in.qmu.Lock()
		in.closed, in.queue = true, nil
		in.qmu.Unlock()
		in.cancel()
	})
	<-in.stopped
}

// enqueue queues a gesture against this session. Typing joins text still
// waiting at the same point, so a burst of keystrokes is one input call.
// Text is never refused for a full queue: a dropped character is a wrong
// word nobody is told about, while a dropped tap is visible. Past the
// limit nothing else is queued, so later text joins the last job and the
// queue grows by one at most.
func (in *revylInput) enqueue(j *revylJob) error {
	in.qmu.Lock()
	defer in.qmu.Unlock()
	if in.closed {
		return errors.New("revyl: stream closed")
	}
	if j.action == "input" {
		if n := len(in.queue); n > 0 {
			if t := in.queue[n-1]; t.action == "input" && t.x == j.x && t.y == j.y {
				t.text += j.text
				return nil
			}
		}
	} else if len(in.queue) >= revylQueue {
		return errors.New("revyl: input queue full; the device is answering slowly")
	}
	in.queue = append(in.queue, j)
	select {
	case in.wake <- struct{}{}:
	default:
	}
	return nil
}

// errRevylNoPoints is returned for iOS input when Revyl reported no screen
// size. iOS takes points, and guessing the screen's scale factor would put
// every tap in the wrong place without saying so.
var errRevylNoPoints = errors.New("revyl: the session reports no screen size, so a stream pixel cannot be turned into an iOS point")

// toDevice maps a stream pixel to Revyl's device units: pixels on Android,
// points on iOS (the flags' help text, cmd/revyl/device.go:4014).
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
		return in.enqueue(&revylJob{action: "drag", x: x0, y: y0, x1: x1, y1: y1})
	}
	in.tapX, in.tapY, in.haveTap = x0, y0, true
	if held := in.now().Sub(p.at); held >= revylLongPress {
		return in.enqueue(&revylJob{action: "longpress", x: x0, y: y0, ms: int(held / time.Millisecond)})
	}
	return in.enqueue(&revylJob{action: "tap", x: x0, y: y0})
}

// key sends a key on its down event; the up is ignored, since Revyl takes
// a whole press. Phones take the keys ENTER and BACKSPACE only
// (internal/mcp/device_web_actions.go:15 ParseDeviceKey, the same names
// on Android and iOS), and back is Android's.
func (in *revylInput) key(k Key) error {
	if k.Action != KeyDown {
		return nil
	}
	switch k.Keycode {
	case KeycodeHome:
		return in.enqueue(&revylJob{action: "go_home"})
	case KeycodeBack:
		if in.platform != "android" {
			return ErrNoControl
		}
		return in.enqueue(&revylJob{action: "back"})
	case keycodeEnter:
		return in.enqueue(&revylJob{action: "key", key: "ENTER"})
	case keycodeDel:
		return in.enqueue(&revylJob{action: "key", key: "BACKSPACE"})
	}
	return ErrNoControl
}

// text types at the last tap, the field the user picked: Revyl types at a
// point.
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
	return in.enqueue(&revylJob{action: "input", x: x, y: y, text: s})
}

// Err returns the last gesture's error, for tests and diagnostics.
func (in *revylInput) err() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.lastErr
}
