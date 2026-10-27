package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// androidTarget is an Installer and a Logcatter with canned answers.
type androidTarget struct {
	id string

	installErr error
	gotAPK     []byte
	gotAPKs    [][]byte
	gotPaths   []string
	gotOpts    target.InstallOptions

	logText string
	logPID  int
	logErr  error
	gotQ    target.LogcatQuery
}

func (f *androidTarget) ID() string        { return f.id }
func (f *androidTarget) Kind() target.Kind { return target.KindDevice }
func (f *androidTarget) Label() string     { return f.id }
func (f *androidTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	return &stubStream{frames: make(chan target.Frame)}, nil
}

func (f *androidTarget) Install(ctx context.Context, apks []string, opts target.InstallOptions) (*target.InstallResult, error) {
	f.gotOpts = opts
	f.gotPaths = apks
	f.gotAPKs = nil
	for _, p := range apks {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read staged apk: %w", err)
		}
		f.gotAPKs = append(f.gotAPKs, b)
	}
	f.gotAPK = f.gotAPKs[0]
	if f.installErr != nil {
		return nil, f.installErr
	}
	return &target.InstallResult{Output: "Performing Streamed Install\nSuccess"}, nil
}

func (f *androidTarget) Logcat(ctx context.Context, q target.LogcatQuery) (*target.Logcat, error) {
	f.gotQ = q
	if f.logErr != nil {
		return nil, f.logErr
	}
	return &target.Logcat{PID: f.logPID, Package: q.Package, ReadCloser: io.NopCloser(strings.NewReader(f.logText))}, nil
}

const apk = "PK\x03\x04rest of the zip"

// serveAgent sends a request through Handler, so the route table and the
// auth middleware are both on the path.
func serveAgent(t *testing.T, tg target.Target, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(tg)
	s := &Server{mgr: m, token: "secret"}
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestInstallAndLogcatAreBehindAuth(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/install?target=dev1"},
		{http.MethodGet, "/api/logcat?target=dev1"},
	} {
		f := &androidTarget{id: "dev1"}
		m := NewManager(target.Options{})
		m.Register(f)
		s := &Server{mgr: m, token: "secret"}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(c.method, c.path, strings.NewReader(apk)))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no token = %d; want 401", c.method, c.path, w.Code)
		}
		if f.gotAPK != nil || f.gotQ != (target.LogcatQuery{}) {
			t.Errorf("%s %s reached the target without a token", c.method, c.path)
		}
	}
}

func TestInstallAndLogcatMethods(t *testing.T) {
	if w := serveAgent(t, &androidTarget{id: "dev1"}, http.MethodGet, "/api/install?target=dev1", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/install = %d; want 405", w.Code)
	}
	if w := serveAgent(t, &androidTarget{id: "dev1"}, http.MethodPost, "/api/logcat?target=dev1", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/logcat = %d; want 405", w.Code)
	}
}

// A target with no Android behind it (stubTarget stands in for synthetic
// and a simulator, neither of which implements either interface) answers
// 501, the way /api/ui and /api/screenshot do.
func TestInstallAndLogcatAre501WithoutTheCapability(t *testing.T) {
	w := serveAgent(t, &stubTarget{}, http.MethodPost, "/api/install?target=stub", strings.NewReader(apk))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("install = %d %s; want 501", w.Code, w.Body)
	}
	w = serveAgent(t, &stubTarget{}, http.MethodGet, "/api/logcat?target=stub", nil)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("logcat = %d %s; want 501", w.Code, w.Body)
	}
}

func TestInstallSuccess(t *testing.T) {
	f := &androidTarget{id: "dev1"}
	w := serveAgent(t, f, http.MethodPost, "/api/install?target=dev1", strings.NewReader(apk))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp InstallResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Bytes != int64(len(apk)) || !strings.HasSuffix(resp.Output, "Success") || resp.Code != "" {
		t.Errorf("response = %+v", resp)
	}
	if f.gotOpts.AllowTest {
		t.Error("AllowTest set without ?allowTest=1")
	}
	// The zip-signature peek must not eat the head of the file.
	if string(f.gotAPK) != apk {
		t.Errorf("target got %q; want the whole body", f.gotAPK)
	}
}

