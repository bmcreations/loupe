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

	"github.com/bmcreations/loupe/internal/target"
)

func remoteFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "remote", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// fakeRemoteCLI answers the android CLI's remote commands from recordings of
// the real one (testdata/remote, captured 10 Nov 2026 on CLI 1.1.16717390).
type fakeRemoteCLI struct {
	t       *testing.T
	release chan struct{} // create waits on it when set
	// holding makes list show the fixture's reservation from the start;
	// otherwise the project holds nothing until create returns.
	holding bool
	fail    map[string]string

	mu    sync.Mutex
	calls []string
}

func (f *fakeRemoteCLI) run(_ context.Context, args ...string) ([]byte, []byte, error) {
	line := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, line)
	f.mu.Unlock()
	for prefix, msg := range f.fail {
		if strings.HasPrefix(line, prefix) {
			return nil, []byte(msg), errors.New("exit status 1")
		}
	}
	switch {
	case strings.HasPrefix(line, "device remote create"):
		if f.release != nil {
			<-f.release
		}
		f.mu.Lock()
		f.holding = true
		f.mu.Unlock()
		return remoteFixture(f.t, "create.txt"), nil, nil
	case strings.HasPrefix(line, "device remote list"):
		f.mu.Lock()
		holding := f.holding
		f.mu.Unlock()
		if !holding {
			return []byte("No reservations found.\n"), nil, nil
		}
		return remoteFixture(f.t, "remote-list.txt"), nil, nil
	case strings.HasPrefix(line, "device remote models"):
		return remoteFixture(f.t, "models.txt"), nil, nil
	case strings.HasPrefix(line, "device remote projects"):
		return remoteFixture(f.t, "projects.txt"), nil, nil
	case line == "auth status":
		return []byte("Logged in as Test Person <test@example.com>\n"), nil, nil
	}
	return nil, nil, nil
}

func (f *fakeRemoteCLI) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type remoteRig struct {
	s     *Server
	rd    *RemoteDevices
	cli   *fakeRemoteCLI
	clock *time.Time
	ended *[]string
}

func remoteServer(t *testing.T, cli *fakeRemoteCLI) remoteRig {
	t.Helper()
	cli.t = t
	// 10:41 on the day the create fixture was recorded; it expires 10:56 AM.
	clock := time.Date(2026, 11, 10, 10, 41, 0, 0, time.Local)
	var mu sync.Mutex
	var ended []string
	rd := NewRemoteDevices(func() string { return "android" }, func(id, reason string) {
		mu.Lock()
		defer mu.Unlock()
		ended = append(ended, id+" "+reason)
	}, nil)
	rd.run = cli.run
	rd.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	if err := rd.Load(filepath.Join(t.TempDir(), "remote-devices.json")); err != nil {
		t.Fatal(err)
	}
	s := &Server{mgr: NewManager(target.Options{})}
	s.SetRemoteDevices(rd)
	return remoteRig{s: s, rd: rd, cli: cli, clock: &clock, ended: &ended}
}

