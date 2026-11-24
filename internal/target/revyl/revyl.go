package revyl

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target/android"

	"github.com/pion/webrtc/v4"

	"github.com/bmcreations/loupe/internal/target"
)

// The shapes and flags below are read from revyl-cli v0.2.27
// (github.com/RevylAI/revyl-cli, tag v0.2.27, commit 35b06ed8). Paths in the
// comments are into that tree. The device-proxy input in revylrest.go, the
// workflow_run_id it needs from `device start` and `device info`, and
// `device tap` were checked against a live account on 2026-11-18, with
// v0.2.18.

// CLI runs the revyl binary.
type CLI struct {
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
func (c CLI) Command(ctx context.Context, extra []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Path, args...)
	cmd.Dir = c.Dir
	cmd.Env = append(append(os.Environ(), revylEnv...), extra...)
	return cmd
}

// Run runs revyl and returns its stdout. --json output goes to stdout and
// progress to stderr (cmd/revyl/device.go:264 jsonOrPrint), so stderr is
// only read for the error.
func (c CLI) Run(ctx context.Context, extra []string, args ...string) ([]byte, error) {
	if c.Path == "" {
		return nil, errors.New("revyl: CLI not installed")
	}
	return c.run(c.Command(ctx, extra, args...), args)
}

func (c CLI) run(cmd *exec.Cmd, args []string) ([]byte, error) {
	stdout, stderr, err := capture(cmd)
	if err != nil {
		msg := lastLine(stderr)
		if msg == "" {
			msg = lastLine(string(stdout))
		}
		return stdout, revylError(args, msg, err)
	}
	return stdout, nil
}

// capture runs cmd and returns its stdout and stderr.
func capture(cmd *exec.Cmd) ([]byte, string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.String(), err
}

