package target

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// simInput drives one simulator's input through `simcap <udid> -input`, which
// injects touches, buttons and keys over SimulatorKit's HID client. Every
// command gets one reply line, `ok` or `err <why>`, so a tap that failed is an
// error here rather than a silence.
//
// It is one process per simulator, apart from the video one, so the stream
// and /api/input share a single HID client, input works with no session open,
// and a video relaunch never takes input down with it. The process is started
// on first use and stopped after simInputIdle without a command; it starts in
// a few hundred milliseconds, so there is little to gain from keeping it.
type simInput struct {
	bin, udid string
	// mode is simcap's flag, -input for a simulator and -phone-input for a
	// physical iPhone, whose udid is then CoreDevice's identifier.
	mode string
	// budget replaces simInputBudget when set, for a process whose first
	// command opens something slower than a simulator's HID client.
	budget time.Duration

	mu   sync.Mutex // one command in flight
	proc *simInputProc
	// gone closes when the last stopped helper has exited. A new helper is
	// not started before then, so two never drive one device at once.
	gone <-chan struct{}
	idle *time.Timer

	// viaHub is set once simcap has opened CoreDevice's HID capabilities,
	// the path Device Hub itself uses. Input sent that way reaches the
	// simulator whether or not Device Hub holds it, so nothing needs
	// taking back. It is read from simcap's `route` reply each time the
	// process starts, and stays false when CoreDevice cannot be opened.
	viaHub atomic.Bool
}

type simInputProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr *tailBuffer
}

const simInputIdle = 2 * time.Minute

// simInputBudget covers the HID client coming up plus one command. A send
// completes in 1-3ms, so this is only reached when simcap is stuck.
const simInputBudget = 5 * time.Second

// simInputRouteBudget is how long a fresh process gets to answer `route`. On a
// simulator the answer waits for simcap's CoreDevice open (HubHID.open, 5s),
// and that clock starts only after the process has started and read the
// command, so it ends later than 5s after our write. With simInputBudget here
// Go stopped simcap just before its `ok legacy` arrived, and every command
// failed in the cases that used to fall back to the legacy client. CoreDevice
// lookups sometimes never return (CLAUDE.md, iOS simulator targets), so this
// needs room for the full open plus startup. A var so tests can shrink it.
var simInputRouteBudget = 10 * time.Second

// errSimInputTimeout wraps a command that got no reply in its budget.
// errUnreachableReply is an `err unreachable: ...` reply, which a phone's
// simcap sends when CoreDevice failed before anything was sent, so the command
// is safe to send again. errUndeliveredReply is `err undelivered: ...`: a send
// went out and then something failed, so the phone may have it. A caller that
// resends on that one performs the action twice.
var (
	errSimInputTimeout  = errors.New("no reply")
	errUnreachableReply = errors.New("unreachable")
	errUndeliveredReply = errors.New("undelivered")

	// errSimInputRouteTimeout is a fresh process that did not answer `route`
	// in its budget. Nothing was sent, and the process is gone.
	errSimInputRouteTimeout = errors.New("no reply to route")

	// errSimcapExited is simcap closing its output with a command written
	// and unanswered.
	errSimcapExited = errors.New("simcap exited")
)

// simInputFailure is a command that failed in simInput.do rather than being
// answered with an `err` line. sent says the command line had been written to
// simcap's stdin, so its effect is unknown; without it nothing was sent. line
// and stderr are in Error() for the log but are not part of cause, because
// line is the command itself, which for a typed character names the key.
type simInputFailure struct {
	line, stderr string
	cause        error
	sent         bool
}

func (f *simInputFailure) Error() string {
	return fmt.Sprintf("simcap input %q: %v%s", f.line, f.cause, f.stderr)
}

func (f *simInputFailure) Unwrap() error { return f.cause }

// sentUnanswered reports whether err is a failure after the command was
// written, which is an action that may have happened.
func sentUnanswered(err error) bool {
	var f *simInputFailure
	return errors.As(err, &f) && f.sent
}

// failureCause is err without the command line and simcap's stderr, for a
// caller that must not carry what was typed.
func failureCause(err error) error {
	var f *simInputFailure
	if errors.As(err, &f) {
		return f.cause
	}
	return err
}

// routeResult is what a fresh process said about its route.
type routeResult int

const (
	routeLegacy  routeResult = iota // an old simcap's err, a write error, or a closed pipe
	routeHub                        // ok hub
	routeTimeout                    // no answer in the budget
)

// base is the time a command gets before its own duration is added.
func (in *simInput) base() time.Duration {
	if in.budget > 0 {
		return in.budget
	}
	return simInputBudget
}

