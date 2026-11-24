package ios

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

func TestSimLogArgs(t *testing.T) {
	since := time.Date(2026, 10, 22, 9, 5, 7, 900e6, time.FixedZone("EDT", -4*3600))
	for _, c := range []struct {
		name   string
		q      target.LogcatQuery
		pid    int
		window time.Duration
		want   []string
	}{
		{"dump", target.LogcatQuery{Lines: 10}, 0, time.Minute,
			[]string{"show", "--style", "compact", "--last", "60s", "--info"}},
		{"since", target.LogcatQuery{Since: since}, 0, 0,
			[]string{"show", "--style", "compact", "--start", "2026-10-22 09:05:07-0400", "--info"}},
		{"follow", target.LogcatQuery{Follow: true, Lines: 10}, 0, 0,
			[]string{"stream", "--style", "compact", "--info"}},
		{"pid", target.LogcatQuery{Lines: 10}, 959, time.Hour,
			[]string{"show", "--style", "compact", "--last", "3600s", "--info", "--predicate", "processID == 959"}},
		{"ndjson", target.LogcatQuery{Follow: true, Format: LogFormatNDJSON}, 0, 0,
			[]string{"stream", "--style", "ndjson", "--info"}},
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
		w := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--last" {
				w = args[i+1]
			}
		}
		asked = append(asked, w)
		return io.NopCloser(strings.NewReader("Timestamp header\n" + byWindow[w])), nil
	}
	t.Cleanup(func() { simLogStart = old })
	return &asked
}

func TestSimLogTailWidensUntilItHasEnough(t *testing.T) {
	asked := fakeSimLog(t, map[string]string{"60s": "a\n", "600s": "a\nb\nc\n", "3600s": "x\n"})
	got, err := simLogTail(context.Background(), "u", target.LogcatQuery{Lines: 2}, 0)
	if err != nil || string(got) != "b\nc\n" {
		t.Errorf("got %q, %v", got, err)
	}
	if !reflect.DeepEqual(*asked, []string{"60s", "600s"}) {
		t.Errorf("windows = %q; want it to stop at the one with enough", *asked)
	}
}

