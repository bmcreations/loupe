package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/composetree"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// InputActions lists every action handleInput's switch dispatches on, in the
// order that switch checks them. It exists so nothing else has to copy the
// list by hand: apischema builds POST /api/input's request schema directly
// from it (see internal/apischema/schema.go's inputActionSchemas), and
// TestEveryInputActionDispatches drives each entry through handleInput and
// requires it not fall through to the "unknown action" branch -- so an
// action added to the switch and forgotten here fails a test rather than
// silently shipping a schema an agent's generated client cannot see.
var InputActions = []string{"tap", "swipe", "pinch", "text", "key", "button", "rotate", "reclaim", "hardwareKeyboard", "launch", "openURL", "move"}

// InputRequiredFields names the JSON fields handleInput requires present --
// not merely non-zero, since x=0 and keycode=0 are both coordinates and
// codes a caller can genuinely mean -- for each action in InputActions.
// validateInputRequest checks the raw decoded body against this table
// before the switch below ever runs, and apischema builds POST /api/input's
// oneOf request schema from the same table (inputActionSchemas), so the
// wire contract and the handler's own validation cannot drift apart. A
// field named here and missing from the request is a 400 naming it; ms and
// to, the two fields no action requires, stay optional (a swipe with no ms
// takes defaultSwipeMs, and a rotate with no to cycles from the current
// rotation).
var InputRequiredFields = map[string][]string{
	"tap":     {"x", "y"},
	"swipe":   {"x", "y", "x2", "y2"},
	"pinch":   {"x", "y", "scale"},
	"text":    {"text"},
	"key":     {"keycode"},
	"button":  {"button"},
	"rotate":  nil,
	"reclaim": nil,
	// on is required rather than defaulting: a switch with a default
	// direction would flip a simulator's keyboard for a caller that forgot
	// which way it meant.
	"hardwareKeyboard": {"on"},
	// activity stays optional: without it the launcher activity opens.
	"launch":  {"package"},
	"openURL": {"url"},
	"move":    {"x", "y"},
}

// launchTimeout covers am start -W, which waits for the activity to draw.
// A cold start of a large debug build runs to several seconds, past
// agentTimeout.
const launchTimeout = 30 * time.Second

// agentTimeout bounds a device round trip. uiautomator has to wait for the
// window to go idle before it can dump, which on a busy screen is not instant,
// and `input` spawns app_process. Ten seconds is generous for both and still
// short enough that a wedged device fails rather than hangs.
const agentTimeout = 10 * time.Second

// UIResponse is what /api/ui returns. Width and Height are on the wire because
// every coordinate below them is a device pixel, and a caller that wants to
// reason about a fraction of the screen cannot get there from the tree alone.
type UIResponse struct {
	Target   string         `json:"target"`
	Width    int            `json:"width"`
	Height   int            `json:"height"`
	Rotation int            `json:"rotation"`
	Nodes    []*target.Node `json:"nodes"`
	// Composables and ComposeError are set only for ?compose=1. A compose
	// failure never fails the request: the uiautomator tree still comes
	// back, with ComposeError saying why it carries no names.
	Composables  []*composetree.Composable `json:"composables,omitempty"`
	ComposeError string                    `json:"composeError,omitempty"`
}

// handleUI reports the on-screen view hierarchy.
//
// This is the read half of driving a device without looking at the video. A
// caller locating a button in a decoded frame is doing image work to recover
// something the accessibility tree states outright, and the tree survives a
// theme change, a resolution change and a MaxSize downscale, none of which the
// pixels do.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	insp, ok := t.(target.Inspector)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no view hierarchy", t.ID()),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout)
	defer cancel()
	ctx = s.peerReadCtx(ctx, r, t)

	ui, err := insp.Inspect(ctx)
	if err != nil {
		s.traceAgent(r, t.ID(), "ui", err)
		if writeOwnerError(w, err) {
			return
		}
		// Conflict rather than a gateway error: the device answered, and the
		// fix is on the caller's side -- stop the other automation client.
		status := http.StatusBadGateway
		if errors.Is(err, target.ErrUIAutomationBusy) {
			status = http.StatusConflict
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}

	resp := UIResponse{
		Target: t.ID(), Width: ui.Width, Height: ui.Height, Rotation: ui.Rotation,
	}
	if r.URL.Query().Get("compose") == "1" {
		// Before pruning: Prune copies nodes, so names set afterwards would
		// land on the full tree and never reach an interactive=1 caller.
		resp.Composables, resp.ComposeError = composables(ctx, t, ui.Root)
	}
	// Full tree by default. A hierarchy is mostly layout containers, so the
	// pruned form is the one worth reading, but defaulting to it would make
	// the endpoint lossy in a way the caller could not detect.
	if r.URL.Query().Get("interactive") == "1" {
		resp.Nodes = target.Prune(ui.Root)
		if resp.Nodes == nil {
			// Prune returns nil, not [], when pruning leaves nothing -- an
			// interesting-free tree is rare but not impossible (a blank
			// activity). Nodes carries no omitempty, so the schema promises
			// an array; a nil slice would marshal as null and break a caller
			// that assumes it can range over the field unconditionally.
			resp.Nodes = []*target.Node{}
		}
	} else {
		resp.Nodes = []*target.Node{ui.Root}
	}
	s.traceAgent(r, t.ID(), "ui", nil)
	writeJSON(w, http.StatusOK, resp)
}

// composeStableEvery is how often /api/ui?compose=1 re-asks the agent while
// the tree is still growing. A first reply right after attach is short
// (trap 53); a few hundred ms later it has settled.
const composeStableEvery = 150 * time.Millisecond

// composables reads the composable tree of t's foreground app and names the
// nodes under root after it. The error comes back as a string for
// UIResponse.ComposeError rather than failing the request.
func composables(ctx context.Context, t target.Target, root *target.Node) ([]*composetree.Composable, string) {
	ci, ok := t.(target.ComposeInspector)
	if !ok {
		return nil, "target has no compose inspector"
	}
	dump, err := ci.ComposeDump(ctx)
	if err != nil {
		return nil, err.Error()
	}
	comps, err := composetree.Stable(ctx, dump.Fetch, composeStableEvery)
	if err != nil {
		return nil, dump.Package + ": " + err.Error()
	}
	target.AnnotateComposables(root, comps)
	return comps, ""
}

// screenshotTimeout is separate from agentTimeout because screencap is a
// different shape of wait: it does not block on window idle the way uiautomator
// does, but it copies a full framebuffer back over the adb transport, which on
// a high-resolution device is several megabytes.
const screenshotTimeout = 15 * time.Second