func (in *simInput) start() (*simInputProc, error) {
	cmd := exec.Command(in.bin, in.udid, cmp.Or(in.mode, "-input"))
	p := &simInputProc{cmd: cmd, lines: make(chan string, 4), stderr: simcapStderr(in.udid)}
	cmd.Stderr = p.stderr
	var err error
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start simcap %s: %w", in.bin, err)
	}
	raw := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(out)
		// A `ui` reply is a whole accessibility tree on one line; Settings'
		// root screen is 3KB, and a long list is far more.
		sc.Buffer(make([]byte, 64*1024), 8<<20)
		for sc.Scan() {
			raw <- sc.Text()
		}
		close(raw)
		_ = cmd.Wait()
	}()
	go assembleReplies(raw, p.lines)
	return p, nil
}

// simInputStopGrace is how long a stopped helper has to exit on its own
// before it is killed.
const simInputStopGrace = time.Second

// simInputLiftGrace is added to a command's own duration when a failed
// command's helper is stopped: the 2s capability check after a swipe's
// touch-up, and a retried lift.
const simInputLiftGrace = 3 * time.Second

// simInputContinuationGrace is how long an `err` reply waits for lines that
// continue it before it is handed on. A var so tests can widen it.
var simInputContinuationGrace = 25 * time.Millisecond

// isReplyLine reports whether line begins a reply: `ok` or `err`, alone or
// followed by a space.
func isReplyLine(line string) bool {
	for _, w := range []string{"ok", "err"} {
		if line == w || strings.HasPrefix(line, w+" ") {
			return true
		}
	}
	return false
}

// assembleReplies turns simcap's output lines into one string per reply. Real
// simcap writes each reply on one line, but a CoreDevice error can carry a
// newline from some other build, and a line that is not a reply would then be
// read as the next command's and shift every reply after it by one. So only a
// line starting `ok` or `err` begins a reply; any other line is folded, joined
// by a space, into an `err` reply that is still waiting (grace after its first
// line), and is dropped if none is: it is never counted as a reply of its own.
// A continuation that itself starts with `ok` or `err` cannot be told from a
// reply. in closing flushes what is held and closes out.
func assembleReplies(in <-chan string, out chan<- string) {
	defer close(out)
	var pending string
	holding := false
	flush := func() {
		if holding {
			out <- pending
			holding = false
		}
	}
	for {
		var grace <-chan time.Time
		if holding {
			grace = time.After(simInputContinuationGrace)
		}
		select {
		case line, ok := <-in:
			if !ok {
				flush()
				return
			}
			switch {
			case isReplyLine(line):
				flush()
				if line == "err" || strings.HasPrefix(line, "err ") {
					pending, holding = line, true
				} else {
					out <- line
				}
			case holding:
				pending += " " + line
			}
		case <-grace:
			flush()
		}
	}
}

// route asks a fresh process which client it sends buttons, keys and touch
// through: "hub" for CoreDevice, "legacy" for SimulatorKit's HID client. An
// older simcap answers `err`, and anything but "hub" is treated as legacy,
// which is what every simcap did before CoreDevice. A process that does not
// answer at all is routeTimeout, not legacy: its late `ok hub` would answer
// the next command and shift every later reply by one.
func (p *simInputProc) route(budget time.Duration) routeResult {
	if _, err := io.WriteString(p.stdin, "route\n"); err != nil {
		return routeLegacy
	}
	select {
	case reply, ok := <-p.lines:
		if !ok {
			return routeLegacy
		}
		f := strings.Fields(reply)
		if len(f) >= 2 && f[0] == "ok" && f[1] == "hub" {
			return routeHub
		}
		return routeLegacy
	case <-time.After(budget):
		return routeTimeout
	}
}

// stop ends the process by closing its stdin, which simcap exits on, and
// kills it if it is still running after grace. It returns at once with a
// channel that closes when the process's output has ended. simcap reads stdin
// between commands, so a command already playing (a slow swipe) finishes,
// lifts and replies before it sees the close.
func (p *simInputProc) stop(grace time.Duration) <-chan struct{} {
	_ = p.stdin.Close()
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		t := time.AfterFunc(grace, func() { _ = p.cmd.Process.Kill() })
		for range p.lines {
		}
		t.Stop()
	}()
	return gone
}