func TestSimLogTailReturnsTheWidestWhenNoneHasEnough(t *testing.T) {
	fakeSimLog(t, map[string]string{"60s": "a\n", "600s": "a\nb\n", "3600s": "a\nb\nc\n"})
	got, err := simLogTail(context.Background(), "u", target.LogcatQuery{Lines: 10}, 0)
	if err != nil || string(got) != "a\nb\nc\n" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestSimLogTailStopsWideningPastTheDeadline(t *testing.T) {
	asked := fakeSimLog(t, map[string]string{"60s": "a\n"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	got, err := simLogTail(ctx, "u", target.LogcatQuery{Lines: 10}, 0)
	if err != nil || string(got) != "a\n" {
		t.Errorf("got %q, %v", got, err)
	}
	if len(*asked) != 1 {
		t.Errorf("windows = %q; want it to stop after the first", *asked)
	}
}

func TestParseCompact(t *testing.T) {
	rn := []string{"com.facebook.react.log", "com.facebook.react.log:javascript"}
	for _, c := range []struct {
		line string
		tags []string
		msg  string
		cont bool
	}{
		{`2026-11-24 07:45:56.764 Df locationd[33406:2f24689] [com.apple.locationd.Utility:QA] {"msg":"x"}`,
			[]string{"com.apple.locationd.Utility", "com.apple.locationd.Utility:QA"}, `{"msg":"x"}`, false},
		// A category may hold spaces.
		{"2026-11-24 07:45:59.281 E  SpringBoard[33390:29d7943] [com.apple.coregraphics:Unknown process name] CGAffineTransformInvert: singular matrix.",
			[]string{"com.apple.coregraphics", "com.apple.coregraphics:Unknown process name"}, "CGAffineTransformInvert: singular matrix.", false},
		// launchd's subsystems hold brackets and spaces, and no category.
		{"2026-11-24 07:45:59.814 Df launchd_sim[33370:2f30703] [user/501/com.apple.managedappdistributiond [23706]:] xpcproxy_sim spawned with pid 23706",
			[]string{"user/501/com.apple.managedappdistributiond [23706]"}, "xpcproxy_sim spawned with pid 23706", false},
		// With no subsystem the sender's image is printed instead.
		{"2026-11-24 07:45:59.978 F  backboardd[33403:29d7a27] (IOSurface) IOSurfaceQueryTransactionList got non-success",
			[]string{"IOSurface"}, "IOSurfaceQueryTransactionList got non-success", false},
		{"2026-11-24 08:00:00.000 Df Runner[4300:1a2b] (Flutter) flutter: porchlight tapped",
			[]string{"Flutter"}, "flutter: porchlight tapped", false},
		// A process name may hold a space, and a message may start with a
		// bracket of its own.
		{"2026-11-24 08:00:00.000 I  Expo Go[4211:1a2b] [com.facebook.react.log:javascript] [porchlight] sent",
			rn, "[porchlight] sent", false},
		{"2026-11-24 08:00:00.000 I  Expo Go[4211:1a2b] [com.facebook.react.log:javascript]", rn, "", false},
		{"}, with animation: true", nil, "", true},
		{"    \"FLAGS\" = 5;", nil, "", true},
	} {
		got := parseCompact([]byte(c.line))
		if !reflect.DeepEqual(got.Tags, c.tags) || string(got.Message) != c.msg || got.Continues != c.cont {
			t.Errorf("parseCompact(%q) = %q %q %v; want %q %q %v", c.line, got.Tags, got.Message, got.Continues, c.tags, c.msg, c.cont)
		}
	}
}

func TestParseNDJSON(t *testing.T) {
	for _, c := range []struct {
		line string
		tags []string
		msg  string
	}{
		{`{"subsystem":"com.facebook.react.log","category":"javascript","senderImagePath":"/x/Expo Go","eventMessage":"sent"}`,
			[]string{"com.facebook.react.log", "com.facebook.react.log:javascript"}, "sent"},
		{`{"subsystem":"","category":"","senderImagePath":"/a/Flutter.framework/Flutter","eventMessage":"flutter: x"}`,
			[]string{"Flutter"}, "flutter: x"},
		{`{"subsystem":"user/501/x [1]","category":"","senderImagePath":"/usr/libexec/launchd_sim","eventMessage":"m"}`,
			[]string{"user/501/x [1]"}, "m"},
		{`not json`, nil, ""},
	} {
		got := parseNDJSON([]byte(c.line))
		if !reflect.DeepEqual(got.Tags, c.tags) || string(got.Message) != c.msg || got.Continues {
			t.Errorf("parseNDJSON(%q) = %q %q %v; want %q %q", c.line, got.Tags, got.Message, got.Continues, c.tags, c.msg)
		}
	}
}

const (
	compactRN    = "2026-11-24 08:00:00.000 I  Expo Go[4211:1a2b] [com.facebook.react.log:javascript] porchlight-js sent {\n"
	compactCont  = "  \"n\" = 1;\n"
	compactOther = "2026-11-24 08:00:01.000 Df locationd[1:2] [com.apple.locationd:QA] tick {\n"
	compactSaid  = "2026-11-24 08:00:02.000 Df Expo Go[4211:1a2b] (Expo Go) PORCHLIGHT-JS said\n"
)

func TestSimLogReaderFiltersByTagAndText(t *testing.T) {
	in := "Timestamp               Ty Process[PID:TID]\n" + compactRN + compactCont + compactOther + compactCont + compactSaid
	for _, c := range []struct {
		q    target.LogcatQuery
		want string
	}{
		{target.LogcatQuery{Tag: "com.facebook.react.log"}, compactRN + compactCont},
		{target.LogcatQuery{Tag: "COM.FACEBOOK.REACT.LOG:javascript"}, compactRN + compactCont},
		{target.LogcatQuery{Tag: "expo go"}, compactSaid},
		{target.LogcatQuery{Text: "porchlight-js"}, compactRN + compactCont + compactSaid},
		{target.LogcatQuery{}, in[len("Timestamp               Ty Process[PID:TID]\n"):]},
	} {
		if got := readAll(t, simLogReader(io.NopCloser(strings.NewReader(in)), c.q)); got != c.want {
			t.Errorf("%+v: got %q; want %q", c.q, got, c.want)
		}
	}
}

// Text looks in eventMessage alone, so a word that is also a key or a field
// value does not keep the line.
func TestSimLogReaderNDJSONTextIsTheMessageOnly(t *testing.T) {
	in := `{"subsystem":"porchlight","eventMessage":"tick"}` + "\n" +
		`{"subsystem":"x","eventMessage":"Porchlight sent"}` + "\n" +
		`{"count":2,"finished":1}` + "\n"
	got := readAll(t, simLogReader(io.NopCloser(strings.NewReader(in)), target.LogcatQuery{Format: LogFormatNDJSON, Text: "porchlight"}))
	if got != `{"subsystem":"x","eventMessage":"Porchlight sent"}`+"\n" {
		t.Errorf("got %q", got)
	}
}

// The window stops widening on matching lines, so a match older than the
// last lines of the window is still found.
func TestSimLogTailCountsOnlyMatchingLines(t *testing.T) {
	asked := fakeSimLog(t, map[string]string{
		"60s":  compactRN + compactOther + compactOther,
		"600s": compactRN + compactRN,
	})
	got, err := simLogTail(context.Background(), "u", target.LogcatQuery{Lines: 1, Tag: "com.facebook.react.log"}, 0)
	if err != nil || string(got) != compactRN {
		t.Errorf("got %q, %v", got, err)
	}
	if !reflect.DeepEqual(*asked, []string{"60s"}) {
		t.Errorf("windows = %q; want the first, which holds a match", *asked)
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