// handleScreenshot returns one PNG of the device screen.
//
// It exists because /api/ui answers structure and labels and nothing else.
// Colour, spacing, custom-drawn surfaces and anything rendered outside the
// accessibility tree need pixels, and before this the only way to get a pixel
// out of loupe was to open a stream and decode it.
//
// It reads the framebuffer rather than the video path deliberately. A frame off
// the canvas has been through MaxSize and through the encoder, and it only
// exists while a viewer is attached; this needs no session and is the device's
// own resolution.
func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	shot, ok := t.(target.Screenshotter)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q cannot be captured", t.ID()),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), screenshotTimeout)
	defer cancel()
	ctx = s.peerReadCtx(ctx, r, t)

	png, err := shot.Screenshot(ctx)
	if err != nil {
		s.traceAgent(r, t.ID(), "screenshot", err)
		if writeOwnerError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}

	s.traceAgent(r, t.ID(), "screenshot", nil)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	// Named so a browser save and a share sheet both produce something
	// identifiable rather than "screenshot.png" from three devices.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("inline; filename=%q", screenshotName(t.ID(), time.Now())))
	w.Write(png)
}

// screenshotName builds a filename that sorts by time and says which device it
// came from. Target ids can contain characters a filename should not, so
// anything outside a conservative set becomes a dash.
func screenshotName(targetID string, now time.Time) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, targetID)
	return fmt.Sprintf("loupe-%s-%s.png", safe, now.Format("20060102-150405"))
}

// defaultSwipeMs is the swipe duration used when a caller omits Ms or sends
// zero. It lives here, not in target.ADBTarget.Swipe, so handleInput can
// resolve it before the call and publish the duration that actually ran
// rather than a zero the target silently replaced (opus review item 6).
const defaultSwipeMs = 300

// InputToMin and InputToMax bound the rotate action's "to" field -- quarter
// turns clockwise, so 0..3 is the whole range a display can report. Both
// handleInput's own check and apischema's generated schema (fieldOverrides,
// internal/apischema/schema.go) read these constants rather than repeating
// the numbers, so TestRotateToRangeMatchesTheConstants can catch the two
// drifting apart at the type rather than only at whatever cases a test
// happened to try.
const (
	InputToMin = 0
	InputToMax = 3
)

// InputRequest is one action in device pixels, the same space /api/ui
// reports. Action is the only field with no omitempty, but it is not the
// only one handleInput requires: which of X/Y/X2/Y2/Text/Keycode/Button are
// required depends on Action, and handleInput checks their presence in the
// decoded JSON body, not their Go zero value here (see
// validateInputRequest and InputRequiredFields) -- x=0 and keycode=0 are
// both values a caller can legitimately mean, and a struct field alone
// cannot distinguish "sent 0" from "omitted". Every field still defaults to
// its Go zero value when the action does not require it (a "tap" with no
// Ms/Text/Keycode is exactly as valid as one that sends them and gets zeros
// back), which is why they keep omitempty regardless. apischema builds POST
// /api/input's request schema as a oneOf, one variant per InputActions
// entry with InputRequiredFields[action] required, so a generated client
// sees this as a sealed type rather than one flat object with everything
// optional.
type InputRequest struct {
	Action string `json:"action"`
	X      int    `json:"x,omitempty"`
	Y      int    `json:"y,omitempty"`
	X2     int    `json:"x2,omitempty"`
	Y2     int    `json:"y2,omitempty"`
	// Scale is the pinch action's zoom: above 1 spreads two fingers apart,
	// below 1 brings them together. See target.Simulator.Pinch for the
	// spreads it becomes.
	Scale float64 `json:"scale,omitempty"`
	// Ms is the swipe or pinch duration. Zero takes the default; a swipe that is too
	// fast is read as a fling and scrolls further than asked.
	Ms      int    `json:"ms,omitempty"`
	Text    string `json:"text,omitempty"`
	Keycode int    `json:"keycode,omitempty"`
	// To is the rotate action's target, 0..3 quarter turns. Nil means "the
	// next one": handleInput reads the current rotation and adds one, mod 4,
	// which is what a rotate button with no state of its own needs to cycle
	// the display. A pointer so an explicit 0 (back to portrait) is
	// distinguishable from "not given" -- a plain int could not tell those
	// apart.
	To *int `json:"to,omitempty"`
	// Button names a device button for the "button" action -- see
	// target.ButtonKeycode. Distinct from Keycode/"key": a button press names
	// what it presses, not a raw code, so an iOS target could resolve the
	// same name to its own HID event.
	Button string `json:"button,omitempty"`
	// On is the hardwareKeyboard action's direction: true connects a
	// hardware keyboard, which hides the target's on-screen one. A pointer
	// so a missing field is told apart from an explicit false.
	On *bool `json:"on,omitempty"`
	// Package is the launch action's app: an Android package name, or an
	// iOS simulator's bundle ID.
	Package string `json:"package,omitempty"`
	// Activity is the launch action's Android activity, qualified or
	// starting with "."; empty opens the launcher activity.
	Activity string `json:"activity,omitempty"`
	// URL is the openURL action's link, handed to whatever the device has
	// registered for it. Only its scheme and host are traced.
	URL string `json:"url,omitempty"`
}

// validateInputRequest checks that body -- the raw request bytes, decoded
// separately from InputRequest -- names every field InputRequiredFields
// requires for action. It reads the raw JSON rather than the already-decoded
// InputRequest because a Go int field cannot distinguish an omitted x from
// an explicit x:0, and 0 is a legitimate coordinate, keycode or rotation
// target -- a caller that means the top-left corner or the hardware key 0
// must not be rejected the same way one that forgot the field is.
//
// action not appearing in InputRequiredFields (an unrecognised action) is
// not an error here: it is left for handleInput's own switch to report as
// "unknown action", which already names the actions it accepts.
func validateInputRequest(body []byte, action string) error {
	required, ok := InputRequiredFields[action]
	if !ok {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		// The body already decoded once into InputRequest above, so a second
		// decode into a map cannot fail on the same bytes; this is here only
		// so the function has no silent failure mode if that ever changes.
		return err
	}
	for _, field := range required {
		v, present := raw[field]
		if !present || string(v) == "null" {
			return fmt.Errorf("action %q requires %q", action, field)
		}
	}
	return nil
}

