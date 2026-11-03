package target

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func writeZip(t *testing.T, entries []zipEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func TestUnpackIOSAppIPA(t *testing.T) {
	p := writeZip(t, []zipEntry{
		{name: "Payload/Handset.app/Info.plist", body: "plist"},
		{name: "Payload/Handset.app/Handset", body: "bin", mode: 0o755},
		{name: "Payload/Handset.app/Frameworks/A.framework/A", body: "a", mode: 0o755},
		{name: "Payload/Handset.app/Current", body: "Frameworks", mode: os.ModeSymlink | 0o777},
		{name: "Symbols/x", body: "skipped"},
		{name: "iTunesMetadata.plist", body: "skipped"},
	})
	dir := t.TempDir()
	app, err := UnpackIOSApp(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	if app != filepath.Join(dir, "Handset.app") {
		t.Fatalf("app = %s", app)
	}
	st, err := os.Stat(filepath.Join(app, "Handset"))
	if err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("main binary lost its executable bit: %v %v", st, err)
	}
	if l, err := os.Readlink(filepath.Join(app, "Current")); err != nil || l != "Frameworks" {
		t.Fatalf("symlink = %q, %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Symbols")); !os.IsNotExist(err) {
		t.Fatal("unpacked an entry outside the bundle")
	}
}

// Finder's Compress puts the bundle at the top and its resource forks
// under __MACOSX.
func TestUnpackIOSAppZippedApp(t *testing.T) {
	p := writeZip(t, []zipEntry{
		{name: "Handset.app/", mode: os.ModeDir | 0o755},
		{name: "Handset.app/Info.plist", body: "plist"},
		{name: "__MACOSX/Handset.app/._Info.plist", body: "fork"},
	})
	app, err := UnpackIOSApp(p, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(app) != "Handset.app" {
		t.Fatalf("app = %s", app)
	}
}

func TestUnpackIOSAppRefuses(t *testing.T) {
	notZip := filepath.Join(t.TempDir(), "x.ipa")
	os.WriteFile(notZip, []byte("PK\x03\x04 but not really"), 0o644)
	cases := map[string]struct {
		path string
		want string
	}{
		"not a zip":       {notZip, "not a zip"},
		"no app":          {writeZip(t, []zipEntry{{name: "readme.txt", body: "x"}}), "no .app"},
		"two apps":        {writeZip(t, []zipEntry{{name: "Payload/A.app/Info.plist"}, {name: "Payload/B.app/Info.plist"}}), "more than one app"},
		"no Info.plist":   {writeZip(t, []zipEntry{{name: "Payload/A.app/A", body: "bin"}}), "no Info.plist"},
		"escape":          {writeZip(t, []zipEntry{{name: "Payload/A.app/../../evil", body: "x"}, {name: "Payload/A.app/Info.plist"}}), "outside the bundle"},
		"absolute link":   {writeZip(t, []zipEntry{{name: "Payload/A.app/Info.plist"}, {name: "Payload/A.app/l", body: "/etc/passwd", mode: os.ModeSymlink | 0o777}}), "points out"},
		"link climbs out": {writeZip(t, []zipEntry{{name: "Payload/A.app/Info.plist"}, {name: "Payload/A.app/l", body: "../../x", mode: os.ModeSymlink | 0o777}}), "points out"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := UnpackIOSApp(c.path, t.TempDir())
			if !errors.Is(err, ErrNotIOSApp) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want ErrNotIOSApp with %q", err, c.want)
			}
		})
	}
}

// The JSON is devicectl 651.32's, for a bundle with no CFBundleName, run
// against an iPhone 16 Pro on iOS 27.
func TestParseDevicectlInstall(t *testing.T) {
	b, err := os.ReadFile("testdata/devicectl-install-invalid-bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	ie, ok := parseDevicectlInstall(b)
	if !ok {
		t.Fatal("no error object read")
	}
	if ie.Code != "com.apple.dt.CoreDeviceError 3000" {
		t.Errorf("Code = %q, want the innermost error", ie.Code)
	}
	for _, want := range []string{"Failed to install the app on the device.", "is not a valid bundle", "CFBundleName"} {
		if !strings.Contains(ie.Reason, want) {
			t.Errorf("Reason %q lacks %q", ie.Reason, want)
		}
	}
	if _, ok := parseDevicectlInstall([]byte(`{"info":{"outcome":"success"}}`)); ok {
		t.Error("a success read as a refusal")
	}
}

func TestIOSDeviceInstallApp(t *testing.T) {
	defer func(f func(context.Context, string) (string, error)) { appBundleID = f }(appBundleID)
	appBundleID = func(context.Context, string) (string, error) { return "dev.bmcreations.handset", nil }
	defer func(f func(context.Context, string, string, string) ([]byte, error)) { devicectlInstall = f }(devicectlInstall)

	d := &IOSDevice{info: IOSDeviceInfo{UDID: "00008140-0004583C22D8801C"}}
	p := writeZip(t, []zipEntry{{name: "Payload/Handset.app/Info.plist", body: "plist"}})

	var gotDevice, gotApp string
	devicectlInstall = func(_ context.Context, device, app, js string) ([]byte, error) {
		gotDevice, gotApp = device, app
		if _, err := os.Stat(filepath.Join(app, "Info.plist")); err != nil {
			t.Errorf("app not unpacked when devicectl ran: %v", err)
		}
		return nil, nil
	}
	res, err := d.InstallApp(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if res.BundleID != "dev.bmcreations.handset" || gotDevice != d.device() || filepath.Base(gotApp) != "Handset.app" {
		t.Fatalf("res = %+v, device %q, app %q", res, gotDevice, gotApp)
	}

	fixture, _ := os.ReadFile("testdata/devicectl-install-invalid-bundle.json")
	devicectlInstall = func(_ context.Context, _, _, js string) ([]byte, error) {
		os.WriteFile(js, fixture, 0o644)
		return []byte("ERROR: Failed to install the app on the device."), errors.New("exit status 1")
	}
	_, err = d.InstallApp(context.Background(), p)
	if ie, ok := IsInstallRefused(err); !ok || ie.Code != "com.apple.dt.CoreDeviceError 3000" {
		t.Fatalf("err = %v, want an InstallError", err)
	}

	devicectlInstall = func(context.Context, string, string, string) ([]byte, error) {
		return []byte("xcrun: error: unable to find utility"), errors.New("exit status 72")
	}
	_, err = d.InstallApp(context.Background(), p)
	if _, ok := IsInstallRefused(err); ok || err == nil {
		t.Fatalf("a devicectl that never ran read as a refusal: %v", err)
	}
}

func TestSimctlReason(t *testing.T) {
	out := "An error was encountered processing the command (domain=IXUserPresentableErrorDomain, code=1):\nThis app could not be installed at this time.\nUnderlying error..."
	if got := simctlReason(out); got != "This app could not be installed at this time." {
		t.Fatalf("got %q", got)
	}
}
