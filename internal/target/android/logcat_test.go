package android

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target/ios"

	"github.com/bmcreations/loupe/internal/target"
)

func TestADBLogcatRefusesAFormat(t *testing.T) {
	tg := &Device{info: DeviceInfo{Serial: "s", State: "device"}}
	_, err := tg.Logcat(context.Background(), target.LogcatQuery{Format: ios.LogFormatNDJSON})
	if !errors.Is(err, ios.ErrLogQueryUnsupported) {
		t.Errorf("err = %v; want ErrLogQueryUnsupported", err)
	}
}

func TestParseThreadtime(t *testing.T) {
	for _, c := range []struct {
		line string
		tags []string
		msg  string
	}{
		{"11-24 07:50:01.123  1234  1250 I ReactNativeJS: 'porchlight-js', 'sent'", []string{"ReactNativeJS"}, "'porchlight-js', 'sent'"},
		// threadtime pads a short tag to eight columns before the colon.
		{"11-24 07:50:01.123  1234  1250 I flutter : porchlight: tapped", []string{"flutter"}, "porchlight: tapped"},
		{"11-24 07:50:01.123   812   812 W ActivityManager: Slow operation: 52ms", []string{"ActivityManager"}, "Slow operation: 52ms"},
		{"11-24 07:50:01.123   812   812 D tag     : ", []string{"tag"}, ""},
		{"--------- beginning of main", nil, ""},
		{"", nil, ""},
	} {
		got := parseThreadtime([]byte(c.line))
		if !reflect.DeepEqual(got.Tags, c.tags) || string(got.Message) != c.msg || got.Continues {
			t.Errorf("parseThreadtime(%q) = %q %q %v; want %q %q", c.line, got.Tags, got.Message, got.Continues, c.tags, c.msg)
		}
	}
}

const threadtimeSample = "--------- beginning of main\n" +
	"11-24 07:50:01.100  4242  4242 I ReactNativeJS: 'porchlight-js', 'sent'\n" +
	"11-24 07:50:01.200  4242  4260 I flutter : porchlight tapped\n" +
	"11-24 07:50:01.300  4242  4242 I ReactNativeJS: 'other'\n" +
	"11-24 07:50:01.400   812   812 I ActivityManager: PORCHLIGHT-JS started\n"

// A filter is applied to what logcat prints, so the request's tag and text
// never reach the device's shell, and a dump keeps the last lines that
// match rather than those of the last lines logcat printed.
func TestLogcatTagAndTextFilterOnTheHost(t *testing.T) {
	adb, argv := fakeADB(t, "cat <<'EOF'\n"+threadtimeSample+"EOF")
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	for _, c := range []struct {
		q    target.LogcatQuery
		want string
	}{
		{target.LogcatQuery{Tag: "reactnativejs", Lines: 5}, "'porchlight-js', 'sent'|'other'"},
		{target.LogcatQuery{Tag: "reactnativejs", Lines: 1}, "'other'"},
		{target.LogcatQuery{Tag: "flutter", Lines: 5}, "porchlight tapped"},
		{target.LogcatQuery{Text: "Porchlight-JS", Lines: 5}, "'porchlight-js', 'sent'|PORCHLIGHT-JS started"},
		{target.LogcatQuery{Tag: "ReactNativeJS", Text: "other", Lines: 5}, "'other'"},
	} {
		lc, err := tgt.Logcat(context.Background(), c.q)
		if err != nil {
			t.Fatalf("%+v: %v", c.q, err)
		}
		body, _ := io.ReadAll(lc)
		lc.Close()
		var msgs []string
		for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			msgs = append(msgs, string(parseThreadtime([]byte(l)).Message))
		}
		if got := strings.Join(msgs, "|"); got != c.want {
			t.Errorf("%+v: kept %q; want %q", c.q, got, c.want)
		}
	}
	for _, a := range readArgv(t, argv) {
		if a != "-s s exec-out logcat -v threadtime -t 10000" {
			t.Errorf("adb argv %q; want the newest lines with nothing from the request", a)
		}
	}
}