// handleInput injects one action.
//
// It goes through adb rather than the live scrcpy control channel on purpose.
// Stream input is in stream coordinates, which exist only while something is
// streaming and which MaxSize rescales; these are device pixels, so what
// /api/ui reported is what gets tapped, with no session required and no scale
// factor to get wrong. It costs a few hundred milliseconds per call.
// agentAwake is how long one /api/input keeps the Mac from idle-sleeping.
const agentAwake = 2 * time.Minute

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	// An agent's requests come seconds apart with no connection held between
	// them, so each one keeps the Mac awake for a while after it.
	s.awake.Touch(agentAwake)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}

	var req InputRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}

	if err := validateInputRequest(body, req.Action); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}

	// A launch the target cannot do, or one naming something that is not
	// a package, is answered before arbitration: under a hold it would
	// otherwise be a 409 that no retry can turn into a launch.
	if req.Action == "launch" {
		ln, ok := t.(target.Launcher)
		if !ok {
			writeJSON(w, http.StatusNotImplemented, ErrorResponse{
				Error: fmt.Sprintf("target %q cannot launch an app", t.ID()),
			})
			return
		}
		if err := ln.CheckLaunch(req.Package, req.Activity); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
	}
	// openURL the same way, for the same reason.
	if req.Action == "openURL" {
		uo, ok := t.(target.URLOpener)
		if !ok {
			writeJSON(w, http.StatusNotImplemented, ErrorResponse{
				Error: fmt.Sprintf("target %q cannot open a url", t.ID()),
			})
			return
		}
		if err := uo.CheckURL(req.URL); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
	}

	c := s.inputCaller(r, t.ID())
	if !s.arbitrate(w, r, t.ID(), req.Action, c) {
		return
	}
	// Every return from here that is not the 200 below is an action that
	// did not reach the device.
	acted := false
	defer func() {
		if !acted {
			s.agentFailed(t.ID(), req.Action, c)
		}
	}()

	timeout := agentTimeout
	if req.Action == "launch" || req.Action == "openURL" {
		timeout = launchTimeout
	}
	if t.Kind() == target.KindIOSDevice {
		// A phone's call can run a timed-out command, a devicectl wake and a
		// retry; agentTimeout would end it with a bare context error first.
		timeout = max(timeout, target.IOSInputDeadline())
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	// An Actuator method takes only a context, so a peer target reads who is
	// acting from it.
	if pt, ok := t.(*target.PeerTarget); ok {
		ctx = target.WithPeerAs(ctx, s.peerInputAs(pt, c))
	}

	// A zero Ms is resolved to the swipe default here, before the call, so
	// what gets published is the duration that actually ran rather than a
	// caller-omitted zero the event log would misreport (opus review item 6).
	// ADBTarget.Swipe no longer substitutes one of its own.
	if (req.Action == "swipe" || req.Action == "pinch") && req.Ms <= 0 {
		req.Ms = defaultSwipeMs
	}

	var redaction *textRedaction
	// extra is added to this action's trace record.
	var extra map[string]any
	switch req.Action {
	case "tap", "swipe", "text", "key", "button":
		// All five need an Actuator: tap/swipe/text/key run one of its
		// methods directly, and "button" resolves to a PressKey call once
		// its name is known to be one of the ones this target has.
		act, ok := t.(target.Actuator)
		if !ok {
			writeJSON(w, http.StatusNotImplemented, ErrorResponse{
				Error: fmt.Sprintf("target %q does not accept input", t.ID()),
			})
			return
		}
		switch req.Action {
		case "tap":
			err = act.Tap(ctx, req.X, req.Y)
		case "swipe":
			err = act.Swipe(ctx, req.X, req.Y, req.X2, req.Y2, req.Ms)
		case "text":
			// The dump has to happen before TypeText, not after: typing can
			// move focus itself (an IME "next" action on the last field of a
			// form is the ordinary case), so reading focus afterwards could
			// name the wrong field entirely. It costs an adb call only when a
			// session exists to publish to -- the same gate publishAgentEvent
			// applies below -- so a bare /api/input still costs exactly one
			// adb call.
			if req.Text != "" && !ownerPublishes(t, c, nil) {
				if _, ok := s.mgr.Session(t.ID()); ok {
					red := redactionForFocusedField(ctx, t, req.Text)
					redaction = &red
				}
			}
			err = act.TypeText(ctx, req.Text)
		case "key":
			err = act.PressKey(ctx, req.Keycode)
		case "button":
			// A target with its own buttons names them itself: an iOS
			// simulator's home is a HID button, not keycode 3.
			if bp, isBP := t.(target.ButtonPresser); isBP {
				err = bp.PressButton(ctx, req.Button)
				if errors.Is(err, target.ErrUnknownButton) {
					writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
					return
				}
				break
			}
			code, bok := target.ButtonKeycode(req.Button)
			if !bok {
				writeJSON(w, http.StatusBadRequest, ErrorResponse{
					Error: fmt.Sprintf("unknown button %q; want %s",
						req.Button, strings.Join(target.ButtonNames(), ", ")),
				})
				return
			}
			err = act.PressKey(ctx, code)
		}
	case "pinch":
		pn, ok := t.(target.Pincher)
		if !ok {
			writeJSON(w, http.StatusNotImplemented, ErrorResponse{
				Error: fmt.Sprintf("target %q cannot take two fingers", t.ID()),
			})
			return
		}
		if req.Scale <= 0 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: fmt.Sprintf("scale %g must be above 0", req.Scale),
			})
			return
		}
		err = pn.Pinch(ctx, req.X, req.Y, req.Scale, req.Ms)
	case "rotate":
		rot, ok := t.(target.Rotator)
		if !ok {
			writeJSON(w, http.StatusNotImplemented, ErrorResponse{
				Error: fmt.Sprintf("target %q cannot rotate", t.ID()),
			})
			return
		}
		to := req.To
		// An explicit out-of-range "to" is rejected here, before the Rotator
		// is ever called and before any event is published. Leaving this to
		// the target (rotateCommands is ADBTarget's own backstop) means a
		// non-ADB Rotator can accept -1 or 7 with a 200, and a real device
		// rejecting it comes back as a 502 with a stray event on the log
		// rather than the request simply being wrong.
		if to != nil && (*to < InputToMin || *to > InputToMax) {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: fmt.Sprintf("rotation %d out of range [%d,%d]", *to, InputToMin, InputToMax),
			})
			return
		}
		if to == nil {
			var cur int
			cur, err = rot.Rotation(ctx)
			if err == nil {
				v := (cur + 1) % 4
				to = &v
			}
		}
		if err == nil {
			err = rot.Rotate(ctx, *to)
		}
		// Publish what actually ran, not the caller-omitted nil -- the same
		// reasoning defaultSwipeMs above applies to Ms.
		req.To = to
	case "reclaim":
		// Takes an iOS simulator's buttons and keyboard back from Device Hub.
		// It restarts SpringBoard and closes every app but the one in front,
		// which it reopens. A simulator this server booted or already
		// reclaimed is taken back without asking; any other answers a button
		// or key with ErrCodeHubHeld, and this is what the client sends once
		// its user agrees.
		rc, ok := t.(target.Reclaimer)
		if !ok {
			writeJSON(w, http.StatusNotImplemented, ErrorResponse{
				Error: fmt.Sprintf("target %q has nothing to reclaim", t.ID()),
			})
			return
		}
		err = rc.Reclaim(ctx)
	case "hardwareKeyboard":
		// The viewer sends this when its user chooses to type on their own
		// phone's keyboard, and again each time it connects. An Android
		// target attaches its keyboard through the open session, so without
		// one this is a 409 (target.ErrNoSession).
		hk, ok := t.(target.HardwareKeyboarder)
		if !ok {
			writeJSON(w, http.StatusNotImplemented, ErrorResponse{
				Error: fmt.Sprintf("target %q has no hardware keyboard to connect", t.ID()),
			})
			return
		}
		err = hk.SetHardwareKeyboard(ctx, *req.On)
	case "launch":
		// A launch replaces what is on the screen, so it is held while a
		// person is using the target, the same as a tap: arbitrated() is
		// true for it, and an agent's launch during the hold is a 409
		// humanHeld before anything reaches the device. Whether it can
		// launch at all, and the names, were checked above.
		ln := t.(target.Launcher)
		var res *target.LaunchResult
		res, err = ln.Launch(ctx, req.Package, req.Activity)
		extra = launchTrace(req.Package, req.Activity, res, began)
		if errors.Is(err, target.ErrBadLaunchName) {
			s.traceInputWith(r, t.ID(), req.Action, c, err, extra)
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
	case "openURL":
		// Held like launch, since it replaces what is on the screen. The
		// URL was checked above, and every error the target returns names
		// only its scheme and host.
		var res *target.LaunchResult
		res, err = t.(target.URLOpener).OpenURL(ctx, req.URL)
		extra = openURLTrace(req.URL, res, began)
	case "move":
		// Points without touching: nothing reaches the device, so it needs
		// no Actuator and cannot fail. A viewer's own move would only draw
		// an agent cursor for a person, so it publishes nothing.
		if c.viewer == 0 {
			s.publishCursor(ctx, t, req, c)
		}
	default:
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("unknown action %q; want %s", req.Action, strings.Join(InputActions, ", ")),
		})
		return
	}
	// A keyboard switch moves nothing on the screen, so it stays out of
	// the event log; the trace below still records it. A move went out
	// above as a MsgCursor, which is not history either. A launch has no
	// field in the event format to name its app, nor openURL its link, so
	// each is traced only.
	// A peer's owner publishes its own, which the relay brings here.
	if req.Action != "hardwareKeyboard" && req.Action != "launch" && req.Action != "openURL" && req.Action != "move" &&
		!ownerPublishes(t, c, err) {
		s.publishAgentEvent(ctx, t, req, c, err, redaction)
	}
	s.traceReclaim(r, t, began)

	if err != nil {
		safeErr := sanitizeActionErr(req, err, redaction)
		s.traceInputWith(r, t.ID(), req.Action, c, safeErr, extra)
		// The owner's own refusal or silence comes before anything this
		// server would say about the error.
		if writeOwnerError(w, err) {
			return
		}
		if code, status, ok := launchErrCode(err); ok {
			writeJSON(w, status, ErrorResponse{Error: safeErr.Error(), Code: code})
			return
		}
		if errors.Is(err, target.ErrNoSession) {
			writeJSON(w, http.StatusConflict, ErrorResponse{Error: safeErr.Error()})
			return
		}
		if errors.Is(err, target.ErrHubHoldsInput) {
			writeJSON(w, http.StatusConflict, ErrorResponse{Error: safeErr.Error(), Code: ErrCodeHubHeld})
			return
		}
		if errors.Is(err, target.ErrTouchRejected) {
			writeJSON(w, http.StatusConflict, ErrorResponse{Error: safeErr.Error(), Code: ErrCodeTouchRejected})
			return
		}
		if errors.Is(err, target.ErrRunnerStarting) {
			writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: safeErr.Error(), Code: ErrCodeRunnerStarting})
			return
		}
		if errors.Is(err, target.ErrRunnerNotBuilt) || errors.Is(err, target.ErrTouchNeedsCable) {
			writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{Error: safeErr.Error(), Code: ErrCodeRunnerUnavailable})
			return
		}
		if errors.Is(err, target.ErrPhoneUnreachable) {
			writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: safeErr.Error(), Code: ErrCodePhoneUnreachable})
			return
		}
		if errors.Is(err, target.ErrPhoneDeliveryUnknown) {
			writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: safeErr.Error(), Code: ErrCodePhoneDeliveryUnknown})
			return
		}
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: safeErr.Error()})
		return
	}
	s.traceInputWith(r, t.ID(), req.Action, c, nil, extra)
	acted = true
	s.agentActed(t.ID(), req.Action, c)
	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

