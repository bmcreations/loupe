package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// fakeCLI stands in for `android`. create, when set, decides what a create
// does; release, when set, holds it until closed.
type fakeCLI struct {
	home    string
	release chan struct{}
	create  func(profile string) (stdout string)
	mu      sync.Mutex
	calls   [][]string
}

func (f *fakeCLI) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
	if args[len(args)-1] == "--list-profiles" {
		return []byte("medium_phone\nsmall_phone\n"), nil, nil
	}
	if f.release != nil {
		<-f.release
	}
	profile := args[len(args)-1]
	if f.create != nil {
		return []byte(f.create(profile)), nil, nil
	}
	writeAVD(f.home, profile)
	return nil, nil, nil
}

func writeAVD(home, name string) {
	dir := filepath.Join(home, name+".avd")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "config.ini"), []byte("hw.device.name=small_phone\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, name+".ini"), []byte("path="+dir+"\n"), 0o644)
}

func avdServer(t *testing.T, cli *fakeCLI) (*Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	c := NewAVDCreator("android", "/sdk", cli.home, func(_ time.Time, avds []target.AVD) {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range avds {
			got = append(got, a.Name)
		}
	}, nil)
	c.run = cli.run
	s := &Server{mgr: NewManager(target.Options{})}
	s.SetAVDCreator(c)
	return s, &got
}

func do(s *Server, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func targetRow(t *testing.T, s *Server, id string) (TargetInfo, bool) {
	t.Helper()
	var list []TargetInfo
	if err := json.Unmarshal(do(s, http.MethodGet, "/api/targets", "").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, ti := range list {
		if ti.ID == id {
			return ti, true
		}
	}
	return TargetInfo{}, false
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCreateAVDShowsACreatingRowThenRegistersTheAVD(t *testing.T) {
	cli := &fakeCLI{home: t.TempDir(), release: make(chan struct{})}
	s, got := avdServer(t, cli)

	rec := do(s, http.MethodPost, "/api/avds", `{"profile":"small_phone"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	row, ok := targetRow(t, s, "avd:small_phone")
	if !ok || row.State != stateCreating || row.Available || row.Label != "Small Phone" {
		t.Fatalf("row = %+v, %v", row, ok)
	}
	if !strings.HasPrefix(row.Reason, "Preparing") {
		t.Errorf("reason = %q", row.Reason)
	}

	// A second create while the first runs would share its download.
	if rec := do(s, http.MethodPost, "/api/avds", `{"profile":"medium_phone"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrCodeCreating) {
		t.Fatalf("second create = %d %s", rec.Code, rec.Body)
	}

	close(cli.release)
	waitFor(t, func() bool { _, ok := targetRow(t, s, "avd:small_phone"); return !ok })
	if len(*got) != 1 || (*got)[0] != "small_phone" {
		t.Errorf("registered %v, want the rescan to pass small_phone", *got)
	}
	cli.mu.Lock()
	defer cli.mu.Unlock()
	last := cli.calls[len(cli.calls)-1]
	if strings.Join(last, " ") != "--sdk=/sdk emulator create small_phone" {
		t.Errorf("ran %v", last)
	}
}

// The CLI exits 0 after "Error: AVD small_phone already exists", so the
// verdict has to come from its output and the AVD's directory.
func TestCreateAVDFailsOnAnErrorLineDespiteExitZero(t *testing.T) {
	cli := &fakeCLI{home: t.TempDir(), create: func(p string) string { return "Error: no space left\n" }}
	s, got := avdServer(t, cli)

	if rec := do(s, http.MethodPost, "/api/avds", `{"profile":"small_phone"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	var row TargetInfo
	waitFor(t, func() bool { row, _ = targetRow(t, s, "avd:small_phone"); return row.State == stateCreateFailed })
	if row.Reason != "Couldn't create: no space left" {
		t.Errorf("reason = %q", row.Reason)
	}
	if len(*got) != 0 {
		t.Errorf("registered %v after a failure", *got)
	}

	// The next create clears the failed row.
	cli.create = nil
	if rec := do(s, http.MethodPost, "/api/avds", `{"profile":"small_phone"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d %s", rec.Code, rec.Body)
	}
	waitFor(t, func() bool { _, ok := targetRow(t, s, "avd:small_phone"); return !ok })
}

func TestCreateAVDRefusesAnExistingOrUnknownProfile(t *testing.T) {
	cli := &fakeCLI{home: t.TempDir()}
	writeAVD(cli.home, "small_phone")
	s, _ := avdServer(t, cli)

	if rec := do(s, http.MethodPost, "/api/avds", `{"profile":"small_phone"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrCodeAVDExists) {
		t.Errorf("existing = %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, http.MethodPost, "/api/avds", `{"profile":"x; rm -rf /"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown = %d %s", rec.Code, rec.Body)
	}

	var resp AVDProfilesResponse
	if err := json.Unmarshal(do(s, http.MethodGet, "/api/avds/profiles", "").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := []AVDProfile{{"medium_phone", "Medium Phone", false}, {"small_phone", "Small Phone", true}}
	if len(resp.Profiles) != 2 || resp.Profiles[0] != want[0] || resp.Profiles[1] != want[1] {
		t.Errorf("profiles = %+v", resp.Profiles)
	}
}

func TestAVDRoutesAre404WithoutTheCLI(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{})}
	if rec := do(s, http.MethodGet, "/api/avds/profiles", ""); rec.Code != http.StatusNotFound {
		t.Errorf("profiles = %d", rec.Code)
	}
	if rec := do(s, http.MethodPost, "/api/avds", `{"profile":"small_phone"}`); rec.Code != http.StatusNotFound {
		t.Errorf("create = %d", rec.Code)
	}
}

func TestCreatingReasonCallsItADownloadOnlyAfterAWait(t *testing.T) {
	for _, c := range []struct {
		elapsed time.Duration
		exists  bool
		want    string
	}{
		{3 * time.Second, false, "Preparing · 3s"},
		{40 * time.Second, false, "Getting the system image · 40s"},
		{40 * time.Second, true, "Creating the device"},
		{3 * time.Second, true, "Creating the device"},
	} {
		if got := creatingReason(c.elapsed, c.exists); got != c.want {
			t.Errorf("creatingReason(%v, %v) = %q, want %q", c.elapsed, c.exists, got, c.want)
		}
	}
}
