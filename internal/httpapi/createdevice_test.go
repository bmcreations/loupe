package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target/ios"

	"github.com/bmcreations/loupe/internal/target/android"

	"github.com/bmcreations/loupe/internal/target"
)

const (
	img36 = "system-images;android-36;google_apis_playstore;arm64-v8a"
	img34 = "system-images;android-34;google_apis;arm64-v8a"
	rt27  = "com.apple.CoreSimulator.SimRuntime.iOS-27-0"
	rt271 = "com.apple.CoreSimulator.SimRuntime.iOS-27-1"
	ip17  = "com.apple.CoreSimulator.SimDeviceType.iPhone-17"
	duo   = "com.apple.CoreSimulator.SimDeviceType.iPhone-Duo"
)

// fakeTools stands in for avdmanager and xcrun.
type fakeTools struct {
	home    string
	release chan struct{}
	mu      sync.Mutex
	calls   []string // name + args
	envs    [][]string
	// simOut/simErr/simFail decide `simctl create`.
	simOut, simErr string
	simFail        bool
	// avdOut decides a failed avdmanager.
	avdFail string
	// lockConfig makes the new AVD's directory read-only, so patching fails.
	lockConfig bool
}

func (f *fakeTools) run(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	f.envs = append(f.envs, env)
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	if name == "xcrun" {
		if f.simFail {
			return []byte(f.simOut), []byte(f.simErr), errors.New("exit status 147")
		}
		return []byte("AAAA-1111\n"), nil, nil
	}
	if f.avdFail != "" {
		return []byte("[=====] 10% Loading\r\nError: " + f.avdFail + "\nnull\n"), nil, errors.New("exit status 1")
	}
	dir := filepath.Join(f.home, args[3]+".avd")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.ini"), []byte("hw.keyboard=no\nhw.device.name=pixel_9\n"), 0o644)
	os.WriteFile(filepath.Join(f.home, args[3]+".ini"), []byte("path="+dir+"\n"), 0o644)
	if f.lockConfig {
		os.Chmod(dir, 0o555)
	}
	return nil, nil, nil
}

type devEnv struct {
	s     *Server
	tools *fakeTools
	home  string
	trace chan map[string]any
	avds  *[]string
	// avdmanager is the stub NewDeviceCreator checked; creates go to tools.
	avdmanager string
}

// avdManagerStub writes an avdmanager that runs script under /bin/sh, for
// NewDeviceCreator's check that avdmanager runs.
func avdManagerStub(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "avdmanager")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func devServer(t *testing.T, withAndroid, withIOS bool) *devEnv {
	t.Helper()
	home := t.TempDir()
	tools := &fakeTools{home: home}
	var mu sync.Mutex
	var registered []string
	trace := make(chan map[string]any, 8)
	cfg := DeviceCreatorConfig{Manager: NewManager(target.Options{}), SDK: t.TempDir(), AVDHome: home,
		Trace: func(m map[string]any) { trace <- m }}
	if withAndroid {
		cfg.AVDManager = avdManagerStub(t, "exit 0")
		cfg.RegisterAVDs = func(_ time.Time, a []android.AVD) {
			mu.Lock()
			defer mu.Unlock()
			for _, x := range a {
				registered = append(registered, x.Name)
			}
		}
	}
	if withIOS {
		cfg.Simcap = "/simcap"
	}
	c := NewDeviceCreator(cfg)
	if c == nil {
		return &devEnv{s: &Server{mgr: cfg.Manager}}
	}
	c.run = tools.run
	c.images = func() []android.SystemImage {
		return []android.SystemImage{
			{ID: img36, API: "36", Family: "google_apis_playstore", Tags: []string{"google_apis_playstore"}, TagDisplay: "Google Play"},
			{ID: img34, API: "34", Family: "google_apis", Tags: []string{"google_apis"}, TagDisplay: "Google APIs"},
		}
	}
	c.devices = func() ([]android.AVDDevice, error) {
		return []android.AVDDevice{
			{ID: "pixel_9", Name: "Pixel 9", Group: target.GroupPhone, DiagonalIn: 6.24, Width: 1080, Height: 2424},
			{ID: "pixel_fold", Name: "Pixel Fold", Group: target.GroupFoldable, MinAPI: 35, DiagonalIn: 7.58, Width: 2208, Height: 1840},
		}, nil
	}
	c.runtimes = func(context.Context) ([]ios.SimRuntime, error) {
		return []ios.SimRuntime{
			{ID: rt27, Name: "iOS 27.0", Version: "27.0", Types: []ios.SimDeviceType{{ID: ip17, Name: "iPhone 17"}}},
			{ID: rt271, Name: "iOS 27.1", Version: "27.1", Types: []ios.SimDeviceType{{ID: duo, Name: "iPhone Duo"}}},
		}, nil
	}
	c.sizes = func(context.Context, []ios.SimDeviceType) map[string]ios.SimTypeInfo {
		return map[string]ios.SimTypeInfo{
			ip17: {Group: target.GroupPhone, Size: `6.3" · 1206×2622`},
			duo:  {Group: target.GroupFoldable, Size: `7.6" · 2853×2007`},
		}
	}
	c.listSims = func(context.Context) ([]ios.SimInfo, error) {
		return []ios.SimInfo{{UDID: "AAAA-1111", Name: "Fresh", OSVersion: "27.0", State: "Shutdown"}}, nil
	}
	s := &Server{mgr: cfg.Manager}
	s.SetDeviceCreator(c)
	return &devEnv{s: s, tools: tools, home: home, trace: trace, avds: &registered, avdmanager: cfg.AVDManager}
}