// errInputFailed replaces a text action's own error everywhere it could
// otherwise reach a log or a response: the trace, and (below) the event
// published to viewers. ADB.ExecOut already folds its command's args into the
// errors it returns, so a shell failure on `input text <secret>` could echo
// the secret straight back through any of those paths.
var errInputFailed = errors.New("input failed")

// sanitizeActionErr replaces a text action's error with a fixed one whenever
// the field it was typed into is not affirmatively known to be a
// non-password one -- the same fail-closed rule redactionForFocusedField
// applies to the event itself. redaction is nil when no session exists to
// publish to, in which case nothing dumped the hierarchy and the field's
// kind is simply unknown, so this also redacts.
//
// A physical iPhone's two 503 errors are the exception. Their messages are
// built in target.IOSDevice.do from CoreDevice's reply, devicectl's wake
// result and fixed words, and leave out the command line (which for a typed
// character names the key), so they cannot carry the text. Redacting them
// would drop what a caller needs to tell a retry-safe failure from one that
// may have landed.
func sanitizeActionErr(req InputRequest, err error, redaction *textRedaction) error {
	if err == nil || req.Action != "text" {
		return err
	}
	if errors.Is(err, target.ErrPhoneUnreachable) || errors.Is(err, target.ErrPhoneDeliveryUnknown) {
		return err
	}
	if redaction != nil && !redaction.redacted {
		return err
	}
	return errInputFailed
}

// eventTextRunes bounds how much of a text action's payload the event log
// keeps. The log is for seeing what happened, not archiving it -- the trace
// already records the action -- so a long string is cut, not kept whole.
const eventTextRunes = 64

// passwordDumpTimeout bounds the view-hierarchy dump made to decide whether a
// text action targets a password field. It is a fraction of agentTimeout, not
// agentTimeout itself, so a slow dump still leaves the TypeText call that
// follows it room to run inside the same request; a dump that does not
// finish within it is treated as inconclusive and redacts (see
// redactionForFocusedField). A var, not a const, so a test can shrink it
// rather than wait out the real value.
var passwordDumpTimeout = 5 * time.Second

// passwordMarker replaces a redacted text event's Text. It is fixed and
// carries no information about length -- that is what TextLen is for -- so
// two redacted events are indistinguishable from each other on the wire.
const passwordMarker = session.TextMarker

// textRedaction is what handleInput decides, before typing, about whether a
// text action's event should carry the characters or withhold them.
// publishAgentEvent applies it when building the event.
type textRedaction struct {
	// redacted is true unless a dump positively identified the focused node
	// as not a password field. Every failure mode -- no Inspector, a dump
	// error, a dump timeout, no focused node -- leaves it true: a benign
	// string losing its detail on the log is fine, a password in the trace
	// is not.
	redacted bool
	// textLen is the rune count of the text actually typed, published
	// alongside a redacted event so a viewer can see how much was entered
	// without seeing what it was.
	textLen int
}

