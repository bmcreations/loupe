package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bmcreations/loupe/internal/httpapi"
	"github.com/bmcreations/loupe/internal/target"
)

// toolRoute is the HTTP route a tool calls, which TestEveryToolNamesARoute
// checks against httpapi.Routes so a renamed route cannot leave a tool
// calling nothing.
type toolRoute struct {
	Tool, Method, Path string
}

// toolRoutes lists every tool and the route behind it. The input tools all
// go to POST /api/input, each with its action fixed: one tool per action
// gives the model a schema with only that action's fields, where the route's
// own schema is a oneOf over all of them.
var toolRoutes = []toolRoute{
	{"targets", http.MethodGet, "/api/targets"},
	{"ui", http.MethodGet, "/api/ui"},
	{"screenshot", http.MethodGet, "/api/screenshot"},
	{"point", http.MethodPost, "/api/input"},
	{"tap", http.MethodPost, "/api/input"},
	{"swipe", http.MethodPost, "/api/input"},
	{"type_text", http.MethodPost, "/api/input"},
	{"key", http.MethodPost, "/api/input"},
	{"button", http.MethodPost, "/api/input"},
	{"launch", http.MethodPost, "/api/input"},
	{"open_url", http.MethodPost, "/api/input"},
	{"sequence", http.MethodPost, "/api/sequence"},
	{"logcat", http.MethodGet, "/api/logcat"},
	{"crashes", http.MethodGet, "/api/crashes"},
	{"crash", http.MethodGet, "/api/crashes/"},
	{"install", http.MethodPost, "/api/install"},
	{"camera", http.MethodPost, "/api/camera"},
	{"drop", http.MethodPost, "/api/drop"},
	{"boot", http.MethodPost, "/api/devices/boot"},
	{"release", http.MethodDelete, "/api/claims"},
	{"flags", http.MethodGet, "/api/flags"},
	{"flag", http.MethodGet, "/api/flags"},
}

// Targeted is the one field every device tool takes.
type Targeted struct {
	Target string `json:"target" jsonschema:"the target id from the targets tool, e.g. avd:Pixel_10, emulator-5554 or sim:<udid>"`
}

type uiIn struct {
	Targeted
	Interactive bool `json:"interactive,omitempty" jsonschema:"drop layout containers and nodes covered by something else"`
}

type pointIn struct {
	Targeted
	X int `json:"x" jsonschema:"device pixels, as /api/ui's center reports them"`
	Y int `json:"y" jsonschema:"device pixels"`
}

// tapIn takes a point, or a node named by its id, text or desc.
type tapIn struct {
	Targeted
	X        *int   `json:"x,omitempty" jsonschema:"device pixels, as /api/ui's center reports them; leave out when naming a node"`
	Y        *int   `json:"y,omitempty" jsonschema:"device pixels"`
	Text     string `json:"text,omitempty" jsonschema:"tap the node with this text (a button's label on Android and iOS)"`
	ID       string `json:"id,omitempty" jsonschema:"tap the node with this resource-id or accessibility identifier; a bare Android name matches any package"`
	Desc     string `json:"desc,omitempty" jsonschema:"tap the node with this content description, often an icon button's only label"`
	Contains bool   `json:"contains,omitempty" jsonschema:"match text and desc as case-insensitive substrings"`
	Index    *int   `json:"index,omitempty" jsonschema:"which of several matches, in tree order from 0; without it more than one match is an error listing them"`
	WaitMs   int    `json:"waitMs,omitempty" jsonschema:"keep looking for the node this long, up to 20000"`
}

func (in tapIn) request() httpapi.InputRequest {
	if in.Text != "" || in.ID != "" || in.Desc != "" {
		return httpapi.InputRequest{Action: "tap", Match: &target.Match{
			ID: in.ID, Text: in.Text, Desc: in.Desc, Contains: in.Contains, Index: in.Index, WaitMs: in.WaitMs,
		}}
	}
	req := httpapi.InputRequest{Action: "tap"}
	if in.X != nil {
		req.X = *in.X
	}
	if in.Y != nil {
		req.Y = *in.Y
	}
	return req
}