func decodeOptions(t *testing.T, s *Server) DeviceOptionsResponse {
	t.Helper()
	rec := do(s, http.MethodGet, "/api/devices/options", "")
	if rec.Code != 200 {
		t.Fatalf("options = %d %s", rec.Code, rec.Body)
	}
	var r DeviceOptionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDeviceOptionsShape(t *testing.T) {
	e := devServer(t, true, true)
	r := decodeOptions(t, e.s)
	if len(r.Platforms) != 2 || r.Platforms[0].ID != "android" || r.Platforms[1].ID != "ios" || r.ProfilesFallback {
		t.Fatalf("platforms = %+v", r.Platforms)
	}
	a := r.Platforms[0]
	if a.OS[0].ID != img36 || a.OS[0].Label != "Android 16" || a.OS[0].Detail != "API 36 · Google Play" {
		t.Errorf("os[0] = %+v", a.OS[0])
	}
	if a.Devices[0].ID != "pixel_9" || a.Devices[0].Group != "phone" || a.Devices[0].Size != `6.2" · 1080×2424` {
		t.Errorf("devices[0] = %+v", a.Devices[0])
	}
	// pixel_fold needs API 35 here, so API 34 pairs only the phone.
	if got := a.Pairs[img36]; len(got) != 2 {
		t.Errorf("pairs[36] = %v", got)
	}
	if got := a.Pairs[img34]; len(got) != 1 || got[0] != "pixel_9" {
		t.Errorf("pairs[34] = %v", got)
	}
	i := r.Platforms[1]
	if got := i.Pairs[rt271]; len(got) != 1 || got[0] != duo {
		t.Errorf("27.1 pairs = %v, want only the Duo", got)
	}
	if got := i.Pairs[rt27]; len(got) != 1 || got[0] != ip17 {
		t.Errorf("27.0 pairs = %v", got)
	}
	if a.NameRule == "" || i.NameRule == "" || r.Job != nil {
		t.Errorf("nameRule/job: %q %q %v", a.NameRule, i.NameRule, r.Job)
	}
}

func TestDeviceOptionsListsAPlatformWithNoImages(t *testing.T) {
	e := devServer(t, true, false)
	e.s.devs.images = func() []android.SystemImage { return nil }
	raw := do(e.s, http.MethodGet, "/api/devices/options", "").Body.String()
	if !strings.Contains(raw, `"os":[]`) {
		t.Errorf("want an empty os list, got %s", raw)
	}
}

func TestDeviceOptions404AndProfilesFallback(t *testing.T) {
	if rec := do(devServer(t, false, false).s, http.MethodGet, "/api/devices/options", ""); rec.Code != 404 {
		t.Errorf("nothing can create = %d", rec.Code)
	}
	// avdmanager absent, android CLI present, iOS available.
	e := devServer(t, false, true)
	e.s.SetAVDCreator(testAVDCreator(e.home, nil))
	r := decodeOptions(t, e.s)
	if !r.ProfilesFallback || len(r.Platforms) != 1 || r.Platforms[0].ID != "ios" {
		t.Errorf("fallback = %v platforms = %+v", r.ProfilesFallback, r.Platforms)
	}
	// CLI only: 200 with no platforms.
	only := &Server{mgr: NewManager(target.Options{})}
	only.SetAVDCreator(testAVDCreator(t.TempDir(), nil))
	r = decodeOptions(t, only)
	if !r.ProfilesFallback || len(r.Platforms) != 0 {
		t.Errorf("cli only = %+v", r)
	}
}

// avdmanager is a script that starts Java. A Mac set up by the android CLI
// alone can keep cmdline-tools from an old Android Studio and have no Java
// runtime: avdmanager is found, a create through it fails, and New device has
// to offer the CLI's profiles instead. The stub prints what that Mac's
// avdmanager printed, and exits 1 as it did.
func TestAVDManagerWithoutJavaLeavesAndroidToTheCLI(t *testing.T) {
	home := t.TempDir()
	stub := avdManagerStub(t, `echo "$0: line 173: test: : integer expression expected" >&2
echo "The operation couldn’t be completed. Unable to locate a Java Runtime." >&2
echo "Please visit http://www.java.com for information on installing Java." >&2
exit 1`)
	var mu sync.Mutex
	var registered []string
	register := func(_ time.Time, a []android.AVD) {
		mu.Lock()
		defer mu.Unlock()
		for _, x := range a {
			registered = append(registered, x.Name)
		}
	}
	mgr := NewManager(target.Options{})
	s := &Server{mgr: mgr}
	if c := NewDeviceCreator(DeviceCreatorConfig{Manager: mgr, SDK: t.TempDir(), AVDManager: stub, AVDHome: home, RegisterAVDs: register}); c != nil {
		s.SetDeviceCreator(c)
	}
	avds := testAVDCreator(home, register)
	avds.run = (&fakeCLI{home: home}).run
	s.SetAVDCreator(avds)

	r := decodeOptions(t, s)
	if !r.ProfilesFallback || len(r.Platforms) != 0 {
		t.Fatalf("profilesFallback = %v, platforms = %+v; want the CLI's profiles and no avdmanager platform", r.ProfilesFallback, r.Platforms)
	}
	if rec := do(s, http.MethodPost, "/api/avds", `{"profile":"medium_phone"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("CLI create = %d %s", rec.Code, rec.Body)
	}
	waitFor(t, func() bool { row, ok := targetRow(t, s, "avd:medium_phone"); return !ok || row.State != stateCreating })
	if row, ok := targetRow(t, s, "avd:medium_phone"); ok {
		t.Errorf("row after create = %+v", row)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(registered, "medium_phone") {
		t.Errorf("registered %v, want medium_phone", registered)
	}
}

// A failed create's row has no target behind it, so DELETE cannot remove it.
// It stays until the next create on either route, or a restart, since the
// slot is only in memory.
func TestAFailedCreateRowStaysUntilTheNextCreate(t *testing.T) {
	e := devServer(t, true, false)
	e.tools.avdFail = "Unable to locate a Java Runtime."
	post(e, `{"platform":"android","os":"`+img36+`","device":"pixel_9","name":"x"}`)
	waitFor(t, func() bool { r, ok := targetRow(t, e.s, "avd:x"); return ok && r.State == stateCreateFailed })
	if rec := do(e.s, http.MethodDelete, "/api/devices?target=avd:x", ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete the failed row = %d %s", rec.Code, rec.Body)
	}
	if _, ok := targetRow(t, e.s, "avd:x"); !ok {
		t.Fatal("the failed row went before another create")
	}
	avds := testAVDCreator(e.home, nil)
	avds.run = (&fakeCLI{home: e.home}).run
	e.s.SetAVDCreator(avds)
	if rec := do(e.s, http.MethodPost, "/api/avds", `{"profile":"small_phone"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("CLI create = %d %s", rec.Code, rec.Body)
	}
	if _, ok := targetRow(t, e.s, "avd:x"); ok {
		t.Error("the failed row outlived the next create")
	}
	// The create writes into home; let it end before the test removes it.
	waitFor(t, func() bool { row, ok := targetRow(t, e.s, "avd:small_phone"); return !ok || row.State != stateCreating })
}

func post(e *devEnv, body string) int {
	return do(e.s, http.MethodPost, "/api/devices", body).Code
}

func TestCreateDeviceRejectsBadRequests(t *testing.T) {
	e := devServer(t, true, true)
	for name, body := range map[string]string{
		"platform":     `{"platform":"tv","os":"x","device":"y","name":"n"}`,
		"os":           `{"platform":"android","os":"nope","device":"pixel_9","name":"n"}`,
		"device":       `{"platform":"android","os":"` + img36 + `","device":"nope","name":"n"}`,
		"pair":         `{"platform":"android","os":"` + img34 + `","device":"pixel_fold","name":"n"}`,
		"android name": `{"platform":"android","os":"` + img36 + `","device":"pixel_9","name":"has space"}`,
		"dash name":    `{"platform":"android","os":"` + img36 + `","device":"pixel_9","name":"--force"}`,
		"dot name":     `{"platform":"android","os":"` + img36 + `","device":"pixel_9","name":".."}`,
		"dash only":    `{"platform":"android","os":"` + img36 + `","device":"pixel_9","name":"-foo"}`,
		"ios pair":     `{"platform":"ios","os":"` + rt27 + `","device":"` + duo + `","name":"n"}`,
		"ios name":     `{"platform":"ios","os":"` + rt27 + `","device":"` + ip17 + `","name":"a/b"}`,
		"ios empty":    `{"platform":"ios","os":"` + rt27 + `","device":"` + ip17 + `","name":""}`,
		"json":         `nope`,
	} {
		if code := post(e, body); code != 400 {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	if len(e.tools.calls) != 0 {
		t.Errorf("a refused request ran %v", e.tools.calls)
	}
}

func TestCreateAndroidDevice(t *testing.T) {
	e := devServer(t, true, false)
	rec := do(e.s, http.MethodPost, "/api/devices", `{"platform":"android","os":"`+img36+`","device":"pixel_9","name":"Pixel_9_API_36"}`)
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"id":"avd:Pixel_9_API_36"`) {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	waitFor(t, func() bool { return decodeOptions(t, e.s).Job.State == "done" })
	job := decodeOptions(t, e.s).Job
	if job.CreatedID != "avd:Pixel_9_API_36" || job.Platform != "android" {
		t.Errorf("job = %+v", job)
	}
	e.tools.mu.Lock()
	call, env := e.tools.calls[0], e.tools.envs[0]
	e.tools.mu.Unlock()
	if !strings.HasPrefix(call, e.avdmanager+" create avd -n Pixel_9_API_36 -k "+img36+" -d pixel_9") || strings.Contains(call, "--force") {
		t.Errorf("call = %q", call)
	}
	if len(env) != 1 || env[0] != "ANDROID_AVD_HOME="+e.home {
		t.Errorf("env = %v", env)
	}
	ini, _ := os.ReadFile(filepath.Join(e.home, "Pixel_9_API_36.avd", "config.ini"))
	for _, want := range []string{"hw.keyboard=yes", "hw.gpu.enabled=yes", "PlayStore.enabled=true", "avd.ini.displayname=Pixel_9_API_36", "hw.device.name=pixel_9"} {
		if !strings.Contains(string(ini), want) {
			t.Errorf("config.ini lacks %s:\n%s", want, ini)
		}
	}
	if len(*e.avds) != 1 || (*e.avds)[0] != "Pixel_9_API_36" {
		t.Errorf("registered = %v", *e.avds)
	}
	if _, ok := targetRow(t, e.s, "avd:Pixel_9_API_36"); ok {
		t.Error("the pending row stayed after success")
	}
	rec2 := (<-e.trace)
	if rec2["kind"] != "agent" || rec2["action"] != "create" || rec2["platform"] != "android" || rec2["outcome"] != "ok" || rec2["name"] != "Pixel_9_API_36" {
		t.Errorf("trace = %v", rec2)
	}
	// Same name again: the AVD exists now.
	if code := post(e, `{"platform":"android","os":"`+img36+`","device":"pixel_9","name":"Pixel_9_API_36"}`); code != 409 {
		t.Errorf("duplicate = %d", code)
	}
}

func TestCreateAndroidFailureKeepsTheToolsMessage(t *testing.T) {
	e := devServer(t, true, false)
	e.tools.avdFail = "Android Virtual Device 'x' already exists."
	post(e, `{"platform":"android","os":"`+img36+`","device":"pixel_9","name":"x"}`)
	waitFor(t, func() bool { r, ok := targetRow(t, e.s, "avd:x"); return ok && r.State == "createFailed" })
	row, _ := targetRow(t, e.s, "avd:x")
	if !strings.Contains(row.Reason, "already exists") || strings.Contains(row.Reason, "null") || strings.Contains(row.Reason, "Loading") {
		t.Errorf("reason = %q", row.Reason)
	}
	job := decodeOptions(t, e.s).Job
	if job.State != "createFailed" || !strings.Contains(job.Error, "already exists") {
		t.Errorf("job = %+v", job)
	}
	if m := <-e.trace; m["outcome"] != "failed" || m["error"] == "" {
		t.Errorf("trace = %v", m)
	}
}

func TestCreateSimulatorRowThenSyncsTheManager(t *testing.T) {
	e := devServer(t, false, true)
	e.tools.release = make(chan struct{})
	rec := do(e.s, http.MethodPost, "/api/devices", `{"platform":"ios","os":"`+rt27+`","device":"`+ip17+`","name":"Fresh"}`)
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"id":"sim-new:Fresh"`) {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	row, ok := targetRow(t, e.s, "sim-new:Fresh")
	if !ok || row.State != "creating" || row.Kind != string(target.KindSimulator) {
		t.Fatalf("row = %+v ok=%v", row, ok)
	}
	// One at a time, from either creator.
	if code := post(e, `{"platform":"ios","os":"`+rt27+`","device":"`+ip17+`","name":"Other"}`); code != 409 {
		t.Errorf("second create = %d", code)
	}
	close(e.tools.release)
	waitFor(t, func() bool { return decodeOptions(t, e.s).Job.State == "done" })
	job := decodeOptions(t, e.s).Job
	if job.CreatedID != "sim:AAAA-1111" {
		t.Errorf("job = %+v", job)
	}
	e.tools.mu.Lock()
	defer e.tools.mu.Unlock()
	if call := e.tools.calls[0]; call != "xcrun simctl create Fresh "+ip17+" "+rt27 {
		t.Errorf("call = %q", call)
	}
	if _, ok := targetRow(t, e.s, "sim-new:Fresh"); ok {
		t.Error("pending row stayed")
	}
	if _, ok := targetRow(t, e.s, "sim:AAAA-1111"); !ok {
		t.Error("new simulator is not a target")
	}
	if names := decodeOptions(t, e.s).ExistingNames["ios"]; len(names) != 1 || names[0] != "Fresh" {
		t.Errorf("existing ios = %v", names)
	}
	// Names are compared without case on iOS.
	if code := post(e, `{"platform":"ios","os":"`+rt27+`","device":"`+ip17+`","name":"fresh"}`); code != 409 {
		t.Errorf("case-insensitive duplicate = %d", code)
	}
}

func TestCreateSimulatorFailureUsesStdoutMessage(t *testing.T) {
	e := devServer(t, false, true)
	e.tools.simFail = true
	e.tools.simOut = "Unable to create a device for device type: iPhone 17 and runtime: iOS 27.1\n"
	e.tools.simErr = "An error was encountered processing the command (domain=com.apple.CoreSimulator.SimError, code=403):\nIncompatible device\n"
	// 27.1 is not paired with the iPhone 17 in the options, so go around the
	// pair check by pairing it for this test.
	e.s.devs.runtimes = func(context.Context) ([]ios.SimRuntime, error) {
		return []ios.SimRuntime{{ID: rt271, Name: "iOS 27.1", Version: "27.1", Types: []ios.SimDeviceType{{ID: ip17, Name: "iPhone 17"}}}}, nil
	}
	post(e, `{"platform":"ios","os":"`+rt271+`","device":"`+ip17+`","name":"Bad"}`)
	waitFor(t, func() bool { r, ok := targetRow(t, e.s, "sim-new:Bad"); return ok && r.State == "createFailed" })
	row, _ := targetRow(t, e.s, "sim-new:Bad")
	if !strings.Contains(row.Reason, "Unable to create a device for device type") || !strings.Contains(row.Reason, "Incompatible device") {
		t.Errorf("reason = %q", row.Reason)
	}
}

func TestCreationIsOneAtATimeAcrossAvdsAndDevices(t *testing.T) {
	e := devServer(t, true, true)
	cli := &fakeCLI{home: e.home, release: make(chan struct{})}
	avds := testAVDCreator(e.home, nil)
	avds.run = cli.run
	e.s.SetAVDCreator(avds)
	if rec := do(e.s, http.MethodPost, "/api/avds", `{"profile":"small_phone"}`); rec.Code != 202 {
		t.Fatalf("avds create = %d %s", rec.Code, rec.Body)
	}
	rec := do(e.s, http.MethodPost, "/api/devices", `{"platform":"ios","os":"`+rt27+`","device":"`+ip17+`","name":"X"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), ErrCodeCreating) {
		t.Errorf("devices during avds = %d %s", rec.Code, rec.Body)
	}
	close(cli.release)
	waitFor(t, func() bool { _, ok := targetRow(t, e.s, "avd:small_phone"); return !ok })
	// And the other way round.
	e.tools.release = make(chan struct{})
	post(e, `{"platform":"ios","os":"`+rt27+`","device":"`+ip17+`","name":"Y"}`)
	if rec := do(e.s, http.MethodPost, "/api/avds", `{"profile":"medium_phone"}`); rec.Code != 409 {
		t.Errorf("avds during devices = %d %s", rec.Code, rec.Body)
	}
	close(e.tools.release)
}

func TestExistingAndroidNamesIncludeAHalfMadeAVD(t *testing.T) {
	e := devServer(t, true, false)
	os.WriteFile(filepath.Join(e.home, "Ghost.ini"), []byte("path=/nowhere\n"), 0o644)
	if code := post(e, `{"platform":"android","os":"`+img36+`","device":"pixel_9","name":"Ghost"}`); code != 409 {
		t.Errorf("half-made = %d", code)
	}
}

// avdmanager compares names with equalsIgnoreCase, so a case-only difference
// is the same AVD.
func TestCreateAndroidNameCollidesWithoutCase(t *testing.T) {
	e := devServer(t, true, false)
	os.WriteFile(filepath.Join(e.home, "Pixel_10.ini"), []byte("path="+filepath.Join(e.home, "Pixel_10.avd")+"\n"), 0o644)
	rec := do(e.s, http.MethodPost, "/api/devices", `{"platform":"android","os":"`+img36+`","device":"pixel_9","name":"pixel_10"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), ErrCodeAVDExists) {
		t.Fatalf("case-only duplicate = %d %s", rec.Code, rec.Body)
	}
	if len(e.tools.calls) != 0 {
		t.Errorf("ran %v", e.tools.calls)
	}
}

// A config.ini that cannot be patched leaves a working AVD: the job is done,
// with a warning, and the trace has the cause.
func TestCreateAndroidPatchFailureIsAWarning(t *testing.T) {
	e := devServer(t, true, false)
	e.tools.lockConfig = true
	t.Cleanup(func() { os.Chmod(filepath.Join(e.home, "Locked.avd"), 0o755) })
	post(e, `{"platform":"android","os":"`+img36+`","device":"pixel_9","name":"Locked"}`)
	waitFor(t, func() bool { return decodeOptions(t, e.s).Job.State == "done" })
	job := decodeOptions(t, e.s).Job
	if job.Error != "" || job.CreatedID != "avd:Locked" || !strings.Contains(job.Warning, "config.ini could not be updated") {
		t.Errorf("job = %+v", job)
	}
	if m := <-e.trace; m["outcome"] != "ok" || m["patchError"] == nil {
		t.Errorf("trace = %v", m)
	}
}

func TestAVDManagerSDKFollowsTheBinary(t *testing.T) {
	for avdmanager, want := range map[string]string{
		"/other/sdk/cmdline-tools/latest/bin/avdmanager": "/other/sdk",
		"/sdk/avdmanager":           "/mine",
		"/usr/local/bin/avdmanager": "/mine",
	} {
		if got := avdManagerSDK(avdmanager, "/mine"); got != want {
			t.Errorf("avdManagerSDK(%q) = %q, want %q", avdmanager, got, want)
		}
	}
}

// A failed listing after a successful create is traced, not dropped.
func TestCreateSimulatorTracesAFailedListing(t *testing.T) {
	e := devServer(t, false, true)
	e.s.devs.listSims = func(context.Context) ([]ios.SimInfo, error) { return nil, errors.New("simctl timed out") }
	post(e, `{"platform":"ios","os":"`+rt27+`","device":"`+ip17+`","name":"Fresh"}`)
	var sawSync bool
	for i := 0; i < 2; i++ {
		select {
		case m := <-e.trace:
			if m["syncError"] == "simctl timed out" {
				sawSync = true
			}
		case <-time.After(3 * time.Second):
		}
	}
	if !sawSync {
		t.Error("no syncError in the trace")
	}
}
