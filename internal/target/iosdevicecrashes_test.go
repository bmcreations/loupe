package target

import (
	"context"
	"errors"
	"path"
	"testing"
	"time"
)

// A trimmed `devicectl device info files --domain-type systemCrashLogs`
// from Brandon's iPhone, 22 October: one app crash among analytics, Jetsam and
// resource reports, a spindump too large to be a crash, and Retired.
const phoneFilesJSON = `{"result":{"files":[
 {"relativePath":"Retired","metadata":{"size":0,"lastModDate":"2026-10-20T10:00:00Z"},"resources":{"isDirectory":true}},
 {"relativePath":"LoupeRunner-Runner-2026-10-22-230313.ips","metadata":{"size":19456,"lastModDate":"2026-10-22T23:03:13Z"},"resources":{"isDirectory":false}},
 {"relativePath":"Retired/Weather-2026-10-19-101500.ips","metadata":{"size":30000,"lastModDate":"2026-10-19T10:15:00Z"},"resources":{"isDirectory":false}},
 {"relativePath":"Analytics-2026-10-22-000000.ips","metadata":{"size":90000,"lastModDate":"2026-10-22T00:00:00Z"},"resources":{"isDirectory":false}},
 {"relativePath":"JetsamEvent-2026-10-21-080000.ips","metadata":{"size":60000,"lastModDate":"2026-10-21T08:00:00Z"},"resources":{"isDirectory":false}},
 {"relativePath":"backboardd.cpu_resource-2026-10-21-090000.ips","metadata":{"size":40000,"lastModDate":"2026-10-21T09:00:00Z"},"resources":{"isDirectory":false}},
 {"relativePath":"stacks+SpringBoard-2026-10-21-090000.ips","metadata":{"size":12582912,"lastModDate":"2026-10-21T09:00:00Z"},"resources":{"isDirectory":false}},
 {"relativePath":"Assistant/SiriSearchFeedback-2026-10-21-090000.ips","metadata":{"size":1000,"lastModDate":"2026-10-21T09:00:00Z"},"resources":{"isDirectory":false}},
 {"relativePath":"log-aggregated-2026-10-21-090000.ips.json","metadata":{"size":1000,"lastModDate":"2026-10-21T09:00:00Z"},"resources":{"isDirectory":false}}
]}}`

func TestPhoneCrashCandidatesSkipNoise(t *testing.T) {
	files, err := parsePhoneFiles([]byte(phoneFilesJSON))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range phoneCrashCandidates(files, time.Time{}) {
		names = append(names, f.RelativePath)
	}
	want := []string{"LoupeRunner-Runner-2026-10-22-230313.ips", "Retired/Weather-2026-10-19-101500.ips"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Errorf("candidates = %v, want %v", names, want)
	}

	// A file last written before since cannot hold a crash after it.
	got := phoneCrashCandidates(files, time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	if len(got) != 1 || path.Base(got[0].RelativePath) != "LoupeRunner-Runner-2026-10-22-230313.ips" {
		t.Errorf("since: %+v", got)
	}
}

// A phone's cache holds only its own reports, so an empty coalition lists
// every crash report and still drops what is not one.
func TestListCrashesWithNoCoalitionTakesAnyCrash(t *testing.T) {
	got, err := listCrashes(context.Background(), crashDir, "", CrashQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d reports, want 4: %+v", len(got), got)
	}
	if _, err := readCrash(crashDir, "", "0E0E0E0E-AAAA-BBBB-CCCC-DDDDDDDDDDDD"); err != nil {
		t.Errorf("sim B's report: %v", err)
	}
	if _, err := readCrash(crashDir, "", "7C7C7C7C-AAAA-BBBB-CCCC-DDDDDDDDDDDD"); !errors.Is(err, ErrCrashNotFound) {
		t.Errorf("non-crash: err = %v, want ErrCrashNotFound", err)
	}
}