// redactionForFocusedField decides whether text is going into a password
// field, by dumping the view hierarchy and checking every node currently
// focused (a dump can carry more than one -- see target.FocusedAll). It must
// be called before the text is typed -- typing can move focus itself, e.g. an
// IME "next" action -- and it fails closed: anything short of a dump that
// positively names every focused node as non-password redacts.
func redactionForFocusedField(ctx context.Context, t target.Target, text string) textRedaction {
	red := textRedaction{redacted: true, textLen: len([]rune(text))}

	insp, ok := t.(target.Inspector)
	if !ok {
		return red
	}

	dctx, cancel := context.WithTimeout(ctx, passwordDumpTimeout)
	defer cancel()

	ui, err := insp.Inspect(dctx)
	if err != nil {
		return red
	}

	nodes := target.FocusedAll(ui)
	if len(nodes) == 0 {
		return red
	}
	// Any focused node naming itself a password field redacts, not just the
	// first or the deepest: a WebView or Compose host can mark itself focused
	// alongside the leaf actually receiving keystrokes, and a dialog's window
	// can leave a focused node behind it in another window. Checking only one
	// of them lets the wrong one answer for the field the user is typing
	// into.
	for _, n := range nodes {
		if n.Password {
			red.redacted = true
			return red
		}
	}
	red.redacted = false
	return red
}

// ownerPublishes reports whether an agent's call on a peer target leaves its
// event to the owner. The owner publishes one for every call it acts on, and
// the relay brings it to this server's session (Session.watchRelayed), so
// publishing here too would log the tap twice. A hub viewer's own call stays
// here: the owner's copy carries the relay's viewer id, which the relay
// drops. So does a call that never reached the owner, which published
// nothing. err is the action's result, nil before it has run.
func ownerPublishes(t target.Target, c inputCaller, err error) bool {
	if _, ok := t.(*target.PeerTarget); !ok || c.viewer != 0 {
		return false
	}
	return !errors.Is(err, target.ErrPeerDown)
}

// publishAgentEvent turns one /api/input call into an Event and publishes it
// to the target's session, if one exists.
//
// It never opens or creates a session: unlike a viewer, /api/input works with
// no session running (CLAUDE.md's Agent API), and a call that exists only to
// have somewhere to publish would defeat that -- Manager.Session looks up
// without acquiring for exactly this reason.
//
// actionErr is the action's own result. A failed action still publishes --
// otherwise the event log shows a device that never moved rather than a tap
// that was attempted and rejected -- with Err set to say so.
func (s *Server) publishAgentEvent(ctx context.Context, t target.Target, req InputRequest, c inputCaller, actionErr error, redaction *textRedaction) {
	sess, ok := s.mgr.Session(t.ID())
	if !ok {
		return
	}

	// An empty text action has nothing to log -- TypeText itself no-ops on
	// empty input (input.go), so an event with no text and no error would be
	// pure noise on the log.
	if req.Action == "text" && req.Text == "" {
		return
	}

	e := session.Event{Source: "agent", Kind: req.Action}
	// A viewer's own /api/input -- an iOS button, a rotation -- is the
	// human's, so it is published as theirs: touches.js then skips it on
	// the viewer that sent it and draws it as a viewer's everywhere else.
	if c.viewer != 0 {
		e.Source, e.Viewer = "viewer", c.viewer
	} else {
		e.Agent = c.agent
	}
	if actionErr != nil {
		e.Err = sanitizeActionErr(req, actionErr, redaction).Error()
	}

	// The screen size is read only for tap and swipe. text and key carry no
	// coordinate, so asking a target for one -- ADBTarget.ScreenSize costs an
	// adb connection -- would be spent for nothing (opus review item 4). A
	// swipe reads it once and reuses it for both ends, so its two ends agree
	// and the target is asked at most once per action.
	switch req.Action {
	case "tap":
		w, h := agentScreenSize(ctx, t)
		pt, outside := fractionalPoint(w, h, req.X, req.Y)
		e.From = pt
		// The action's own error takes priority: publishing "outside the
		// screen" over an adb failure would bury what actually went wrong.
		// A swipe landing past an edge is ordinary (it is clamped with no
		// Err below); a tap starting there usually means the caller is
		// working from a stale screen size.
		if outside && actionErr == nil {
			e.Err = fmt.Sprintf("outside the screen (%dx%d)", w, h)
		}
	case "swipe":
		w, h := agentScreenSize(ctx, t)
		e.From, _ = fractionalPoint(w, h, req.X, req.Y)
		e.To, _ = fractionalPoint(w, h, req.X2, req.Y2)
		e.Ms = req.Ms
	case "pinch":
		w, h := agentScreenSize(ctx, t)
		e.From, _ = fractionalPoint(w, h, req.X, req.Y)
		e.Scale = req.Scale
		e.Ms = req.Ms
	case "text":
		// redaction is decided in handleInput, before the characters were
		// typed, because typing can move focus. Its absence here (should not
		// happen once a session exists to publish to) redacts the same way
		// every other inconclusive case does -- see redactionForFocusedField.
		if redaction == nil || redaction.redacted {
			e.Text = passwordMarker
			e.Redacted = true
			if redaction != nil {
				e.TextLen = redaction.textLen
			} else {
				e.TextLen = len([]rune(req.Text))
			}
		} else {
			e.Text = truncateRunes(req.Text, eventTextRunes)
		}
	case "key":
		// A keycode that names a device button (target.ButtonForKeycode)
		// publishes as kind:"button" with the name, matching what the
		// "button" action itself publishes and what ViewerKey publishes for
		// the same keycode arriving from a viewer. An unnamed keycode stays
		// kind:"key" with the raw code -- see the known gap in Task 5: this
		// path is not covered by the text-action password redaction, so a
		// PIN typed one digit at a time as key actions is not redacted.
		if name, ok := target.ButtonForKeycode(req.Keycode); ok {
			e.Kind = "button"
			e.Button = name
		} else {
			e.Keycode = req.Keycode
		}
	case "button":
		e.Button = req.Button
	case "rotate":
		e.Rotation = req.To
	}

	sess.PublishEvent(e)
}

// publishCursor sends an agent's move to the target's viewers, as a
// fraction of the screen like every agent event. With no session there is
// no one to show it to, and with no screen size there is nowhere to put it.
func (s *Server) publishCursor(ctx context.Context, t target.Target, req InputRequest, c inputCaller) {
	sess, ok := s.mgr.Session(t.ID())
	if !ok {
		return
	}
	w, h := agentScreenSize(ctx, t)
	pt, _ := fractionalPoint(w, h, req.X, req.Y)
	if pt == nil {
		return
	}
	sess.PublishCursor(session.Event{Source: "agent", Agent: c.agent, Kind: "move", From: pt})
}

