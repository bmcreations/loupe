package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readDropboxFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "dropbox", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The fixtures are redacted captures: a Java crash on API 37 (whose body
// opens with SystemUptimeMs ahead of Process) and on API 29 (which opens
// with Process), a native crash and an ANR from API 37, the ANR's 197KB
// body cut down to its header and subject.
func TestDropboxReportReadsEachKind(t *testing.T) {
	cases := []struct {
		file, tag string
		want      CrashReport
	}{
		{"dropbox-data_app_crash-api37.txt", "data_app_crash", CrashReport{
			ID: "data_app_crash-1792722095531", Kind: "crash",
			Process: "dev.bmcreations.selv.sample", BundleID: "dev.bmcreations.selv.sample",
			Version: "1.0", Build: "1", PID: 5783,
			Summary: "android.app.RemoteServiceException$CrashedByAdbException: shell-induced crash",
		}},
		{"dropbox-system_app_crash-api29.txt", "system_app_crash", CrashReport{
			ID: "system_app_crash-1792722196248", Kind: "crash",
			Process: "com.android.settings", BundleID: "com.android.settings",
			Version: "10", Build: "29", PID: 4312,
			Summary: "android.app.RemoteServiceException: shell-induced crash",
		}},
		{"dropbox-system_app_native_crash.txt", "system_app_native_crash", CrashReport{
			ID: "system_app_native_crash-1792694108563", Kind: "nativeCrash",
			Process: "com.google.android.bluetooth", BundleID: "com.google.android.bluetooth",
			Version: "17", Build: "37", PID: 1145,
			Summary: "signal 6 (SIGABRT), code -1 (SI_QUEUE), fault addr --------, 'system/gd/hci/hci_layer.cc:565 on_hardware_error: Hardware Error Event with code 0x42'",
		}},
		{"dropbox-system_app_anr.txt", "system_app_anr", CrashReport{
			ID: "system_app_anr-1792700263858", Kind: "anr",
			Process: "com.android.systemui", BundleID: "com.android.systemui",
			Version: "17", Build: "37", PID: 23525,
			Summary: "executing service com.android.systemui/.keyguard.KeyguardService, waited 20367ms",
		}},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			entries := parseDropbox(readDropboxFixture(t, c.file), c.tag)
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(entries))
			}
			got := dropboxReport(entries[0])
			want := c.want
			want.Time = time.UnixMilli(entries[0].millis).UTC()
			if got != want {
				t.Errorf("report\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// The header's wall-clock time is the device's local time, so the row's
// time comes from the file name's epoch millis instead.
func TestDropboxTimeIsTheFileNamesMillis(t *testing.T) {
	e := parseDropbox(readDropboxFixture(t, "dropbox-system_app_crash-api29.txt"), "system_app_crash")[0]
	if want := time.Date(2026, 10, 23, 2, 23, 16, 248e6, time.UTC); !dropboxReport(e).Time.Equal(want) {
		t.Errorf("time = %v, want %v", dropboxReport(e).Time, want)
	}
}

func TestParseDropboxSkipsTheEmptyTagAndOtherTags(t *testing.T) {
	if got := parseDropbox(readDropboxFixture(t, "dropbox-empty.txt"), "data_app_anr"); len(got) != 0 {
		t.Errorf("empty tag parsed %d entries", len(got))
	}
	// An entry is only one of the tag asked for.
	if got := parseDropbox(readDropboxFixture(t, "dropbox-data_app_crash-api37.txt"), "system_app_crash"); len(got) != 0 {
		t.Errorf("another tag's output parsed %d entries", len(got))
	}
}

func TestDropboxReportsNewestFirstWithSinceAndLimit(t *testing.T) {
	var entries []dropboxEntry
	for _, f := range []struct{ file, tag string }{
		{"dropbox-data_app_crash-api37.txt", "data_app_crash"},
		{"dropbox-system_app_crash-api29.txt", "system_app_crash"},
		{"dropbox-system_app_native_crash.txt", "system_app_native_crash"},
		{"dropbox-system_app_anr.txt", "system_app_anr"},
	} {
		entries = append(entries, parseDropbox(readDropboxFixture(t, f.file), f.tag)...)
	}
	ids := func(rs []CrashReport) string {
		var s []string
		for _, r := range rs {
			s = append(s, r.ID)
		}
		return strings.Join(s, " ")
	}
	all := dropboxReports(append([]dropboxEntry(nil), entries...), CrashQuery{})
	if want := "system_app_crash-1792722196248 data_app_crash-1792722095531 system_app_anr-1792700263858 system_app_native_crash-1792694108563"; ids(all) != want {
		t.Errorf("order = %s", ids(all))
	}
	if got := dropboxReports(append([]dropboxEntry(nil), entries...), CrashQuery{Limit: 2}); len(got) != 2 {
		t.Errorf("limit 2 gave %d", len(got))
	}
	since := time.UnixMilli(1792700263858)
	if got := ids(dropboxReports(append([]dropboxEntry(nil), entries...), CrashQuery{Since: since})); got != "system_app_crash-1792722196248 data_app_crash-1792722095531 system_app_anr-1792700263858" {
		t.Errorf("since = %s", got)
	}
}

func TestValidCrashIDTakesBothShapes(t *testing.T) {
	for _, ok := range []string{"0F1E2D3C-4B5A-6978-8796-A5B4C3D2E1F0", "data_app_crash-1792722095531", "system_server_native_crash-1792694108563"} {
		if !ValidCrashID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"data_app_wtf-1792722095531", "data_app_crash-", "data_app_crash-12", "data_app_crash-1792722095531;ls", "../data_app_crash-1792722095531"} {
		if ValidCrashID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// writeFakeDropboxADB stands in for adb: it answers `exec-out dumpsys
// dropbox --print --file <tag>` with $FAKE_DROPBOX_DIR/<tag>.txt, or the
// empty tag's output when there is none.
func writeFakeDropboxADB(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for tag, fixture := range files {
		if err := os.WriteFile(filepath.Join(dir, tag+".txt"), readDropboxFixture(t, fixture), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "empty"), readDropboxFixture(t, "dropbox-empty.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fake-adb")
	script := "#!/bin/sh\n" +
		"for a; do tag=$a; done\n" +
		"if [ -f \"" + dir + "/$tag.txt\" ]; then cat \"" + dir + "/$tag.txt\"; else cat \"" + dir + "/empty\"; fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestADBTargetListsAndFetchesDropboxCrashes(t *testing.T) {
	adb := writeFakeDropboxADB(t, map[string]string{
		"data_app_crash":   "dropbox-data_app_crash-api37.txt",
		"system_app_crash": "dropbox-system_app_crash-api29.txt",
	})
	tgt := NewADBTarget(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	list, err := tgt.Crashes(context.Background(), CrashQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "system_app_crash-1792722196248" {
		t.Fatalf("list = %+v", list)
	}
	body, err := tgt.Crash(context.Background(), "data_app_crash-1792722095531")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "2026-10-22 22:21:35 data_app_crash") || !strings.Contains(string(body), "shell-induced crash") {
		t.Errorf("body = %q", body)
	}
	for _, id := range []string{"data_app_crash-1792722095530", "data_app_anr-1792722095531", "0F1E2D3C-4B5A-6978-8796-A5B4C3D2E1F0"} {
		if _, err := tgt.Crash(context.Background(), id); !errors.Is(err, ErrCrashNotFound) {
			t.Errorf("Crash(%q) = %v, want ErrCrashNotFound", id, err)
		}
	}
}