// stepIn is one step of a sequence: a POST /api/input body. Its numbers are
// pointers so a 0 is sent rather than dropped, which the server would read
// as a missing field.
type stepIn struct {
	Action   string        `json:"action" jsonschema:"tap, swipe, text, key, button, launch, openURL, move or wait"`
	X        *int          `json:"x,omitempty" jsonschema:"device pixels: tap, swipe start, move"`
	Y        *int          `json:"y,omitempty" jsonschema:"device pixels"`
	X2       *int          `json:"x2,omitempty" jsonschema:"swipe end, device pixels"`
	Y2       *int          `json:"y2,omitempty" jsonschema:"swipe end, device pixels"`
	Ms       *int          `json:"ms,omitempty" jsonschema:"swipe duration, or how long a wait without a match pauses (up to 20000)"`
	Text     string        `json:"text,omitempty" jsonschema:"text to type"`
	Keycode  *int          `json:"keycode,omitempty" jsonschema:"an Android KeyEvent keycode"`
	Button   string        `json:"button,omitempty"`
	Package  string        `json:"package,omitempty" jsonschema:"launch: Android package or iOS bundle ID"`
	Activity string        `json:"activity,omitempty"`
	URL      string        `json:"url,omitempty" jsonschema:"openURL: the URL"`
	Match    *target.Match `json:"match,omitempty" jsonschema:"tap or wait: the node by id, text or desc instead of x and y; a wait with a match waits for the node to appear, 10s unless waitMs says otherwise"`
}

type sequenceIn struct {
	Targeted
	Steps []stepIn `json:"steps" jsonschema:"up to 50 steps, run in order; the first that fails stops the rest"`
}

type swipeIn struct {
	Targeted
	X  int `json:"x" jsonschema:"start, device pixels"`
	Y  int `json:"y" jsonschema:"start, device pixels"`
	X2 int `json:"x2" jsonschema:"end, device pixels"`
	Y2 int `json:"y2" jsonschema:"end, device pixels"`
	Ms int `json:"ms,omitempty" jsonschema:"duration; a short one is a fling and scrolls further than asked"`
}

type textIn struct {
	Targeted
	Text string `json:"text" jsonschema:"typed into whatever has focus"`
}

type keyIn struct {
	Targeted
	Keycode int `json:"keycode" jsonschema:"an Android KeyEvent keycode, e.g. 66 Enter, 67 Backspace"`
}

type buttonIn struct {
	Targeted
	Button string `json:"button"`
}

type launchIn struct {
	Targeted
	Package  string `json:"package" jsonschema:"Android package name, or an iOS simulator's bundle ID"`
	Activity string `json:"activity,omitempty" jsonschema:"Android only: an activity, qualified or starting with '.'; empty opens the launcher activity"`
}

type urlIn struct {
	Targeted
	URL string `json:"url" jsonschema:"a deep link, universal link or https URL, handed to whatever the device has registered for it"`
}

type logcatIn struct {
	Targeted
	Lines      int    `json:"lines,omitempty" jsonschema:"how many lines at most; the server defaults to 500"`
	Since      string `json:"since,omitempty" jsonschema:"a duration back from now (5m) or an RFC 3339 time"`
	Package    string `json:"package,omitempty" jsonschema:"only this app's process"`
	Foreground bool   `json:"foreground,omitempty" jsonschema:"only the app in front"`
	Format     string `json:"format,omitempty" jsonschema:"simulators only: ndjson for one JSON object per line; text by default"`
}

type crashesIn struct {
	Targeted
	Limit int    `json:"limit,omitempty" jsonschema:"how many of the newest reports at most; the server defaults to 20"`
	Since string `json:"since,omitempty" jsonschema:"a duration back from now (5m) or an RFC 3339 time"`
}

type crashIn struct {
	Targeted
	ID string `json:"id" jsonschema:"a report id from the crashes tool"`
}

type installIn struct {
	Targeted
	Paths     []string `json:"paths" jsonschema:"APK files on the Mac running loupe mcp: one APK, or a base and its splits"`
	AllowTest bool     `json:"allowTest,omitempty" jsonschema:"allow an APK marked testOnly, as Android Studio's Run builds are"`
	Launch    bool     `json:"launch,omitempty" jsonschema:"open the app once it is installed"`
}

type cameraIn struct {
	Targeted
	Path  string `json:"path,omitempty" jsonschema:"a PNG or JPEG on the Mac running loupe mcp"`
	Clear bool   `json:"clear,omitempty" jsonschema:"give the camera back instead of setting an image"`
}