// do sends one command and returns what followed `ok`. budget is added to
// base() (simInputBudget, or the phone's longer one) for commands that take
// time on purpose, like a slow swipe. A fresh process is first asked `route`,
// which has its own simInputRouteBudget; if that goes unanswered the process
// is stopped and nothing is sent. After the command is written, a missing
// reply, a context that ends, or simcap exiting is a *simInputFailure with
// sent set, since the action may have happened.
func (in *simInput) do(ctx context.Context, budget time.Duration, format string, a ...any) (string, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.proc == nil {
		if in.gone != nil {
			select {
			case <-in.gone:
				in.gone = nil
			case <-ctx.Done():
				return "", &simInputFailure{
					line:  fmt.Sprintf(format, a...),
					cause: fmt.Errorf("waiting for the previous helper to exit: %w", ctx.Err()),
				}
			}
		}
		p, err := in.start()
		if err != nil {
			return "", err
		}
		switch p.route(simInputRouteBudget) {
		case routeTimeout:
			in.gone = p.stop(simInputStopGrace)
			return "", &simInputFailure{
				line:   fmt.Sprintf(format, a...),
				stderr: p.stderr.suffix(),
				cause:  fmt.Errorf("%w in %s", errSimInputRouteTimeout, simInputRouteBudget),
			}
		case routeHub:
			in.viaHub.Store(true)
		default:
			in.viaHub.Store(false)
		}
		in.proc = p
	}
	if in.idle != nil {
		in.idle.Stop()
	}
	in.idle = time.AfterFunc(simInputIdle, in.close)

	p := in.proc
	line := fmt.Sprintf(format, a...)
	fail := func(err error, sent bool) (string, error) {
		// The command may still be playing, a swipe's lift included, so the
		// helper gets its duration and the 2s capability check plus a
		// retried lift before the kill. The next command waits for it to
		// exit before starting a fresh helper.
		in.gone = p.stop(budget + simInputLiftGrace)
		in.proc = nil
		return "", &simInputFailure{line: line, stderr: p.stderr.suffix(), cause: err, sent: sent}
	}
	if _, err := io.WriteString(p.stdin, line+"\n"); err != nil {
		return fail(err, false)
	}
	select {
	case reply, ok := <-p.lines:
		if !ok {
			return fail(errSimcapExited, true)
		}
		if rest, found := strings.CutPrefix(reply, "err "); found {
			if why, ok := strings.CutPrefix(rest, "unreachable: "); ok {
				return "", fmt.Errorf("%w: %s", errUnreachableReply, why)
			}
			if why, ok := strings.CutPrefix(rest, "undelivered: "); ok {
				return "", fmt.Errorf("%w: %s", errUndeliveredReply, why)
			}
			return "", errors.New(rest)
		}
		if reply == "ok" {
			return "", nil
		}
		if rest, found := strings.CutPrefix(reply, "ok "); found {
			return rest, nil
		}
		return fail(fmt.Errorf("unexpected reply %q", reply), true)
	case <-ctx.Done():
		// The reply is still coming and would answer the next command, so
		// the process cannot be reused.
		return fail(ctx.Err(), true)
	case <-time.After(in.base() + budget):
		return fail(fmt.Errorf("%w in %s", errSimInputTimeout, in.base()+budget), true)
	}
}

func (in *simInput) close() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.proc != nil {
		in.gone = in.proc.stop(simInputStopGrace)
		in.proc = nil
	}
}

// CheckSimcap confirms the simcap at bin has input mode. A build from before
// it reads -input as nothing and streams video into the reply pipe, and every
// touch would then fail as a garbled reply. Such a build also fails -caps
// outright, taking it for a UDID, which is the answer this relies on.
func CheckSimcap(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-caps").Output()
	if err != nil {
		return fmt.Errorf("%s predates input mode", bin)
	}
	for _, c := range strings.Fields(string(out)) {
		if c == "input" {
			return nil
		}
	}
	return fmt.Errorf("%s has no input mode", bin)
}

// iosKey is what an Android keycode or a typed character becomes on an iOS
// simulator: a named button, or a HID keyboard usage (page 7), with shift
// held for the characters that need it on a US layout.
type iosKey struct {
	button string
	usage  int
	shift  bool
}

// androidToIOS maps the Android keycodes the viewer and /api/input send onto
// iOS. Anything absent has no iOS equivalent -- back above all -- and is an
// error rather than a guess.
var androidToIOS = func() map[int32]iosKey {
	m := map[int32]iosKey{
		3:   {button: "home"},
		26:  {button: "lock"},
		187: {button: "appSwitcher"},
		7:   {usage: 39}, // KEYCODE_0; 1-9 are 30-38
		19:  {usage: 82}, // DPAD_UP
		20:  {usage: 81},
		21:  {usage: 80},
		22:  {usage: 79},
		55:  {usage: 54}, // COMMA
		56:  {usage: 55}, // PERIOD
		57:  {usage: 226},
		59:  {usage: 225}, // SHIFT_LEFT
		60:  {usage: 229},
		61:  {usage: 43}, // TAB
		62:  {usage: 44}, // SPACE
		66:  {usage: 40}, // ENTER
		67:  {usage: 42}, // DEL, which is backspace
		68:  {usage: 53}, // GRAVE
		69:  {usage: 45}, // MINUS
		70:  {usage: 46}, // EQUALS
		71:  {usage: 47},
		72:  {usage: 48},
		73:  {usage: 49}, // BACKSLASH
		74:  {usage: 51}, // SEMICOLON
		75:  {usage: 52}, // APOSTROPHE
		76:  {usage: 56}, // SLASH
		111: {usage: 41}, // ESCAPE
		112: {usage: 76}, // FORWARD_DEL
		113: {usage: 224},
		117: {usage: 227}, // META_LEFT, which is command
	}
	for i := int32(1); i <= 9; i++ {
		m[7+i] = iosKey{usage: int(29 + i)}
	}
	for i := int32(0); i < 26; i++ {
		m[29+i] = iosKey{usage: int(4 + i)} // KEYCODE_A..Z
	}
	return m
}()

