package ios

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// simLogErrorsFixture is what simLogErrorArgs' predicate read from a
// simulator after the Crash button in a React Native dev build and in a
// Flutter debug build: one entry from each that is an error, and two of
// Flutter's startup lines that are not.
const simLogErrorsFixture = "testdata/simlog/logerrors.ndjson"

func TestParseSimLogErrorsReadsTheRealEntries(t *testing.T) {
	f, err := os.Open(simLogErrorsFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := parseSimLogErrors(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4 (the count line skipped): %+v", len(entries), entries)
	}
	rn := entries[0]
	if rn.Runtime != target.LogReactNative || !rn.Error || rn.PID != 50866 || rn.Process != "Sample" {
		t.Errorf("RN entry = %+v", rn)
	}
	if want := time.Date(2026, 11, 24, 12, 50, 29, 597175000, time.UTC); !rn.Time.Equal(want) {
		t.Errorf("RN time = %v, want %v", rn.Time, want)
	}
	if entries[2].Runtime != target.LogFlutter || entries[2].Error {
		t.Errorf("Flutter print = %+v", entries[2])
	}
	if !strings.HasPrefix(entries[2].Text, "The Dart VM service") {
		t.Errorf("print text = %q; want the flutter: prefix dropped", entries[2].Text)
	}
	if !strings.Contains(entries[3].Text, "\n#1 ") {
		t.Errorf("the Dart stack is not in its entry: %q", entries[3].Text)
	}
}

func TestScanFindsTheRealSimulatorErrorsAndNothingElse(t *testing.T) {
	f, err := os.Open(simLogErrorsFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	parsed, err := parseSimLogErrors(f)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]target.LogEntry, len(parsed))
	for i, e := range parsed {
		entries[i] = e.LogEntry
	}
	got := target.ScanLogErrors(entries)
	if len(got) != 2 {
		t.Fatalf("got %d errors, want 2: %+v", len(got), got)
	}
	if got[0].Kind != target.KindJSError || got[0].Summary != "Error: sample-js: test crash from the Crash button" {
		t.Errorf("JS error = %+v", got[0])
	}
	if got[0].ID() != "jsError-1795524629597175" {
		t.Errorf("id = %q", got[0].ID())
	}
	if got[1].Kind != target.KindDartError || got[1].Summary != "Unhandled Exception: Bad state: sample-dart: test crash from the Crash button" || got[1].Process != "Runner" {
		t.Errorf("Dart error = %+v", got[1])
	}
}

func TestSimLogErrorArgs(t *testing.T) {
	got := strings.Join(simLogErrorArgs(time.Time{}), " ")
	if !strings.HasPrefix(got, "show --style ndjson --last 3600s --predicate ") {
		t.Errorf("no since: %q", got)
	}
	since := time.Date(2026, 11, 24, 12, 50, 29, 597e6, time.UTC)
	args := simLogErrorArgs(since)
	if args[3] != "--start" || args[4] != since.Format("2006-01-02 15:04:05-0700") {
		t.Errorf("since: %q", args)
	}
	if args[len(args)-1] != simLogErrorPredicate {
		t.Errorf("the predicate is not one argument: %q", args)
	}
}

// fakeSimErrors stands in for the simulator's log, launchctl and the apps'
// Info.plists, and points the reports at testdata, where crashSimA has two.
// Runner.app's plist reads; Sample.app's does not, as for an app deleted
// since, and launchctl names it.
func fakeSimErrors(t *testing.T, log func(ctx context.Context) (io.ReadCloser, error)) *[][]string {
	t.Helper()
	var asked [][]string
	oldLog, oldList, oldDir, oldBundle := simLogStart, simAppList, simCrashDir, appBundleID
	simLogStart = func(ctx context.Context, udid string, args []string) (io.ReadCloser, error) {
		if udid != crashSimA {
			t.Errorf("udid = %q", udid)
		}
		asked = append(asked, args)
		return log(ctx)
	}
	simAppList = func(context.Context, *Simulator) (string, error) {
		return "PID\tStatus\tLabel\n50866\t0\tUIKitApplication:com.example.sample[2b1c][rb-legacy]\n", nil
	}
	simCrashDir = func() string { return crashDir }
	appBundleID = func(_ context.Context, app string) (string, error) {
		if strings.HasSuffix(app, "/DC19B955-F187-4A9A-A6F6-50B05CEDD98E/Runner.app") {
			return "com.example.dart", nil
		}
		return "", ErrNotIOSApp
	}
	t.Cleanup(func() { simLogStart, simAppList, simCrashDir, appBundleID = oldLog, oldList, oldDir, oldBundle })
	return &asked
}

