package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/target/android"
)

// injectTarget is a launchTarget that can launch with extensions, recording
// what it was handed.
type injectTarget struct {
	launchTarget
	facts   target.LaunchFacts
	err     error
	got     [][]target.LaunchExtension
	results *target.LaunchWithResult
}

func (f *injectTarget) LaunchFacts(context.Context) (target.LaunchFacts, error) { return f.facts, nil }

func (f *injectTarget) LaunchWith(ctx context.Context, pkg, activity string, exts []target.LaunchExtension) (*target.LaunchWithResult, error) {
	f.got = append(f.got, exts)
	if f.err != nil {
		return nil, f.err
	}
	if f.results != nil {
		return f.results, nil
	}
	res := &target.LaunchWithResult{LaunchResult: target.LaunchResult{Activity: pkg + "/.Main"}}
	for _, e := range exts {
		res.With = append(res.With, target.LaunchedExtension{Name: e.Name, PID: 1234, Attached: true})
	}
	return res, nil
}

// phoneTarget is a physical iPhone, which takes no launch injection.
type phoneTarget struct{ injectTarget }

func (f *phoneTarget) Kind() target.Kind { return target.KindIOSDevice }

// stubExtension answers /launch-spec with status and body, and records each
// request it got.
type stubExtension struct {
	srv    *httptest.Server
	status int
	body   string
	reqs   []*http.Request
	bodies []string
}

func newStubExtension(t *testing.T, status int, body string) *stubExtension {
	t.Helper()
	x := &stubExtension{status: status, body: body}
	x.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		x.reqs = append(x.reqs, r)
		x.bodies = append(x.bodies, string(b))
		if x.status == http.StatusFound {
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(x.status)
		_, _ = io.WriteString(w, x.body)
	}))
	t.Cleanup(x.srv.Close)
	return x
}