// fakeLogcatBuffer stands in for adb over a ring buffer of the given lines.
// It answers -t N with the buffer's last N lines and hangs on any argv that
// matches hang, a shell case pattern, until it is killed, the way a dump of
// a buffer too large to move in time does.
func fakeLogcatBuffer(t *testing.T, lines []string, hang string) (*Device, string) {
	t.Helper()
	buf := filepath.Join(t.TempDir(), "buffer")
	if err := os.WriteFile(buf, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	adb, argv := fakeADB(t, `args="$*"
case "$args" in
`+hang+`) exec sleep 30 ;;
*" -t "*) n=${args##* -t }; n=${n%% *}; exec tail -n "$n" '`+buf+`' ;;
*) exec cat '`+buf+`' ;;
esac`)
	return NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", ""), argv
}

// logBuffer is n noise lines with match lines at the given indexes.
func logBuffer(n int, matchAt ...int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("11-24 07:50:01.100  1828  2018 V serviceDiscovery: noise %d", i)
	}
	for _, i := range matchAt {
		lines[i] = fmt.Sprintf("11-24 07:50:01.200  4242  4242 I ReactNativeJS: match %d", i)
	}
	return lines
}

// filteredDump is the dump tgt gives for q within budget, and its Cut.
func filteredDump(t *testing.T, tgt *Device, q target.LogcatQuery, budget time.Duration) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	lc, err := tgt.Logcat(ctx, q)
	if err != nil {
		return "", "", err
	}
	// The handler reports a read cut short by its Close, so this does too.
	b, err := io.ReadAll(lc)
	if cerr := lc.Close(); err == nil {
		err = cerr
	}
	return string(b), lc.Cut, err
}

// A whole-buffer dump of a phone with large buffers outlasts the dump
// deadline, so a filtered dump reads the newest lines first and answers
// from them when they hold enough matches.
func TestLogcatFilteredDumpReadsTheNewestLinesFirst(t *testing.T) {
	tgt, argv := fakeLogcatBuffer(t, logBuffer(30000, 29997, 29998, 29999), "*' -d'*")
	got, cut, err := filteredDump(t, tgt, target.LogcatQuery{Tag: "reactnativejs", Lines: 3}, 5*time.Second)
	if err != nil {
		t.Fatalf("Logcat: %v", err)
	}
	if cut != "" {
		t.Errorf("Cut %q; want none, the window had every line asked for", cut)
	}
	if want := strings.Join(logBuffer(30000, 29997, 29998, 29999)[29997:], "\n") + "\n"; got != want {
		t.Errorf("kept %q; want %q", got, want)
	}
	if calls := readArgv(t, argv); !reflect.DeepEqual(calls, []string{"-s s exec-out logcat -v threadtime -t 10000"}) {
		t.Errorf("adb calls %q; want one read of the last 10000 lines", calls)
	}
}

// Matches older than the first window send it wider, and a window that
// comes back short was the whole buffer, so it stops there.
func TestLogcatFilteredDumpWidensUntilTheBufferEnds(t *testing.T) {
	tgt, argv := fakeLogcatBuffer(t, logBuffer(30000, 0, 1), "*' -d'*")
	got, cut, err := filteredDump(t, tgt, target.LogcatQuery{Tag: "ReactNativeJS", Lines: 5}, 5*time.Second)
	if err != nil {
		t.Fatalf("Logcat: %v", err)
	}
	if n := strings.Count(got, "match"); n != 2 {
		t.Errorf("kept %d matches; want both:\n%s", n, got)
	}
	if cut != "" {
		t.Errorf("Cut %q; want none, the whole buffer was read", cut)
	}
	calls := readArgv(t, argv)
	if len(calls) != 2 || calls[0] != "-s s exec-out logcat -v threadtime -t 10000" {
		t.Fatalf("adb calls %q; want the last 10000 lines, then a wider window", calls)
	}
	var n int
	if _, err := fmt.Sscanf(calls[1], "-s s exec-out logcat -v threadtime -t %d", &n); err != nil || n <= 30000 {
		t.Errorf("second call %q; want -t wider than the 30000-line buffer", calls[1])
	}
}