// A refusal is the package manager's answer, not a gateway failure, so it
// is a 422 carrying the INSTALL_FAILED_* code a caller branches on.
func TestInstallRefusedIs422WithTheCode(t *testing.T) {
	f := &androidTarget{id: "dev1", installErr: &target.InstallError{
		Code: "INSTALL_FAILED_UPDATE_INCOMPATIBLE", Reason: "signatures do not match",
		Output: "Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: signatures do not match]",
	}}
	w := serveAgent(t, f, http.MethodPost, "/api/install?target=dev1", strings.NewReader(apk))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp InstallResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OK || resp.Code != "INSTALL_FAILED_UPDATE_INCOMPATIBLE" || resp.Reason != "signatures do not match" || resp.Output == "" {
		t.Errorf("response = %+v", resp)
	}
}

func TestInstallAdbFailureIs502(t *testing.T) {
	f := &androidTarget{id: "dev1", installErr: errors.New("adb install: exit status 1: device offline")}
	w := serveAgent(t, f, http.MethodPost, "/api/install?target=dev1", strings.NewReader(apk))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d: %s", w.Code, w.Body)
	}
}

func TestInstallRejectsABodyThatIsNotAZip(t *testing.T) {
	for _, body := range []string{"", "PK", `{"apk":"x"}`} {
		f := &androidTarget{id: "dev1"}
		w := serveAgent(t, f, http.MethodPost, "/api/install?target=dev1", strings.NewReader(body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d; want 400", body, w.Code)
		}
		if f.gotAPK != nil {
			t.Errorf("body %q reached the target", body)
		}
	}
}

func TestInstallSizeLimit(t *testing.T) {
	defer func(n int64) { maxInstallBytes = n }(maxInstallBytes)
	maxInstallBytes = 64
	big := apk + strings.Repeat("x", 100)

	// A declared length is refused before the target is asked.
	f := &androidTarget{id: "dev1"}
	w := serveAgent(t, f, http.MethodPost, "/api/install?target=dev1", strings.NewReader(big))
	if w.Code != http.StatusRequestEntityTooLarge || f.gotAPK != nil {
		t.Errorf("declared length: status %d, target read %d bytes; want 413 and nothing read", w.Code, len(f.gotAPK))
	}

	// A chunked body has no length and is cut off mid-copy.
	f = &androidTarget{id: "dev1"}
	r := io.MultiReader(strings.NewReader(big)) // not a *strings.Reader, so no ContentLength
	w = serveAgent(t, f, http.MethodPost, "/api/install?target=dev1", r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked: status %d %s; want 413", w.Code, w.Body)
	}
}

func TestLogcatQueryParsing(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	ok := []struct {
		query string
		want  target.LogcatQuery
	}{
		{"", target.LogcatQuery{Lines: defaultLogcatLines}},
		{"lines=20&pid=959", target.LogcatQuery{Lines: 20, PID: 959}},
		{"since=5m", target.LogcatQuery{Lines: defaultLogcatLines, Since: now.Add(-5 * time.Minute)}},
		{"since=2026-09-28T11:00:00Z&package=com.android.settings", target.LogcatQuery{
			Lines: defaultLogcatLines, Since: now.Add(-time.Hour), Package: "com.android.settings",
		}},
		{"foreground=1&follow=1", target.LogcatQuery{Lines: defaultLogcatLines, Foreground: true, Follow: true}},
		{"format=ndjson", target.LogcatQuery{Lines: defaultLogcatLines, Format: target.LogFormatNDJSON}},
		{"format=text", target.LogcatQuery{Lines: defaultLogcatLines}},
		{"package=dev.bmc-creations.app", target.LogcatQuery{Lines: defaultLogcatLines, Package: "dev.bmc-creations.app"}},
	}
	for _, c := range ok {
		v, _ := url.ParseQuery(c.query)
		got, err := logcatQuery(v, now)
		if err != nil {
			t.Errorf("%q: %v", c.query, err)
			continue
		}
		if !got.Since.Equal(c.want.Since) {
			t.Errorf("%q: since %v; want %v", c.query, got.Since, c.want.Since)
		}
		got.Since, c.want.Since = time.Time{}, time.Time{}
		if got != c.want {
			t.Errorf("%q = %+v; want %+v", c.query, got, c.want)
		}
	}
	for _, bad := range []string{
		"lines=0", "lines=abc", "lines=999999", "pid=-1", "pid=x",
		"since=yesterday", "since=-5m", "package=com.x%3Breboot", "package=com.x%27%20id", "format=xml",
		"pid=1&package=com.a.b", "pid=1&foreground=1",
	} {
		v, _ := url.ParseQuery(bad)
		if _, err := logcatQuery(v, now); err == nil {
			t.Errorf("%q parsed; want an error", bad)
		}
	}
}

func TestLogcatDumpIsTextWithTheProcessNamed(t *testing.T) {
	f := &androidTarget{id: "dev1", logText: "09-28 12:00:00.000  959  959 I tag: hi\n", logPID: 959}
	w := serveAgent(t, f, http.MethodGet, "/api/logcat?target=dev1&package=com.android.settings", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if w.Header().Get("X-Loupe-Pid") != "959" || w.Header().Get("X-Loupe-Package") != "com.android.settings" {
		t.Errorf("headers = %v", w.Header())
	}
	if w.Body.String() != f.logText {
		t.Errorf("body = %q", w.Body)
	}
}

// -T has no line limit, so a since dump is trimmed to ?lines= here.
func TestLogcatSinceIsTrimmedToLines(t *testing.T) {
	f := &androidTarget{id: "dev1", logText: "a\nb\nc\nd\n"}
	w := serveAgent(t, f, http.MethodGet, "/api/logcat?target=dev1&since=1m&lines=2", nil)
	if w.Body.String() != "c\nd\n" {
		t.Errorf("body = %q; want the last two lines", w.Body)
	}
}

func TestLogcatAppNotRunningIs409(t *testing.T) {
	f := &androidTarget{id: "dev1", logErr: fmt.Errorf("%w: com.a.b", target.ErrAppNotRunning)}
	w := serveAgent(t, f, http.MethodGet, "/api/logcat?target=dev1&package=com.a.b", nil)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d: %s", w.Code, w.Body)
	}
}

func TestLogcatUnsupportedQueryIs422(t *testing.T) {
	f := &androidTarget{id: "dev1", logErr: fmt.Errorf("%w: logcat has no ndjson format", target.ErrLogQueryUnsupported)}
	w := serveAgent(t, f, http.MethodGet, "/api/logcat?target=dev1&format=ndjson", nil)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "notSupported") {
		t.Errorf("status = %d: %s; want 422 notSupported", w.Code, w.Body)
	}
}