// charToIOS maps a typed character to the key that produces it on a US
// layout. The simulator's hardware keyboard is a US one unless someone
// changed it, and nothing reports which layout it has.
var charToIOS = func() map[rune]iosKey {
	m := map[rune]iosKey{'\n': {usage: 40}, '\t': {usage: 43}, ' ': {usage: 44}, '0': {usage: 39}, ')': {usage: 39, shift: true}}
	for i := rune(0); i < 26; i++ {
		m['a'+i] = iosKey{usage: int(4 + i)}
		m['A'+i] = iosKey{usage: int(4 + i), shift: true}
	}
	for i, c := range "123456789" {
		m[c] = iosKey{usage: 30 + i}
	}
	for i, c := range "!@#$%^&*(" {
		m[c] = iosKey{usage: 30 + i, shift: true}
	}
	for _, p := range []struct {
		plain, shifted rune
		usage          int
	}{
		{'-', '_', 45}, {'=', '+', 46}, {'[', '{', 47}, {']', '}', 48}, {'\\', '|', 49},
		{';', ':', 51}, {'\'', '"', 52}, {'`', '~', 53}, {',', '<', 54}, {'.', '>', 55}, {'/', '?', 56},
	} {
		m[p.plain] = iosKey{usage: p.usage}
		m[p.shifted] = iosKey{usage: p.usage, shift: true}
	}
	return m
}()

func (k iosKey) command() string {
	if k.shift {
		return fmt.Sprintf("key %d shift", k.usage)
	}
	return fmt.Sprintf("key %d", k.usage)
}

// input returns the simulator's input process, creating it on first use.
func (s *Simulator) input() *simInput {
	s.inputOnce.Do(func() { s.in = &simInput{bin: s.simcap, udid: s.UDID()} })
	return s.in
}

func (s *Simulator) requireBooted() error {
	if !s.booted.Load() {
		return fmt.Errorf("%s is not booted; open it in the viewer to boot it", s.Label())
	}
	return nil
}

// Tap implements Actuator, in framebuffer pixels -- the space Screenshot and
// the video both use.
func (s *Simulator) Tap(ctx context.Context, x, y int) error {
	if err := s.requireBooted(); err != nil {
		return err
	}
	_, err := s.input().do(ctx, 0, "tap %d %d", x, y)
	return err
}

// Swipe implements Actuator.
func (s *Simulator) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error {
	if err := s.requireBooted(); err != nil {
		return err
	}
	_, err := s.input().do(ctx, time.Duration(ms)*time.Millisecond, "swipe %d %d %d %d %d", x1, y1, x2, y2, ms)
	return err
}

// pinchSpan is the wider of a pinch's two finger spreads, as a fraction of
// the screen's short side. The narrower is that divided by the scale, so
// scale 2 runs 30% to 60% and scale 0.5 the reverse; 60% keeps both fingers
// on an iPhone screen when the centre is.
const pinchSpan = 0.6

// Pinch implements Pincher. It needs the CoreDevice route; the legacy
// client carries one point, and simcap answers that with an error.
func (s *Simulator) Pinch(ctx context.Context, x, y int, scale float64, ms int) error {
	if err := s.requireBooted(); err != nil {
		return err
	}
	w, h, err := s.ScreenSize(ctx)
	if err != nil {
		return err
	}
	wide := pinchSpan * float64(min(w, h))
	narrow := wide * min(scale, 1/scale)
	from, to := narrow, wide
	if scale < 1 {
		from, to = wide, narrow
	}
	_, err = s.input().do(ctx, time.Duration(ms)*time.Millisecond, "pinch %d %d %d %d %d", x, y, int(from), int(to), ms)
	return err
}

