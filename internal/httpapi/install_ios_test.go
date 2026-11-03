package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// iosTarget is an AppInstaller with canned answers, standing in for a
// simulator or an iPhone.
type iosTarget struct {
	stubTarget
	id  string
	err error
	got []byte
}

func (f *iosTarget) ID() string { return f.id }

func (f *iosTarget) InstallApp(ctx context.Context, archive string) (*target.AppInstallResult, error) {
	b, err := os.ReadFile(archive)
	if err != nil {
		return nil, fmt.Errorf("read staged archive: %w", err)
	}
	f.got = b
	if f.err != nil {
		return nil, f.err
	}
	return &target.AppInstallResult{BundleID: "dev.bmcreations.handset"}, nil
}

const ipa = "PK\x03\x04an ipa"

func TestInstallIOSSuccess(t *testing.T) {
	f := &iosTarget{id: "ios:phone"}
	w := serveAgent(t, f, http.MethodPost, "/api/install?target=ios:phone", strings.NewReader(ipa))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp InstallResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Package != "dev.bmcreations.handset" || string(f.got) != ipa {
		t.Errorf("response = %+v, staged %q", resp, f.got)
	}
}

func TestInstallIOSRefusals(t *testing.T) {
	cases := map[string]struct {
		err  error
		body string
		want int
		code string
	}{
		"not a zip":   {body: "hello", want: http.StatusBadRequest},
		"not an app":  {body: ipa, err: fmt.Errorf("%w: no .app in the archive", target.ErrNotIOSApp), want: http.StatusBadRequest},
		"refused":     {body: ipa, err: &target.InstallError{Code: "com.apple.dt.CoreDeviceError 3000", Reason: "not a valid bundle"}, want: http.StatusUnprocessableEntity, code: "com.apple.dt.CoreDeviceError 3000"},
		"never asked": {body: ipa, err: fmt.Errorf("devicectl install: exit status 72"), want: http.StatusBadGateway},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := &iosTarget{id: "ios:phone", err: c.err}
			w := serveAgent(t, f, http.MethodPost, "/api/install?target=ios:phone", strings.NewReader(c.body))
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.want, w.Body)
			}
			if c.code != "" {
				var resp InstallResponse
				json.Unmarshal(w.Body.Bytes(), &resp)
				if resp.Code != c.code {
					t.Errorf("code = %q, want %q", resp.Code, c.code)
				}
			}
		})
	}
}

func TestInstallIOSOneFile(t *testing.T) {
	f := &iosTarget{id: "ios:phone"}
	w := serveMultipart(t, f, "/api/install?target=ios:phone",
		map[string]string{"a.ipa": ipa, "b.ipa": ipa}, nil, []string{"a.ipa", "b.ipa"})
	if w.Code != http.StatusBadRequest || f.got != nil {
		t.Fatalf("two files = %d %s; want 400 before the target", w.Code, w.Body)
	}
}