// revylError names a failed command and what the CLI said, or err when it
// said nothing.
func revylError(args []string, msg string, err error) error {
	if msg == "" {
		return fmt.Errorf("revyl %s: %w", revylVerb(args), err)
	}
	// A session's viewer, worker and WHEP URLs can carry its access, and
	// errors reach the trace and the API.
	msg = revylURL.ReplaceAllString(msg, "<url>")
	return fmt.Errorf("revyl %s: %s", revylVerb(args), clip(msg, 300))
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

// Session is one cloud device session, as `revyl device start|info
// --json` print it: mcp.DeviceSession, internal/mcp/device_session.go:109.
// Only the fields loupe reads are kept.
type Session struct {
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
func (s Session) Ready() bool {
	return s.WHEPURL != nil && *s.WHEPURL != "" && (s.DeviceReady == nil || *s.DeviceReady)
}

// DeviceTarget is one model and OS a session can start on. The struct
// in internal/devicetargets/targets.go:25 has no JSON tags, so the keys are
// the Go field names.
type DeviceTarget struct {
	Model   string `json:"Model"`
	Runtime string `json:"Runtime"`
}

// Targets lists the models and OS versions per platform, from `revyl device
// targets --json` (cmd/revyl/device.go:2708): {"android":[...],"ios":[...]}.
func (c CLI) Targets(ctx context.Context) (map[string][]DeviceTarget, error) {
	out, err := c.Run(ctx, nil, "device", "targets", "--json")
	if err != nil {
		return nil, err
	}
	var t map[string][]DeviceTarget
	if err := json.Unmarshal(out, &t); err != nil {
		return nil, fmt.Errorf("revyl device targets: %w", err)
	}
	return t, nil
}

// Start says what device to start.
type Start struct {
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
func (c CLI) Start(ctx context.Context, s Start) (Session, error) {
	if s.Platform != "android" && s.Platform != "ios" {
		return Session{}, fmt.Errorf("revyl: platform %q, want android or ios", s.Platform)
	}
	if c.Path == "" {
		return Session{}, errors.New("revyl: CLI not installed")
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
	var sess Session
	if json.Unmarshal(out, &sess) != nil || sess.SessionID == "" {
		if err != nil {
			return Session{}, err
		}
		return Session{}, errors.New("revyl device start: no session in its output")
	}
	return sess, err
}

// Info reads one session, from `revyl device info --json`. A session that
// has ended prints {"active":false,...} (cmd/revyl/device.go:3130), which
// is reported as ok == false.
func (c CLI) Info(ctx context.Context, sessionID string) (s Session, ok bool, err error) {
	out, err := c.Run(ctx, nil, "device", "info", "--session-id", sessionID, "--json")
	if err != nil {
		return Session{}, false, err
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return Session{}, false, fmt.Errorf("revyl device info: %w", err)
	}
	return s, s.SessionID != "", nil
}

// List reads the sessions the account has open, from `revyl device list
// --json` (cmd/revyl/device.go:3239).
func (c CLI) List(ctx context.Context) ([]Session, error) {
	out, err := c.Run(ctx, nil, "device", "list", "--json")
	if err != nil {
		return nil, err
	}
	var ss []Session
	if err := json.Unmarshal(out, &ss); err != nil {
		return nil, fmt.Errorf("revyl device list: %w", err)
	}
	return ss, nil
}

// Stop ends a session, from `revyl device stop --json`: {"stopped":true},
// or stopped false with stop_requested true while it winds down
// (cmd/revyl/device.go, deviceStopCmd).
func (c CLI) Stop(ctx context.Context, sessionID string) error {
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

// Auth is `revyl auth status --json` (cmd/revyl/auth.go:629). The
// account's key never appears in it.
type Auth struct {
	Authenticated bool   `json:"authenticated"`
	Email         string `json:"email,omitempty"`
	OrgName       string `json:"org_name,omitempty"`
	AuthMethod    string `json:"auth_method,omitempty"`
	Expired       bool   `json:"expired,omitempty"`
}

// AuthStatus reports whether the CLI has credentials: a login in
// ~/.revyl/credentials.json, or REVYL_API_KEY in loupe's environment.
func (c CLI) AuthStatus(ctx context.Context) (Auth, error) {
	out, err := c.Run(ctx, nil, "auth", "status", "--json")
	var a Auth
	if jerr := json.Unmarshal(out, &a); jerr == nil {
		// Signed out may exit non-zero with the JSON still printed.
		return a, nil
	}
	if err != nil {
		return Auth{}, err
	}
	return Auth{}, errors.New("revyl auth status: unreadable output")
}

// Billing is the part of `revyl billing status --json` loupe reads,
// from billingstatus.Status (internal/billingstatus/status.go:241), which
// cmd/revyl/billing.go:65 runBillingStatus writes. The rest of it (card,
// payment, prepaid balance, spending, and the organization ID in
// add_card_url) is never decoded.
type Billing struct {
	Plan     string `json:"plan"`
	PlanName string `json:"plan_name"`
	// PeriodEnd is RFC 3339, when this period's free minutes reset.
	PeriodEnd string `json:"period_end"`
	// FreeMinutes is null off the Free plan (status.go:410).
	FreeMinutes *BillingMinutes `json:"free_minutes"`
	Rates       []BillingRate   `json:"rates"`
	BillingURL  string          `json:"billing_url"`
}

// BillingMinutes is FreeMinutes, status.go:229. Each platform's
// minutes are null when Revyl does not know its credit rate.
type BillingMinutes struct {
	AndroidLeft  *int `json:"android_left"`
	AndroidTotal *int `json:"android_total"`
	IOSLeft      *int `json:"ios_left"`
	IOSTotal     *int `json:"ios_total"`
}

// BillingRate is one device type's price, Rate at status.go:216:
// platform "android" with device_type "Emulator", "ios" with "Simulator",
// and others loupe does not start.
type BillingRate struct {
	Platform     string   `json:"platform"`
	DeviceType   string   `json:"device_type"`
	USDPerMinute *float64 `json:"usd_per_minute"`
}

var (
	// ErrUnsupported is a CLI older than the command: cobra's
	// `unknown command "billing" for "revyl"`.
	ErrUnsupported = errors.New("revyl: the CLI has no such command")
	// ErrSignedOut is getAPIKey's refusal with no credentials
	// (cmd/revyl/cancel.go:252): "Error: not authenticated".
	ErrSignedOut = errors.New("revyl: not signed in")

	revylUnknownCommand = regexp.MustCompile(`unknown command "`)
	revylNoAuth         = regexp.MustCompile(`(?i)\bnot authenticated\b`)
	revylErrorLine      = regexp.MustCompile(`(?m)^Error: (.+)$`)
)

// BillingStatus reads the plan, free minutes and rates, from `revyl
// billing status --json` (cmd/revyl/billing.go:33). A failure is read from
// the whole of stderr rather than its last line: the CLI prints its agent
// hints (cmd/revyl/feedback_hint.go:149) after cobra's "Error:" line. The
// output, the signed-out stderr and the form of cobra's unknown-command
// stderr were checked against a live account on 2026-11-19, with v0.2.18.
func (c CLI) BillingStatus(ctx context.Context) (Billing, error) {
	if c.Path == "" {
		return Billing{}, errors.New("revyl: CLI not installed")
	}
	args := []string{"billing", "status", "--json"}
	out, stderr, err := capture(c.Command(ctx, nil, args...))
	if err != nil {
		switch {
		case revylUnknownCommand.MatchString(stderr):
			return Billing{}, ErrUnsupported
		case revylNoAuth.MatchString(stderr):
			return Billing{}, ErrSignedOut
		}
		msg := lastLine(stderr)
		if m := revylErrorLine.FindAllStringSubmatch(stderr, -1); m != nil {
			msg = strings.TrimSpace(m[len(m)-1][1])
		}
		return Billing{}, revylError(args, msg, err)
	}
	var b Billing
	if err := json.Unmarshal(out, &b); err != nil {
		return Billing{}, fmt.Errorf("revyl billing status: %w", err)
	}
	return b, nil
}

// Target is one Revyl session as a loupe target.
type Target struct {
	CLI     CLI
	Session Session
	// Model and OSVersion are what was asked for at start; Revyl's session
	// does not echo them back. Either may be empty for an adopted session.
	Model, OSVersion string
	// WHEPHeader is sent with the WHEP offer. Revyl does not document
	// whether its whep_url needs one; nil sends none.
	WHEPHeader http.Header
	// REST sends input over Revyl's device proxy. Nil sends every gesture
	// through the CLI.
	REST *REST
	// Trace gets one record per gesture: which way it went and how long it
	// took. Nil drops them.
	Trace func(map[string]any)

	settings *webrtc.SettingEngine // tests: loopback candidates
	poll     time.Duration         // tests: how often Open re-reads info

	// streams are the open streams, oldest first. An agent's input goes
	// through the newest, so it queues behind a viewer's gestures instead
	// of racing them on a second queue.
	mu      sync.Mutex
	streams []*revylStream
}

var (
	_ target.Actuator    = (*Target)(nil)
	_ target.ScreenSizer = (*Target)(nil)
)

// errRevylNoStream is an agent's input with no stream to send it through.
var errRevylNoStream = android.NoSessionError("revyl: open this device in a viewer first; loupe sends a Revyl device's input through the viewer's stream")

func (t *Target) ID() string        { return "revyl:" + t.Session.SessionID }
func (t *Target) Kind() target.Kind { return target.KindRevyl }

func (t *Target) Label() string {
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
func (t *Target) Describe(context.Context) (target.Description, error) {
	d := target.Description{OS: t.Session.Platform, OSVersion: t.OSVersion, Form: "phone", Transport: "cloud"}
	if strings.Contains(strings.ToLower(t.Model), "tablet") || strings.Contains(t.Model, "iPad") {
		d.Form = "tablet"
	}
	return d, nil
}

// Open waits for the session to stream, then connects to its WHEP URL.
func (t *Target) Open(ctx context.Context, _ target.Options) (target.Stream, error) {
	s, err := t.readySession(ctx)
	if err != nil {
		return nil, err
	}
	rcv, err := dialWHEP(ctx, WHEPConfig{
		URL: *s.WHEPURL, Header: t.WHEPHeader, Name: t.Label(), settings: t.settings,
		Trace: t.inputTrace(),
	})
	if err != nil {
		return nil, err
	}
	// t.Session is not updated: listing reads it from other goroutines, and
	// the stream keeps s itself.
	st := &revylStream{whepReceiver: rcv, in: newRevylInput(t.CLI, t.REST, s, t.inputTrace()), owner: t}
	t.mu.Lock()
	t.streams = append(t.streams, st)
	t.mu.Unlock()
	return st, nil
}

// forget drops a stream that is closing, so agent input stops going to it.
func (t *Target) forget(st *revylStream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, s := range t.streams {
		if s == st {
			t.streams = slices.Delete(t.streams, i, i+1)
			return
		}
	}
}

// liveStream is the newest open stream, or nil.
func (t *Target) liveStream() *revylStream {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.streams); n > 0 {
		return t.streams[n-1]
	}
	return nil
}

func (t *Target) agentStream() (*revylStream, error) {
	if st := t.liveStream(); st != nil {
		return st, nil
	}
	return nil, errRevylNoStream
}

// Tap, Swipe, TypeText and PressKey are an agent's input. They go through
// the open stream's queue, in order with a viewer's gestures, and return
// what Revyl answered. Coordinates are stream pixels, mapped the same way
// as a viewer's touches. With no stream open they return ErrNoSession.
func (t *Target) Tap(ctx context.Context, x, y int) error {
	st, err := t.agentStream()
	if err != nil {
		return err
	}
	return st.in.agentTap(ctx, clamp32(x), clamp32(y), st.Meta())
}

// Swipe sends a drag. ms is ignored: Revyl's drag takes no duration (the
// device proxy's drag body and `revyl device drag` have only the two
// points), and its swipe, which has one, takes a direction instead of an
// end point.
func (t *Target) Swipe(ctx context.Context, x1, y1, x2, y2, _ int) error {
	st, err := t.agentStream()
	if err != nil {
		return err
	}
	return st.in.agentSwipe(ctx, clamp32(x1), clamp32(y1), clamp32(x2), clamp32(y2), st.Meta())
}

// TypeText types at the last tap, an agent's or a viewer's, since Revyl
// types at a point. Before any tap it returns ErrNeedsTap.
func (t *Target) TypeText(ctx context.Context, s string) error {
	st, err := t.agentStream()
	if err != nil {
		return err
	}
	return st.in.agentText(ctx, s)
}

func (t *Target) PressKey(ctx context.Context, keycode int) error {
	st, err := t.agentStream()
	if err != nil {
		return err
	}
	return st.in.agentKey(ctx, keycode)
}

// ScreenSize is the open stream's size: an agent's coordinates are stream
// pixels, so this is the space its taps are placed in.
func (t *Target) ScreenSize(context.Context) (int, int, error) {
	st, err := t.agentStream()
	if err != nil {
		return 0, 0, err
	}
	m := st.Meta()
	if m.Width <= 0 || m.Height <= 0 {
		return 0, 0, errRevylNoSize
	}
	return m.Width, m.Height, nil
}

func clamp32(v int) int32 {
	return int32(min(max(v, math.MinInt32), math.MaxInt32))
}

func (t *Target) inputTrace() func(map[string]any) {
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
func (t *Target) readySession(ctx context.Context) (Session, error) {
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
			return Session{}, err
		case !ok:
			return Session{}, errors.New("revyl: the session has ended")
		case s.Ready():
			return s, nil
		}
		select {
		case <-ctx.Done():
			return Session{}, errors.New("revyl: the device has not started streaming")
		case <-time.After(poll):
		}
	}
}

// revylStream is a WHEP video stream with Revyl gestures for input.
type revylStream struct {
	*whepReceiver
	in    *revylInput
	owner *Target // forgets the stream on Close
}

func (s *revylStream) Touch(t target.Touch) error { return s.in.touch(t, s.Meta()) }
func (s *revylStream) Scroll(target.Scroll) error { return target.ErrNoControl }
func (s *revylStream) Key(k target.Key) error     { return s.in.key(k) }
func (s *revylStream) Text(text string) error     { return s.in.text(text) }
func (s *revylStream) Close() error {
	// Forgotten first, so no agent input is queued on an input closing.
	if s.owner != nil {
		s.owner.forget(s)
	}
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
// On Android, a viewer's press goes to the device as it happens, over the
// worker websocket Revyl's own viewer uses (revyllive.go), so a drag moves
// the screen while the finger is down. Otherwise, and whenever that socket
// is not open, a press is buffered from Down to Up and sent as one tap,
// long-press or drag when it ends: the drag shows on the device only after
// the finger lifts. Only the first pointer is tracked; a second finger is
// ignored.
//
// Each gesture is a network round trip, so gestures queue for one worker
// that sends them one at a time, in order, and the input methods return at
// once instead of holding up the viewer that sent them. The worker sends
// over the device proxy when it can and through the CLI otherwise. A live
// press is one job too: the worker stays on it from the down to the lift,
// so nothing else reaches the device mid-press. An agent's gesture (the
// agent* methods) joins the same queue as a finished gesture and waits for
// Revyl's answer, which the worker gives back on the job's done channel.
type revylInput struct {
	cli      CLI
	rest     *REST  // nil: the CLI sends everything
	runID    string // the session's workflow_run_id; "" leaves REST out
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

	// The worker socket for live presses. lmu is taken alone, or after
	// mu.
	lmu        sync.Mutex
	live       *revylLiveConn
	dialing    bool
	liveClosed bool
	backoff    time.Duration
	retryAt    time.Time
	liveErr    string // why the last try at the socket failed, for the trace
	// liveIdle lets go of a press nothing has been heard of for this long;
	// revylLiveIdle, shorter in tests.
	liveIdle time.Duration
	dials    sync.WaitGroup
}

type revylPress struct {
	id     uint64
	x0, y0 int32
	x, y   int32
	at     time.Time
	live   *revylLivePress // nil: sent as one gesture at the lift
	// notLive is why an open socket did not take the press, for the
	// trace.
	notLive string
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
	// done, for an agent's gesture, gets its one result: from the worker
	// once Revyl answered, or from close if it never went. It has room for
	// that result, so neither waits on a caller that gave up. Nil for a
	// viewer's gesture, which nobody waits on.
	done chan error
	// live is a viewer's press sent as it happens, action "live".
	live *revylLivePress
	// notLive is why a viewer's press went as one gesture with the socket
	// open, for the trace.
	notLive string
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

func newRevylInput(cli CLI, rest *REST, s Session, trace func(map[string]any)) *revylInput {
	ctx, cancel := context.WithCancel(context.Background())
	in := &revylInput{
		cli: cli, rest: rest, runID: s.WorkflowRunID, session: s.SessionID, platform: s.Platform,
		screenW: s.ScreenWidth, screenH: s.ScreenHeight, trace: trace,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), stopped: make(chan struct{}),
		now: time.Now, liveIdle: revylLiveIdle,
	}
	go in.work()
	in.liveFor() // open the socket now, so the first press can go live
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
	if j.live != nil {
		in.runLive(j)
		return
	}
	rec := map[string]any{"action": "input", "gesture": j.action}
	if j.done == nil {
		why := j.notLive
		if why == "" {
			why = in.noLive()
		}
		if why != "" {
			rec["live"] = why
		}
	}
	in.finish(j, rec)
}

// finish sends a finished gesture over the device proxy or the CLI.
func (in *revylInput) finish(j *revylJob, rec map[string]any) {
	done, err := in.sendREST(j, rec)
	if !done {
		env, args := j.argv()
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(in.ctx, 30*time.Second)
		_, err = in.cli.Run(ctx, env, append(args, "--session-id", in.session)...)
		cancel()
		rec["via"], rec["ms"] = "cli", time.Since(t0).Milliseconds()
	}
	in.done(j, rec, err)
}

// done records a gesture's result and hands it to an agent waiting on it.
// j is nil for a live press's frames.
func (in *revylInput) done(j *revylJob, rec map[string]any, err error) {
	if err != nil {
		if j != nil {
			err = revylHideText(err, j.text)
		}
		rec["error"] = err.Error()
	}
	in.mu.Lock()
	in.lastErr = err
	in.mu.Unlock()
	if in.trace != nil {
		in.trace(rec)
	}
	if j != nil && j.done != nil {
		if err != nil && in.ctx.Err() != nil {
			err = errRevylClosedInFlight
		}
		j.done <- err
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

// Once the stream closes there is nothing to send an agent's gesture
// through, which /api/input answers as having no session.
var (
	errRevylClosed         = android.NoSessionError("revyl: the viewer's stream closed before the gesture was sent; open the device in a viewer again")
	errRevylClosedInFlight = android.NoSessionError("revyl: the viewer's stream closed while the gesture was in flight, so it may have landed; open the device in a viewer again")
)

func (in *revylInput) close() {
	in.once.Do(func() {
		in.qmu.Lock()
		dropped := in.queue
		in.closed, in.queue = true, nil
		in.qmu.Unlock()
		for _, j := range dropped {
			if j.done != nil {
				j.done <- errRevylClosed
			}
		}
		in.lmu.Lock()
		in.liveClosed = true
		in.lmu.Unlock()
		in.cancel()
	})
	<-in.stopped
	in.closeLive()
}

// enqueue queues a gesture against this session. A viewer's typing joins
// its text still waiting at the same point, so a burst of keystrokes is
// one input call. A viewer's text is never refused for a full queue: a
// dropped character is a wrong word nobody is told about, while a dropped
// tap is visible. Past the limit nothing else is queued, so later text
// joins the last job and the queue grows by one at most.
//
// An agent's text is its own job, joined to nothing and joined by
// nothing, so its result is its own. It is refused for a full queue like
// any other gesture: the agent is told, so nothing is lost unseen.
func (in *revylInput) enqueue(j *revylJob) error {
	in.qmu.Lock()
	defer in.qmu.Unlock()
	if in.closed {
		return errRevylClosed
	}
	if j.action == "input" && j.done == nil {
		if n := len(in.queue); n > 0 {
			if t := in.queue[n-1]; t.action == "input" && t.done == nil && t.x == j.x && t.y == j.y {
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

var errRevylNoSize = errors.New("revyl: stream size not known yet")

// toDevice maps a stream pixel to Revyl's device units: pixels on Android,
// points on iOS (the flags' help text, cmd/revyl/device.go:4014).
func (in *revylInput) toDevice(x, y int32, m target.Meta) (int, int, error) {
	if m.Width <= 0 || m.Height <= 0 {
		return 0, 0, errRevylNoSize
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
	dx, dy := scaleTo(x, y, m, sw, sh)
	return dx, dy, nil
}

// scaleTo maps a pixel of a stream of a known size onto a sw x sh screen,
// turned with the stream.
func scaleTo(x, y int32, m target.Meta, sw, sh int) (int, int) {
	sw, sh = turnedTo(m, sw, sh)
	dx := int(math.Round(float64(x) * float64(sw) / float64(m.Width)))
	dy := int(math.Round(float64(y) * float64(sh) / float64(m.Height)))
	return min(max(dx, 0), sw-1), min(max(dy, 0), sh-1)
}

// turnedTo turns a sw x sh screen to the stream's orientation. The device
// rotated since the session started when the two differ: the stream is the
// screen turned, and so are its units.
func turnedTo(m target.Meta, sw, sh int) (int, int) {
	if (m.Width > m.Height) != (sw > sh) {
		return sh, sw
	}
	return sw, sh
}

func (in *revylInput) touch(t target.Touch, m target.Meta) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	switch t.Action {
	case target.TouchDown:
		if in.press != nil {
			return nil // a second finger: multi-touch is not supported
		}
		in.press = &revylPress{id: t.PointerID, x0: t.X, y0: t.Y, x: t.X, y: t.Y, at: in.now()}
		in.goLive(in.press, m)
		return nil
	case target.TouchMove:
		if p := in.press; p != nil && p.id == t.PointerID {
			p.x, p.y = t.X, t.Y
			if lp := p.live; lp != nil {
				if pt, err := in.livePoint(lp.w, lp.h, t.X, t.Y, m); err == nil {
					lp.move(pt)
				}
			}
		}
		return nil
	case target.TouchCancel:
		if p := in.press; p != nil && p.id == t.PointerID {
			in.press = nil
			if p.live != nil {
				p.live.cancel(in.now().Sub(p.at))
			}
		}
		return nil
	case target.TouchUp:
	default:
		return nil
	}
	p := in.press
	if p == nil || p.id != t.PointerID {
		return nil
	}
	in.press = nil
	p.x, p.y = t.X, t.Y
	held := in.now().Sub(p.at)
	j, err := in.lifted(p, m, held)
	if p.live != nil {
		if err != nil {
			p.live.cancel(held)
			return err
		}
		pt, err := in.livePoint(p.live.w, p.live.h, p.x, p.y, m)
		if err != nil {
			p.live.cancel(held)
			return err
		}
		gesture := j.action
		if gesture == "longpress" {
			gesture = "long_press"
		}
		p.live.lift(pt, gesture, held, j)
		return nil
	}
	if err != nil {
		return err
	}
	j.notLive = p.notLive
	return in.enqueue(j)
}

// lifted is a press that ended as one gesture: a drag when it moved past
// the slop, else a long-press or a tap, which is where typing goes next.
func (in *revylInput) lifted(p *revylPress, m target.Meta, held time.Duration) (*revylJob, error) {
	x0, y0, err := in.toDevice(p.x0, p.y0, m)
	if err != nil {
		return nil, err
	}
	slop := max(6, min(m.Width, m.Height)/50)
	moved := math.Hypot(float64(p.x-p.x0), float64(p.y-p.y0))
	if moved >= float64(slop) {
		x1, y1, err := in.toDevice(p.x, p.y, m)
		if err != nil {
			return nil, err
		}
		return &revylJob{action: "drag", x: x0, y: y0, x1: x1, y1: y1}, nil
	}
	in.tapX, in.tapY, in.haveTap = x0, y0, true
	if held >= revylLongPress {
		return &revylJob{action: "longpress", x: x0, y: y0, ms: int(held / time.Millisecond)}, nil
	}
	return &revylJob{action: "tap", x: x0, y: y0}, nil
}

// goLive queues a press to be sent as it happens, when the worker socket
// is open and the stream is the shape of the screen the worker reports.
// Otherwise the press goes as one gesture at the lift.
func (in *revylInput) goLive(p *revylPress, m target.Meta) {
	lc := in.liveFor()
	if lc == nil {
		return
	}
	// The press keeps this size to its lift, whatever the socket reports
	// meanwhile.
	w, h := lc.size()
	pt, err := in.livePoint(w, h, p.x0, p.y0, m)
	if err != nil {
		return
	}
	if w > 0 && h > 0 && !revylSameShape(m, w, h) {
		// A stream of another shape may be cropped or padded, and a touch
		// scaled onto the screen would land off where the viewer pressed.
		p.notLive = fmt.Sprintf("revyl live touch: the stream is %dx%d, not the shape of the %dx%d screen the worker reports",
			m.Width, m.Height, w, h)
		return
	}
	lp := &revylLivePress{w: w, h: h, p0: pt, p: pt, start: p.at, heard: time.Now(), nudge: make(chan struct{}, 1)}
	if in.enqueue(&revylJob{action: "live", live: lp}) == nil {
		p.live = lp
	}
}

// key sends a key on its down event; the up is ignored, since Revyl takes
// a whole press. Phones take the keys ENTER and BACKSPACE only
// (internal/mcp/device_web_actions.go:15 ParseDeviceKey, the same names
// on Android and iOS), and back is Android's.
func (in *revylInput) key(k target.Key) error {
	if k.Action != target.KeyDown {
		return nil
	}
	j, err := in.keyJob(int(k.Keycode))
	if err != nil {
		return err
	}
	return in.enqueue(j)
}

// keyJob is the gesture for an Android keycode, or an error matching
// ErrNoControl for one Revyl has no gesture for.
func (in *revylInput) keyJob(keycode int) (*revylJob, error) {
	switch keycode {
	case target.KeycodeHome:
		return &revylJob{action: "go_home"}, nil
	case target.KeycodeBack:
		if in.platform == "android" {
			return &revylJob{action: "back"}, nil
		}
	case keycodeEnter:
		return &revylJob{action: "key", key: "ENTER"}, nil
	case keycodeDel:
		return &revylJob{action: "key", key: "BACKSPACE"}, nil
	}
	takes := "home (3), enter (66) and delete (67)"
	if in.platform == "android" {
		takes = "home (3), back (4), enter (66) and delete (67)"
	}
	return nil, revylNoKey(fmt.Sprintf("revyl: keycode %d has no Revyl gesture on %s; it takes %s", keycode, in.platform, takes))
}

// revylNoKey is a key Revyl cannot press, in words that say which it can.
// errors.Is matches it to ErrNoControl.
type revylNoKey string

func (e revylNoKey) Error() string     { return string(e) }
func (e revylNoKey) Is(err error) bool { return err == target.ErrNoControl }

// ErrNeedsTap is typing on a Revyl device before anything was
// tapped: Revyl types at a point, and the last tap is the field.
var ErrNeedsTap = errors.New("revyl: tap a text field first; Revyl types at a point")

// text types at the last tap, the field the user picked: Revyl types at a
// point.
func (in *revylInput) text(s string) error {
	if s == "" {
		return nil
	}
	x, y, err := in.typingPoint()
	if err != nil {
		return err
	}
	return in.enqueue(&revylJob{action: "input", x: x, y: y, text: s})
}

func (in *revylInput) typingPoint() (int, int, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.haveTap {
		return 0, 0, ErrNeedsTap
	}
	return in.tapX, in.tapY, nil
}

// agentTap taps a stream pixel and waits for Revyl's answer. Like a
// viewer's tap, it is where typing goes next.
func (in *revylInput) agentTap(ctx context.Context, x, y int32, m target.Meta) error {
	dx, dy, err := in.toDevice(x, y, m)
	if err != nil {
		return err
	}
	j := &revylJob{action: "tap", x: dx, y: dy, done: make(chan error, 1)}
	if err := in.enqueue(j); err != nil {
		return err
	}
	in.mu.Lock()
	in.tapX, in.tapY, in.haveTap = dx, dy, true
	in.mu.Unlock()
	return in.wait(ctx, j)
}

func (in *revylInput) agentSwipe(ctx context.Context, x0, y0, x1, y1 int32, m target.Meta) error {
	dx0, dy0, err := in.toDevice(x0, y0, m)
	if err != nil {
		return err
	}
	dx1, dy1, err := in.toDevice(x1, y1, m)
	if err != nil {
		return err
	}
	return in.send(ctx, &revylJob{action: "drag", x: dx0, y: dy0, x1: dx1, y1: dy1})
}

func (in *revylInput) agentText(ctx context.Context, s string) error {
	if s == "" {
		return nil
	}
	x, y, err := in.typingPoint()
	if err != nil {
		return err
	}
	return in.send(ctx, &revylJob{action: "input", x: x, y: y, text: s})
}

func (in *revylInput) agentKey(ctx context.Context, keycode int) error {
	j, err := in.keyJob(keycode)
	if err != nil {
		return err
	}
	return in.send(ctx, j)
}

// send queues an agent's gesture and waits for its result.
func (in *revylInput) send(ctx context.Context, j *revylJob) error {
	j.done = make(chan error, 1)
	if err := in.enqueue(j); err != nil {
		return err
	}
	return in.wait(ctx, j)
}

// wait waits for a queued gesture's result. A caller that gives up first
// takes its gesture out of the queue if it is still there, so it cannot
// land after the caller was told it failed and tried again.
func (in *revylInput) wait(ctx context.Context, j *revylJob) error {
	select {
	case err := <-j.done:
		return err
	case <-ctx.Done():
	}
	if in.unqueue(j) {
		return fmt.Errorf("revyl: %s not sent: %w", j.action, ctx.Err())
	}
	select {
	case err := <-j.done:
		return err
	default:
	}
	return fmt.Errorf("revyl: stopped waiting for %s, which was already sent and may still land: %w", j.action, ctx.Err())
}

// unqueue takes j out of the queue, reporting whether it was there.
func (in *revylInput) unqueue(j *revylJob) bool {
	in.qmu.Lock()
	defer in.qmu.Unlock()
	for i, q := range in.queue {
		if q == j {
			in.queue = slices.Delete(in.queue, i, i+1)
			return true
		}
	}
	return false
}

// Err returns the last gesture's error, for tests and diagnostics.
func (in *revylInput) err() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.lastErr
}