// SetHardwareKeyboard implements HardwareKeyboarder. It is a CoreSimulator
// call rather than HID input, so Device Hub holding the simulator's keys does
// not stop it and nothing is reclaimed first. There is no getter: the
// simulator keeps it until someone changes it again.
func (s *Simulator) SetHardwareKeyboard(ctx context.Context, on bool) error {
	if err := s.requireBooted(); err != nil {
		return err
	}
	state := "off"
	if on {
		state = "on"
	}
	_, err := s.input().do(ctx, 0, "hwkeyboard %s", state)
	return err
}

// TypeText implements Actuator one keystroke per character. Every character
// is checked before any is sent, so an unsupported one fails the call rather
// than leaving half the text typed.
func (s *Simulator) TypeText(ctx context.Context, text string) error {
	if err := s.requireBooted(); err != nil {
		return err
	}
	keys := make([]iosKey, 0, len(text))
	for _, r := range text {
		k, ok := charToIOS[r]
		if !ok {
			return fmt.Errorf("cannot type %q on a simulator: only US keyboard characters are mapped", r)
		}
		keys = append(keys, k)
	}
	ctx, err := s.checkHub(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := s.input().do(ctx, 0, "%s", k.command()); err != nil {
			return err
		}
	}
	return nil
}

// PressKey implements Actuator for the Android keycodes that have an iOS
// meaning: home, power and app switch press buttons, and keyboard keys become
// HID usages.
func (s *Simulator) PressKey(ctx context.Context, keycode int) error {
	if err := s.requireBooted(); err != nil {
		return err
	}
	k, ok := androidToIOS[int32(keycode)]
	if !ok {
		return fmt.Errorf("keycode %d has no iOS equivalent", keycode)
	}
	if k.button != "" {
		return s.PressButton(ctx, k.button)
	}
	ctx, err := s.checkHub(ctx)
	if err != nil {
		return err
	}
	_, err = s.input().do(ctx, 0, "%s", k.command())
	return err
}

// iosButtons are the buttons PressButton knows. Siri is absent on purpose:
// baguette reports it crashing backboardd on every path it tried.
var iosButtons = []string{"appSwitcher", "home", "lock"}

// PressButton implements ButtonPresser.
func (s *Simulator) PressButton(ctx context.Context, name string) error {
	if err := s.requireBooted(); err != nil {
		return err
	}
	known := false
	for _, b := range iosButtons {
		known = known || b == name
	}
	if !known {
		return fmt.Errorf("%w %q; want %s", ErrUnknownButton, name, strings.Join(iosButtons, ", "))
	}
	ctx, err := s.checkHub(ctx)
	if err != nil {
		return err
	}
	_, err = s.input().do(ctx, 0, "button %s", name)
	return err
}

// Buttons implements ButtonPresser.
func (s *Simulator) Buttons() []string { return iosButtons }

// litDisplay is the screen ID of the panel simcap streams, or 0 when simcap
// does not know or is too old to answer, which leaves the choice to simctl.
func (s *Simulator) litDisplay(ctx context.Context) int {
	out, err := s.input().do(ctx, 0, "display")
	if err != nil {
		return 0
	}
	var id int
	if _, err := fmt.Sscanf(out, "%d", &id); err != nil {
		return 0
	}
	return id
}

// ScreenSize implements ScreenSizer from the framebuffer simcap reads.
func (s *Simulator) ScreenSize(ctx context.Context) (int, int, error) {
	if err := s.requireBooted(); err != nil {
		return 0, 0, err
	}
	out, err := s.input().do(ctx, 0, "size")
	if err != nil {
		return 0, 0, err
	}
	var w, h int
	if _, err := fmt.Sscanf(out, "%d %d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("simcap size %q", out)
	}
	return w, h, nil
}

// Screenshot implements Screenshotter through simctl, which writes a file:
// `-` as the path names a file called "-" rather than stdout. It took
// 0.47-0.57s on an iPhone 17 simulator.
//
// On a simulator with two panels it names the lit one, as simcap reports it.
// Left to choose, simctl took the iPhone Duo's outer panel while it was
// unfolded and dark.
func (s *Simulator) Screenshot(ctx context.Context) ([]byte, error) {
	if err := s.requireBooted(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "loupe-simshot")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "shot.png")
	args := []string{"simctl", "io", s.UDID(), "screenshot", "--type=png"}
	if id := s.litDisplay(ctx); id > 0 {
		args = append(args, fmt.Sprintf("--display=%d", id))
	}
	if out, err := exec.CommandContext(ctx, "xcrun", append(args, path)...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("simctl screenshot: %v: %s", err, strings.TrimSpace(string(out)))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < len(pngMagic) || string(raw[:len(pngMagic)]) != string(pngMagic) {
		return nil, fmt.Errorf("simctl screenshot did not write a PNG (%d bytes)", len(raw))
	}
	return raw, nil
}

