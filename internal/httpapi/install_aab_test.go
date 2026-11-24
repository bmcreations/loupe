package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/target/android"
)

// bundleTarget is an androidTarget that can also build APKs from a bundle,
// as android.Device does with bundletool.
type bundleTarget struct {
	androidTarget
	bundleErr  error
	gotBundle  []byte
	gotAABPath string
	gotCmd     []string
}

func (f *bundleTarget) BundleAPKs(ctx context.Context, bundletool []string, aab, dir string) ([]string, error) {
	f.gotCmd, f.gotAABPath = bundletool, aab
	b, err := os.ReadFile(aab)
	if err != nil {
		return nil, err
	}
	f.gotBundle = b
	if f.bundleErr != nil {
		return nil, f.bundleErr
	}
	out := filepath.Join(dir, "apks")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	var apks []string
	for _, n := range []string{"base-master.apk", "base-xxhdpi.apk"} {
		p := filepath.Join(out, n)
		if err := os.WriteFile(p, []byte(apk+n), 0o644); err != nil {
			return nil, err
		}
		apks = append(apks, p)
	}
	return apks, nil
}

// aab is a zip with BundleConfig.pb at its root, which is what tells a
// bundle from an APK.
func aab(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, n := range []string{"BundleConfig.pb", "base/manifest/AndroidManifest.xml"} {
		if _, err := zw.Create(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

var fakeBundletool = func() ([]string, error) { return []string{"/opt/homebrew/bin/bundletool"}, nil }

// serveInstall posts body to /api/install on a server whose bundletool
// lookup is find. A non-empty name sends body as one multipart file part,
// the way the MCP install tool and the CLI always send it.
func serveInstall(t *testing.T, tg target.Target, find func() ([]string, error), name, body string) *httptest.ResponseRecorder {
	t.Helper()
	m := NewManager(target.Options{})
	m.Register(tg)
	s := &Server{mgr: m, token: "secret"}
	s.SetBundletool(find)
	ct := "application/octet-stream"
	if name != "" {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		fw, err := mw.CreateFormFile("apk", name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(fw, body)
		mw.Close()
		body, ct = b.String(), mw.FormDataContentType()
	}
	r := httptest.NewRequest(http.MethodPost, "/api/install?target="+tg.ID(), strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// An .aab used to reach adb as 0.apk and fail on its manifest. It now goes
// through bundletool, and what reaches Install is the APKs bundletool built,
// never the bundle.
func TestInstallAABInstallsTheAPKsBundletoolBuilt(t *testing.T) {
	for _, name := range []string{"", "app-debug.aab"} {
		f := &bundleTarget{androidTarget: androidTarget{id: "dev1"}}
		bundle := aab(t)
		w := serveInstall(t, f, fakeBundletool, name, bundle)
		if w.Code != http.StatusOK {
			t.Fatalf("name %q: status = %d: %s", name, w.Code, w.Body)
		}
		var resp InstallResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.OK || strings.Join(resp.Splits, " ") != "base-master.apk base-xxhdpi.apk" {
			t.Errorf("name %q: response = %+v; want ok and the two splits named", name, resp)
		}
		if string(f.gotBundle) != bundle || !strings.HasSuffix(f.gotAABPath, ".aab") {
			t.Errorf("name %q: bundletool got %s (%d bytes); want the whole upload staged as .aab", name, f.gotAABPath, len(f.gotBundle))
		}
		if strings.Join(f.gotCmd, " ") != "/opt/homebrew/bin/bundletool" {
			t.Errorf("name %q: bundletool command = %q", name, f.gotCmd)
		}
		if len(f.gotAPKs) != 2 || string(f.gotAPKs[0]) != apk+"base-master.apk" {
			t.Errorf("name %q: Install got %q; want the two built APKs, base first", name, f.gotAPKs)
		}
		for _, p := range append(f.gotPaths, f.gotAABPath) {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("name %q: %s still exists after the install", name, p)
			}
		}
	}
}

// An APK is installed as it always was, and bundletool is never looked up.
func TestInstallAPKNeverLooksUpBundletool(t *testing.T) {
	f := &bundleTarget{androidTarget: androidTarget{id: "dev1"}}
	looked := false
	find := func() ([]string, error) { looked = true; return nil, android.ErrNoBundletool }
	w := serveInstall(t, f, find, "", apk)
	if w.Code != http.StatusOK || string(f.gotAPK) != apk {
		t.Fatalf("status = %d, Install got %q: %s", w.Code, f.gotAPK, w.Body)
	}
	if looked || f.gotBundle != nil {
		t.Error("an APK upload went near bundletool")
	}
	var resp InstallResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Splits != nil {
		t.Errorf("splits = %q on an APK install; want none", resp.Splits)
	}
}

// No bundletool is the Mac's to fix, so it is a 501 whose error says what to
// install, sent before anything reaches the device.
func TestInstallAABWithoutBundletoolSaysWhatToInstall(t *testing.T) {
	for _, c := range []struct {
		err      error
		code     string
		mentions string
	}{
		{android.ErrNoBundletool, ErrCodeNoBundletool, "brew install bundletool"},
		{android.ErrNoJava, ErrCodeNoJava, "brew install openjdk"},
	} {
		f := &bundleTarget{androidTarget: androidTarget{id: "dev1"}}
		w := serveInstall(t, f, func() ([]string, error) { return nil, c.err }, "app.aab", aab(t))
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("%s: status = %d: %s", c.code, w.Code, w.Body)
		}
		var resp ErrorResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Code != c.code || !strings.Contains(resp.Error, c.mentions) {
			t.Errorf("response = %+v; want code %s and an error naming %q", resp, c.code, c.mentions)
		}
		if f.gotBundle != nil || f.gotAPKs != nil {
			t.Errorf("%s: the upload reached the target", c.code)
		}
	}
	// A server never given a lookup has no bundletool either.
	f := &bundleTarget{androidTarget: androidTarget{id: "dev1"}}
	if w := serveInstall(t, f, nil, "", aab(t)); w.Code != http.StatusNotImplemented {
		t.Errorf("no lookup: status = %d: %s", w.Code, w.Body)
	}
}

// bundletool refusing the bundle is a 422 like the package manager refusing
// an APK, with bundletool's sentence as the reason.
func TestInstallAABBundletoolRefusalIs422(t *testing.T) {
	f := &bundleTarget{androidTarget: androidTarget{id: "dev1"}, bundleErr: &target.InstallError{
		Code: android.BundletoolCode, Reason: "Module 'base' has no AndroidManifest.xml",
		Output: "[BT:1.17.2] Error: Module 'base' has no AndroidManifest.xml\n\tat ...",
	}}
	w := serveInstall(t, f, fakeBundletool, "app.aab", aab(t))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp InstallResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OK || resp.Code != android.BundletoolCode || resp.Reason != "Module 'base' has no AndroidManifest.xml" {
		t.Errorf("response = %+v", resp)
	}
	if f.gotAPKs != nil {
		t.Error("Install ran after bundletool refused the bundle")
	}
}

// The device, adb or the Mac failing while bundletool runs is not the
// bundle's fault, so it is a 502 rather than a 422 telling the caller to fix
// their upload.
func TestInstallAABDeviceOrHostFailureIs502(t *testing.T) {
	for _, msg := range []string{
		"bundletool get-device-spec: exit status 1: Device not found.",
		"bundletool build-apks: exit status 1: java.nio.file.NoSuchFileException: /tmp/gone/AutoValue_BuildApksCommand1",
	} {
		f := &bundleTarget{androidTarget: androidTarget{id: "dev1"}, bundleErr: errors.New(msg)}
		w := serveInstall(t, f, fakeBundletool, "", aab(t))
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), msg[len(msg)-20:]) {
			t.Errorf("%s: status = %d: %s", msg, w.Code, w.Body)
		}
		if f.gotAPKs != nil {
			t.Errorf("%s: Install ran", msg)
		}
	}
}

// With no debug keystore bundletool builds unsigned APKs, which is the
// Mac's to fix: a 501 saying so, sent before anything reaches the device.
func TestInstallAABWithNoDebugKeystoreIs501(t *testing.T) {
	f := &bundleTarget{androidTarget: androidTarget{id: "dev1"}, bundleErr: android.ErrNoDebugKeystore}
	w := serveInstall(t, f, fakeBundletool, "", aab(t))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Code != ErrCodeNoDebugKeystore || !strings.Contains(resp.Error, "debug.keystore") {
		t.Errorf("response = %+v; want code %s and an error naming debug.keystore", resp, ErrCodeNoDebugKeystore)
	}
	if f.gotAPKs != nil {
		t.Error("Install ran with unsigned APKs")
	}
}

// Should bundletool's warning ever change, unsigned APKs reach the package
// manager, and its refusal after a bundle says which file is missing.
func TestInstallAABUnsignedRefusalNamesTheDebugKeystore(t *testing.T) {
	f := &bundleTarget{androidTarget: androidTarget{id: "dev1", installErr: &target.InstallError{
		Code: "INSTALL_PARSE_FAILED_NO_CERTIFICATES", Output: "Failure [INSTALL_PARSE_FAILED_NO_CERTIFICATES]",
	}}}
	w := serveInstall(t, f, fakeBundletool, "", aab(t))
	var resp InstallResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(resp.Hint, "debug.keystore") {
		t.Errorf("status = %d, hint = %q; want 422 and a hint naming debug.keystore", w.Code, resp.Hint)
	}
}

// A bundle is a whole app, so it cannot be one part of a split install.
func TestInstallAABWithOtherPartsIs400(t *testing.T) {
	f := &bundleTarget{androidTarget: androidTarget{id: "dev1"}}
	files := map[string]string{"app.aab": aab(t), "base.apk": apk}
	w := serveMultipart(t, f, "/api/install?target=dev1", files, nil, []string{"base.apk", "app.aab"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), ".aab") || f.gotBundle != nil || f.gotAPKs != nil {
		t.Errorf("body %s, bundle reached bundletool: %v, Install ran: %v", w.Body, f.gotBundle != nil, f.gotAPKs != nil)
	}
}

// A target that installs APKs but cannot build them from a bundle refuses
// one, rather than handing adb a bundle named .apk again.
func TestInstallAABOnATargetWithoutBundleSupportIs501(t *testing.T) {
	f := &androidTarget{id: "dev1"}
	w := serveInstall(t, f, fakeBundletool, "", aab(t))
	if w.Code != http.StatusNotImplemented || f.gotAPKs != nil {
		t.Errorf("status = %d, Install ran: %v: %s", w.Code, f.gotAPKs != nil, w.Body)
	}
}
