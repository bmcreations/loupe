package target

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	crashSimA = "AAAAAAAA-1111-2222-3333-444444444444"
	crashSimB = "BBBBBBBB-1111-2222-3333-444444444444"
	crashDir  = "testdata/crashes"
)

func TestListCrashesKeepsOneSimulatorsCrashesNewestFirst(t *testing.T) {
	got, err := listCrashes(context.Background(), crashDir, simCoalition(crashSimA), CrashQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d reports, want 2: %+v", len(got), got)
	}
	first := got[0]
	if first.ID != "DC1854F2-979F-4AC8-915C-9E6DA11A4AAC" || first.Process != "Sample" ||
		first.BundleID != "com.example.sample" || first.Version != "1.2" || first.Build != "34" ||
		first.PID != 4242 || first.Kind != "crash" {
		t.Errorf("first = %+v", first)
	}
	if want := "EXC_CRASH (SIGABRT), Abort trap: 6"; first.Summary != want {
		t.Errorf("summary = %q, want %q", first.Summary, want)
	}
	// captureTime, not the header's timestamp, which is when the file was
	// written.
	if want := time.Date(2026, 10, 22, 23, 2, 7, 990e6, time.UTC); !first.Time.Equal(want) {
		t.Errorf("time = %v, want %v", first.Time, want)
	}
	// The daemon has no bundle and is in Retired; it is still listed.
	if got[1].Process != "daemond" || got[1].BundleID != "" {
		t.Errorf("second = %+v", got[1])
	}
}

func TestListCrashesSinceAndLimit(t *testing.T) {
	since := time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC)
	got, err := listCrashes(context.Background(), crashDir, simCoalition(crashSimA), CrashQuery{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	// The fixtures' mtimes are when they were checked out, after since, so
	// this exercises the captureTime comparison.
	if len(got) != 1 || got[0].Process != "Sample" {
		t.Errorf("since: %+v", got)
	}
	got, _ = listCrashes(context.Background(), crashDir, simCoalition(crashSimA), CrashQuery{Limit: 1})
	if len(got) != 1 || got[0].Process != "Sample" {
		t.Errorf("limit: %+v", got)
	}
}

func TestReadCrashRefusesAnotherTargetsReport(t *testing.T) {
	raw, err := readCrash(crashDir, simCoalition(crashSimA), "dc1854f2-979f-4ac8-915c-9e6da11a4aac")
	if err != nil || len(raw) == 0 {
		t.Fatalf("own report: %v", err)
	}
	for _, id := range []string{
		"0E0E0E0E-AAAA-BBBB-CCCC-DDDDDDDDDDDD", // sim B's
		"5A5A5A5A-AAAA-BBBB-CCCC-DDDDDDDDDDDD", // the Mac's adb
		"7C7C7C7C-AAAA-BBBB-CCCC-DDDDDDDDDDDD", // sim A's, not a crash
		"00000000-0000-0000-0000-000000000000",
		"../../etc/passwd",
	} {
		if _, err := readCrash(crashDir, simCoalition(crashSimA), id); !errors.Is(err, ErrCrashNotFound) {
			t.Errorf("%s: err = %v, want ErrCrashNotFound", id, err)
		}
	}
}