// agentScreenSize reports the target's current on-screen size in device
// pixels, or 0, 0 if it cannot: the target does not implement ScreenSizer,
// or the read failed, or it reported a non-positive size. All three are
// "unknown" and treated identically -- fractionalPoint's job is to turn that
// into "no coordinate on the event" rather than a guessed or divide-by-zero
// one.
func agentScreenSize(ctx context.Context, t target.Target) (int, int) {
	sizer, ok := t.(target.ScreenSizer)
	if !ok {
		return 0, 0
	}
	w, h, err := sizer.ScreenSize(ctx)
	if err != nil || w <= 0 || h <= 0 {
		return 0, 0
	}
	return w, h
}

// fractionalPoint is session.NormPoint clamped to [0,1], the range
// Event.From/To carry for an agent action. outside is NormPoint's, reported
// before clamping: a swipe's far end landing past an edge is ordinary, but a
// tap starting there usually means the caller is working from a stale screen
// size, so the caller decides whether it is worth an Err.
func fractionalPoint(w, h, x, y int) (pt *session.Point, outside bool) {
	pt, outside = session.NormPoint(x, y, w, h)
	if pt != nil {
		pt.X, pt.Y = clamp01(pt.X), clamp01(pt.Y)
	}
	return pt, outside
}

// clamp01 restricts f to [0, 1].
func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

// truncateRunes cuts s to at most n runes, not bytes, so a multi-byte
// character at the boundary is never split into invalid UTF-8.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// agentTarget resolves the ?target= parameter, writing the error response
// itself so both handlers report a missing target identically.
func (s *Server) agentTarget(w http.ResponseWriter, r *http.Request) (target.Target, bool) {
	id := r.URL.Query().Get("target")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "missing ?target=; GET /api/targets lists them",
		})
		return nil, false
	}
	t, ok := s.mgr.Target(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, ErrorResponse{
			Error: fmt.Sprintf("no target %q", id),
		})
		return nil, false
	}
	// Any call from a claim's holder renews it, reads included. A peer
	// target's claim is the owner's, and the call forwarded to it carries the
	// same headers, so it renews there.
	agent, session := agentName(r.Header.Get(HeaderAgent)), agentName(r.Header.Get(HeaderSession))
	if _, peer := t.(*target.PeerTarget); !peer {
		s.claims.seen(t.ID(), agent, session)
	}
	// So the person can send a flag to a session that drives over HTTP
	// and never waits on flags.
	s.flags.saw(agent, session, t.ID())
	return t, true
}

// traceAgent records every agent action in the same trace as the video
// counters. A device that a human and a process are both driving will produce
// behaviour neither of them can explain alone, and this is what puts the taps
// on the same timeline as the frames.
// traceReclaim records a Device Hub reclaim that happened inside this
// request. A button or key on a claimed simulator takes input back on its
// own, and that press then takes around ten seconds and restarts
// SpringBoard; without this the trace shows only a slow press.
func (s *Server) traceReclaim(r *http.Request, t target.Target, since time.Time) {
	rc, ok := t.(target.Reclaimer)
	if !ok {
		return
	}
	last, ok := rc.LastReclaim()
	if !ok || last.At.Before(since) {
		return
	}
	s.trace.Record(map[string]any{
		"kind": "agent", "target": t.ID(), "action": "reclaimed",
		"tookMs": last.Took.Milliseconds(), "reopened": last.Reopened,
	})
}

func (s *Server) traceAgent(r *http.Request, targetID, action string, err error) {
	s.traceInput(r, targetID, action, inputCaller{}, err)
}

// traceInput is traceAgent for /api/input, which also names who sent it: a
// person pressing Home through the viewer and an agent doing the same are
// otherwise one record.
func (s *Server) traceInput(r *http.Request, targetID, action string, c inputCaller, err error) {
	s.traceInputWith(r, targetID, action, c, err, nil)
}

// traceInputWith is traceInput with fields of the action's own added.
func (s *Server) traceInputWith(r *http.Request, targetID, action string, c inputCaller, err error, extra map[string]any) {
	rec := map[string]any{
		"kind": "agent", "target": targetID, "action": action,
		"ua": r.UserAgent(),
	}
	for k, v := range extra {
		rec[k] = v
	}
	if c != (inputCaller{}) {
		c.annotate(rec)
	}
	if err != nil {
		rec["err"] = err.Error()
	}
	s.trace.Record(rec)
}

// maxInstallBytes bounds an APK upload. A debug build of a large app runs to
// a few hundred megabytes; anything past this is more likely a wrong file
// than an app. A var so a test can shrink it.
var maxInstallBytes int64 = 1 << 30

// installTimeout covers streaming the APK to the device and the package
// manager's dexopt, which on a large app is most of a minute.
const installTimeout = 5 * time.Minute

// zipMagic starts every APK, which is a zip. Checked before anything is
// staged, so a JSON body or an HTML error page sent by mistake is a 400
// rather than a minute of adb telling the same thing less clearly.
var zipMagic = []byte("PK\x03\x04")

// InstallResponse is what POST /api/install answers, on success (200) and
// when the package manager refused the APK (422). Output is adb's own text
// either way. Code is the INSTALL_FAILED_* reason, which is what a caller
// branches on; it is empty on success.
type InstallResponse struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	Bytes  int64  `json:"bytes"`
	TookMs int64  `json:"tookMs"`
	// Package is the installed app's package name, read from the APK with
	// aapt2, or ?package= when aapt2 could not read it. Empty when neither
	// gave one; PackageError then says why.
	Package      string `json:"package,omitempty"`
	PackageError string `json:"packageError,omitempty"`
	// Launch is present when ?launch=1 asked to open the app. It is a
	// separate verdict: the install above stands whatever it says.
	Launch *LaunchResponse `json:"launch,omitempty"`
}

// LaunchResponse is the outcome of ?launch=1 on /api/install. Code is
// notInstalled, noLaunchActivity or humanHeld when it is one of those.
type LaunchResponse struct {
	OK           bool   `json:"ok"`
	Activity     string `json:"activity,omitempty"`
	Error        string `json:"error,omitempty"`
	Code         string `json:"code,omitempty"`
	RetryAfterMs int64  `json:"retryAfterMs,omitempty"`
	TookMs       int64  `json:"tookMs"`
}