func (r remoteRig) reserve(t *testing.T) string {
	t.Helper()
	rec := do(r.s, http.MethodPost, "/api/remote", `{"project":"demo-project","codename":"tokay","api":36,"model":"Pixel 9"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reserve = %d: %s", rec.Code, rec.Body)
	}
	var out struct{ Key string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Key == "" {
		t.Fatalf("reserve body %s: %v", rec.Body, err)
	}
	return out.Key
}

func (r remoteRig) only(t *testing.T) RemoteReservation {
	t.Helper()
	list := r.rd.List()
	if len(list) != 1 {
		t.Fatalf("reservations = %+v", list)
	}
	return list[0]
}

func TestReserveShowsAReservingRowThenMarksTheAttachedDevice(t *testing.T) {
	rig := remoteServer(t, &fakeRemoteCLI{release: make(chan struct{})})
	key := rig.reserve(t)

	row, ok := targetRow(t, rig.s, "remote:"+key)
	if !ok || row.State != remoteReserving || row.Available || row.Label != "Pixel 9" || row.Remote == nil || row.Remote.Project != "demo-project" {
		t.Fatalf("reserving row = %+v, %v", row, ok)
	}
	if rec := do(rig.s, http.MethodPost, "/api/remote", `{"project":"demo-project","codename":"tokay","api":36}`); rec.Code != http.StatusConflict {
		t.Fatalf("second reserve = %d %s", rec.Code, rec.Body)
	}

	close(rig.cli.release)
	waitFor(t, func() bool { return rig.only(t).State == remoteActive })
	got := rig.only(t)
	if got.Reservation != "21kulawh96ut3" || got.Target != "localhost:53523" {
		t.Errorf("reservation = %+v", got)
	}
	if want := time.Date(2026, 11, 10, 10, 56, 0, 0, time.Local); !got.ExpiresAt.Equal(want) {
		t.Errorf("expires %v, want %v", got.ExpiresAt, want)
	}
	if _, ok := targetRow(t, rig.s, "remote:"+key); ok {
		t.Error("the reserving row outlived the reservation becoming active")
	}
	// adb's own row for the device gains the marker.
	rows := rig.rd.decorate([]TargetInfo{{ID: "localhost:53523", Label: "localhost:53523"}, {ID: "localhost:5555"}})
	if rows[0].Label != "Pixel 9" {
		t.Errorf("attached row label = %q, want the reserved model", rows[0].Label)
	}
	if rows[0].Remote == nil || rows[0].Remote.Key != key || rows[0].Remote.ExpiresAt == "" || rows[1].Remote != nil {
		t.Errorf("decorated = %+v / %+v", rows[0].Remote, rows[1].Remote)
	}
	if ran := rig.cli.ran(); ran[len(ran)-1] != "device remote create --connect --project=demo-project tokay/36" {
		t.Errorf("ran %v", ran)
	}
}

func TestReleaseDisconnectsRemovesAndForgetsTheRow(t *testing.T) {
	rig := remoteServer(t, &fakeRemoteCLI{})
	key := rig.reserve(t)
	waitFor(t, func() bool { return rig.only(t).State == remoteActive })
	*rig.clock = rig.clock.Add(3*time.Minute + 10*time.Second)

	if rec := do(rig.s, http.MethodDelete, "/api/remote?key="+key, ""); rec.Code != http.StatusOK {
		t.Fatalf("release = %d %s", rec.Code, rec.Body)
	}
	got := rig.only(t)
	if got.State != remoteEnded || got.Reason != "released" {
		t.Errorf("after release = %+v", got)
	}
	ran := rig.cli.ran()
	if n := len(ran); ran[n-2] != "device remote disconnect --project=demo-project 21kulawh96ut3" || ran[n-1] != "device remote remove --project=demo-project 21kulawh96ut3" {
		t.Errorf("ran %v", ran)
	}
	if e := *rig.ended; len(e) != 1 || e[0] != "localhost:53523 released" {
		t.Errorf("ended targets = %v", e)
	}
	// 3 min 10 s held counts as 4 minutes, against that project only.
	if u := rig.rd.UsageMinutes(); u["demo-project"] != 4 || len(u) != 1 {
		t.Errorf("usage = %v", u)
	}
}

func TestAReservationIsNeverExtendedUnlessAsked(t *testing.T) {
	rig := remoteServer(t, &fakeRemoteCLI{})
	key := rig.reserve(t)
	waitFor(t, func() bool { return rig.only(t).State == remoteActive })

	// One second short of the expiry: nothing happens, and nothing extends.
	*rig.clock = time.Date(2026, 11, 10, 10, 55, 59, 0, time.Local)
	rig.rd.ExpireDue()
	if rig.only(t).State != remoteActive {
		t.Fatal("ended before its expiry")
	}
	*rig.clock = time.Date(2026, 11, 10, 10, 56, 0, 0, time.Local)
	rig.rd.ExpireDue()
	got := rig.only(t)
	if got.State != remoteEnded || got.Reason != "expired" {
		t.Fatalf("after expiry = %+v", got)
	}
	if e := *rig.ended; len(e) != 1 || e[0] != "localhost:53523 expired" {
		t.Errorf("ended targets = %v", e)
	}
	for _, c := range rig.cli.ran() {
		if strings.Contains(c, "extend") {
			t.Errorf("extended without being asked: %s", c)
		}
	}
	_ = key
}

func TestExtendAsksForTheMinutesAndReadsTheNewExpiryBack(t *testing.T) {
	rig := remoteServer(t, &fakeRemoteCLI{})
	key := rig.reserve(t)
	waitFor(t, func() bool { return rig.only(t).State == remoteActive })

	for _, body := range []string{`{"key":"` + key + `","minutes":0}`, `{"key":"` + key + `","minutes":31}`} {
		if rec := do(rig.s, http.MethodPost, "/api/remote/extend", body); rec.Code != http.StatusBadRequest {
			t.Errorf("extend %s = %d", body, rec.Code)
		}
	}
	if rec := do(rig.s, http.MethodPost, "/api/remote/extend", `{"key":"`+key+`","minutes":15}`); rec.Code != http.StatusOK {
		t.Fatalf("extend = %d %s", rec.Code, rec.Body)
	}
	ran := rig.cli.ran()
	if ran[len(ran)-2] != "device remote extend --project=demo-project --duration=15 21kulawh96ut3" {
		t.Errorf("ran %v", ran)
	}
	// The list fixture says 10:56 AM for this reservation, so that is shown
	// rather than the 11:11 the arithmetic would give.
	if want := time.Date(2026, 11, 10, 10, 56, 0, 0, time.Local); !rig.only(t).ExpiresAt.Equal(want) {
		t.Errorf("expires %v, want %v", rig.only(t).ExpiresAt, want)
	}
}

func TestAFailedReserveSaysWhyAndFlagsADisabledAPI(t *testing.T) {
	rig := remoteServer(t, &fakeRemoteCLI{fail: map[string]string{
		"device remote create": "Error: Device Streaming API has not been used in project demo-project before or it is disabled.\n",
	}})
	rig.reserve(t)
	waitFor(t, func() bool { return rig.only(t).State == remoteEnded })
	got := rig.only(t)
	if got.Reason != "failed" || !got.APIDisabled || !strings.HasPrefix(got.Error, "Device Streaming API has not been used") {
		t.Errorf("failed reservation = %+v", got)
	}
	if u := rig.rd.UsageMinutes(); len(u) != 0 {
		t.Errorf("a failed reserve counted minutes: %v", u)
	}
}

func TestExitReleasesAndARestartReleasesWhatACrashLeft(t *testing.T) {
	rig := remoteServer(t, &fakeRemoteCLI{})
	rig.reserve(t)
	waitFor(t, func() bool { return rig.only(t).State == remoteActive })
	path := rig.rd.path
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	rig.rd.ReleaseAll()
	if got := rig.only(t); got.State != remoteEnded || got.Reason != "released" {
		t.Errorf("after ReleaseAll = %+v", got)
	}

	// A crash: the file still says the reservation is held.
	if err := os.WriteFile(path, held, 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &fakeRemoteCLI{t: t}
	next := NewRemoteDevices(func() string { return "android" }, nil, nil)
	next.run = cli.run
	next.now = rig.rd.now
	if err := next.Load(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(cli.ran()) == 2 })
	if ran := cli.ran(); ran[1] != "device remote remove --project=demo-project 21kulawh96ut3" {
		t.Errorf("restart ran %v", ran)
	}
	if list := next.List(); len(list) != 1 || list[0].State != remoteEnded {
		t.Errorf("after restart = %+v", list)
	}
}

func TestRemoteRoutesRefuseBadInputAndListAccountAndModels(t *testing.T) {
	rig := remoteServer(t, &fakeRemoteCLI{})
	for _, body := range []string{
		`{"project":"Bad Project","codename":"tokay","api":36}`,
		`{"project":"demo-project","codename":"tokay; rm -rf","api":36}`,
		`{"project":"demo-project","codename":"tokay"}`,
	} {
		if rec := do(rig.s, http.MethodPost, "/api/remote", body); rec.Code != http.StatusBadRequest {
			t.Errorf("reserve %s = %d", body, rec.Code)
		}
	}
	if len(rig.cli.ran()) != 0 {
		t.Errorf("bad input reached the CLI: %v", rig.cli.ran())
	}

	rec := do(rig.s, http.MethodGet, "/api/remote/account", "")
	var acct RemoteAccountResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &acct); err != nil {
		t.Fatal(err)
	}
	if !acct.SignedIn || acct.Account != "Test Person <test@example.com>" || len(acct.Projects) != 3 || acct.Projects[0] != "demo-project" {
		t.Errorf("account = %+v", acct)
	}

	rec = do(rig.s, http.MethodGet, "/api/remote/models?project=demo-project", "")
	var listed RemoteModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("%s: %v", rec.Body, err)
	}
	models := listed.Models
	var pixel9 RemoteModel
	for _, m := range models {
		if m.Model == "Pixel 9" {
			pixel9 = m
		}
	}
	if pixel9.Manufacturer != "Google" || pixel9.Codename != "tokay" || len(pixel9.APIs) != 3 || pixel9.APIs[0] != 36 {
		t.Errorf("Pixel 9 = %+v (of %d models)", pixel9, len(models))
	}
	if last := models[len(models)-1]; last.Model != "Xiaomi 14" || last.Manufacturer != "Xiaomi" {
		t.Errorf("last model = %+v", last)
	}
}

func TestResolveClockRollsPastMidnight(t *testing.T) {
	now := time.Date(2026, 11, 10, 23, 50, 0, 0, time.Local)
	if got, want := resolveClock("12:05 AM", now), time.Date(2026, 11, 11, 0, 5, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, want := resolveClock("12:30 PM", time.Date(2026, 11, 10, 12, 20, 0, 0, time.Local)), time.Date(2026, 11, 10, 12, 30, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if !resolveClock("soon", now).IsZero() {
		t.Error("unreadable clock should be zero")
	}
}

func TestExitWaitsForAReserveInFlightThenReleasesIt(t *testing.T) {
	cli := &fakeRemoteCLI{release: make(chan struct{})}
	rig := remoteServer(t, cli)
	rig.reserve(t)

	exited := make(chan struct{})
	go func() { rig.rd.ReleaseAll(); close(exited) }()
	select {
	case <-exited:
		t.Fatal("exit did not wait for the reserve; its device would bill until it expired")
	case <-time.After(50 * time.Millisecond):
	}
	close(cli.release)
	<-exited

	if got := rig.only(t); got.State != remoteEnded || got.Reason != "released" {
		t.Errorf("after exit = %+v", got)
	}
	ran := cli.ran()
	if last := ran[len(ran)-1]; last != "device remote remove --project=demo-project 21kulawh96ut3" {
		t.Errorf("exit ran %v", ran)
	}
}

func TestARestartAfterAQuitMidReserveReleasesOnlyWhatThatReserveMade(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before []string
		want   bool
	}{
		{"made by the reserve", []string{}, true},
		{"held before the reserve", []string{"21kulawh96ut3"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "remote-devices.json")
			left := []RemoteReservation{{Key: "k1", Project: "demo-project", Codename: "tokay", API: 36, State: remoteReserving, Before: tc.before}}
			raw, _ := json.Marshal(left)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			cli := &fakeRemoteCLI{t: t, holding: true}
			rd := NewRemoteDevices(func() string { return "android" }, nil, nil)
			rd.run = cli.run
			if err := rd.Load(path); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return len(cli.ran()) >= 1 })
			time.Sleep(50 * time.Millisecond)
			removed := slices.Contains(cli.ran(), "device remote remove --project=demo-project 21kulawh96ut3")
			if removed != tc.want {
				t.Errorf("removed = %v, want %v; ran %v", removed, tc.want, cli.ran())
			}
		})
	}
}

// A client must tell "no CLI on this host" from "server too old": the old
// server answers 404, so a missing CLI answers 200 cliInstalled false and 503.
// The lookup runs per call, so installing the CLI needs no restart.
func TestAMissingCLIIsReportedAndPickedUpOnceInstalled(t *testing.T) {
	path := ""
	rd := NewRemoteDevices(func() string { return path }, nil, nil)
	s := &Server{mgr: NewManager(target.Options{})}
	s.SetRemoteDevices(rd)

	rec := do(s, http.MethodGet, "/api/remote/account", "")
	var acct RemoteAccountResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &acct); err != nil || rec.Code != http.StatusOK || acct.CLIInstalled {
		t.Fatalf("account without a CLI = %d %s (%v), want 200 cliInstalled false", rec.Code, rec.Body, err)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/remote", `{"project":"demo-project","codename":"tokay","api":36}`},
		{http.MethodPost, "/api/remote/account", ""},
		{http.MethodDelete, "/api/remote/account", ""},
		{http.MethodGet, "/api/remote/models?project=demo-project", ""},
	} {
		if rec := do(s, c.method, c.path, c.body); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a CLI = %d, want 503", c.method, c.path, rec.Code)
		}
	}
	if rec := do(s, http.MethodGet, "/api/remote", ""); rec.Code != http.StatusOK {
		t.Errorf("list without a CLI = %d, want 200", rec.Code)
	}

	// "Installed": a script that answers auth status, at a path found lazily.
	path = filepath.Join(t.TempDir(), "android")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'Logged in as Test Person <test@example.com>'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec = do(s, http.MethodGet, "/api/remote/account", "")
	acct = RemoteAccountResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &acct); err != nil || !acct.CLIInstalled || !acct.SignedIn {
		t.Fatalf("account after install = %s (%v), want installed and signed in", rec.Body, err)
	}
}