// Device Hub, which replaced Simulator.app in Xcode 27, runs dtuhidd in each
// simulator it sees and sets this flag. While it is set, backboardd takes
// buttons and keys from dtuhidd and ignores simcap's legacy HID client: Home
// was sent twice and accepted both times, and nothing happened. Touch
// survived on that boot, which baguette describes as a boot-order race.
const hubFlag = "com.apple.coredevice.dtuhidd.active"

// ErrHubHoldsInput is returned for buttons and keys while Device Hub holds
// them; Reclaim takes them back.
var ErrHubHoldsInput = errors.New("Device Hub holds this simulator's buttons and keyboard")

func (s *Simulator) hubActive(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "spawn", s.UDID(), "notifyutil", "-g", hubFlag).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("read %s: %v: %s", hubFlag, err, strings.TrimSpace(string(out)))
	}
	f := strings.Fields(string(out))
	return len(f) == 2 && f[1] != "0", nil
}

// checkHub runs before every button and key, since while Device Hub holds
// them the simulator accepts them and does nothing. On a simulator this
// server has claimed, it takes them back and lets the input through; on any
// other it fails with ErrHubHoldsInput, because taking them back closes the
// apps of whoever booted it. The flag is read at most every hubCheckEvery,
// so typing a word does not cost a simctl spawn per letter; a failed read
// lets the input through rather than blocking it.
//
// It returns the context to send the input under. A reclaim can take most of
// a request's deadline, and the press that caused it should still land.
func (s *Simulator) checkHub(ctx context.Context) (context.Context, error) {
	if s.viaHub(ctx) || !s.hubHeldNow() {
		return ctx, nil
	}
	if !s.claimed.Load() {
		return ctx, fmt.Errorf(`%w; POST /api/input {"action":"reclaim"} takes them back, restarting SpringBoard and closing its apps`, ErrHubHoldsInput)
	}
	if err := s.Reclaim(ctx); err != nil {
		return ctx, err
	}
	return context.WithoutCancel(ctx), nil
}

// viaHub reports whether this simulator's input goes through CoreDevice,
// starting the input process to find out if it is not running.
func (s *Simulator) viaHub(ctx context.Context) bool {
	in := s.input()
	if _, err := in.do(ctx, 0, "route"); err != nil {
		return false
	}
	return in.viaHub.Load()
}

func (s *Simulator) hubHeldNow() bool {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if time.Since(s.hubChecked) >= hubCheckEvery {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		active, err := s.hubActive(ctx)
		s.hubChecked = time.Now()
		s.hubHeld = err == nil && active
	}
	return s.hubHeld
}

const hubCheckEvery = 10 * time.Second

// Reclaimer is implemented by a target whose input something else can take
// away and this server can take back. /api/input's "reclaim" action calls it.
type Reclaimer interface {
	Reclaim(ctx context.Context) error
	// LastReclaim reports the most recent reclaim that restarted SpringBoard,
	// so a caller can tell a press that paid for one from a slow press.
	LastReclaim() (ReclaimRecord, bool)
}

// ReclaimRecord is one reclaim: when it finished, how long it took, and the
// app it reopened, "" when the Home Screen was in front.
type ReclaimRecord struct {
	At       time.Time
	Took     time.Duration
	Reopened string
}

func (s *Simulator) LastReclaim() (ReclaimRecord, bool) {
	r := s.lastReclaim.Load()
	if r == nil {
		return ReclaimRecord{}, false
	}
	return *r, true
}

var _ Reclaimer = (*Simulator)(nil)

// Reclaim takes buttons and keys back from Device Hub: clear its flag, then
// restart backboardd so it picks the legacy HID client up again. Nothing
// short of the restart works: clearing the flag, posting it, and killing
// dtuhidd each left Home doing nothing, because backboardd chooses its input
// owner once, at startup. The restart takes SpringBoard and every running
// app with it, so the app that was in front is launched again once
// SpringBoard is back. What that app held in memory is still lost. It holds
// until Device Hub is relaunched.
//
// A successful reclaim claims the simulator, so later holds are taken back
// without asking.
func (s *Simulator) Reclaim(ctx context.Context) error {
	if err := s.requireBooted(); err != nil {
		return err
	}
	// Once backboardd is restarted, stopping early would leave SpringBoard
	// gone and the app unopened, so the caller's deadline does not apply.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reclaimBudget)
	defer cancel()
	s.reclaimMu.Lock()
	defer s.reclaimMu.Unlock()
	// Another press may have reclaimed while this one waited.
	s.hubMu.Lock()
	done := !s.hubHeld && time.Since(s.hubChecked) < time.Second
	s.hubMu.Unlock()
	if done {
		return nil
	}

	start := time.Now()
	udid := s.UDID()
	app := s.frontApp(ctx)
	springboard := s.springBoardPid(ctx)
	for _, args := range [][]string{
		{"simctl", "spawn", udid, "notifyutil", "-s", hubFlag, "0"},
		{"simctl", "spawn", udid, "launchctl", "kickstart", "-k", "system/com.apple.backboardd"},
	} {
		if out, err := exec.CommandContext(ctx, "xcrun", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(args[3:], " "), err, strings.TrimSpace(string(out)))
		}
	}
	// The input process's HID client belonged to the backboardd that just
	// went away.
	s.input().close()
	s.hubMu.Lock()
	s.hubHeld, s.hubChecked = false, time.Now()
	s.hubMu.Unlock()
	s.claimed.Store(true)

	if err := s.awaitSpringBoard(ctx, springboard); err != nil {
		return err
	}
	if app != "" {
		if out, err := exec.CommandContext(ctx, "xcrun", "simctl", "launch", udid, app).CombinedOutput(); err != nil {
			return fmt.Errorf("reopen %s: %v: %s", app, err, strings.TrimSpace(string(out)))
		}
	}
	s.lastReclaim.Store(&ReclaimRecord{At: time.Now(), Took: time.Since(start), Reopened: app})
	return nil
}

