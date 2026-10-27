package target

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSimLogArgs(t *testing.T) {
	since := time.Date(2026, 10, 22, 9, 5, 7, 900e6, time.FixedZone("EDT", -4*3600))
	for _, c := range []struct {
		name   string
		q      LogcatQuery
		pid    int
		window time.Duration
		want   []string
	}{
		{"dump", LogcatQuery{Lines: 10}, 0, time.Minute,
			[]string{"show", "--style", "compact", "--last", "60s"}},
		{"since", LogcatQuery{Since: since}, 0, 0,
			[]string{"show", "--style", "compact", "--start", "2026-10-22 09:05:07-0400"}},
		{"follow", LogcatQuery{Follow: true, Lines: 10}, 0, 0,
			[]string{"stream", "--style", "compact"}},
		{"pid", LogcatQuery{Lines: 10}, 959, time.Hour,
			[]string{"show", "--style", "compact", "--last", "3600s", "--predicate", "processID == 959"}},
		{"ndjson", LogcatQuery{Follow: true, Format: LogFormatNDJSON}, 0, 0,
			[]string{"stream", "--style", "ndjson"}},
	} {
		if got := simLogArgs(c.q, c.pid, c.window); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSimLogFilterDropsTheHeaderOnlyFirst(t *testing.T) {
	in := "Timestamp               Ty Process[PID:TID]\n" +
		"2026-10-22 09:00:00.000 Df a[1:2] one\n" +
		"Timestamp is a word a message may start with\n"
	got := readAll(t, newSimLogFilter(io.NopCloser(strings.NewReader(in)), ""))
	want := "2026-10-22 09:00:00.000 Df a[1:2] one\nTimestamp is a word a message may start with\n"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestSimLogFilterDropsTheNDJSONTrailer(t *testing.T) {
	in := `{"eventMessage":"one"}` + "\n" + `{"count":1,"finished":1}` + "\n"
	got := readAll(t, newSimLogFilter(io.NopCloser(strings.NewReader(in)), LogFormatNDJSON))
	if got != `{"eventMessage":"one"}`+"\n" {
		t.Errorf("got %q", got)
	}
}

func TestSimLogFilterKeepsAnUnterminatedLastLine(t *testing.T) {
	got := readAll(t, newSimLogFilter(io.NopCloser(strings.NewReader("Timestamp x\na\nb")), ""))
	if got != "a\nb" {
		t.Errorf("got %q", got)
	}
}

func TestPidForApp(t *testing.T) {
	list := "PID\tStatus\tLabel\n" +
		"-\t0\tUIKitApplication:com.apple.mobilesafari[9a1c][rb-legacy]\n" +
		"4211\t0\tUIKitApplication:com.apple.Preferences[2f3e][rb-legacy]\n" +
		"4300\t0\tcom.apple.Preferences.extension\n"
	if got := pidForApp(list, "com.apple.Preferences"); got != 4211 {
		t.Errorf("Preferences = %d; want 4211", got)
	}
	if got := pidForApp(list, "com.apple.mobilesafari"); got != 0 {
		t.Errorf("a listed app with no pid = %d; want 0", got)
	}
	if got := pidForApp(list, "com.apple"); got != 0 {
		t.Errorf("a prefix of a bundle = %d; want 0", got)
	}
}

func TestLastFilteredKeepsTheLastN(t *testing.T) {
	got, err := lastFiltered(io.NopCloser(strings.NewReader("a\nb\nc\nd\ne\n")), 2)
	if err != nil || !reflect.DeepEqual(got, []string{"d", "e"}) {
		t.Errorf("got %q, %v", got, err)
	}
	got, _ = lastFiltered(io.NopCloser(strings.NewReader("a\n")), 3)
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("short: got %q", got)
	}
}

// fakeSimLog stands in for simctl, answering each --last window with the
// lines given for it.
func fakeSimLog(t *testing.T, byWindow map[string]string) *[]string {
	t.Helper()
	var asked []string
	old := simLogStart
	simLogStart = func(ctx context.Context, udid string, args []string) (io.ReadCloser, error) {
		w := args[len(args)-1]
		asked = append(asked, w)
		return io.NopCloser(strings.NewReader("Timestamp header\n" + byWindow[w])), nil
	}
	t.Cleanup(func() { simLogStart = old })
	return &asked
}

func TestSimLogTailWidensUntilItHasEnough(t *testing.T) {
	asked := fakeSimLog(t, map[string]string{"60s": "a\n", "600s": "a\nb\nc\n", "3600s": "x\n"})
	got, err := simLogTail(context.Background(), "u", LogcatQuery{Lines: 2}, 0)
	if err != nil || string(got) != "b\nc\n" {
		t.Errorf("got %q, %v", got, err)
	}
	if !reflect.DeepEqual(*asked, []string{"60s", "600s"}) {
		t.Errorf("windows = %q; want it to stop at the one with enough", *asked)
	}
}

func TestSimLogTailReturnsTheWidestWhenNoneHasEnough(t *testing.T) {
	fakeSimLog(t, map[string]string{"60s": "a\n", "600s": "a\nb\n", "3600s": "a\nb\nc\n"})
	got, err := simLogTail(context.Background(), "u", LogcatQuery{Lines: 10}, 0)
	if err != nil || string(got) != "a\nb\nc\n" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestSimLogTailStopsWideningPastTheDeadline(t *testing.T) {
	asked := fakeSimLog(t, map[string]string{"60s": "a\n"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	got, err := simLogTail(ctx, "u", LogcatQuery{Lines: 10}, 0)
	if err != nil || string(got) != "a\n" {
		t.Errorf("got %q, %v", got, err)
	}
	if len(*asked) != 1 {
		t.Errorf("windows = %q; want it to stop after the first", *asked)
	}
}

func TestADBLogcatRefusesAFormat(t *testing.T) {
	tg := &ADBTarget{info: DeviceInfo{Serial: "s", State: "device"}}
	_, err := tg.Logcat(context.Background(), LogcatQuery{Format: LogFormatNDJSON})
	if !errors.Is(err, ErrLogQueryUnsupported) {
		t.Errorf("err = %v; want ErrLogQueryUnsupported", err)
	}
}

func TestValidBundleID(t *testing.T) {
	for _, ok := range []string{"com.apple.Preferences", "dev.bmc-creations.app", "Foo"} {
		if !ValidBundleID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-com.a", ".com.a", "com.a;reboot", "com a", "com/a"} {
		if ValidBundleID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