type dropIn struct {
	Targeted
	X    int    `json:"x" jsonschema:"device pixels, as /api/ui's center reports them"`
	Y    int    `json:"y" jsonschema:"device pixels, as /api/ui's center reports them"`
	Path string `json:"path" jsonschema:"a PNG, JPEG, GIF, WebP or HEIC on the Mac running loupe mcp"`
}

// addTools registers every tool in toolRoutes on s.
func addTools(s *mcp.Server, c *Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "targets",
		Description: "List every device loupe can stream or drive: Android devices and emulators, AVDs that are shut down, iOS simulators and phones. Each row has its id, state, and who holds it (claim, handsOff, drivenBy). Start here to pick a target.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return jsonResult(c.getJSON(ctx, "/api/targets", nil))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "ui",
		Description: "Read the target's view hierarchy. Each node carries center, in device pixels, which goes straight into tap. Prefer this to reading a screenshot for finding something to tap.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in uiIn) (*mcp.CallToolResult, any, error) {
		q := url.Values{"target": {in.Target}}
		if in.Interactive {
			q.Set("interactive", "1")
		}
		return jsonResult(c.getJSON(ctx, "/api/ui", q))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "screenshot",
		Description: "A PNG of the target's screen at its own resolution.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in Targeted) (*mcp.CallToolResult, any, error) {
		b, _, err := c.do(ctx, http.MethodGet, "/api/screenshot", url.Values{"target": {in.Target}}, "", nil)
		if err != nil {
			return errResult(err), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: b, MIMEType: "image/png"}}}, nil, nil
	})

	const driving = " Claims the target for this session first, so another agent cannot drive it at the same time; refused while a person is using it."

	addInput(s, c, "point", "Move this agent's cursor to a point in device pixels, without touching the device, so a person watching sees what you are about to act on. tap and swipe already point first; call this to show a target while you work something out. Claims the target like the other input tools, but is not refused while a person is using it.",
		func(in pointIn) (string, httpapi.InputRequest) {
			return in.Target, httpapi.InputRequest{Action: "move", X: in.X, Y: in.Y}
		})
	addInput(s, c, "tap", "Tap a point in device pixels, or a node named by text, id or desc: the server finds it in the view hierarchy, taps its center and answers with the node it hit. A name that matches several nodes is an error listing them; pass index to pick one."+driving,
		func(in tapIn) (string, httpapi.InputRequest) {
			return in.Target, in.request()
		})
	addInput(s, c, "swipe", "Drag from one point to another, in device pixels."+driving,
		func(in swipeIn) (string, httpapi.InputRequest) {
			return in.Target, httpapi.InputRequest{Action: "swipe", X: in.X, Y: in.Y, X2: in.X2, Y2: in.Y2, Ms: in.Ms}
		})
	addInput(s, c, "type_text", "Type text into the focused field."+driving,
		func(in textIn) (string, httpapi.InputRequest) {
			return in.Target, httpapi.InputRequest{Action: "text", Text: in.Text}
		})
	addInput(s, c, "key", "Press one key by keycode."+driving,
		func(in keyIn) (string, httpapi.InputRequest) {
			return in.Target, httpapi.InputRequest{Action: "key", Keycode: in.Keycode}
		})
	addInput(s, c, "button", "Press a device button by name: "+strings.Join(target.ButtonNames(), ", ")+"."+driving,
		func(in buttonIn) (string, httpapi.InputRequest) {
			return in.Target, httpapi.InputRequest{Action: "button", Button: in.Button}
		})
	addInput(s, c, "launch", "Open an installed app."+driving,
		func(in launchIn) (string, httpapi.InputRequest) {
			return in.Target, httpapi.InputRequest{Action: "launch", Package: in.Package, Activity: in.Activity}
		})
	addInput(s, c, "open_url", "Open a URL on the device."+driving,
		func(in urlIn) (string, httpapi.InputRequest) {
			return in.Target, httpapi.InputRequest{Action: "openURL", URL: in.URL}
		})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "logcat",
		Description: "Recent log lines, optionally narrowed to one app's process: logcat's threadtime format on Android, the unified log's compact style on an iOS simulator. package is an Android package or an iOS bundle ID. At most one of package and foreground.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in logcatIn) (*mcp.CallToolResult, any, error) {
		q := url.Values{"target": {in.Target}}
		if in.Lines > 0 {
			q.Set("lines", strconv.Itoa(in.Lines))
		}
		if in.Since != "" {
			q.Set("since", in.Since)
		}
		if in.Package != "" {
			q.Set("package", in.Package)
		}
		if in.Foreground {
			q.Set("foreground", "1")
		}
		if in.Format != "" {
			q.Set("format", in.Format)
		}
		b, _, err := c.do(ctx, http.MethodGet, "/api/logcat", q, "", nil)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(string(b)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "crashes",
		Description: "The target's crash reports, newest first, one summary row each: a simulator's or a physical iPhone's .ips reports, or an Android device's dropbox crashes, native crashes and ANRs. A simulator report can take 40s after the crash to appear.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in crashesIn) (*mcp.CallToolResult, any, error) {
		q := url.Values{"target": {in.Target}}
		if in.Limit > 0 {
			q.Set("limit", strconv.Itoa(in.Limit))
		}
		if in.Since != "" {
			q.Set("since", in.Since)
		}
		return jsonResult(c.getJSON(ctx, "/api/crashes", q))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "crash",
		Description: "One crash report as written. On a simulator an .ips: a JSON header line, then the JSON body with the exception, the termination reason and every thread's backtrace. On Android the dropbox entry: a header line, then the process, package and stack trace or ANR traces.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in crashIn) (*mcp.CallToolResult, any, error) {
		b, _, err := c.do(ctx, http.MethodGet, "/api/crashes/"+url.PathEscape(in.ID), url.Values{"target": {in.Target}}, "", nil)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(string(b)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "install",
		Description: "Android only: install APKs read from this Mac's disk, where loupe mcp runs, not the agent's. Pass a base and its splits together for an App Bundle build. With launch, opening the app is claimed and arbitrated like the input tools.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in installIn) (*mcp.CallToolResult, any, error) {
		if len(in.Paths) == 0 {
			return errResult(fmt.Errorf("paths: at least one APK")), nil, nil
		}
		body, ct, err := multipartAPKs(in.Paths)
		if err != nil {
			return errResult(err), nil, nil
		}
		q := url.Values{"target": {in.Target}}
		if in.AllowTest {
			q.Set("allowTest", "1")
		}
		send := func() (json.RawMessage, error) {
			b, _, err := c.do(ctx, http.MethodPost, "/api/install", q, ct, bytes.NewReader(body))
			return b, err
		}
		if !in.Launch {
			return jsonResult(send())
		}
		q.Set("launch", "1")
		return jsonResult(c.drive(ctx, in.Target, send))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "camera",
		Description: "Point an AVD's, iOS simulator's or physical iPhone's back camera at a PNG or JPEG read from this Mac's disk, or give the camera back with clear. On an iPhone only development-signed apps signed by a team this Mac has an Apple Development identity for can take it. The answer's applies says when an app sees it: cameraOpen (AVD, next time an app opens the camera), boot (AVD, after it is booted again through loupe), live (iOS app already running with loupe's camera shim), launch (iOS, after the app is launched through loupe, which relaunches it).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cameraIn) (*mcp.CallToolResult, any, error) {
		q := url.Values{"target": {in.Target}}
		if in.Clear {
			b, _, err := c.do(ctx, http.MethodDelete, "/api/camera", q, "", nil)
			return jsonResult(b, err)
		}
		if in.Path == "" {
			return errResult(fmt.Errorf("path: an image, or clear")), nil, nil
		}
		body, err := os.ReadFile(in.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		b, _, err := c.do(ctx, http.MethodPost, "/api/camera", q, "application/octet-stream", bytes.NewReader(body))
		return jsonResult(b, err)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "drop",
		Description: "Paste an image read from this Mac's disk into the text field at x, y on an iOS simulator, as dropping a picture onto it does: the field is tapped to raise the edit menu and its Paste tapped, and the field's value is read to confirm the image went in. A point with no Paste is an error with code noPaste; a Paste that left the field unchanged twice is pasteFailed; a simcap too old to set an image is simcapOutdated." + driving,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dropIn) (*mcp.CallToolResult, any, error) {
		if in.Path == "" {
			return errResult(fmt.Errorf("path: an image")), nil, nil
		}
		body, err := os.ReadFile(in.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		q := url.Values{"target": {in.Target}, "x": {strconv.Itoa(in.X)}, "y": {strconv.Itoa(in.Y)}}
		return jsonResult(c.drive(ctx, in.Target, func() (json.RawMessage, error) {
			b, _, err := c.do(ctx, http.MethodPost, "/api/drop", q, "application/octet-stream", bytes.NewReader(body))
			return b, err
		}))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "sequence",
		Description: "Run several inputs in one call, so a known flow (fill a form, tap through a dialog) costs one round trip instead of one per step. Each step is an input tool's body with its action named: tap (x,y or match), swipe, text, key, button, launch, openURL, move, and wait (ms, or match to wait for a node). Steps run in order and the cursor moves to each touch first; the first step that fails stops the rest, and the answer names it and holds every step's result. Read the screen again before a step whose node depends on what an earlier step opened, or use a wait with a match." + driving,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sequenceIn) (*mcp.CallToolResult, any, error) {
		return jsonResult(c.drive(ctx, in.Target, func() (json.RawMessage, error) {
			return c.postJSON(ctx, "/api/sequence", url.Values{"target": {in.Target}}, struct {
				Steps []stepIn `json:"steps"`
			}{in.Steps})
		}))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "boot",
		Description: "Boot a shut-down AVD or iOS simulator without opening a stream, and return its row once it is up. Already running returns at once.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in Targeted) (*mcp.CallToolResult, any, error) {
		return jsonResult(c.drive(ctx, in.Target, func() (json.RawMessage, error) {
			return c.postJSON(ctx, "/api/devices/boot", url.Values{"target": {in.Target}}, nil)
		}))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "release",
		Description: "Give back this session's claim on a target, so another agent can drive it. Claims are also released when loupe mcp exits, and lapse after 10 idle minutes.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in Targeted) (*mcp.CallToolResult, any, error) {
		if err := c.release(ctx, in.Target); err != nil {
			return errResult(err), nil, nil
		}
		return textResult("released " + in.Target), nil, nil
	})
}

// addInput registers one POST /api/input tool, its action fixed by build.
func addInput[In any](s *mcp.Server, c *Client, name, desc string, build func(In) (string, httpapi.InputRequest)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			t, req := build(in)
			q := url.Values{"target": {t}}
			return jsonResult(c.drive(ctx, t, func() (json.RawMessage, error) {
				// A tap naming a node has no point yet; the server points
				// once it has found one.
				if pointsFirst(req.Action) && req.Match == nil {
					// The cursor starts its trip while the action is on its
					// way, so it reaches the point about when the touch
					// lands. Only the viewers' cursor depends on it, so a
					// failed move is not the action's failure.
					_, _ = c.postJSON(ctx, "/api/input", q, inputBody(httpapi.InputRequest{Action: "move", X: req.X, Y: req.Y}))
				}
				return c.postJSON(ctx, "/api/input", q, inputBody(req))
			}))
		})
}