// handleInstall installs the request body as an APK, or a multipart body as
// a base and its splits, replacing any version already installed. ?allowTest=1 accepts a test-only APK, which is what
// Android Studio's Run builds. It is the step between building an app and driving it
// that otherwise needs its own adb invocation on the Mac.
//
// ?launch=1 then opens the installed app's launcher activity, so a PR build
// is installed and in front in one request. The status is the install's: a
// launch that fails, or that a person at a viewer holds off, is reported in
// the response's launch object with a 200, because the APK is on the device
// either way and a 409 or 502 would read as the install failing.
func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	inst, ok := t.(target.Installer)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q cannot install an APK", t.ID()),
		})
		return
	}
	// A declared length past the limit is refused before a byte is staged.
	// A chunked body has none, and MaxBytesReader catches it mid-copy.
	if r.ContentLength > maxInstallBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{
			Error: fmt.Sprintf("APK is over %d bytes", maxInstallBytes),
		})
		return
	}
	s.awake.Touch(agentAwake)

	body := &countingReader{r: http.MaxBytesReader(w, r.Body, maxInstallBytes)}
	dir, err := os.MkdirTemp("", "loupe-install-")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	defer os.RemoveAll(dir)
	apks, err := stageAPKs(r, body, dir)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		s.trace.Record(map[string]any{
			"kind": "agent", "target": t.ID(), "action": "install", "ua": r.UserAgent(),
			"bytes": body.n, "tookMs": time.Since(began).Milliseconds(), "outcome": "tooLarge",
		})
		writeJSON(w, http.StatusRequestEntityTooLarge, ErrorResponse{
			Error: fmt.Sprintf("upload is over %d bytes", maxInstallBytes),
		})
		return
	case err != nil:
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), installTimeout)
	defer cancel()

	opts := target.InstallOptions{AllowTest: r.URL.Query().Get("allowTest") == "1"}
	res, err := inst.Install(ctx, apks, opts)
	resp := InstallResponse{Target: t.ID(), Bytes: body.n, TookMs: time.Since(began).Milliseconds()}
	rec := map[string]any{
		"kind": "agent", "target": t.ID(), "action": "install",
		"ua": r.UserAgent(), "bytes": body.n, "apks": len(apks), "tookMs": resp.TookMs,
	}
	if opts.AllowTest {
		rec["allowTest"] = true
	}
	switch refused, isRefused := target.IsInstallRefused(err); {
	case err == nil:
		rec["outcome"] = "ok"
		resp.OK, resp.Output = true, res.Output
		resp.Package, resp.PackageError = s.installedPackage(ctx, r, apks)
		if resp.Package != "" {
			rec["package"] = resp.Package
		}
		s.trace.Record(rec)
		if r.URL.Query().Get("launch") == "1" {
			resp.Launch = s.launchInstalled(r, t, resp.Package, resp.PackageError)
		}
		writeJSON(w, http.StatusOK, resp)
	case isRefused:
		rec["outcome"], rec["code"] = "refused", refused.Code
		s.trace.Record(rec)
		resp.Output, resp.Code, resp.Reason = refused.Output, refused.Code, refused.Reason
		writeJSON(w, http.StatusUnprocessableEntity, resp)
	default:
		rec["outcome"], rec["err"] = "error", err.Error()
		s.trace.Record(rec)
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
	}
}

// installedPackage names the app an install put on the device. aapt2 reads
// it from the first APK; a config split carries its base's package name, so
// which one is first does not matter. ?package= is the fallback for a Mac
// without SDK build-tools, and is checked against the APK when aapt2 can
// read it, since a launch of the wrong package would open some other app.
func (s *Server) installedPackage(ctx context.Context, r *http.Request, apks []string) (string, string) {
	asked := r.URL.Query().Get("package")
	name, err := target.APKPackage(ctx, s.aapt2, apks[0])
	switch {
	case err == nil && asked != "" && asked != name:
		return name, fmt.Sprintf("?package=%s does not match the APK's %s", asked, name)
	case err == nil:
		return name, ""
	case asked != "" && target.ValidPackageName(asked):
		return asked, ""
	case asked != "":
		return "", fmt.Sprintf("%q is not a package name", asked)
	}
	return "", err.Error()
}

// launchInstalled is ?launch=1: the launch action of /api/input, arbitrated
// and traced the same way, against the package the install just put there.
func (s *Server) launchInstalled(r *http.Request, t target.Target, pkg, pkgErr string) *LaunchResponse {
	began := time.Now()
	out := &LaunchResponse{}
	defer func() { out.TookMs = time.Since(began).Milliseconds() }()
	if pkg == "" || pkgErr != "" {
		out.Error = "not launched: " + pkgErr
		return out
	}
	ln, ok := t.(target.Launcher)
	if !ok {
		out.Error = fmt.Sprintf("target %q cannot launch an app", t.ID())
		return out
	}
	c := s.inputCaller(r, t.ID())
	if refused := s.mayAct(r, t.ID(), "launch", c); refused != nil {
		out.Error, out.Code, out.RetryAfterMs = refused.Error, refused.Code, refused.RetryAfterMs
		return out
	}
	ctx, cancel := context.WithTimeout(r.Context(), launchTimeout)
	defer cancel()
	res, err := ln.Launch(ctx, pkg, "")
	s.traceInputWith(r, t.ID(), "launch", c, err, launchTrace(pkg, "", res, began))
	if err != nil {
		s.agentFailed(t.ID(), "launch", c)
		out.Error = err.Error()
		out.Code, _, _ = launchErrCode(err)
		return out
	}
	s.agentActed(t.ID(), "launch", c)
	out.OK, out.Activity = true, res.Activity
	return out
}

// stageAPKs writes the upload into dir and returns the paths, in the order
// they arrived. A raw body is one APK. A multipart/form-data body is a base
// and its splits, one file part each under any field name, and goes to
// `adb install-multiple`; the package manager finds the base by its manifest,
// so the order does not matter.
//
// Each file is named by its index rather than by the part's filename, so a
// caller's name never reaches a path. adb only needs the .apk suffix.
func stageAPKs(r *http.Request, body io.Reader, dir string) ([]string, error) {
	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "multipart/form-data" {
		p, err := stageAPK(body, dir, 0)
		if err != nil {
			return nil, err
		}
		return []string{p}, nil
	}
	mr := multipart.NewReader(body, params["boundary"])
	var apks []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart body: %w", err)
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		if len(apks) == maxInstallParts {
			return nil, fmt.Errorf("more than %d APKs in one install", maxInstallParts)
		}
		p, err := stageAPK(part, dir, len(apks))
		part.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", part.FileName(), err)
		}
		apks = append(apks, p)
	}
	if len(apks) == 0 {
		return nil, errors.New("multipart body has no file parts; send each APK as a file")
	}
	return apks, nil
}

// maxInstallParts bounds the splits in one install. A bundle's config splits
// are one per ABI, density and language the device needs, plus one per
// dynamic feature, which is a handful rather than hundreds.
const maxInstallParts = 64

// stageAPK copies one APK into dir after checking its zip signature, so a
// JSON body or an HTML error page sent by mistake is a 400 rather than an
// adb error.
func stageAPK(src io.Reader, dir string, i int) (string, error) {
	br := bufio.NewReader(src)
	head, err := br.Peek(len(zipMagic))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return "", err
	}
	if err != nil || !bytes.Equal(head, zipMagic) {
		return "", errors.New("not an APK (no zip signature)")
	}
	p := filepath.Join(dir, fmt.Sprintf("%d.apk", i))
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, br); err != nil {
		f.Close()
		return "", err
	}
	return p, f.Close()
}

// countingReader counts what an upload delivered, for the trace and the
// response.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

const (
	defaultLogcatLines = 500
	maxLogcatLines     = 20000
	// maxLogcatBytes bounds a dump held in memory before it is trimmed to
	// the lines asked for. A full main buffer is a few megabytes.
	maxLogcatBytes = 32 << 20
	// logcatDumpTimeout covers reading the whole ring buffer back over adb.
	logcatDumpTimeout = 15 * time.Second
)