func fixtureLog(t *testing.T) func(context.Context) (io.ReadCloser, error) {
	return func(context.Context) (io.ReadCloser, error) { return os.Open(simLogErrorsFixture) }
}

func bootedCrashSim() *Simulator {
	s := NewSimulator(SimInfo{UDID: crashSimA, Name: "crashes", State: "Booted"}, "")
	s.booted.Store(true)
	return s
}

func TestSimulatorCrashesListsLogErrorsWithReports(t *testing.T) {
	asked := fakeSimErrors(t, fixtureLog(t))
	got, err := bootedCrashSim().Crashes(context.Background(), target.CrashQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range got {
		kinds = append(kinds, r.Kind)
	}
	// The log errors are from 2026-11-24, the reports from October.
	if strings.Join(kinds, ",") != "dartError,jsError,crash,crash" {
		t.Fatalf("kinds = %v: %+v", kinds, got)
	}
	if got[1].BundleID != "com.example.sample" || got[1].PID != 50866 {
		t.Errorf("JS error row = %+v", got[1])
	}
	if got[0].BundleID != "com.example.dart" || got[0].Process != "Runner" {
		t.Errorf("Dart error row = %+v; want the bundle ID from its app's Info.plist", got[0])
	}
	if len(*asked) != 1 {
		t.Errorf("log ran %d times", len(*asked))
	}
}

// The limit applies to the reports and the log errors each, so the newest
// of both come back.
func TestSimulatorCrashesLimitIsPerSource(t *testing.T) {
	fakeSimErrors(t, fixtureLog(t))
	got, err := bootedCrashSim().Crashes(context.Background(), target.CrashQuery{Limit: 1})
	if err != nil || len(got) != 2 || got[0].Kind != target.KindDartError || got[1].Kind != "crash" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestShutDownSimulatorListsReportsWithoutTheLog(t *testing.T) {
	fakeSimErrors(t, func(context.Context) (io.ReadCloser, error) {
		t.Error("log ran on a shut-down simulator")
		return nil, errors.New("not booted")
	})
	s := NewSimulator(SimInfo{UDID: crashSimA, Name: "crashes", State: "Shutdown"}, "")
	got, err := s.Crashes(context.Background(), target.CrashQuery{})
	if err != nil || len(got) != 2 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestSimulatorCrashReadsALogErrorByID(t *testing.T) {
	asked := fakeSimErrors(t, fixtureLog(t))
	body, err := bootedCrashSim().Crash(context.Background(), "jsError-1795524629597175")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Sample[50866] [Error: sample-js: test crash from the Crash button]") {
		t.Errorf("body = %q", body)
	}
	if a := (*asked)[0]; a[3] != "--start" {
		t.Errorf("args = %q; want a read from the id's time", a)
	}
	if _, err := bootedCrashSim().Crash(context.Background(), "jsError-1795524629597176"); !errors.Is(err, target.ErrCrashNotFound) {
		t.Errorf("a time with no error: %v", err)
	}
}

func TestSimulatorLogErrorScanIsBounded(t *testing.T) {
	fakeSimErrors(t, func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(blockingReader{ctx}), nil
	})
	old := simLogErrorTimeout
	simLogErrorTimeout = 50 * time.Millisecond
	t.Cleanup(func() { simLogErrorTimeout = old })
	start := time.Now()
	_, err := bootedCrashSim().Crashes(context.Background(), target.CrashQuery{})
	if err == nil || !strings.Contains(err.Error(), "took over") {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

// blockingReader reads nothing until its context ends, as log does when
// the process is killed.
type blockingReader struct{ ctx context.Context }

func (r blockingReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