// frontApp is the bundle ID of the app in front, or "" on the Home Screen
// or when it cannot be told. simcap names the frontmost process, and the
// simulator's launchd names the app behind it.
func (s *Simulator) frontApp(ctx context.Context) string {
	pid, err := s.input().do(ctx, 0, "front")
	if err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "spawn", s.UDID(), "launchctl", "list").Output()
	if err != nil {
		return ""
	}
	return appForPid(string(out), pid)
}

// appForPid reads `launchctl list` for the app running as pid. Apps are
// listed as UIKitApplication:<bundle>[<hex>][rb-legacy]; anything else,
// SpringBoard included, is not an app to reopen.
func appForPid(list, pid string) string {
	for _, line := range strings.Split(list, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != pid {
			continue
		}
		label, ok := strings.CutPrefix(f[2], "UIKitApplication:")
		if !ok {
			return ""
		}
		if i := strings.IndexByte(label, '['); i >= 0 {
			label = label[:i]
		}
		return label
	}
	return ""
}

func (s *Simulator) springBoardPid(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "spawn", s.UDID(), "launchctl", "list").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[2] == "com.apple.SpringBoard" && f[0] != "-" {
			return f[0]
		}
	}
	return ""
}

// awaitSpringBoard waits for a SpringBoard other than old to be running.
// Launching an app before it is back fails, and a button pressed before it
// is back reaches nothing.
func (s *Simulator) awaitSpringBoard(ctx context.Context, old string) error {
	deadline := time.Now().Add(springBoardBudget)
	for time.Now().Before(deadline) {
		if pid := s.springBoardPid(ctx); pid != "" && pid != old {
			// Running is not yet drawing the Home Screen.
			time.Sleep(springBoardSettle)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("SpringBoard did not come back within %s of restarting backboardd", springBoardBudget)
}

const (
	reclaimBudget     = 30 * time.Second
	springBoardBudget = 20 * time.Second
	springBoardSettle = time.Second
)

// reclaimAfterBoot runs Reclaim on a simulator this server just booted, if
// Device Hub has already taken it. Failing here is not failing the open: the
// video works regardless, and buttons will report the hold themselves.
func (s *Simulator) reclaimAfterBoot(ctx context.Context) {
	if s.viaHub(ctx) {
		return
	}
	if active, err := s.hubActive(ctx); err == nil && active {
		_ = s.Reclaim(ctx)
	}
}

// Touch forwards up to two of the stream's pointers. Through CoreDevice
// the digitizer takes two points per event, so a second finger turns the
// gesture into a two-point one and lifting either turns it back; that the
// digitizer accepts a one-point start continuing as two points, and the
// reverse, was run on loupe-probe with Photos zooming as a pinch does. The
// legacy client's HID call carries one point, so there a second finger is
// dropped rather than turned into a jump of the first.
func (st *simStream) Touch(t Touch) error {
	if st.in == nil {
		return ErrNoControl
	}
	st.touchMu.Lock()
	defer st.touchMu.Unlock()
	i := slices.IndexFunc(st.fingers, func(f finger) bool { return f.id == t.PointerID })
	switch t.Action {
	case TouchDown:
		switch {
		case i >= 0:
			return nil
		case len(st.fingers) == 0:
			st.fingers = append(st.fingers, finger{t.PointerID, t.X, t.Y})
			return st.sendFingers("down")
		case len(st.fingers) == 1 && st.in.viaHub.Load() && !st.oneFinger:
			st.fingers = append(st.fingers, finger{t.PointerID, t.X, t.Y})
			return st.sendFingers("move")
		}
		return nil
	case TouchMove:
		if i < 0 {
			return nil
		}
		st.fingers[i].x, st.fingers[i].y = t.X, t.Y
		return st.sendFingers("move")
	case TouchUp, TouchCancel:
		if i < 0 {
			return nil
		}
		st.fingers[i].x, st.fingers[i].y = t.X, t.Y
		if len(st.fingers) == 2 {
			st.fingers = slices.Delete(st.fingers, i, i+1)
			return st.sendFingers("move")
		}
		err := st.sendFingers("up")
		st.fingers = st.fingers[:0]
		return err
	}
	return nil
}

// sendFingers sends the fingers that are down, one or two, as a move.
func (st *simStream) sendFingers(phase string) error {
	a, err := st.toScreen(st.fingers[0])
	if err != nil {
		return err
	}
	if len(st.fingers) == 1 {
		return st.send("touch %s %d %d", phase, a.x, a.y)
	}
	b, err := st.toScreen(st.fingers[1])
	if err != nil {
		return err
	}
	return st.send("touch2 %s %d %d %d %d", phase, a.x, a.y, b.x, b.y)
}

// toScreen maps a point in stream pixels to the framebuffer pixels simcap's
// input takes. They differ when simcap downscales for MaxSize. The
// framebuffer's size is asked for once per picture size, so a new shape
// reads it again; with no picture size known the point passes through.
// Called under touchMu.
func (st *simStream) toScreen(f finger) (finger, error) {
	m := st.Meta()
	if m.Width <= 0 || m.Height <= 0 {
		return f, nil
	}
	if st.forW != m.Width || st.forH != m.Height {
		out, err := st.do(context.Background(), 0, "size")
		if err != nil {
			return f, err
		}
		var w, h int
		if _, err := fmt.Sscanf(out, "%d %d", &w, &h); err != nil || w <= 0 || h <= 0 {
			return f, fmt.Errorf("simcap size %q", out)
		}
		st.screenW, st.screenH, st.forW, st.forH = w, h, m.Width, m.Height
	}
	if st.screenW == m.Width && st.screenH == m.Height {
		return f, nil
	}
	f.x = int32((int64(f.x)*int64(st.screenW) + int64(m.Width)/2) / int64(m.Width))
	f.y = int32((int64(f.y)*int64(st.screenH) + int64(m.Height)/2) / int64(m.Height))
	return f, nil
}

func (st *simStream) send(format string, a ...any) error {
	_, err := st.do(context.Background(), 0, format, a...)
	return err
}

// do sends one input line through route when the device set one, and
// straight to the input process otherwise.
func (st *simStream) do(ctx context.Context, budget time.Duration, format string, a ...any) (string, error) {
	if st.route != nil {
		return st.route(ctx, budget, format, a...)
	}
	return st.in.do(ctx, budget, format, a...)
}

// Key forwards a viewer's key. Keyboard keys go down and up as they arrive;
// a key that is a button presses it once, on the up.
func (st *simStream) Key(k Key) error {
	if st.sim == nil {
		return ErrNoControl
	}
	ik, ok := androidToIOS[k.Keycode]
	if !ok {
		return fmt.Errorf("keycode %d has no iOS equivalent", k.Keycode)
	}
	if ik.button != "" {
		if k.Action != KeyUp {
			return nil
		}
		return st.sim.PressButton(context.Background(), ik.button)
	}
	ctx, err := st.sim.keyContext(context.Background())
	if err != nil {
		return err
	}
	verb := "keydown"
	if k.Action == KeyUp {
		verb = "keyup"
	}
	_, err = st.do(ctx, 0, "%s %s", verb, strconv.Itoa(ik.usage))
	return err
}

// simcapInputOwner is what a simcap stream's keys and text go through
// beside its input process: a simulator or a physical iPhone.
type simcapInputOwner interface {
	PressButton(ctx context.Context, name string) error
	TypeText(ctx context.Context, text string) error
	// keyContext readies a key: on a simulator it checks Device Hub's hold
	// and may reclaim the simulator first.
	keyContext(ctx context.Context) (context.Context, error)
}

func (s *Simulator) keyContext(ctx context.Context) (context.Context, error) { return s.checkHub(ctx) }

// Text implements TextTyper through the device's input process, the same
// keystrokes /api/input's text action sends.
func (st *simStream) Text(text string) error {
	if st.sim == nil {
		return ErrNoControl
	}
	return st.sim.TypeText(context.Background(), text)
}

// Scroll has no iOS equivalent the HID client offers; a phone viewer sends
// touches rather than wheel events anyway.
func (st *simStream) Scroll(Scroll) error { return ErrNoControl }