// logcatQuery reads GET /api/logcat's parameters. since takes a duration
// back from now ("30s", "5m") or an RFC 3339 time; either is compared
// against the device's clock, which on an emulator is the Mac's.
func logcatQuery(q url.Values, now time.Time) (target.LogcatQuery, error) {
	lq := target.LogcatQuery{Lines: defaultLogcatLines, Follow: q.Get("follow") == "1"}
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogcatLines {
			return lq, fmt.Errorf("lines %q: want 1 to %d", v, maxLogcatLines)
		}
		lq.Lines = n
	}
	if v := q.Get("since"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			lq.Since = now.Add(-d)
		} else if at, err := time.Parse(time.RFC3339Nano, v); err == nil {
			lq.Since = at
		} else {
			return lq, fmt.Errorf("since %q: want a duration such as 5m or an RFC 3339 time", v)
		}
	}
	named := 0
	if v := q.Get("pid"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return lq, fmt.Errorf("pid %q is not a pid", v)
		}
		lq.PID = n
		named++
	}
	if v := q.Get("package"); v != "" {
		// An Android package or an Apple bundle ID; each target checks
		// its own rule again before using it.
		if !target.ValidPackageName(v) && !target.ValidBundleID(v) {
			return lq, fmt.Errorf("package %q is not a package name or bundle ID", v)
		}
		lq.Package = v
		named++
	}
	if q.Get("foreground") == "1" {
		lq.Foreground = true
		named++
	}
	if named > 1 {
		return lq, errors.New("give at most one of pid, package and foreground")
	}
	switch v := q.Get("format"); v {
	case "", "text":
	case target.LogFormatNDJSON:
		lq.Format = v
	default:
		return lq, fmt.Errorf("format %q: want text or %s", v, target.LogFormatNDJSON)
	}
	return lq, nil
}

// handleLogcat returns the device log as text, one line per entry
// (logcat's threadtime on Android, the unified log's compact style on a
// simulator), or with ?format=ndjson one JSON object per line where the
// target has that form: the last ?lines= of it, or everything since ?since=, narrowed to
// one process by ?pid=, ?package= or ?foreground=1. With ?follow=1 it
// stays open and streams new lines until the caller hangs up.
//
// The process it narrowed to is in X-Loupe-Pid and X-Loupe-Package, since
// ?foreground=1 names whichever app is in front and the caller cannot know
// that in advance. A pid is fixed when the log opens, so a follow on an app
// that restarts keeps reading the process that died.
func (s *Server) handleLogcat(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	lc, ok := t.(target.Logcatter)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no log to read", t.ID()),
		})
		return
	}
	q, err := logcatQuery(r.URL.Query(), time.Now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	s.awake.Touch(agentAwake)

	ctx := r.Context()
	if !q.Follow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, logcatDumpTimeout)
		defer cancel()
	}
	log, err := lc.Logcat(ctx, q)
	if err != nil {
		s.traceAgent(r, t.ID(), "logcat", err)
		if errors.Is(err, target.ErrLogQueryUnsupported) {
			writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{Error: err.Error(), Code: ErrCodeNotSupported})
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, target.ErrAppNotRunning) {
			status = http.StatusConflict
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	defer log.Close()

	h := w.Header()
	if q.Format == target.LogFormatNDJSON {
		h.Set("Content-Type", "application/x-ndjson")
	} else {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if log.PID > 0 {
		h.Set("X-Loupe-Pid", strconv.Itoa(log.PID))
	}
	if log.Package != "" {
		h.Set("X-Loupe-Package", log.Package)
	}

	if q.Follow {
		s.traceAgent(r, t.ID(), "logcat", nil)
		followLogcat(w, log)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(log, maxLogcatBytes))
	if cerr := log.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		h.Del("Content-Type")
		s.traceAgent(r, t.ID(), "logcat", err)
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	s.traceAgent(r, t.ID(), "logcat", nil)
	// -T has no line limit of its own, so a since dump is trimmed here.
	if !q.Since.IsZero() {
		raw = lastLines(raw, q.Lines)
	}
	w.Write(raw)
}

// followLogcat copies a following log to the response, flushing after each
// read so a line reaches the caller when logcat prints it rather than when
// a buffer fills. It returns when the caller hangs up, which cancels the
// request context and with it adb.
func followLogcat(w http.ResponseWriter, log io.Reader) {
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	rc.Flush()
	buf := make([]byte, 32<<10)
	for {
		n, err := log.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// lastLines keeps the last n lines of b, and logcat's "--------- beginning
// of" buffer markers only when they fall inside them.
func lastLines(b []byte, n int) []byte {
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	seen := 0
	for i := end - 1; i >= 0; i-- {
		if b[i] == '\n' {
			seen++
			if seen == n {
				return b[i+1:]
			}
		}
	}
	return b
}

// launchTrace is what a launch adds to its kind:"agent" record: the app,
// the activity asked for and the one that came up, and how long it took
// from the request's start.
func launchTrace(pkg, activity string, res *target.LaunchResult, began time.Time) map[string]any {
	rec := map[string]any{"package": pkg, "tookMs": time.Since(began).Milliseconds()}
	if activity != "" {
		rec["activity"] = activity
	}
	if res != nil && res.Activity != "" {
		rec["started"] = res.Activity
	}
	if res != nil && res.TimedOut {
		rec["timedOut"] = true
	}
	return rec
}

// openURLTrace is launchTrace for openURL. It records the scheme, the host
// and the length, never the URL: a deep link's path and query routinely
// carry a token.
func openURLTrace(u string, res *target.LaunchResult, began time.Time) map[string]any {
	scheme, host := target.URLParts(u)
	rec := map[string]any{"scheme": scheme, "urlLen": len(u), "tookMs": time.Since(began).Milliseconds()}
	if host != "" {
		rec["host"] = host
	}
	if res != nil && res.Activity != "" {
		rec["started"] = res.Activity
	}
	if res != nil && res.TimedOut {
		rec["timedOut"] = true
	}
	return rec
}

// launchErrCode maps a launch the target refused to its status and code. A
// missing app or activity is 404 and an app with nothing to open is 422: each
// is the request naming the wrong thing, not the device failing.
func launchErrCode(err error) (code string, status int, ok bool) {
	switch {
	case errors.Is(err, target.ErrAppNotInstalled):
		return ErrCodeNotInstalled, http.StatusNotFound, true
	case errors.Is(err, target.ErrNoLaunchActivity):
		return ErrCodeNoLaunchActivity, http.StatusUnprocessableEntity, true
	case errors.Is(err, target.ErrNoSuchActivity):
		return ErrCodeNoSuchActivity, http.StatusNotFound, true
	case errors.Is(err, target.ErrNoURLHandler):
		return ErrCodeNoURLHandler, http.StatusUnprocessableEntity, true
	}
	return "", 0, false
}
