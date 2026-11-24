package android

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

func readLogcatFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "logcat", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// logerrors-uid.txt is logErrorArgs's dump from an API 37 emulator: a React
// Native debug build's console.error of a string, then a thrown Error, then a
// Flutter debug build's print and a Dart error thrown from a button, its
// names changed to a sample's.
func TestParseLogErrorDumpReadsARealDump(t *testing.T) {
	entries := parseLogErrorDump(readLogcatFixture(t, "logerrors-uid.txt"))
	if len(entries) != 4 {
		t.Fatalf("entries = %+v", entries)
	}
	if e := entries[0]; e.Runtime != target.LogReactNative || !e.Error || e.UID != 10233 || e.PID != 4551 || e.Text != "sample-js: test error string" {
		t.Errorf("entries[0] = %+v", e)
	}
	if e := entries[1]; e.Text != "[Error: sample-js: test crash from the Crash button]" || !e.Time.Equal(time.Unix(1795527011, 236862000)) {
		t.Errorf("entries[1] = %+v", e)
	}
	if e := entries[2]; e.Runtime != target.LogFlutter || e.Error || e.UID != 10236 {
		t.Errorf("entries[2] = %+v", e)
	}
	e := entries[3]
	if e.Runtime != target.LogFlutter || !e.Error || e.PID != 5994 || e.UID != 10236 || !e.Time.Equal(time.Unix(1795527034, 63098000)) {
		t.Errorf("entries[3] = %+v", e)
	}
	lines := strings.Split(e.Text, "\n")
	if len(lines) != 8 || !strings.HasPrefix(lines[0], "[ERROR:flutter/runtime/dart_vm_initializer.cc(40)] Unhandled Exception: Bad state: ") || lines[7] != "" {
		t.Errorf("text = %q", e.Text)
	}
	if !strings.HasPrefix(e.Raw, "         1795527034.063098 10236  5994  5994 E flutter : [ERROR:") {
		t.Errorf("raw = %q", e.Raw)
	}
}

func TestParseLogErrorDumpSplitsMessagesByHeader(t *testing.T) {
	dump := "--------- beginning of main\n" +
		"  1795524490.276100 10233  4551  4675 E ReactNativeJS: [Error: sample-js: thrown]\n" +
		"  1795524490.276100 10233  4551  4675 E ReactNativeJS: [Error: same time, other message]\n" +
		"  1795524491.000001 10233  4551  4675 W ReactNativeJS: [Error: warn level]\n" +
		"  1795524492.000001 10236  4600  4600 I flutter : Another exception was thrown: x\n" +
		"  1795524492.000001 10236  4600  4601 I flutter : other thread\n" +
		"  1795524493.000001 10236  4600  4600 E flutter :\n" +
		"  1795524494.000001  root     1     1 E ReactNativeJS: named uid\n"
	entries := parseLogErrorDump([]byte(dump))
	var got []string
	for _, e := range entries {
		got = append(got, e.Text)
	}
	// Two lines with one header are one message, even when a second log
	// call stamped its line alike.
	want := []string{"[Error: sample-js: thrown]\n[Error: same time, other message]", "[Error: warn level]", "Another exception was thrown: x", "other thread", "", "named uid"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("texts = %q", got)
	}
	if entries[0].Runtime != target.LogReactNative || !entries[0].Error || entries[0].UID != 10233 || entries[1].Error || entries[2].Runtime != target.LogFlutter || entries[2].Error || entries[5].UID != 0 {
		t.Errorf("entries = %+v", entries)
	}
}

func TestLogErrorArgsNamesOnlyTheTwoTags(t *testing.T) {
	got := logErrorArgs(time.Time{})
	want := []string{"logcat", "-d", "-v", "threadtime", "-v", "epoch", "-v", "usec", "-v", "uid", "-s", "ReactNativeJS:E", "flutter:I"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %q", got)
	}
	got = logErrorArgs(time.Unix(1795524584, 382140000))
	if strings.Join(got[10:12], " ") != "-T 1795524584.382" {
		t.Errorf("args = %q", got)
	}
}