func TestLogcatNDJSONIsServedAsNDJSON(t *testing.T) {
	f := &androidTarget{id: "dev1", logText: `{"eventMessage":"hi"}` + "\n"}
	w := serveAgent(t, f, http.MethodGet, "/api/logcat?target=dev1&format=ndjson", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q; want application/x-ndjson", ct)
	}
	if f.gotQ.Format != target.LogFormatNDJSON {
		t.Errorf("Format = %q; want ndjson", f.gotQ.Format)
	}
}

func TestLogcatFollowStreams(t *testing.T) {
	f := &androidTarget{id: "dev1", logText: "one\ntwo\n"}
	w := serveAgent(t, f, http.MethodGet, "/api/logcat?target=dev1&follow=1", nil)
	if w.Code != http.StatusOK || w.Body.String() != "one\ntwo\n" || !w.Flushed {
		t.Errorf("status %d body %q flushed %v", w.Code, w.Body, w.Flushed)
	}
	if !f.gotQ.Follow {
		t.Error("the target was not asked to follow")
	}
}

func TestLastLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\n", 5, "a\nb\n"},
		{"", 3, ""},
	} {
		if got := string(lastLines([]byte(c.in), c.n)); got != c.want {
			t.Errorf("lastLines(%q, %d) = %q; want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestInstallAllowTest(t *testing.T) {
	f := &androidTarget{id: "dev1"}
	serveAgent(t, f, http.MethodPost, "/api/install?target=dev1&allowTest=1", strings.NewReader(apk))
	if !f.gotOpts.AllowTest {
		t.Error("?allowTest=1 did not reach the target")
	}
}

// serveMultipart posts each file as its own part, the way
// `curl -F apk=@base.apk -F apk=@split.apk` does. A part with no filename
// is a plain form field.
func serveMultipart(t *testing.T, tg target.Target, path string, files map[string]string, fields map[string]string, order []string) *httptest.ResponseRecorder {
	t.Helper()
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range order {
		fw, err := mw.CreateFormFile("apk", name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(fw, files[name])
	}
	mw.Close()
	m := NewManager(target.Options{})
	m.Register(tg)
	s := &Server{mgr: m, token: "secret"}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body.String()))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestInstallSplitsAsMultipart(t *testing.T) {
	f := &androidTarget{id: "dev1"}
	files := map[string]string{
		"base.apk":                apk + "base",
		"split_config.arm64.apk":  apk + "abi",
		"split_config.xxhdpi.apk": apk + "density",
	}
	order := []string{"base.apk", "split_config.arm64.apk", "split_config.xxhdpi.apk"}
	w := serveMultipart(t, f, "/api/install?target=dev1", files, map[string]string{"note": "ignored"}, order)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(f.gotAPKs) != 3 {
		t.Fatalf("target got %d APKs; want 3", len(f.gotAPKs))
	}
	for i, name := range order {
		if string(f.gotAPKs[i]) != files[name] {
			t.Errorf("APK %d = %q; want %s's bytes", i, f.gotAPKs[i], name)
		}
	}
	// Staged names come from the index, never the caller's filename, and
	// nothing is left behind once the response is written.
	for _, p := range f.gotPaths {
		if strings.Contains(p, "split_config") || !strings.HasSuffix(p, ".apk") {
			t.Errorf("staged path %q", p)
		}
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after the install", p)
		}
	}
}

// A split that is not a zip fails the whole install before adb runs, and
// the error names which part it was.
func TestInstallMultipartRefusesANonAPKPart(t *testing.T) {
	f := &androidTarget{id: "dev1"}
	files := map[string]string{"base.apk": apk, "oops.apk": "<html>"}
	w := serveMultipart(t, f, "/api/install?target=dev1", files, nil, []string{"base.apk", "oops.apk"})
	if w.Code != http.StatusBadRequest || f.gotAPKs != nil {
		t.Fatalf("status = %d, target got %d APKs; want 400 and nothing installed", w.Code, len(f.gotAPKs))
	}
	if !strings.Contains(w.Body.String(), "oops.apk") {
		t.Errorf("error %s does not name the part", w.Body)
	}
}

func TestInstallMultipartWithNoFilesIs400(t *testing.T) {
	f := &androidTarget{id: "dev1"}
	w := serveMultipart(t, f, "/api/install?target=dev1", nil, map[string]string{"apk": "not a file"}, nil)
	if w.Code != http.StatusBadRequest || f.gotAPKs != nil {
		t.Fatalf("status = %d; want 400 and nothing installed", w.Code)
	}
}

func TestInstallRawBodyIsOneAPK(t *testing.T) {
	f := &androidTarget{id: "dev1"}
	w := serveAgent(t, f, http.MethodPost, "/api/install?target=dev1", strings.NewReader(apk))
	if w.Code != http.StatusOK || len(f.gotPaths) != 1 {
		t.Fatalf("status = %d, %d APKs; want 200 and one", w.Code, len(f.gotPaths))
	}
	if _, err := os.Stat(f.gotPaths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists after the install", f.gotPaths[0])
	}
}