// inputBody encodes req with every coordinate its action requires, 0
// included. InputRequest's omitempty drops a 0, and the server refuses a
// tap at x 0 as one with no x at all. A tap naming a node sends no point.
func inputBody(req httpapi.InputRequest) map[string]any {
	b, _ := json.Marshal(req)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	if req.Match != nil {
		return body
	}
	coords := map[string]int{"x": req.X, "y": req.Y, "x2": req.X2, "y2": req.Y2}
	for _, field := range httpapi.InputRequiredFields[req.Action] {
		if v, ok := coords[field]; ok {
			body[field] = v
		}
	}
	return body
}

// pointsFirst reports whether an input tool moves the agent's cursor to its
// start point before it acts: every action that touches a point.
func pointsFirst(action string) bool {
	return action == "tap" || action == "swipe" || action == "longPress"
}

// multipartAPKs reads each path into one multipart body, the form
// /api/install takes for a base and its splits.
func multipartAPKs(paths []string) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, "", err
		}
		w, err := mw.CreateFormFile("apk", filepath.Base(p))
		if err == nil {
			_, err = io.Copy(w, f)
		}
		f.Close()
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", p, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// jsonResult hands the server's JSON back as the tool's text. An HTTP error
// is a tool error, not a protocol one, so the model reads the server's code
// (humanHeld, claimedBy, handsOff) and its retry time and can act on them.
func jsonResult(b json.RawMessage, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return errResult(err), nil, nil
	}
	return textResult(string(b)), nil, nil
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errResult(err error) *mcp.CallToolResult {
	r := textResult(err.Error())
	r.IsError = true
	return r
}