// When the deadline cuts a wider window, the dump answers with what the
// last finished window kept rather than failing.
func TestLogcatFilteredDumpKeepsTheLastWindowWhenTheNextIsCut(t *testing.T) {
	// Any -t of six digits or more hangs, so every window after the first.
	tgt, argv := fakeLogcatBuffer(t, logBuffer(30000, 29999), "*' -d'*|*' -t '??????*")
	got, cut, err := filteredDump(t, tgt, target.LogcatQuery{Text: "MATCH", Lines: 5}, 2*time.Second)
	if err != nil {
		t.Fatalf("Logcat: %v", err)
	}
	if cut != target.LogCutTime {
		t.Errorf("Cut %q; want %q, so the caller knows older lines went unread", cut, target.LogCutTime)
	}
	if want := logBuffer(30000, 29999)[29999] + "\n"; got != want {
		t.Errorf("kept %q; want the first window's one match %q", got, want)
	}
	if calls := readArgv(t, argv); len(calls) != 2 {
		t.Errorf("adb calls %q; want the first window and a wider one that was cut", calls)
	}
}

// A window too slow for a wider one to fit in what is left of the deadline
// is the answer, and the dump says time stopped it.
func TestLogcatFilteredDumpSaysWhenTheNextWindowWouldNotFit(t *testing.T) {
	buf := filepath.Join(t.TempDir(), "buffer")
	if err := os.WriteFile(buf, []byte(strings.Join(logBuffer(30000, 29999), "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	adb, argv := fakeADB(t, `args="$*"; n=${args##* -t }; n=${n%% *}; sleep 1.5; exec tail -n "$n" '`+buf+`'`)
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	got, cut, err := filteredDump(t, tgt, target.LogcatQuery{Tag: "ReactNativeJS", Lines: 5}, 3*time.Second)
	if err != nil {
		t.Fatalf("Logcat: %v", err)
	}
	if want := logBuffer(30000, 29999)[29999] + "\n"; got != want {
		t.Errorf("kept %q; want the first window's one match %q", got, want)
	}
	if cut != target.LogCutTime {
		t.Errorf("Cut %q; want %q", cut, target.LogCutTime)
	}
	if calls := readArgv(t, argv); len(calls) != 1 {
		t.Errorf("adb calls %q; want the first window alone", calls)
	}
}

// With nothing finished before the deadline there is no answer to give.
func TestLogcatFilteredDumpFailsWhenTheFirstWindowIsCut(t *testing.T) {
	tgt, _ := fakeLogcatBuffer(t, logBuffer(10, 9), "*")
	if _, _, err := filteredDump(t, tgt, target.LogcatQuery{Tag: "ReactNativeJS", Lines: 5}, 500*time.Millisecond); err == nil {
		t.Error("Logcat answered; want the cut read's error")
	}
}

// A pid narrows the read on the device, which usually brings the whole
// buffer back in time, so that is tried first, with half the deadline, and
// windows follow only when it does not finish.
func TestLogcatFilteredDumpWithAPidTriesTheWholeBufferFirst(t *testing.T) {
	q := target.LogcatQuery{PID: 42, Tag: "ReactNativeJS", Lines: 5}
	tgt, argv := fakeLogcatBuffer(t, logBuffer(30000, 0, 29999), "*' -t '*")
	got, _, err := filteredDump(t, tgt, q, 5*time.Second)
	if err != nil {
		t.Fatalf("Logcat: %v", err)
	}
	if n := strings.Count(got, "match"); n != 2 {
		t.Errorf("kept %d matches; want both:\n%s", n, got)
	}
	if calls := readArgv(t, argv); !reflect.DeepEqual(calls, []string{"-s s exec-out logcat -v threadtime -d --pid=42"}) {
		t.Errorf("adb calls %q; want one whole-buffer read for the pid", calls)
	}

	tgt, argv = fakeLogcatBuffer(t, logBuffer(30000, 29999), "*' -d'*")
	start := time.Now()
	got, _, err = filteredDump(t, tgt, q, 2*time.Second)
	if err != nil {
		t.Fatalf("Logcat with a slow whole buffer: %v", err)
	}
	if !strings.Contains(got, "match 29999") {
		t.Errorf("kept %q; want the newest match from a window", got)
	}
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Errorf("answered in %v; want the whole-buffer read given half the 2s deadline", took)
	}
	calls := readArgv(t, argv)
	if len(calls) < 2 || calls[0] != "-s s exec-out logcat -v threadtime -d --pid=42" || calls[1] != "-s s exec-out logcat -v threadtime -t 10000 --pid=42" {
		t.Errorf("adb calls %q; want the whole buffer, then the last 10000 lines", calls)
	}
}