// writeFakeLogcatADB stands in for adb, answering a command by its last
// argument: the error dump ends in flutter:I, ps in PID,NAME and pm's
// package list in -U. Anything else, dumpsys among them, gets an empty
// dropbox.
func writeFakeLogcatADB(t *testing.T, dump []byte, ps, pkgs string) (path, argsLog string) {
	t.Helper()
	dir := t.TempDir()
	for name, b := range map[string][]byte{
		"flutter:I.txt": dump,
		"PID,NAME.txt":  []byte(ps),
		"-U.txt":        []byte(pkgs),
		"empty":         readDropboxFixture(t, "dropbox-empty.txt"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	argsLog = filepath.Join(dir, "args")
	path = filepath.Join(dir, "fake-adb")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> \"" + argsLog + "\"\n" +
		"for a; do last=$a; done\n" +
		"if [ -f \"" + dir + "/$last.txt\" ]; then cat \"" + dir + "/$last.txt\"; else cat \"" + dir + "/empty\"; fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argsLog
}

// samplePackages is `pm list packages -U` for the fixture's two apps and two
// packages that share the system uid.
const samplePackages = "package:com.example.js uid:10233\n" +
	"package:com.example.dart uid:10236\n" +
	"package:com.android.a uid:1000\n" +
	"package:com.android.b uid:1000\n"

func TestDeviceListsAndFetchesLogErrors(t *testing.T) {
	// The React Native app has died since its error; the Flutter one runs.
	adb, _ := writeFakeLogcatADB(t, readLogcatFixture(t, "logerrors-uid.txt"), "  PID NAME\n 5994 com.example.dart\n", samplePackages)
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	list, err := tgt.Crashes(context.Background(), target.CrashQuery{})
	if err != nil {
		t.Fatal(err)
	}
	// The console.error of a string and the print are not errors.
	want := []target.CrashReport{
		{ID: "dartError-1795527034063098", Kind: "dartError", Time: time.Unix(1795527034, 63098000).UTC(), Process: "com.example.dart", BundleID: "com.example.dart", PID: 5994,
			Summary: "Unhandled Exception: Bad state: sample-dart: test crash from the Crash button"},
		{ID: "jsError-1795527011236862", Kind: "jsError", Time: time.Unix(1795527011, 236862000).UTC(), BundleID: "com.example.js", PID: 4551,
			Summary: "Error: sample-js: test crash from the Crash button"},
	}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("list = %+v", list)
	}
	body, err := tgt.Crash(context.Background(), "dartError-1795527034063098")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "         1795527034.063098 10236  5994  5994 E flutter : [ERROR:") || strings.Count(string(body), "\n") != 8 {
		t.Errorf("body = %q", body)
	}
	if _, err := tgt.Crash(context.Background(), "dartError-1795527034063099"); !errors.Is(err, target.ErrCrashNotFound) {
		t.Errorf("Crash of a time with no error = %v, want ErrCrashNotFound", err)
	}
}

// A uid names the app when one package holds it. ps's name is kept only
// where it agrees, since a dead app's pid can be another process's now.
func TestDeviceLogErrorsNameTheAppByUID(t *testing.T) {
	dump := "  1795524490.000001 10233  4551  4551 E ReactNativeJS: [Error: pid reused]\n" +
		"  1795524491.000001  1000   700   700 E ReactNativeJS: [Error: shared uid]\n" +
		"  1795524492.000001 10999   800   800 E ReactNativeJS: [Error: uid with no package]\n"
	ps := "  PID NAME\n 4551 com.example.other\n  700 com.android.b\n  800 com.example.gone:remote\n"
	adb, _ := writeFakeLogcatADB(t, []byte(dump), ps, samplePackages)
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	list, err := tgt.Crashes(context.Background(), target.CrashQuery{})
	if err != nil {
		t.Fatal(err)
	}
	type app struct{ process, bundle string }
	var got []app
	for _, r := range list {
		got = append(got, app{r.Process, r.BundleID})
	}
	want := []app{{"com.example.gone:remote", "com.example.gone"}, {"com.android.b", "com.android.b"}, {"", "com.example.js"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("apps = %+v", got)
	}
}

// The dropbox's crashes and the log's errors are one listing, newest
// first, under one limit.
func TestDeviceCrashesMergesTheDropboxAndTheLog(t *testing.T) {
	dir := t.TempDir()
	dropbox := filepath.Join(dir, "data_app_crash.txt")
	if err := os.WriteFile(dropbox, readDropboxFixture(t, "dropbox-data_app_crash-api37.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 1792722095531 is the dropbox crash's millis; one log error a second
	// after it and one a second before.
	dump := "  1792722096.531000 10233  10  10 E ReactNativeJS: [Error: after]\n" +
		"  1792722094.531000 10233  10  10 E ReactNativeJS: [Error: before]\n"
	adb, argsLog := writeFakeLogcatADB(t, []byte(dump), "", "")
	// The dropbox tag's fixture goes beside the fake's own files.
	if err := os.Rename(dropbox, filepath.Join(filepath.Dir(adb), "data_app_crash.txt")); err != nil {
		t.Fatal(err)
	}
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	list, err := tgt.Crashes(context.Background(), target.CrashQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "jsError-1792722096531000" || list[1].ID != "data_app_crash-1792722095531" {
		t.Fatalf("list = %+v", list)
	}
	args, _ := os.ReadFile(argsLog)
	if !strings.Contains(string(args), "exec-out logcat -d -v threadtime -v epoch -v usec -v uid -s ReactNativeJS:E flutter:I") {
		t.Errorf("adb calls = %s", args)
	}
}