func (x *stubExtension) port(t *testing.T) int {
	u, _ := url.Parse(x.srv.URL)
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// agentSpec is an Android answer naming real files.
func agentSpec(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	so, dex := filepath.Join(dir, "agent.so"), filepath.Join(dir, "agent.dex")
	for _, p := range []string{so, dex} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(map[string]any{
		"agent":   map[string]any{"so": so, "dex": dex, "options": map[string]string{"runtime": "@dex", "server": "local:ext-sock"}},
		"reverse": "ext-sock",
	})
	return string(b)
}

func newInjectServer(t *testing.T, f target.Target, exts map[string]*stubExtension, launch bool) *Server {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(f)
	s := &Server{mgr: m, token: "secret", exts: newExtensionRegistry()}
	for name, x := range exts {
		if _, err := s.exts.register(ExtensionRequest{Name: name, URL: x.srv.URL + "/base", Launch: launch}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func androidInject() *injectTarget {
	return &injectTarget{
		launchTarget: launchTarget{androidTarget: androidTarget{id: "dev1"}},
		facts:        target.LaunchFacts{Platform: target.LaunchAndroid, ABI: "arm64-v8a", SDK: 36},
	}
}

func TestLaunchWithAsksTheExtensionAndLaunches(t *testing.T) {
	x := newStubExtension(t, http.StatusOK, agentSpec(t))
	f := androidInject()
	s := newInjectServer(t, f, map[string]*stubExtension{"ext": x}, true)
	w := serveOn(s, http.MethodPost, "/api/input?target=dev1", `{"action":"launch","package":"com.example.app","with":["ext"]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp InputResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.With) != 1 || resp.With[0].Name != "ext" || resp.With[0].PID != 1234 || resp.With[0].Attached != true {
		t.Errorf("with = %+v", resp.With)
	}

	if len(x.reqs) != 1 {
		t.Fatalf("extension got %d requests", len(x.reqs))
	}
	r := x.reqs[0]
	if r.Method != http.MethodPost || r.URL.Path != "/base/launch-spec" {
		t.Errorf("extension got %s %s", r.Method, r.URL.Path)
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		t.Errorf("extension got loupe's credentials: %v", r.Header)
	}
	var asked map[string]any
	_ = json.Unmarshal([]byte(x.bodies[0]), &asked)
	want := map[string]any{"target": "dev1", "platform": "android", "package": "com.example.app", "abi": "arm64-v8a", "sdk": float64(36)}
	if !jsonEqual(asked, want) {
		t.Errorf("extension was asked %v, want %v", asked, want)
	}

	if len(f.got) != 1 || len(f.got[0]) != 1 {
		t.Fatalf("LaunchWith got %+v", f.got)
	}
	e := f.got[0][0]
	if e.Reverse != "ext-sock" || e.ReversePort != x.port(t) || e.Agent == nil || e.Agent.Options["runtime"] != "@dex" {
		t.Errorf("LaunchWith got %+v (agent %+v); want the reverse to the registered port %d", e, e.Agent, x.port(t))
	}
	if len(f.launched) != 0 {
		t.Errorf("plain Launch ran too: %v", f.launched)
	}
}

func jsonEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Every refusal is an error with a reason, and nothing is launched: not
// with the extension, and not plainly without it.
func TestLaunchWithRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  func() target.Target
		status  int
		body    string // the extension's answer
		code    int    // the extension's status
		launch  bool   // registered with launch
		request string
		want    string
	}{
		{name: "unregistered", status: http.StatusBadRequest, launch: true,
			request: `{"action":"launch","package":"com.example.app","with":["other"]}`, want: `extension "other" is not registered`},
		{name: "registered without launch", status: http.StatusBadRequest, launch: false,
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: `did not register with "launch": true`},
		{name: "named twice", status: http.StatusBadRequest, launch: true,
			request: `{"action":"launch","package":"com.example.app","with":["ext","ext"]}`, want: "named twice"},
		{name: "empty with", status: http.StatusBadRequest, launch: true,
			request: `{"action":"launch","package":"com.example.app","with":[]}`, want: "names no extension"},
		{name: "with on a tap", status: http.StatusBadRequest, launch: true,
			request: `{"action":"tap","x":1,"y":1,"with":["ext"]}`, want: "only for the launch action"},
		{name: "a target that cannot", status: http.StatusNotImplemented, launch: true,
			target:  func() target.Target { return &launchTarget{androidTarget: androidTarget{id: "dev1"}} },
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "cannot launch with extensions"},
		{name: "a physical iPhone", status: http.StatusBadRequest, launch: true,
			target:  func() target.Target { return &phoneTarget{injectTarget: *androidInject()} },
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "launch injection is simulator-only"},
		{name: "extension refuses", status: http.StatusBadGateway, launch: true, code: http.StatusInternalServerError, body: "no build for\nsdk 36",
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "extension ext: no build for sdk 36"},
		{name: "extension redirects", status: http.StatusBadGateway, launch: true, code: http.StatusFound,
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "extension ext: 302 Found"},
		{name: "not JSON", status: http.StatusBadGateway, launch: true, code: http.StatusOK, body: "<html>",
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "extension ext: launch spec"},
		{name: "nothing for the platform", status: http.StatusBadGateway, launch: true, code: http.StatusOK, body: `{}`,
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "no agent or reverse for Android"},
		{name: "env for android", status: http.StatusBadGateway, launch: true, code: http.StatusOK, body: `{"env":{"A":"b"}}`,
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "not env"},
		{name: "a bad file", status: http.StatusBadGateway, launch: true, code: http.StatusOK,
			body:    `{"agent":{"so":"relative.so","dex":"/nope.dex"}}`,
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "not a clean absolute path"},
		{name: "a bad option", status: http.StatusBadGateway, launch: true, code: http.StatusOK,
			body:    `{"reverse":"Bad Name"}`,
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "reverse"},
		{name: "a spec that is too big", status: http.StatusBadGateway, launch: true, code: http.StatusOK,
			body:    `{"reverse":"` + strings.Repeat("a", maxLaunchSpecBody) + `"}`,
			request: `{"action":"launch","package":"com.example.app","with":["ext"]}`, want: "over 64 KiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newStubExtension(t, tc.code, tc.body)
			var tg target.Target = androidInject()
			if tc.target != nil {
				tg = tc.target()
			}
			s := newInjectServer(t, tg, map[string]*stubExtension{"ext": x}, tc.launch)
			w := serveOn(s, http.MethodPost, "/api/input?target=dev1", tc.request, nil)
			var resp ErrorResponse
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if w.Code != tc.status || !strings.Contains(resp.Error, tc.want) {
				t.Errorf("status %d %q; want %d containing %q", w.Code, resp.Error, tc.status, tc.want)
			}
			switch f := tg.(type) {
			case *injectTarget:
				if len(f.got) != 0 || len(f.launched) != 0 {
					t.Errorf("launched: with %+v, plain %v", f.got, f.launched)
				}
			case *launchTarget:
				if len(f.launched) != 0 {
					t.Errorf("plain launch ran: %v", f.launched)
				}
			}
		})
	}
}

// One bad answer among several stops the launch, after the good ones were
// asked.
func TestLaunchWithStopsAtTheFirstBadAnswer(t *testing.T) {
	good := newStubExtension(t, http.StatusOK, agentSpec(t))
	bad := newStubExtension(t, http.StatusServiceUnavailable, "warming up")
	f := androidInject()
	s := newInjectServer(t, f, map[string]*stubExtension{"good": good, "bad": bad}, true)
	w := serveOn(s, http.MethodPost, "/api/input?target=dev1", `{"action":"launch","package":"com.example.app","with":["good","bad"]}`, nil)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "extension bad: warming up") {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(good.reqs) != 1 || len(f.got) != 0 {
		t.Errorf("good asked %d times, launched %+v", len(good.reqs), f.got)
	}
}

func TestLaunchWithDeviceErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		want   string
	}{
		{android.ErrNotDebuggable, http.StatusBadRequest, "com.example.app is not debuggable"},
		{target.ErrAppNotInstalled, http.StatusNotFound, "not installed"},
		{target.ErrBadLaunchSpec, http.StatusBadGateway, "bad launch spec"},
	} {
		x := newStubExtension(t, http.StatusOK, agentSpec(t))
		f := androidInject()
		f.err = tc.err
		s := newInjectServer(t, f, map[string]*stubExtension{"ext": x}, true)
		w := serveOn(s, http.MethodPost, "/api/input?target=dev1", `{"action":"launch","package":"com.example.app","with":["ext"]}`, nil)
		var resp ErrorResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if w.Code != tc.status || !strings.Contains(resp.Error, tc.want) {
			t.Errorf("%v: status %d %q; want %d containing %q", tc.err, w.Code, resp.Error, tc.status, tc.want)
		}
	}
}

func TestLaunchWithSaysRelaunched(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "loader.dylib")
	if err := os.WriteFile(lib, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"env": map[string]string{"DYLD_INSERT_LIBRARIES": lib}})
	x := newStubExtension(t, http.StatusOK, string(b))
	f := androidInject()
	f.facts = target.LaunchFacts{Platform: target.LaunchIOSSimulator, UDID: "U", IOSVersion: "27.0"}
	f.results = &target.LaunchWithResult{Relaunched: true, With: []target.LaunchedExtension{{Name: "ext", PID: 9}}}
	s := newInjectServer(t, f, map[string]*stubExtension{"ext": x}, true)
	w := serveOn(s, http.MethodPost, "/api/input?target=dev1", `{"action":"launch","package":"com.example.app","with":["ext"]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"relaunched":true`) || !strings.Contains(w.Body.String(), `"with":[{"name":"ext","pid":9}]`) {
		t.Errorf("body = %s", w.Body)
	}
	var asked map[string]any
	_ = json.Unmarshal([]byte(x.bodies[0]), &asked)
	if asked["udid"] != "U" || asked["iosVersion"] != "27.0" || asked["platform"] != "ios-simulator" {
		t.Errorf("extension was asked %v", asked)
	}
	if got := f.got[0][0].Env["DYLD_INSERT_LIBRARIES"]; got != lib {
		t.Errorf("env = %q", got)
	}
}

func TestExtensionPortDefaults(t *testing.T) {
	for raw, want := range map[string]int{
		"http://127.0.0.1:18492": 18492,
		"http://localhost":       80,
		"https://[::1]/x":        443,
	} {
		u, _ := url.Parse(raw)
		if got := extensionPort(&registeredExtension{target: u}); got != want {
			t.Errorf("%s: port %d, want %d", raw, got, want)
		}
	}
}

// A launch registration shows in the list, so a client can tell which
// extensions take part in a launch.
func TestExtensionRegistersLaunch(t *testing.T) {
	x := newExtensionRegistry()
	e, err := x.register(ExtensionRequest{Name: "ext", URL: "http://127.0.0.1:1", Launch: true})
	if err != nil || !e.Launch {
		t.Fatalf("register = %+v, %v", e, err)
	}
	if l := x.list(); len(l) != 1 || !l[0].Launch {
		t.Errorf("list = %+v", l)
	}
}
